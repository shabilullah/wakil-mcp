package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shabilullah/wakil-mcp/internal/audit"
	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/server"
	"github.com/shabilullah/wakil-mcp/internal/testutil"
)

// ---------------------------------------------------------------------------
// Test rig
// ---------------------------------------------------------------------------

// env is a running proxy plus an HTTP client that can reach it.
type env struct {
	app    *server.App
	server *httptest.Server
	config *config.Config
	data   string
}

// newEnv writes a config, starts a proxy and returns it with its HTTP address.
func newEnv(t *testing.T, opts server.Options, body string) *env {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the test config is not valid: %v", err)
	}

	if opts.Logger == nil {
		opts.Logger = discardLogger()
	}
	if opts.ReapInterval == 0 {
		opts.ReapInterval = 200 * time.Millisecond
	}
	if opts.AuditPath == "" {
		opts.AuditPath = filepath.Join(dir, "audit.db")
	}

	app, err := server.New(cfg, opts)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	app.Start(ctx)

	ts := httptest.NewServer(app.Handler())
	t.Cleanup(func() {
		ts.Close()
		cancel()
		if err := app.Close(); err != nil {
			t.Logf("closing the app: %v", err)
		}
	})

	return &env{app: app, server: ts, config: cfg, data: dir}
}

// mcpURL is the Streamable HTTP endpoint of the running proxy.
func (e *env) mcpURL() string { return e.server.URL + server.MCPEndpoint }

// dial connects a real MCP client over HTTP, optionally presenting a token.
func dial(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	return dialVersion(t, url, token, "")
}

// dialVersion connects a client pinned to a protocol revision. An empty
// version lets the client negotiate the newest revision it knows, which is what
// a current harness does.
func dialVersion(t *testing.T, url, token, version string) *mcp.ClientSession {
	t.Helper()
	return dialWanting(t, url, token, version, nil)
}

// dialWanting connects a client with extra options, returning the session.
func dialWanting(t *testing.T, url, token, version string, mutate func(*mcp.ClientOptions)) *mcp.ClientSession {
	t.Helper()

	opts := &mcp.ClientOptions{
		// The harness proves the proxy forwards input-required results rather
		// than answering them itself, so automatic handling is off and the
		// test drives the retry loop by hand.
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	}
	if mutate != nil {
		mutate(opts)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test-harness", Version: "1.0.0"}, opts)

	transport := &mcp.StreamableClientTransport{
		Endpoint: url,
		HTTPClient: &http.Client{
			Transport: headerTransport{token: token, base: http.DefaultTransport},
			Timeout:   30 * time.Second,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var sessionOpts *mcp.ClientSessionOptions
	if version != "" {
		sessionOpts = &mcp.ClientSessionOptions{ProtocolVersion: version}
	}

	session, err := client.Connect(ctx, transport, sessionOpts)
	if err != nil {
		t.Fatalf("connecting to %s: %v", url, err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// headerTransport adds the bearer token to every request.
type headerTransport struct {
	token string
	base  http.RoundTripper
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	if h.token != "" {
		clone.Header.Set("Authorization", "Bearer "+h.token)
	}
	return h.base.RoundTrip(clone)
}

// listTools fetches the full tool list, following cursors.
func listTools(t *testing.T, s *mcp.ClientSession) []*mcp.Tool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var all []*mcp.Tool
	for cursor := ""; ; {
		res, err := s.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			return all
		}
		cursor = res.NextCursor
	}
}

// toolNames returns the exposed names, sorted.
func toolNames(tools []*mcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

// findTool locates a tool by exposed name.
func findTool(tools []*mcp.Tool, name string) *mcp.Tool {
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	return nil
}

// call invokes a tool and returns the result.
func call(t *testing.T, s *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}
	return res
}

// textOf joins the text content blocks of a result.
func textOf(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// callExpectingError invokes a tool and requires a protocol-level error.
func callExpectingError(t *testing.T, s *mcp.ClientSession, name string, args any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err == nil {
		t.Fatalf("calling %s should have failed", name)
	}
	return err
}

// startHTTPUpstream runs a modern MCP server over Streamable HTTP, in process.
// It requires a shared secret header, which proves the proxy forwards the
// headers a server is configured with.
func startHTTPUpstream(t *testing.T) *httptest.Server {
	t.Helper()

	upstream := mcp.NewServer(
		&mcp.Implementation{Name: "remote-fixture", Version: "2.0.0"},
		&mcp.ServerOptions{
			SupportedProtocolVersions: []string{"2026-07-28"},
			Capabilities:              &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		},
	)
	upstream.AddTool(&mcp.Tool{
		Name:        "ping",
		Description: "Reply with pong.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "pong"}},
		}, nil
	})

	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return upstream },
		&mcp.StreamableHTTPOptions{Stateless: true})

	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != remoteSecret {
			http.Error(w, "missing or wrong X-Api-Key", http.StatusUnauthorized)
			return
		}
		streamable.ServeHTTP(w, r)
	})

	ts := httptest.NewServer(guarded)
	t.Cleanup(ts.Close)
	return ts
}

