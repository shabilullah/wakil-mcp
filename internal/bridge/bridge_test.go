package bridge_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shabilullah/wakil-mcp/internal/bridge"
	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/server"
	"github.com/shabilullah/wakil-mcp/internal/testutil"
)

// ---------------------------------------------------------------------------
// Daemon side
// ---------------------------------------------------------------------------

// daemon is a running wakil-mcp proxy backed by real fixture upstreams.
type daemon struct {
	app  *server.App
	url  string
	path string
}

// startDaemon writes a config, starts the proxy and returns its MCP endpoint.
func startDaemon(t *testing.T, body string) *daemon {
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

	app, err := server.New(cfg, server.Options{
		Logger:       testLogger(),
		AuditPath:    filepath.Join(dir, "audit.db"),
		ReapInterval: 200 * time.Millisecond,
	})
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

	return &daemon{app: app, url: ts.URL + server.MCPEndpoint, path: path}
}

// rewrite replaces the config file on disk. Nothing tells the daemon; picking
// the change up is the behaviour under test.
func (d *daemon) rewrite(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(d.path, []byte(body), 0o600); err != nil {
		t.Fatalf("rewriting config: %v", err)
	}
}

// exposed counts the tools the daemon is currently publishing.
func (d *daemon) exposed() int { return d.app.Index().Len() }

// testLogger writes to stderr only when WAKIL_TEST_LOG is set, because stdout is
// asserted on.
func testLogger() *slog.Logger {
	if os.Getenv("WAKIL_TEST_LOG") == "" {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// ---------------------------------------------------------------------------
// Harness side
// ---------------------------------------------------------------------------

// harness drives the bridge the way a stdio client does: newline-delimited
// JSON-RPC written to a pipe, messages read back from another.
type harness struct {
	in      *io.PipeWriter
	inRead  *io.PipeReader
	out     *io.PipeWriter
	replies chan map[string]any
	notices chan map[string]any
	junk    chan string
}

func newHarness() *harness {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()

	h := &harness{
		in:      inWriter,
		inRead:  inReader,
		out:     outWriter,
		replies: make(chan map[string]any, 64),
		notices: make(chan map[string]any, 64),
		junk:    make(chan string, 8),
	}

	go func() {
		scanner := bufio.NewScanner(outReader)
		scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)

		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}

			var message map[string]any
			if err := json.Unmarshal(line, &message); err != nil {
				h.junk <- string(line)
				continue
			}
			// Replies carry an id; notifications do not.
			if _, isReply := message["id"]; isReply {
				h.replies <- message
				continue
			}
			h.notices <- message
		}

		close(h.replies)
		close(h.notices)
	}()

	return h
}

// reader returns the stream the bridge reads from.
func (h *harness) reader() io.Reader { return h.inRead }

// writer returns the stream the bridge writes to.
func (h *harness) writer() io.Writer { return h.out }

// send writes one message to the bridge.
func (h *harness) send(t *testing.T, message map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("encoding a message: %v", err)
	}
	if _, err := h.in.Write(append(encoded, '\n')); err != nil {
		t.Fatalf("writing to the bridge: %v", err)
	}
}

// closeInput ends the harness's message stream, which is how a stdio client
// shuts the bridge down.
func (h *harness) closeInput() { _ = h.in.Close() }

// await waits for a reply to one request id.
func (h *harness) await(t *testing.T, id string) map[string]any {
	t.Helper()
	return h.awaitFrom(t, h.replies, "a reply to "+id, func(message map[string]any) bool {
		return fmt.Sprint(message["id"]) == id
	})
}

// awaitNotification waits for a notification with a given method.
func (h *harness) awaitNotification(t *testing.T, method string) map[string]any {
	t.Helper()
	return h.awaitFrom(t, h.notices, method, func(message map[string]any) bool {
		return message["method"] == method
	})
}

