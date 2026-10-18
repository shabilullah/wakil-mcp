package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/server"
	"github.com/shabilullah/wakil-mcp/internal/testutil"
)

// mintToken creates a credential and the hash that goes into the config.
func mintToken(t *testing.T) (token, hash string) {
	t.Helper()
	token, hash, err := config.GenerateToken()
	if err != nil {
		t.Fatalf("generating a token: %v", err)
	}
	return token, hash
}

// TestLegacyHarnessOverHTTP proves a client that only speaks the 2025-06-18
// handshake works against the proxy, since that is what most harnesses in the
// wild still do.
func TestLegacyHarnessOverHTTP(t *testing.T) {
	fixture := testutil.FixtureBinary(t)

	env := newEnv(t, server.Options{}, fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "disableAuth": true},
	  "defaults": {"startPolicy": "eager", "connectTimeout": "30s"},
	  "mcpServers": {"legacy": {"command": %q, "env": {"WAKIL_FIXTURE_MODE": "legacy"}}}
	}`, fixture))

	testutil.WaitFor(t, 30*time.Second, func() bool {
		return env.app.Index().Len() > 0
	}, "the upstream to enumerate")

	// Pinning the revision skips server/discover and uses initialize, which is
	// exactly what an older harness does.
	session := dialVersion(t, env.mcpURL(), "", "2025-06-18")

	if got := session.InitializeResult().ProtocolVersion; got != "2025-06-18" {
		t.Errorf("negotiated version = %q, want 2025-06-18", got)
	}

	names := toolNames(listTools(t, session))
	if !contains(names, "legacy.echo") {
		t.Fatalf("a legacy client cannot see the aggregated tools: %v", names)
	}
	if !contains(names, "_wakil.list_servers") {
		t.Errorf("a legacy client cannot see the synthetic tools: %v", names)
	}

	res := call(t, session, "legacy.echo", map[string]any{"text": "old school"})
	if got := textOf(res); got != "echo: old school" {
		t.Errorf("got %q, want %q", got, "echo: old school")
	}
}

// TestModernHarnessNegotiatesNewestRevision proves a current client gets the
// newest protocol revision, which is only possible because the HTTP endpoint
// runs without sessions.
func TestModernHarnessNegotiatesNewestRevision(t *testing.T) {
	fixture := testutil.FixtureBinary(t)

	env := newEnv(t, server.Options{}, fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "disableAuth": true},
	  "defaults": {"startPolicy": "eager", "connectTimeout": "30s"},
	  "mcpServers": {"modern": {"command": %q}}
	}`, fixture))

	session := dial(t, env.mcpURL(), "")

	got := session.InitializeResult().ProtocolVersion
	if got < "2026-07-28" {
		t.Errorf("negotiated version = %q; a current client should reach 2026-07-28 or later", got)
	}
	if !contains(toolNames(listTools(t, session)), "modern.echo") {
		t.Error("a modern client cannot see the aggregated tools")
	}
}