// remoteSecret is the shared secret the in-process HTTP upstream demands.
const remoteSecret = "upstream-shared-secret"

// discardLogger returns a logger that writes nothing except warnings, so test
// output stays readable even though the proxy logs on every connect. Set
// WAKIL_TEST_LOG=1 to see everything, which is what you want when a connection
// test fails.
func discardLogger() *slog.Logger {
	level := slog.LevelWarn
	var w io.Writer = io.Discard
	if os.Getenv("WAKIL_TEST_LOG") != "" {
		level = slog.LevelDebug
		w = os.Stderr
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// ---------------------------------------------------------------------------
// The main end-to-end test
// ---------------------------------------------------------------------------

// TestProxyEndToEnd is the whole thing working together: three upstreams across
// both transports and both protocol eras, reached through a real HTTP client
// speaking MCP to the proxy.
func TestProxyEndToEnd(t *testing.T) {
	fixture := testutil.FixtureBinary(t)
	remote := startHTTPUpstream(t)

	env := newEnv(t, server.Options{}, fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "disableAuth": true},
	  "defaults": {
	    "startPolicy": "eager",
	    "connectTimeout": "30s",
	    "callTimeout": "30s",
	    "idleTimeout": "10m"
	  },
	  "mcpServers": {
	    "legacy": {
	      "description": "hand-written legacy stdio server",
	      "command": %q,
	      "env": {
	        "WAKIL_FIXTURE_MODE": "legacy",
	        "WAKIL_FIXTURE_SCHEMA": "missing"
	      }
	    },
	    "modern": {
	      "description": "SDK-based modern stdio server",
	      "command": %q,
	      "env": {
	        "WAKIL_FIXTURE_MODE": "modern",
	        "WAKIL_FIXTURE_SCHEMA": "rich",
	        "WAKIL_FIXTURE_MRTR": "1"
	      }
	    },
	    "remote": {
	      "description": "Streamable HTTP server",
	      "url": %q,
	      "headers": {"X-Api-Key": %q}
	    }
	  }
	}`, fixture, fixture, remote.URL, remoteSecret))

	// Wait for the eager enumeration to finish before asserting.
	testutil.WaitFor(t, 30*time.Second, func() bool {
		ix := env.app.Index()
		return ix.Len() > 0 && len(ix.ForServer("legacy")) > 0 &&
			len(ix.ForServer("modern")) > 0 && len(ix.ForServer("remote")) > 0
	}, "all three upstreams to contribute tools")

	session := dial(t, env.mcpURL(), "")

	t.Run("all tools are exposed with namespaced names", func(t *testing.T) {
		names := toolNames(listTools(t, session))

		want := []string{
			"legacy.add", "legacy.echo", "legacy.error",
			"modern.add", "modern.confirm", "modern.echo", "modern.error",
			"remote.ping",
			"_wakil.describe_tool", "_wakil.list_servers", "_wakil.search_tools",
		}
		for _, name := range want {
			if !contains(names, name) {
				t.Errorf("tool %q is missing; got %v", name, names)
			}
		}
	})

	t.Run("the config key is the namespace, not serverInfo.name", func(t *testing.T) {
		// The legacy fixture reports itself as "legacy-fixture"; the key is
		// "legacy". Only the key may appear.
		names := toolNames(listTools(t, session))
		for _, name := range names {
			if strings.HasPrefix(name, "legacy-fixture.") {
				t.Errorf("the namespace came from serverInfo.name: %q", name)
			}
		}
	})

	t.Run("a missing input schema is repaired", func(t *testing.T) {
		// The legacy fixture omits inputSchema entirely for its echo tool,
		// which is invalid and would panic the SDK's AddTool.
		tool := findTool(listTools(t, session), "legacy.echo")
		if tool == nil {
			t.Fatal("legacy.echo is missing")
		}
		schema := decodeSchema(t, tool.InputSchema)
		if schema["type"] != "object" {
			t.Errorf("expected a repaired object schema, got %v", tool.InputSchema)
		}
	})

	t.Run("a rich input schema survives unchanged", func(t *testing.T) {
		tool := findTool(listTools(t, session), "modern.echo")
		if tool == nil {
			t.Fatal("modern.echo is missing")
		}
		// The fixture's schema deliberately uses nested objects, $defs,
		// anyOf, defaults and a vendor extension. A proxy that round-trips
		// schemas through a typed struct would silently drop the extras.
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling the schema: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("the schema is not a JSON object: %v (%s)", err, raw)
		}

		if got["title"] != "Echo input" {
			t.Errorf("the schema title was lost: %s", raw)
		}
		if _, ok := got["$defs"]; !ok {
			t.Errorf("$defs was dropped: %s", raw)
		}
		props, _ := got["properties"].(map[string]any)
		text, _ := props["text"].(map[string]any)
		if _, ok := text["x-vendor-extension"]; !ok {
			t.Errorf("an unknown schema keyword was dropped: %s", raw)
		}
		if text["default"] != "hello" {
			t.Errorf("a default value was dropped: %s", raw)
		}
	})

	t.Run("calls reach the legacy stdio upstream", func(t *testing.T) {
		res := call(t, session, "legacy.echo", map[string]any{"text": "hello"})
		if res.IsError {
			t.Fatalf("unexpected tool error: %s", textOf(res))
		}
		if got := textOf(res); got != "echo: hello" {
			t.Errorf("got %q, want %q", got, "echo: hello")
		}
	})

	t.Run("calls reach the modern stdio upstream", func(t *testing.T) {
		res := call(t, session, "modern.add", map[string]any{"a": 2, "b": 3})
		if got := textOf(res); got != "5" {
			t.Errorf("got %q, want %q", got, "5")
		}
	})

	t.Run("calls reach the HTTP upstream through its required header", func(t *testing.T) {
		res := call(t, session, "remote.ping", map[string]any{})
		if got := textOf(res); got != "pong" {
			t.Errorf("got %q, want %q", got, "pong")
		}
	})

	t.Run("a tool-level error is reported as a result, not a failure", func(t *testing.T) {
		res := call(t, session, "modern.error", map[string]any{})
		if !res.IsError {
			t.Fatal("expected isError to be set")
		}
		if !strings.Contains(textOf(res), "deliberate failure") {
			t.Errorf("unexpected error text: %q", textOf(res))
		}
	})

	t.Run("calling an unknown tool fails cleanly", func(t *testing.T) {
		callExpectingError(t, session, "nope.does_not_exist", map[string]any{})
	})

	t.Run("input-required is passed through to the harness", func(t *testing.T) {
		// The proxy must not answer elicitation itself: it has no user. The
		// harness gets the input request and the opaque state, then retries.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		first, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "modern.confirm",
			Arguments: map[string]any{"action": "delete"},
		})
		if err != nil {
			t.Fatalf("first call: %v", err)
		}
		if !first.NeedsInput() {
			t.Fatalf("the harness should have received an input-required result, got: %+v (content %q)",
				first, textOf(first))
		}
		if len(first.InputRequests) == 0 {
			t.Fatal("expected at least one input request")
		}
		if first.RequestState == "" {
			t.Error("expected opaque request state to be forwarded")
		}

		// Answer every requested input and retry with the state echoed back.
		responses := mcp.InputResponseMap{}
		for id := range first.InputRequests {
			responses[id] = &mcp.ElicitResult{
				Action:  "accept",
				Content: map[string]any{"approve": true},
			}
		}

		second, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:           "modern.confirm",
			Arguments:      map[string]any{"action": "delete"},
			InputResponses: responses,
			RequestState:   first.RequestState,
		})
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
		if second.IsError {
			t.Fatalf("retry reported an error: %s", textOf(second))
		}
		if !strings.Contains(textOf(second), first.RequestState) {
			t.Errorf("the upstream did not receive the echoed state: %q", textOf(second))
		}
	})

	t.Run("synthetic tools describe the deployment", func(t *testing.T) {
		res := call(t, session, "_wakil.list_servers", map[string]any{})
		body := textOf(res)
		for _, key := range []string{"legacy", "modern", "remote"} {
			if !strings.Contains(body, key) {
				t.Errorf("list_servers does not mention %q:\n%s", key, body)
			}
		}
	})

	t.Run("synthetic search finds tools by description", func(t *testing.T) {
		res := call(t, session, "_wakil.search_tools", map[string]any{"query": "pong"})
		body := textOf(res)
		if !strings.Contains(body, "remote.ping") {
			t.Errorf("searching for a description did not find remote.ping:\n%s", body)
		}
	})

	t.Run("synthetic describe returns the schema", func(t *testing.T) {
		res := call(t, session, "_wakil.describe_tool", map[string]any{"name": "modern.add"})
		body := textOf(res)
		if !strings.Contains(body, "\"upstreamName\": \"add\"") {
			t.Errorf("describe_tool should report the upstream name:\n%s", body)
		}
		if !strings.Contains(body, "inputSchema") {
			t.Errorf("describe_tool should include the schema:\n%s", body)
		}
	})

	t.Run("every call is recorded in the audit log", func(t *testing.T) {
		testutil.WaitFor(t, 5*time.Second, func() bool {
			events, err := env.app.Audit().Query(context.Background(), audit.Filter{Limit: 500})
			if err != nil {
				return false
			}
			var sawLegacy, sawModern, sawRemote bool
			for _, ev := range events {
				switch ev.Server {
				case "legacy":
					sawLegacy = sawLegacy || ev.Tool == "legacy.echo" && ev.Status == audit.StatusOK
				case "modern":
					sawModern = sawModern || ev.Tool == "modern.add" && ev.Status == audit.StatusOK
				case "remote":
					sawRemote = sawRemote || ev.Tool == "remote.ping" && ev.Status == audit.StatusOK
				}
			}
			return sawLegacy && sawModern && sawRemote
		}, "the audit log to record calls to all three upstreams")
	})
}

// ---------------------------------------------------------------------------
// Focused behaviour tests
// ---------------------------------------------------------------------------

// TestToolListGrowsWhenAnUpstreamAddsTools proves a list-changed notification
// from an upstream reaches harnesses connected to the proxy.
func TestToolListGrowsWhenAnUpstreamAddsTools(t *testing.T) {
	fixture := testutil.FixtureBinary(t)

	env := newEnv(t, server.Options{}, fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "disableAuth": true},
	  "defaults": {"startPolicy": "eager", "connectTimeout": "30s"},
	  "mcpServers": {
	    "growing": {
	      "command": %q,
	      "env": {
	        "WAKIL_FIXTURE_MODE": "modern",
	        "WAKIL_FIXTURE_CHANGED_MS": "1200"
	      }
	    }
	  }
	}`, fixture))

	session := dial(t, env.mcpURL(), "")

	testutil.WaitFor(t, 30*time.Second, func() bool {
		return findTool(listTools(t, session), "growing.echo") != nil
	}, "the initial tool list")

	testutil.WaitFor(t, 30*time.Second, func() bool {
		return findTool(listTools(t, session), "growing.arrived_later") != nil
	}, "the tool added after startup to appear in the aggregate")

	res := call(t, session, "growing.arrived_later", map[string]any{})
	if got := textOf(res); got != "late arrival" {
		t.Errorf("got %q, want %q", got, "late arrival")
	}
}

