package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/secrets"
	"github.com/shabilullah/wakil-mcp/internal/version"
)

// clientName identifies wakil-mcp to upstream servers.
const clientName = "wakil-mcp"

// ErrNotConnected reports that an upstream has no live session.
var ErrNotConnected = errors.New("upstream is not connected")

// Tool is a proxied tool definition.
//
// Schemas are held as raw JSON rather than as decoded maps. The SDK accepts any
// value for Tool.InputSchema that marshals to a JSON Schema, so keeping the
// bytes the upstream sent means we re-emit them unchanged, including keywords
// we do not model.
type Tool struct {
	Name         string               `json:"name"`
	Title        string               `json:"title,omitempty"`
	Description  string               `json:"description,omitempty"`
	InputSchema  json.RawMessage      `json:"inputSchema,omitempty"`
	OutputSchema json.RawMessage      `json:"outputSchema,omitempty"`
	Annotations  *mcp.ToolAnnotations `json:"annotations,omitempty"`
	Icons        []mcp.Icon           `json:"icons,omitempty"`
	Meta         mcp.Meta             `json:"-"`
}

// Info describes a connected upstream.
type Info struct {
	ProtocolVersion string                  `json:"protocolVersion"`
	Legacy          bool                    `json:"legacy"`
	ServerInfo      *mcp.Implementation     `json:"serverInfo,omitempty"`
	Capabilities    *mcp.ServerCapabilities `json:"capabilities,omitempty"`
	Instructions    string                  `json:"instructions,omitempty"`
	Tools           []Tool                  `json:"tools"`
	Transport       string                  `json:"transport"`
	Endpoint        string                  `json:"endpoint,omitempty"`
	ConnectedAt     time.Time               `json:"connectedAt"`
}

// Conn is one live connection to an upstream MCP server.
type Conn struct {
	key     string
	session *mcp.ClientSession
	proc    *Proc

	stopGrace time.Duration
	log       *slog.Logger

	mu   sync.RWMutex
	info Info

	done      chan struct{}
	doneOnce  sync.Once
	closeOnce sync.Once
	closeErr  error
}

// Def is a resolved upstream definition together with the defaults that apply
// to it.
type Def struct {
	Key      string
	Cfg      *config.Upstream
	Defaults config.ServerDefaults
}

// Key returns the upstream's config key.
func (c *Conn) Key() string { return c.key }

// Done is closed once the connection is no longer usable.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Stderr returns retained child stderr, or "" for network transports.
func (c *Conn) Stderr() string {
	if c.proc == nil {
		return ""
	}
	return c.proc.Stderr()
}

// Pid returns the child process ID for stdio upstreams, or 0.
func (c *Conn) Pid() int {
	if c.proc == nil {
		return 0
	}
	return c.proc.Pid()
}

// Info returns a copy of the connection metadata.
func (c *Conn) Info() Info {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := c.info
	out.Tools = append([]Tool(nil), c.info.Tools...)
	return out
}

// Tools returns the tool list captured at connect time.
func (c *Conn) Tools() []Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]Tool(nil), c.info.Tools...)
}

// RefreshTools re-reads the upstream tool list.
func (c *Conn) RefreshTools(ctx context.Context) ([]Tool, error) {
	tools, err := listTools(ctx, c.session)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.info.Tools = tools
	c.mu.Unlock()
	return tools, nil
}

// CallTool invokes a tool on the upstream, forwarding MRTR fields verbatim.
func (c *Conn) CallTool(ctx context.Context, name string, args json.RawMessage, inputResponses mcp.InputResponseMap, requestState string) (*mcp.CallToolResult, error) {
	if c.session == nil {
		return nil, ErrNotConnected
	}
	params := &mcp.CallToolParams{
		Name: name,
		// Arguments is typed any, so a json.RawMessage marshals through
		// unchanged. That keeps the proxy from reinterpreting tool input.
		Arguments:      args,
		InputResponses: inputResponses,
		RequestState:   requestState,
	}
	return c.session.CallTool(ctx, params)
}

// Close tears down the session and, for stdio upstreams, the child process.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		if c.session != nil {
			c.closeErr = c.session.Close()
		}
		if c.proc != nil {
			if err := c.proc.Stop(c.stopGrace); err != nil && c.closeErr == nil {
				c.closeErr = err
			}
		}
	})
	c.markDone()
	return c.closeErr
}

func (c *Conn) markDone() {
	c.doneOnce.Do(func() { close(c.done) })
}

