// Package proxy exposes every enabled upstream server as one MCP server.
//
// Harnesses connect here instead of being configured per-server. Tool names
// from different servers are disambiguated by prefixing with the operator's
// config key, and a lookup table maps each exposed name back to the upstream
// tool it came from, so no name is ever parsed apart.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shabilullah/wakil-mcp/internal/aggregate"
	"github.com/shabilullah/wakil-mcp/internal/audit"
	"github.com/shabilullah/wakil-mcp/internal/auth"
	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/logging"
	"github.com/shabilullah/wakil-mcp/internal/upstream"
)

// SyntheticToolPrefix is the reserved namespace for tools the proxy provides
// itself. Config validation refuses server keys that would land in it.
const SyntheticToolPrefix = config.ReservedNamespace

// Tool names the proxy provides on top of the aggregated set.
const (
	ToolSearch     = SyntheticToolPrefix + ".search_tools"
	ToolListServer = SyntheticToolPrefix + ".list_servers"
	ToolGetTool    = SyntheticToolPrefix + ".describe_tool"
)

// syntheticHolder is the name collisions against a reserved tool are attributed
// to, so the web UI can explain who already holds the name.
const syntheticHolder = "wakil-mcp"

// syntheticTools is the set of tools the proxy provides itself. Membership is
// tested by exact name rather than by prefix, so nothing an upstream server
// happens to call "_wakil..." can pass for one.
var syntheticTools = map[string]bool{
	ToolSearch:     true,
	ToolListServer: true,
	ToolGetTool:    true,
}

// isSyntheticTool reports whether name is one of the proxy's own tools.
func isSyntheticTool(name string) bool { return syntheticTools[name] }

// Options configures a Proxy.
type Options struct {
	// Manager supplies upstream connections. Required.
	Manager *upstream.Manager
	// Defaults supplies namespace and timeout settings.
	Defaults config.ServerDefaults
	// Logger receives request-level events.
	Logger *slog.Logger
	// Version is the proxy's own version, reported as serverInfo.version.
	Version string
	// Registry resolves caller profiles. May be nil, which disables
	// authorisation and treats every caller as local.
	Registry *auth.Registry
	// Recorder receives audit events. May be nil.
	Recorder audit.Recorder
	// Instructions is sent to clients during initialization.
	Instructions string
	// Ready, when set, is consulted before the first tools/list is answered.
	// It lets the proxy hold that request until the upstreams have been
	// enumerated, so a harness connecting during start-up still receives a
	// complete tool list instead of a partial one it might cache.
	Ready func(context.Context) error
}

// Proxy is the aggregated MCP server.
type Proxy struct {
	opts   Options
	server *mcp.Server

	// mu guards index and registered. It is never held while calling into the
	// SDK, which takes its own locks.
	mu         sync.Mutex
	index      *aggregate.Index
	registered map[string]aggregate.Entry
}

// New builds a Proxy and registers its synthetic tools.
func New(opts Options) *Proxy {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Recorder == nil {
		opts.Recorder = audit.NopRecorder{}
	}
	if opts.Instructions == "" {
		opts.Instructions = defaultInstructions
	}

	p := &Proxy{
		opts:       opts,
		registered: map[string]aggregate.Entry{},
		index:      aggregate.NewBuilder(opts.Defaults).Build(),
	}

	p.server = mcp.NewServer(
		&mcp.Implementation{Name: "wakil-mcp", Version: opts.Version},
		&mcp.ServerOptions{
			Instructions: opts.Instructions,
			// The SDK's own session messages are noise here: see logging.Quiet.
			Logger: logging.Quiet(opts.Logger, slog.LevelWarn),
			// Tools are the whole point, and the list changes as upstreams
			// start and stop, so listChanged is advertised.
			Capabilities: &mcp.ServerCapabilities{
				Tools: &mcp.ToolCapabilities{ListChanged: true},
			},
		},
	)

	// Authorization runs first so a denied request never reaches a handler.
	p.server.AddReceivingMiddleware(p.authorizeMiddleware)

	p.registerSyntheticTools()
	return p
}

// SetDefaults updates the namespace and timeout settings used by the next sync.
//
// Without this the aggregator would keep whatever defaults the process started
// with, so a reload that changed the namespace separator or prefix would not
// reach the exposed tool names.
func (p *Proxy) SetDefaults(defaults config.ServerDefaults) {
	p.mu.Lock()
	p.opts.Defaults = defaults
	p.mu.Unlock()
}

// defaults returns the settings for the next sync.
func (p *Proxy) defaults() config.ServerDefaults {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opts.Defaults
}