// TestIdleReapKeepsToolsVisible proves the aggregate never shrinks just because
// an upstream's process was shut down for being idle. This is the difference
// between a usable proxy and one whose tool list flickers.
func TestIdleReapKeepsToolsVisible(t *testing.T) {
	fixture := testutil.FixtureBinary(t)

	env := newEnv(t, server.Options{ReapInterval: 300 * time.Millisecond}, fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "disableAuth": true},
	  "defaults": {
	    "startPolicy": "eager",
	    "connectTimeout": "30s",
	    "callTimeout": "30s",
	    "idleTimeout": "600ms"
	  },
	  "mcpServers": {"idler": {"command": %q}}
	}`, fixture))

	session := dial(t, env.mcpURL(), "")
	testutil.WaitFor(t, 30*time.Second, func() bool {
		return findTool(listTools(t, session), "idler.echo") != nil
	}, "the tool list to populate")

	// Wait for the reaper to stop the idle process.
	testutil.WaitFor(t, 20*time.Second, func() bool {
		snap, ok := env.app.Manager().Snapshot("idler")
		return ok && snap.State == "stopped" && snap.Pid == 0
	}, "the idle upstream to be reaped")

	if findTool(listTools(t, session), "idler.echo") == nil {
		t.Fatal("the tool list shrank when the upstream went idle")
	}
	if env.app.Index().Len() == 0 {
		t.Fatal("the index emptied when the upstream went idle")
	}

	// A call must transparently bring the process back.
	res := call(t, session, "idler.echo", map[string]any{"text": "back"})
	if got := textOf(res); got != "echo: back" {
		t.Errorf("after an idle restart got %q, want %q", got, "echo: back")
	}

	snap, _ := env.app.Manager().Snapshot("idler")
	if snap.State != "ready" {
		t.Errorf("state after a call = %q, want ready", snap.State)
	}
}

// TestCrashIsRecovered proves a killed upstream comes back, and that the
// failure is visible in the meantime rather than silent.
func TestCrashIsRecovered(t *testing.T) {
	fixture := testutil.FixtureBinary(t)

	env := newEnv(t, server.Options{}, fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "disableAuth": true},
	  "defaults": {
	    "startPolicy": "eager",
	    "connectTimeout": "30s",
	    "callTimeout": "10s",
	    "idleTimeout": "10m",
	    "restartBackoff": {"initial": "200ms", "max": "5s", "factor": 2}
	  },
	  "mcpServers": {
	    "chaos": {
	      "command": %q,
	      "env": {"WAKIL_FIXTURE_MODE": "chaos"}
	    }
	  }
	}`, fixture))

	session := dial(t, env.mcpURL(), "")
	testutil.WaitFor(t, 30*time.Second, func() bool {
		return findTool(listTools(t, session), "chaos.echo") != nil
	}, "the chaos fixture to connect")

	// Kill the process. The in-flight call must fail rather than hang, and the
	// failure must be attributable to the upstream.
	err := callExpectingError(t, session, "chaos.crash", map[string]any{})
	if !strings.Contains(err.Error(), "chaos") {
		t.Errorf("the failure should name the upstream: %v", err)
	}

	// The manager notices the session died and reconnects with backoff.
	testutil.WaitFor(t, 60*time.Second, func() bool {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "chaos.echo",
			Arguments: map[string]any{"text": "recovered"},
		})
		return err == nil && !res.IsError && textOf(res) == "echo: recovered"
	}, "the crashed upstream to come back and serve a call")

	if findTool(listTools(t, session), "chaos.echo") == nil {
		t.Error("the tool list lost an entry across a crash")
	}
}