// watchTransport marks the connection done as soon as the underlying transport
// dies, so the manager notices crashes without polling.
func (c *Conn) watchTransport() {
	if c.proc != nil {
		<-c.proc.Done()
	} else if c.session != nil {
		// A session that ends by itself is worth a line in the log: for an
		// HTTP or SSE upstream it is the only sign that the far end went away.
		if err := c.session.Wait(); err != nil {
			c.log.Debug("upstream session ended", "server", c.key, "error", err)
		} else {
			c.log.Debug("upstream session ended", "server", c.key)
		}
	}
	c.markDone()
}

// listTools reads every page of the upstream tool list.
func listTools(ctx context.Context, cs *mcp.ClientSession) ([]Tool, error) {
	if cs == nil {
		return nil, ErrNotConnected
	}
	var out []Tool
	for t, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		out = append(out, convertTool(t))
	}
	return out, nil
}

// convertTool copies an SDK tool into the proxy's representation, preserving
// schemas byte-for-byte.
func convertTool(t *mcp.Tool) Tool {
	out := Tool{
		Name:        t.Name,
		Title:       t.Title,
		Description: t.Description,
		Annotations: t.Annotations,
		Icons:       t.Icons,
		Meta:        t.Meta,
	}
	out.InputSchema = rawSchema(t.InputSchema)
	out.OutputSchema = rawSchema(t.OutputSchema)
	return out
}

// rawSchema normalizes whatever the SDK decoded for a schema back into bytes.
// The SDK documents that this field carries the default JSON marshalling of the
// server's schema, so re-marshalling is faithful.
func rawSchema(v any) json.RawMessage {
	switch s := v.(type) {
	case nil:
		return nil
	case json.RawMessage:
		return s
	case []byte:
		return json.RawMessage(s)
	default:
		b, err := json.Marshal(s)
		if err != nil {
			return nil
		}
		return b
	}
}

// endpointFor renders a human-readable target for the UI, with secrets
// replaced by their references.
func endpointFor(cfg *config.Upstream) string {
	switch cfg.EffectiveType() {
	case config.TransportStdio:
		s := cfg.Command
		for _, a := range cfg.Args {
			s += " " + a
		}
		return secrets.Redact(s)
	default:
		return cfg.URL
	}
}

// headerRoundTripper injects configured headers into every upstream request.
// Static headers are handled here rather than per-request because the
// Streamable HTTP transport owns request construction.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(h.headers) > 0 {
		// Clone so redirects and retries do not mutate a shared request.
		clone := req.Clone(req.Context())
		for k, v := range h.headers {
			clone.Header.Set(k, v)
		}
		req = clone
	}
	base := h.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// connectOptions carries everything connectServer needs.
type connectOptions struct {
	def       Def
	resolver  *secrets.Resolver
	log       *slog.Logger
	stopGrace time.Duration
	onChanged func()
}

// connectServer establishes a session with one upstream and reads its tool
// list. On any failure the partially built resources are released.
func connectServer(ctx context.Context, opts connectOptions) (*Conn, error) {
	cfg := opts.def.Cfg
	kind := cfg.EffectiveType()

	var (
		transport mcp.Transport
		proc      *Proc
		err       error
	)

	switch kind {
	case config.TransportStdio:
		transport, proc, err = buildStdio(ctx, cfg, opts)
	case config.TransportHTTP, config.TransportSSE:
		transport, err = buildHTTP(cfg, opts, kind == config.TransportSSE)
	default:
		err = fmt.Errorf("unsupported transport %q", kind)
	}
	if err != nil {
		return nil, err
	}

	client := mcp.NewClient(
		&mcp.Implementation{Name: clientName, Version: version.Version},
		&mcp.ClientOptions{
			Logger: opts.log,
			// The SDK's automatic multi-round-trip handling is on by default
			// and would have the proxy answer elicitation and sampling
			// requests itself. A proxy has neither a user nor a model, so it
			// must pass input-required results straight back to the harness,
			// which is the only participant that can answer them. Disabling
			// this leaves inputRequests and requestState intact on the result
			// for the harness to act on and retry.
			MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
			// The SDK subscribes to list changes only when a handler is set,
			// and for modern servers it opens a subscriptions/listen stream.
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				if opts.onChanged != nil {
					opts.onChanged()
				}
			},
		},
	)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		if proc != nil {
			_ = proc.Stop(opts.stopGrace)
		}
		// A server that only speaks the deprecated HTTP+SSE transport will
		// reject a Streamable HTTP connect. Retry once over SSE so that older
		// servers work without extra configuration.
		if kind == config.TransportHTTP && !isSSEEndpoint(cfg.URL) {
			if sseTransport, ok := buildSSEFallback(cfg, opts); ok {
				if s, serr := client.Connect(ctx, sseTransport, nil); serr == nil {
					return finishConn(ctx, opts, s, nil, config.TransportSSE)
				}
			}
		}
		return nil, err
	}

	return finishConn(ctx, opts, session, proc, kind)
}