// TestHarnessIsNotifiedOfNewTools proves a connected harness learns about new
// tools without polling. Harnesses cache the tool list, so a proxy that cannot
// announce changes looks broken when an upstream starts slowly.
func TestHarnessIsNotifiedOfNewTools(t *testing.T) {
	fixture := testutil.FixtureBinary(t)

	notified := make(chan struct{}, 8)

	env := newEnv(t, server.Options{}, fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "disableAuth": true},
	  "defaults": {"startPolicy": "eager", "connectTimeout": "30s"},
	  "mcpServers": {
	    "growing": {
	      "command": %q,
	      "env": {"WAKIL_FIXTURE_CHANGED_MS": "2500"}
	    }
	  }
	}`, fixture))

	session := dialWanting(t, env.mcpURL(), "", "", func(o *mcp.ClientOptions) {
		o.ToolListChangedHandler = func(context.Context, *mcp.ToolListChangedRequest) {
			select {
			case notified <- struct{}{}:
			default:
			}
		}
	})

	// Establish a baseline before the upstream adds its extra tool. This first
	// list also blocks until the start-up enumeration is done, which is what
	// makes the ordering deterministic.
	baseline := toolNames(listTools(t, session))
	if !contains(baseline, "growing.echo") {
		t.Fatalf("the initial tool list is missing its tools: %v", baseline)
	}
	if contains(baseline, "growing.arrived_later") {
		t.Fatal("the fixture added its tool before the baseline was read; lengthen the delay")
	}

	// Wait for a notification that actually announces the change. A resync
	// from start-up can deliver one just after the baseline is read, so a
	// single notification proves nothing on its own: what matters is that a
	// notification coincides with the new tool being listed.
	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-notified:
			if findTool(listTools(t, session), "growing.arrived_later") != nil {
				return
			}
		case <-deadline:
			t.Fatal("no tools/list_changed notification announced the new tool")
		}
	}
}

// TestAuthEndToEnd exercises both credential types over real HTTP: a client
// token scoped to one server, and the admin token for the REST API.
func TestAuthEndToEnd(t *testing.T) {
	fixture := testutil.FixtureBinary(t)

	readerToken, readerHash := mintToken(t)
	adminToken, adminHash := mintToken(t)

	env := newEnv(t, server.Options{}, fmt.Sprintf(`{
	  "version": 1,
	  "server": {
	    "bind": "127.0.0.1:0",
	    "adminTokenHash": %q
	  },
	  "defaults": {"startPolicy": "eager", "connectTimeout": "30s"},
	  "clients": [
	    {
	      "name": "reader",
	      "tokenHash": %q,
	      "allow": {"servers": ["allowed"], "tools": {"allowed": ["echo"]}}
	    }
	  ],
	  "mcpServers": {
	    "allowed": {"command": %q},
	    "secret": {"command": %q}
	  }
	}`, adminHash, readerHash, fixture, fixture))

	testutil.WaitFor(t, 30*time.Second, func() bool {
		idx := env.app.Index()
		return len(idx.ForServer("allowed")) > 0 && len(idx.ForServer("secret")) > 0
	}, "both upstreams to enumerate")

	t.Run("an unauthenticated request is rejected", func(t *testing.T) {
		for _, header := range []string{"", "Bearer not-a-real-token"} {
			status := postMCP(t, env.mcpURL(), header)
			if status != http.StatusUnauthorized {
				t.Errorf("Authorization %q gave %d, want 401", header, status)
			}
		}
	})

	t.Run("the admin endpoint rejects a client token", func(t *testing.T) {
		if got := getStatus(t, env.server.URL+"/api/status", ""); got != http.StatusUnauthorized {
			t.Errorf("no token gave %d, want 401", got)
		}
		if got := getStatus(t, env.server.URL+"/api/status", readerToken); got != http.StatusForbidden {
			t.Errorf("a client token gave %d, want 403", got)
		}
		if got := getStatus(t, env.server.URL+"/api/status", adminToken); got != http.StatusOK {
			t.Errorf("the admin token gave %d, want 200", got)
		}
	})

	t.Run("a scoped client only sees the tools it may call", func(t *testing.T) {
		session := dial(t, env.mcpURL(), readerToken)
		names := toolNames(listTools(t, session))

		if !contains(names, "allowed.echo") {
			t.Errorf("the permitted tool is missing: %v", names)
		}
		for _, hidden := range []string{"allowed.add", "secret.echo", "secret.add"} {
			if contains(names, hidden) {
				t.Errorf("%q should not be visible to this client: %v", hidden, names)
			}
		}
	})

	t.Run("the synthetic tools respect the same scope", func(t *testing.T) {
		session := dial(t, env.mcpURL(), readerToken)

		visible := toolNames(listTools(t, session))

		res := call(t, session, "_wakil.list_servers", map[string]any{})
		body := textOf(res)
		if !strings.Contains(body, "allowed") {
			t.Errorf("the permitted server should be listed:\n%s", body)
		}
		if strings.Contains(body, "secret") {
			t.Errorf("a server the client cannot use leaked into the listing:\n%s", body)
		}

		// The headline count has to describe the caller's own roster, not the
		// daemon's: reporting the aggregate total would tell a narrow token how
		// much exists behind the tools it may see. Synthetic tools come from
		// the proxy rather than any server, so they are not part of it.
		upstreamTools := 0
		for _, name := range visible {
			if !strings.HasPrefix(name, "_wakil.") {
				upstreamTools++
			}
		}

		var listing struct {
			ServerCount int `json:"serverCount"`
			ToolCount   int `json:"toolCount"`
		}
		if err := json.Unmarshal([]byte(body), &listing); err != nil {
			t.Fatalf("decoding the server listing: %v\n%s", err, body)
		}
		if listing.ToolCount != upstreamTools {
			t.Errorf("the listing reports %d tools, but the client can reach %d on its servers: %v",
				listing.ToolCount, upstreamTools, visible)
		}

		res = call(t, session, "_wakil.search_tools", map[string]any{"query": "echo"})
		if strings.Contains(textOf(res), "secret.") {
			t.Errorf("search leaked an out-of-scope tool:\n%s", textOf(res))
		}

		callExpectingError(t, session, "_wakil.describe_tool",
			map[string]any{"name": "secret.echo"})
	})

	t.Run("permitting one tool does not permit its neighbours", func(t *testing.T) {
		session := dial(t, env.mcpURL(), readerToken)

		res := call(t, session, "allowed.echo", map[string]any{"text": "permitted"})
		if got := textOf(res); got != "echo: permitted" {
			t.Errorf("got %q, want %q", got, "echo: permitted")
		}

		// Denied tools must fail rather than silently succeed.
		callExpectingError(t, session, "allowed.add", map[string]any{"a": 1, "b": 2})
		callExpectingError(t, session, "secret.echo", map[string]any{"text": "nope"})
	})

	t.Run("the admin token can also drive the proxy", func(t *testing.T) {
		session := dial(t, env.mcpURL(), adminToken)
		names := toolNames(listTools(t, session))
		for _, want := range []string{"allowed.echo", "secret.echo"} {
			if !contains(names, want) {
				t.Errorf("an admin should see %q, got %v", want, names)
			}
		}
	})
}

// TestAdminAPIEditsConfig proves the REST API the web UI is built on can add a
// server, apply it live, and hand back a working token.
func TestAdminAPIEditsConfig(t *testing.T) {
	fixture := testutil.FixtureBinary(t)

	adminToken, adminHash := mintToken(t)
	_, seedHash := mintToken(t)

	env := newEnv(t, server.Options{}, fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "adminTokenHash": %q},
	  "defaults": {"startPolicy": "eager", "connectTimeout": "30s"},
	  "clients": [{"name": "seed", "tokenHash": %q}],
	  "mcpServers": {}
	}`, adminHash, seedHash))
	client := &http.Client{Timeout: 60 * time.Second}

	// The editor round-trips the file itself, so that is what we exercise.
	current := getBody(t, client, env.server.URL+"/api/config", adminToken)
	if !strings.Contains(current, "mcpServers") {
		t.Fatalf("GET /api/config did not return the config text:\n%s", current)
	}

	created := postBody(t, client, env.server.URL+"/api/clients", adminToken,
		`{"name":"harness-one","allow":{"servers":["added"]}}`)
	if !strings.Contains(created, "\"token\"") {
		t.Fatalf("creating a client did not return a token:\n%s", created)
	}

	// Add a server through the same path the UI uses.
	next := fmt.Sprintf(`{
	  "version": 1,
	  "server": {"bind": "127.0.0.1:0", "adminTokenHash": %q},
	  "defaults": {"startPolicy": "eager", "connectTimeout": "30s"},
	  "clients": [{"name":"harness-one","tokenHash":"%s","allow":{"servers":["added"]}}],
	  "mcpServers": {"added": {"command": %q}}
	}`, adminHash, extractTokenHash(t, env, "harness-one"), fixture)

	body := `{"json":` + jsonString(t, next) + `}`

	t.Run("an invalid config is rejected with detail", func(t *testing.T) {
		bad := `{"json":` + jsonString(t, `{"version":1,"mcpServers":{"x":{}}}`) + `}`
		resp := putBody(t, client, env.server.URL+"/api/config", adminToken, bad)
		if !strings.Contains(resp, "not valid") && !strings.Contains(resp, "detail") {
			t.Errorf("an empty server definition should be rejected:\n%s", resp)
		}
	})

	t.Run("a valid config is saved and applied", func(t *testing.T) {
		resp := putBody(t, client, env.server.URL+"/api/config", adminToken, body)
		if !strings.Contains(resp, "\"ok\"") {
			t.Fatalf("PUT /api/config failed:\n%s", resp)
		}

		testutil.WaitFor(t, 30*time.Second, func() bool {
			return len(env.app.Index().ForServer("added")) > 0
		}, "the added server to contribute tools live")

		// The change must be on disk, not just in memory.
		onDisk := readConfigFile(t, env.app.Config().Path())
		if !strings.Contains(onDisk, "\"added\"") {
			t.Errorf("the change was not persisted:\n%s", onDisk)
		}
	})

	t.Run("the proxy serves the newly added server", func(t *testing.T) {
		session := dial(t, env.mcpURL(), adminToken)
		if !contains(toolNames(listTools(t, session)), "added.echo") {
			t.Error("the newly added server's tools are not exposed")
		}
	})
}