// TestToolFilteringRemovesTools proves include and exclude patterns are applied.
func TestToolFilteringRemovesTools(t *testing.T) {
	fixture := testutil.FixtureBinary(t)

	env := newEnv(t, server.Options{}, fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "disableAuth": true},
	  "defaults": {"startPolicy": "eager", "connectTimeout": "30s"},
	  "mcpServers": {
	    "filtered": {
	      "command": %q,
	      "tools": {"exclude": ["error"], "aliases": {"echo": "shout"}}
	    }
	  }
	}`, fixture))

	session := dial(t, env.mcpURL(), "")
	testutil.WaitFor(t, 30*time.Second, func() bool {
		return env.app.Index().Len() > 0
	}, "the upstream to enumerate")

	names := toolNames(listTools(t, session))
	if !contains(names, "filtered.shout") {
		t.Errorf("the alias was not applied; got %v", names)
	}
	if contains(names, "filtered.echo") {
		t.Errorf("the un-aliased name is still exposed; got %v", names)
	}
	if contains(names, "filtered.error") {
		t.Errorf("the excluded tool is still exposed; got %v", names)
	}

	// The alias must route to the real upstream tool name.
	res := call(t, session, "filtered.shout", map[string]any{"text": "hey"})
	if got := textOf(res); got != "echo: hey" {
		t.Errorf("an aliased call returned %q, want %q", got, "echo: hey")
	}
	callExpectingError(t, session, "filtered.error", map[string]any{})
}

// TestMaxToolsPerServer proves a runaway server cannot flood the tool list.
func TestMaxToolsPerServer(t *testing.T) {
	fixture := testutil.FixtureBinary(t)

	env := newEnv(t, server.Options{}, fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "disableAuth": true},
	  "defaults": {
	    "startPolicy": "eager",
	    "connectTimeout": "30s",
	    "maxToolsPerServer": 1
	  },
	  "mcpServers": {"capped": {"command": %q}}
	}`, fixture))

	testutil.WaitFor(t, 30*time.Second, func() bool {
		return env.app.Index().Len() > 0
	}, "the upstream to enumerate")

	if got := env.app.Index().Len(); got != 1 {
		t.Errorf("the cap is not enforced: %d tools exposed, want 1", got)
	}
}