// Server returns the underlying MCP server, for mounting on a transport.
func (p *Proxy) Server() *mcp.Server { return p.server }

// Index returns the current exposed tool index.
func (p *Proxy) Index() *aggregate.Index {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.index
}

// defaultInstructions tells the model how the namespace is laid out. Harnesses
// pass this through to the model, so it is worth being explicit.
const defaultInstructions = `This server aggregates multiple MCP servers into one namespace.

Tool names are prefixed with the upstream server's name and a dot, so ` +
	"`github.create_issue`" + ` is the ` + "`create_issue`" + ` tool of the server configured as ` + "`github`" + `.

The ` + SyntheticToolPrefix + ` namespace is reserved for this proxy:
  ` + ToolListServer + `  list configured servers and their state
  ` + ToolSearch + `      search tool names and descriptions
  ` + ToolGetTool + `     show one tool's full definition

Call a namespaced tool directly; routing and upstream startup are automatic.`

// Sync rebuilds the exposed tool set from the current upstream snapshots and
// reconciles it against the live server. It returns the new index.
//
// Tools whose origin changed are re-registered, which replaces the existing
// registration. The SDK coalesces list-changed notifications, so a bulk
// re-registration produces at most one notification per method.
func (p *Proxy) Sync(snaps []upstream.Snapshot) *aggregate.Index {
	b := aggregate.NewBuilder(p.defaults())
	// The synthetic tools are registered by name, so an upstream tool that
	// resolves to the same name would silently replace one of them. Reserving
	// the names gives the clash a suffix and a line in the UI instead.
	b.Reserve(syntheticHolder, ToolSearch, ToolListServer, ToolGetTool)
	for _, s := range snaps {
		if !s.Enabled || s.Config == nil {
			continue
		}
		b.AddServer(s.Key, s.Config, s.Tools)
	}
	next := b.Build()

	p.mu.Lock()
	previous := p.registered

	var changed []aggregate.Entry
	for _, e := range next.Entries() {
		prev, existed := previous[e.Exposed]
		if !existed || prev.ServerKey != e.ServerKey || prev.UpstreamName != e.UpstreamName {
			changed = append(changed, e)
		}
	}
	var removed []string
	for name := range previous {
		if _, ok := next.Lookup(name); !ok {
			removed = append(removed, name)
		}
	}

	registered := make(map[string]aggregate.Entry, next.Len())
	for _, e := range next.Entries() {
		registered[e.Exposed] = e
	}

	p.index = next
	p.registered = registered
	p.mu.Unlock()

	// Register before removing so a name moving between servers is never
	// briefly absent from the list.
	for _, e := range changed {
		p.server.AddTool(p.sdkTool(e), p.toolHandler(e))
	}
	if len(removed) > 0 {
		p.server.RemoveTools(removed...)
	}

	p.opts.Logger.Debug("tool index synced",
		"exposed", next.Len(),
		"addedOrUpdated", len(changed),
		"removed", len(removed),
		"collisions", len(next.Collisions()))
	return next
}

// sdkTool converts an exposed entry into the SDK tool definition. Schemas are
// passed through as raw JSON so the upstream's schema reaches the harness
// unmodified.
func (p *Proxy) sdkTool(e aggregate.Entry) *mcp.Tool {
	tool := &mcp.Tool{
		Name:         e.Exposed,
		Title:        e.Tool.Title,
		Description:  e.Tool.Description,
		InputSchema:  schemaField(e.Tool.InputSchema),
		OutputSchema: schemaField(e.Tool.OutputSchema),
		Annotations:  e.Tool.Annotations,
		Icons:        e.Tool.Icons,
	}

	// inputSchema is the one schema the protocol requires, and the SDK's field
	// carries no omitempty, so a nil here would go out as the literal null.
	// The aggregate substitutes a schema for a missing one; this is belt and
	// braces for a future caller that forgets to.
	if tool.InputSchema == nil {
		tool.InputSchema = json.RawMessage(fallbackInputSchema)
	}

	return tool
}

// fallbackInputSchema stands in when an entry carries no input schema at all.
const fallbackInputSchema = `{"type":"object"}`

// schemaField converts a raw schema into a value for the SDK's any-typed schema
// fields.
//
// The conversion is not cosmetic. A json.RawMessage is a byte slice, so an
// empty one is a non-nil interface holding a nil slice: it survives omitempty
// and marshals as the JSON literal null. A null schema is not valid, and a
// strict client rejects the entire tools/list over it — the official
// TypeScript SDK fails with "expected object, received null" — so an absent
// schema has to stay absent rather than become null. The aggregate drops null
// schemas already; this keeps that guarantee local to the conversion.
func schemaField(raw json.RawMessage) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullSchema) {
		return nil
	}
	return raw
}