// awaitFrom waits for a message matching a predicate.
func (h *harness) awaitFrom(t *testing.T, ch <-chan map[string]any, want string, match func(map[string]any) bool) map[string]any {
	t.Helper()

	deadline := time.After(30 * time.Second)
	for {
		select {
		case message, open := <-ch:
			if !open {
				t.Fatalf("the bridge stopped writing while waiting for %s", want)
			}
			if match(message) {
				return message
			}
		case line := <-h.junk:
			t.Fatalf("the bridge wrote something that is not JSON-RPC: %s", line)
		case <-deadline:
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

// result returns the result object of a reply, failing if the reply is an error.
func result(t *testing.T, reply map[string]any) map[string]any {
	t.Helper()

	if failure, ok := reply["error"]; ok {
		t.Fatalf("the request failed: %v", failure)
	}
	out, _ := reply["result"].(map[string]any)
	if out == nil {
		t.Fatalf("the reply has no result object: %v", reply)
	}
	return out
}

// ---------------------------------------------------------------------------
// Test rig helpers
// ---------------------------------------------------------------------------

// runBridge starts the bridge against a daemon and returns the harness plus a
// channel that receives the bridge's exit error.
func runBridge(t *testing.T, url string, poll time.Duration) (*harness, <-chan error) {
	t.Helper()

	h := newHarness()
	done := make(chan error, 1)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		done <- bridge.Run(ctx, bridge.Options{
			URL:       url,
			Logger:    testLogger(),
			In:        h.reader(),
			Out:       h.writer(),
			PollEvery: poll,
		})
	}()

	return h, done
}

// handshake runs initialize and the initialized notification, which is what a
// stdio client does before anything else.
func handshake(t *testing.T, h *harness) {
	t.Helper()

	h.send(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      "init",
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "stdio-harness", "version": "1.0.0"},
		},
	})

	reply := h.await(t, "init")
	version, _ := result(t, reply)["protocolVersion"].(string)
	if version == "" {
		t.Fatalf("the handshake did not report a protocol version: %v", reply)
	}

	// A notification gets no reply at all, so there is nothing to wait for.
	h.send(t, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
}

// listedTools asks for the tool list over the bridge.
func listedTools(t *testing.T, h *harness, id string) []string {
	t.Helper()

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/list"})
	reply := h.await(t, id)

	raw, _ := result(t, reply)["tools"].([]any)
	names := make([]string, 0, len(raw))
	for _, item := range raw {
		tool, _ := item.(map[string]any)
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// call invokes a tool over the bridge.
func call(t *testing.T, h *harness, id, name string, args map[string]any) map[string]any {
	t.Helper()

	h.send(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	})
	return result(t, h.await(t, id))
}

// textOf joins the text content of a call result.
func textOf(t *testing.T, res map[string]any) string {
	t.Helper()

	raw, _ := res["content"].([]any)
	var parts []string
	for _, item := range raw {
		block, _ := item.(map[string]any)
		if text, ok := block["text"].(string); ok {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "")
}

// modernConfig is a daemon config with one SDK-based stdio fixture.
func modernConfig(fixture string) string {
	return fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "disableAuth": true},
	  "defaults": {
	    "startPolicy": "eager",
	    "connectTimeout": "30s",
	    "callTimeout": "30s",
	    "idleTimeout": "10m"
	  },
	  "mcpServers": {
	    "modern": {
	      "command": %q,
	      "env": {"WAKIL_FIXTURE_MODE": "modern"}
	    }
	  }
	}`, fixture)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestBridgeRelaysStdioToDaemon is the core contract: a stdio harness that
// cannot speak HTTP still reaches every aggregated tool.
func TestBridgeRelaysStdioToDaemon(t *testing.T) {
	fixture := testutil.FixtureBinary(t)
	d := startDaemon(t, modernConfig(fixture))

	testutil.WaitFor(t, 30*time.Second, func() bool { return d.exposed() > 0 },
		"the daemon to publish its upstream tools")

	h, done := runBridge(t, d.url, 200*time.Millisecond)
	handshake(t, h)

	names := listedTools(t, h, "list-1")
	for _, want := range []string{"modern.echo", "_wakil.list_servers", "_wakil.search_tools"} {
		if !contains(names, want) {
			t.Errorf("tool %q is missing over the bridge; got %v", want, names)
		}
	}

	t.Run("a tool call round-trips", func(t *testing.T) {
		res := call(t, h, "call-1", "modern.echo", map[string]any{"text": "through the bridge"})
		if text := textOf(t, res); !strings.Contains(text, "through the bridge") {
			t.Errorf("the echo did not come back intact: %q", text)
		}
	})

	t.Run("an upstream error is reported as a failed call", func(t *testing.T) {
		res := call(t, h, "call-2", "modern.error", map[string]any{"message": "expected failure"})
		if isError, _ := res["isError"].(bool); !isError {
			t.Errorf("the upstream error was not marked as one: %v", res)
		}
	})

	t.Run("an unknown tool is a JSON-RPC error", func(t *testing.T) {
		h.send(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      "call-3",
			"method":  "tools/call",
			"params":  map[string]any{"name": "modern.nope", "arguments": map[string]any{}},
		})
		reply := h.await(t, "call-3")
		if _, ok := reply["error"]; !ok {
			t.Errorf("calling an unknown tool should have failed: %v", reply)
		}
	})

	t.Run("closing stdin stops the bridge", func(t *testing.T) {
		h.closeInput()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the bridge reported an error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the bridge did not exit after its input stream closed")
		}
	})
}

// TestBridgeNotifiesOnToolListChange covers the translation a stdio harness
// needs: it cannot be pushed a notification over HTTP, so the bridge polls and
// synthesises one.
func TestBridgeNotifiesOnToolListChange(t *testing.T) {
	fixture := testutil.FixtureBinary(t)

	// The second upstream starts switched off, then is switched on behind the
	// daemon's back.
	body := fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "disableAuth": true},
	  "defaults": {
	    "startPolicy": "eager",
	    "connectTimeout": "30s",
	    "callTimeout": "30s",
	    "idleTimeout": "10m"
	  },
	  "mcpServers": {
	    "modern": {
	      "command": %q,
	      "env": {"WAKIL_FIXTURE_MODE": "modern"}
	    },
	    "extra": {
	      "enabled": false,
	      "command": %q,
	      "env": {"WAKIL_FIXTURE_MODE": "legacy"}
	    }
	  }
	}`, fixture, fixture)

	d := startDaemon(t, body)
	testutil.WaitFor(t, 30*time.Second, func() bool { return d.exposed() > 0 },
		"the daemon to publish its upstream tools")

	h, _ := runBridge(t, d.url, 200*time.Millisecond)
	handshake(t, h)

	before := listedTools(t, h, "list-1")
	if contains(before, "extra.echo") {
		t.Fatalf("the disabled server is already exposed: %v", before)
	}
	if !contains(before, "modern.echo") {
		t.Fatalf("the enabled server is not exposed: %v", before)
	}

	// Enable the second server by editing the file. The daemon is not told.
	enabled := strings.Replace(body, `"enabled": false,`, `"enabled": true,`, 1)
	if enabled == body {
		t.Fatal("the test config did not change")
	}
	d.rewrite(t, enabled)

	// The daemon has to notice the edit, connect, and then the bridge has to
	// notice the daemon's list changed.
	h.awaitNotification(t, "notifications/tools/list_changed")

	after := listedTools(t, h, "list-2")
	if !contains(after, "extra.echo") {
		t.Errorf("the newly enabled server is missing after the change: %v", after)
	}
	if !contains(after, "modern.echo") {
		t.Errorf("the original server disappeared after the change: %v", after)
	}
}