// TestLazyStart proves a lazy server is not started at boot, that its tools
// appear once it is started, and that calling one starts it on demand.
func TestLazyStart(t *testing.T) {
	fixture := testutil.FixtureBinary(t)

	env := newEnv(t, server.Options{}, fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "disableAuth": true},
	  "defaults": {"startPolicy": "lazy", "connectTimeout": "30s", "idleTimeout": "10m"},
	  "mcpServers": {"lazybones": {"command": %q, "startPolicy": "never"}}
	}`, fixture))

	// "never" means the startup enumeration must skip it entirely.
	time.Sleep(500 * time.Millisecond)
	snap, ok := env.app.Manager().Snapshot("lazybones")
	if !ok {
		t.Fatal("the server should still be configured")
	}
	if snap.State == "ready" {
		t.Errorf("a \"never\" server must not be started at boot; state = %q", snap.State)
	}
}

// TestDisabledServerContributesNothing proves an operator can switch a server
// off without deleting its configuration.
func TestDisabledServerContributesNothing(t *testing.T) {
	fixture := testutil.FixtureBinary(t)

	env := newEnv(t, server.Options{}, fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "disableAuth": true},
	  "defaults": {"startPolicy": "eager", "connectTimeout": "30s"},
	  "mcpServers": {
	    "on": {"command": %q},
	    "off": {"command": %q, "enabled": false}
	  }
	}`, fixture, fixture))

	testutil.WaitFor(t, 30*time.Second, func() bool {
		return env.app.Index().Len() > 0
	}, "the enabled upstream to enumerate")

	if got := env.app.Index().ForServer("off"); len(got) != 0 {
		t.Errorf("a disabled server exposed %d tools", len(got))
	}
	if got := env.app.Index().ForServer("on"); len(got) == 0 {
		t.Error("the enabled server exposed nothing")
	}
}