// nullSchema is the JSON literal an absent schema must never be turned into.
var nullSchema = []byte("null")

// errUnknownTool is returned when no server provides the requested name.
func errUnknownTool(name string) error {
	return &jsonrpc.Error{
		Code:    jsonrpc.CodeInvalidParams,
		Message: fmt.Sprintf("unknown tool %q", name),
	}
}

// errDenied is returned when a client's profile forbids the call.
func errDenied(name string) error {
	return &jsonrpc.Error{
		Code:    jsonrpc.CodeInvalidParams,
		Message: fmt.Sprintf("not authorized to call tool %q", name),
	}
}

// errUpstream wraps a failure that happened on the far side, so the harness can
// tell a proxy problem from an upstream problem.
func errUpstream(server, tool string, err error) error {
	return &jsonrpc.Error{
		Code:    jsonrpc.CodeInternalError,
		Message: fmt.Sprintf("upstream %s tool %s failed: %v", server, tool, err),
	}
}

// toolHandler builds the call handler for one exposed tool. The entry is
// captured, so the handler never has to re-resolve a name.
func (p *Proxy) toolHandler(e aggregate.Entry) mcp.ToolHandler {
	serverKey := e.ServerKey
	upstreamName := e.UpstreamName
	exposed := e.Exposed

	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		started := time.Now()

		var args []byte
		if req != nil && req.Params != nil {
			args = req.Params.Arguments
		}

		res, err := p.forward(ctx, serverKey, upstreamName, req)

		p.record(ctx, req, audit.Event{
			Client:       profileName(ctx),
			Server:       serverKey,
			Tool:         exposed,
			UpstreamTool: upstreamName,
			Status:       statusFor(res, err),
			Error:        errText(err, res),
			DurationMS:   time.Since(started).Milliseconds(),
		})
		p.opts.Logger.Debug("tool call",
			"tool", exposed, "server", serverKey,
			"bytes", len(args),
			"ms", time.Since(started).Milliseconds(),
			"error", err)

		return res, err
	}
}

// forward acquires the upstream and performs the call, forwarding every MRTR
// field so multi-round-trip flows survive the hop.
func (p *Proxy) forward(ctx context.Context, serverKey, upstreamName string, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// The call timeout bounds the whole hop, including a lazy start.
	timeout := p.opts.Defaults.EffectiveCallTimeout()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := p.opts.Manager.Acquire(cctx, serverKey)
	if err != nil {
		return nil, errUpstream(serverKey, upstreamName, err)
	}
	defer p.opts.Manager.Release(serverKey)

	var (
		args           []byte
		inputResponses mcp.InputResponseMap
		requestState   string
	)
	if req != nil && req.Params != nil {
		args = req.Params.Arguments
		inputResponses = req.Params.InputResponses
		requestState = req.Params.RequestState
	}

	res, err := conn.CallTool(cctx, upstreamName, args, inputResponses, requestState)
	if err != nil {
		return nil, errUpstream(serverKey, upstreamName, err)
	}
	return res, nil
}

// statusFor classifies the outcome of a call for the audit log.
func statusFor(res *mcp.CallToolResult, err error) string {
	switch {
	case err != nil:
		return audit.StatusError
	case res != nil && res.IsError:
		// The tool ran and reported a failure; that is still a completed call,
		// but recording it as an error is what an operator wants to see.
		return audit.StatusError
	default:
		return audit.StatusOK
	}
}