// finishConn reads the negotiated identity and tool list, then assembles the
// Conn.
func finishConn(ctx context.Context, opts connectOptions, session *mcp.ClientSession, proc *Proc, kind string) (*Conn, error) {
	initRes := session.InitializeResult()

	tools, err := listTools(ctx, session)
	if err != nil {
		_ = session.Close()
		if proc != nil {
			_ = proc.Stop(opts.stopGrace)
		}
		return nil, fmt.Errorf("listing tools: %w", err)
	}

	info := Info{
		Tools:       tools,
		Transport:   kind,
		Endpoint:    endpointFor(opts.def.Cfg),
		ConnectedAt: time.Now(),
	}
	if initRes != nil {
		info.ProtocolVersion = initRes.ProtocolVersion
		info.ServerInfo = initRes.ServerInfo
		info.Capabilities = initRes.Capabilities
		info.Instructions = initRes.Instructions
		info.Legacy = initRes.ProtocolVersion < protocolVersionModern
	}

	c := &Conn{
		key:       opts.def.Key,
		session:   session,
		proc:      proc,
		stopGrace: opts.stopGrace,
		log:       opts.log,
		info:      info,
		done:      make(chan struct{}),
	}
	go c.watchTransport()
	return c, nil
}

// protocolVersionModern is the first revision that dropped the initialize
// handshake. Anything below it is a legacy session.
const protocolVersionModern = "2026-07-28"

// buildStdio spawns a child process and wraps its pipes in an MCP transport.
func buildStdio(_ context.Context, cfg *config.Upstream, opts connectOptions) (mcp.Transport, *Proc, error) {
	command, err := opts.resolver.Resolve(cfg.Command)
	if err != nil {
		return nil, nil, fmt.Errorf("command: %w", err)
	}
	args, err := opts.resolver.ResolveSlice(cfg.Args)
	if err != nil {
		return nil, nil, fmt.Errorf("args: %w", err)
	}
	env, err := opts.resolver.ResolveMap(cfg.Env)
	if err != nil {
		return nil, nil, fmt.Errorf("env: %w", err)
	}
	dir := cfg.Cwd

	proc, err := startProcess(command, args, env, dir)
	if err != nil {
		return nil, nil, err
	}
	transport := &mcp.IOTransport{Reader: proc.stdout, Writer: proc.stdin}
	return transport, proc, nil
}

// buildHTTP constructs a Streamable HTTP transport, or an SSE transport when
// the config asks for it explicitly.
func buildHTTP(cfg *config.Upstream, opts connectOptions, forceSSE bool) (mcp.Transport, error) {
	headers, err := opts.resolver.ResolveMap(cfg.Headers)
	if err != nil {
		return nil, fmt.Errorf("headers: %w", err)
	}
	hc := newHTTPClient(headers)

	if forceSSE {
		return &mcp.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: hc}, nil
	}
	return &mcp.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: hc}, nil
}

// buildSSEFallback prepares an SSE transport for the HTTP fallback path.
func buildSSEFallback(cfg *config.Upstream, opts connectOptions) (mcp.Transport, bool) {
	headers, err := opts.resolver.ResolveMap(cfg.Headers)
	if err != nil {
		return nil, false
	}
	return &mcp.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: newHTTPClient(headers)}, true
}

// newHTTPClient builds a client that injects headers and never applies an
// overall timeout, since SSE streams stay open for the session's lifetime.
func newHTTPClient(headers map[string]string) *http.Client {
	return &http.Client{
		Transport: &headerRoundTripper{base: http.DefaultTransport, headers: headers},
	}
}

// isSSEEndpoint reports whether the URL already points at an SSE endpoint, in
// which case the Streamable HTTP attempt is skipped.
func isSSEEndpoint(u string) bool {
	return strings.HasSuffix(u, "/sse")
}