// TestLegacySSEEndToEnd covers the deprecated HTTP+SSE transport in both
// directions, because it is supported in both: an upstream that offers nothing
// but SSE, and a harness that can speak nothing but SSE. The second is the
// reason `server.allowLegacySSE` exists at all.
//
// Nothing else in the suite exercises this path, so without this test a
// regression here would only be visible to whoever is running the oldest
// harness in the building.
func TestLegacySSEEndToEnd(t *testing.T) {
	sseUpstream := startSSEUpstream(t)

	env := newEnv(t, server.Options{}, fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "disableAuth": true, "allowLegacySSE": true},
	  "defaults": {"startPolicy": "eager", "connectTimeout": "30s", "callTimeout": "30s"},
	  "mcpServers": {
	    "sseup": {
	      "description": "an upstream that only speaks the deprecated transport",
	      "type": "sse",
	      "url": %q
	    }
	  }
	}`, sseUpstream.URL))

	testutil.WaitFor(t, 30*time.Second, func() bool {
		return len(env.app.Index().ForServer("sseup")) > 0
	}, "the SSE upstream to contribute a tool")

	t.Run("an SSE upstream contributes its tools to the aggregate", func(t *testing.T) {
		session := dial(t, env.mcpURL(), "")
		names := toolNames(listTools(t, session))
		if !contains(names, "sseup.ping") {
			t.Fatalf("the SSE upstream's tool is missing: %v", names)
		}

		if got := textOf(call(t, session, "sseup.ping", map[string]any{})); got != "pong" {
			t.Errorf("the call came back as %q, want pong", got)
		}
	})

	t.Run("a harness can connect over the deprecated transport", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		client := mcp.NewClient(&mcp.Implementation{Name: "sse-harness", Version: "1.0.0"}, nil)
		session, err := client.Connect(ctx,
			&mcp.SSEClientTransport{Endpoint: env.server.URL + server.SSEEndpoint}, nil)
		if err != nil {
			t.Fatalf("connecting over SSE: %v", err)
		}
		defer session.Close()

		names := toolNames(listTools(t, session))
		if !contains(names, "sseup.ping") || !contains(names, "_wakil.list_servers") {
			t.Errorf("an SSE harness sees the wrong tools: %v", names)
		}

		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "sseup.ping"})
		if err != nil {
			t.Fatalf("calling over SSE: %v", err)
		}
		if got := textOf(res); got != "pong" {
			t.Errorf("the call came back as %q, want pong", got)
		}
	})
}

// startSSEUpstream runs an in-process MCP server that speaks only the deprecated
// HTTP+SSE transport.
func startSSEUpstream(t *testing.T) *httptest.Server {
	t.Helper()

	upstream := mcp.NewServer(
		&mcp.Implementation{Name: "sse-fixture", Version: "1.0.0"},
		&mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}},
	)
	upstream.AddTool(&mcp.Tool{
		Name:        "ping",
		Description: "Reply with pong, over the deprecated transport.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil
	})

	// The handler serves both halves of the transport: the GET that opens the
	// stream and the POSTs that carry messages back.
	ts := httptest.NewServer(mcp.NewSSEHandler(
		func(*http.Request) *mcp.Server { return upstream },
		&mcp.SSEOptions{},
	))
	t.Cleanup(ts.Close)
	return ts
}

// contains reports whether a string slice holds a value.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// decodeSchema unmarshals a tool input schema.
func decodeSchema(t *testing.T, schema any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshalling a schema: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the schema is not an object: %v (%s)", err, raw)
	}
	return out
}