// errText renders a failure for the audit log.
func errText(err error, res *mcp.CallToolResult) string {
	if err != nil {
		return err.Error()
	}
	if res != nil && res.IsError {
		var parts []string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				parts = append(parts, tc.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
		return "tool reported an error"
	}
	return ""
}

// record writes an audit event, filling in the client name from the context.
func (p *Proxy) record(ctx context.Context, _ *mcp.CallToolRequest, e audit.Event) {
	e.Client = profileName(ctx)
	if e.Client == "" {
		e.Client = "anonymous"
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	p.opts.Recorder.Record(&e)
}

// profileFromContext returns the caller's profile, defaulting to unrestricted
// when the request arrived in-process. The authorize middleware always stores a
// profile, so the fallback only applies to callers that bypassed it.
func profileFromContext(ctx context.Context) auth.Profile {
	if p, ok := auth.ProfileFromContext(ctx); ok {
		return p
	}
	return auth.LocalProfile()
}

// profileName returns the caller's profile name from the context.
func profileName(ctx context.Context) string {
	if p, ok := auth.ProfileFromContext(ctx); ok {
		return p.Name
	}
	return ""
}

// authorizeMiddleware enforces the caller's allowlist and filters the tool
// list they see. It runs for every inbound request.
func (p *Proxy) authorizeMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		profile, err := p.profileFor(ctx, req)
		if err != nil {
			return nil, err
		}
		ctx = auth.WithProfile(ctx, profile)

		switch method {
		case "tools/list":
			if p.opts.Ready != nil {
				if err := p.opts.Ready(ctx); err != nil {
					return nil, err
				}
			}
			res, err := next(ctx, method, req)
			if err != nil {
				return res, err
			}
			if ltr, ok := res.(*mcp.ListToolsResult); ok {
				ltr.Tools = p.filterTools(profile, ltr.Tools)
			}
			return res, nil

		case "tools/call":
			name := callName(req)
			entry, ok := p.Index().Lookup(name)
			if !ok {
				// Synthetic and unknown names are handled by their own
				// handlers; only aggregated tools are authorized here.
				if !isSyntheticTool(name) {
					return nil, errUnknownTool(name)
				}
				return next(ctx, method, req)
			}
			if !profile.AllowsTool(entry.ServerKey, entry.Exposed, entry.UpstreamName) {
				p.record(ctx, nil, audit.Event{
					Server:       entry.ServerKey,
					Tool:         entry.Exposed,
					UpstreamTool: entry.UpstreamName,
					Status:       audit.StatusDenied,
					Error:        "not permitted by the caller's allow list",
				})
				return nil, errDenied(name)
			}
		}
		return next(ctx, method, req)
	}
}

// profileFor resolves the caller's profile.
//
// A token-verified request carries TokenInfo. A request that reached the
// protocol layer over HTTP without TokenInfo means the auth middleware did not
// run, which is only acceptable when authentication is switched off. A request
// with no transport information at all is in-process and therefore trusted.
func (p *Proxy) profileFor(ctx context.Context, req mcp.Request) (auth.Profile, error) {
	if extra := req.GetExtra(); extra != nil && extra.TokenInfo != nil {
		if prof, ok := auth.ProfileFromTokenInfo(extra.TokenInfo); ok {
			return prof, nil
		}
		return auth.Profile{}, errUnauthorized("token carried no profile")
	}
	if p.opts.Registry != nil && !p.opts.Registry.AuthDisabled() {
		if extra := req.GetExtra(); extra != nil && extra.Header != nil {
			return auth.Profile{}, errUnauthorized("missing credentials")
		}
	}
	if prof, ok := auth.ProfileFromContext(ctx); ok {
		return prof, nil
	}
	return auth.LocalProfile(), nil
}

// errUnauthorized renders a protocol-level authorization failure.
func errUnauthorized(reason string) error {
	return &jsonrpc.Error{
		Code:    jsonrpc.CodeInvalidParams,
		Message: "unauthorized: " + reason,
	}
}

// callName extracts the tool name from a tools/call request.
func callName(req mcp.Request) string {
	if ctr, ok := req.(*mcp.CallToolRequest); ok && ctr.Params != nil {
		return ctr.Params.Name
	}
	// Fall back to a generic peek so the middleware never panics on an
	// unexpected request shape.
	type named interface{ GetParams() mcp.Params }
	if n, ok := req.(named); ok {
		if raw, ok := n.GetParams().(*mcp.CallToolParamsRaw); ok {
			return raw.Name
		}
	}
	return ""
}

// filterTools removes tools the profile may not see.
func (p *Proxy) filterTools(profile auth.Profile, tools []*mcp.Tool) []*mcp.Tool {
	if profile.Admin {
		return tools
	}
	ix := p.Index()
	out := tools[:0]
	for _, t := range tools {
		if isSyntheticTool(t.Name) {
			out = append(out, t)
			continue
		}
		e, ok := ix.Lookup(t.Name)
		if !ok {
			continue
		}
		if profile.AllowsTool(e.ServerKey, e.Exposed, e.UpstreamName) {
			out = append(out, t)
		}
	}
	return out
}

// errBadRequest renders a malformed synthetic-tool argument.
func errBadRequest(format string, args ...any) error {
	return &jsonrpc.Error{
		Code:    jsonrpc.CodeInvalidParams,
		Message: fmt.Sprintf(format, args...),
	}
}

// errInternal wraps an unexpected internal failure.
var errInternal = errors.New("internal error")