// TestBridgeReportsAnUnreachableDaemon checks that a daemon outage surfaces to
// the harness as a failed call rather than a vanished child process.
func TestBridgeReportsAnUnreachableDaemon(t *testing.T) {
	// A port that nothing is listening on: the listener is closed immediately.
	dead := "http://" + testutil.FreeAddr(t) + server.MCPEndpoint

	h, _ := runBridge(t, dead, time.Hour)

	// The handshake is the first thing to fail, and it has to fail in a way
	// the harness can report.
	h.send(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      "init",
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "stdio-harness", "version": "1.0.0"},
		},
	})
	explainFailure(t, h.await(t, "init"))

	h.send(t, map[string]any{"jsonrpc": "2.0", "id": "list-1", "method": "tools/list"})
	message := explainFailure(t, h.await(t, "list-1"))

	if !strings.Contains(message, "unreachable") {
		t.Errorf("the error does not explain the problem: %q", message)
	}
}

// explainFailure returns the message of a JSON-RPC error reply, failing the
// test if the reply was not an error.
func explainFailure(t *testing.T, reply map[string]any) string {
	t.Helper()

	failure, ok := reply["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON-RPC error, got %v", reply)
	}
	message, _ := failure["message"].(string)
	return message
}

// TestBridgeHasNoSideChannels guards the reason the relay is frame-level: the
// bridge must not write anything to stdout that is not JSON-RPC, because a
// harness parses that stream.
func TestBridgeHasNoSideChannels(t *testing.T) {
	fixture := testutil.FixtureBinary(t)
	d := startDaemon(t, modernConfig(fixture))

	testutil.WaitFor(t, 30*time.Second, func() bool { return d.exposed() > 0 },
		"the daemon to publish its upstream tools")

	// Debug logging is on, which is the most likely source of stray output.
	var out bytes.Buffer
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- bridge.Run(ctx, bridge.Options{
			URL: d.url,
			// Tee the protocol stream: the harness gets a working bridge and
			// the test gets every byte that was written.
			Logger:    testLogger(),
			In:        h.reader(),
			Out:       io.MultiWriter(h.writer(), &out),
			PollEvery: 200 * time.Millisecond,
		})
	}()

	handshake(t, h)
	listedTools(t, h, "list-1")
	h.closeInput()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge did not exit")
	}

	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var message map[string]any
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Errorf("the bridge wrote a non-JSON-RPC line to stdout: %q", line)
		}
	}
}

// contains reports whether a slice holds a value.
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