// postMCP sends a bare tools/list to the MCP endpoint and returns the status.
func postMCP(t *testing.T, url, authHeader string) int {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, url,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// getStatus performs an authenticated GET and returns the status code.
func getStatus(t *testing.T, url, token string) int {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// getBody performs an authenticated GET and returns the body.
func getBody(t *testing.T, c *http.Client, url, token string) string {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned %d:\n%s", url, resp.StatusCode, body)
	}
	return string(body)
}

// postBody performs an authenticated POST and returns the body.
func postBody(t *testing.T, c *http.Client, url, token, body string) string {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if resp.StatusCode/100 != 2 {
		t.Fatalf("POST %s returned %d:\n%s", url, resp.StatusCode, out)
	}
	return string(out)
}

// putBody performs an authenticated PUT and returns the body whatever the
// status, so a rejection can be inspected.
func putBody(t *testing.T, c *http.Client, url, token, body string) string {
	t.Helper()

	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	defer resp.Body.Close()

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	return string(out)
}

// readConfigFile reads the config file from disk.
func readConfigFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// extractTokenHash pulls the stored hash of a named client out of the live
// config, so a round-tripped config keeps the credential it was given.
func extractTokenHash(t *testing.T, env *env, name string) string {
	t.Helper()
	for _, c := range env.app.Config().Clients {
		if c.Name == name {
			return c.TokenHash
		}
	}
	t.Fatalf("client %q is not in the config", name)
	return ""
}

// jsonString quotes a string as a JSON literal, for embedding config text
// inside an API request body.
func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("encoding a string: %v", err)
	}
	return string(b)
}
