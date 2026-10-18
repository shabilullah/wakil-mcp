package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/server"
	"github.com/shabilullah/wakil-mcp/internal/testutil"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// syncBuffer collects a command's output while another goroutine may still be
// writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runner invokes the command line against one config file, the way a shell
// would, and returns what the operator would see.
type runner struct {
	t    *testing.T
	path string
}

func (r *runner) run(args ...string) (stdout, stderr string, code int) {
	r.t.Helper()

	var out, errOut bytes.Buffer
	full := append([]string{"--config", r.path}, args...)
	code = RunWith(full, &out, &errOut)
	return out.String(), errOut.String(), code
}

// mustRun fails the test unless the command succeeded, and returns its output.
func (r *runner) mustRun(args ...string) string {
	r.t.Helper()

	out, errOut, code := r.run(args...)
	if code != 0 {
		r.t.Fatalf("wakil-mcp %s exited %d\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), code, out, errOut)
	}
	return out
}

// mustFail fails the test unless the command reported a problem.
func (r *runner) mustFail(args ...string) string {
	r.t.Helper()

	out, errOut, code := r.run(args...)
	if code == 0 {
		r.t.Fatalf("wakil-mcp %s should have failed\nstdout:\n%s", strings.Join(args, " "), out)
	}
	return out + errOut
}

// newConfigFile writes a config file and returns its path along with a client
// and an admin token, so tests do not depend on the bootstrap banner.
func newConfigFile(t *testing.T, body func(clientHash, adminHash string) string) (path, clientToken, adminToken string) {
	t.Helper()

	var err error
	clientToken, clientHash, err := config.GenerateToken()
	if err != nil {
		t.Fatalf("minting a client token: %v", err)
	}
	adminToken, adminHash, err := config.GenerateToken()
	if err != nil {
		t.Fatalf("minting an admin token: %v", err)
	}

	path = filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body(clientHash, adminHash)), 0o600); err != nil {
		t.Fatalf("writing the config: %v", err)
	}

	// Fail early with a readable message if the fixture config is not valid.
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the test config is not loadable: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the test config is not valid: %v", err)
	}

	return path, clientToken, adminToken
}

// loadConfig reads a config file.
func loadConfig(t *testing.T, path string) *config.Config {
	t.Helper()

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("loading %s: %v", path, err)
	}
	return cfg
}

// decodeJSON unmarshals into a value, failing the test on malformed output.
func decodeJSON(t *testing.T, text string, into any) {
	t.Helper()

	if err := json.Unmarshal([]byte(text), into); err != nil {
		t.Fatalf("the command did not print valid JSON (%v):\n%s", err, text)
	}
}

// ---------------------------------------------------------------------------
// Command line basics
// ---------------------------------------------------------------------------

func TestCLIUsageAndExitCodes(t *testing.T) {
	t.Run("no arguments prints usage and fails", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if code := RunWith(nil, &out, &errOut); code != ExitUsage {
			t.Errorf("exit code %d, want %d", code, ExitUsage)
		}
		if !strings.Contains(out.String(), "Usage:") {
			t.Errorf("the usage did not reach stdout:\n%s", out.String())
		}
	})

	t.Run("an unknown command is rejected", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if code := RunWith([]string{"frobnicate"}, &out, &errOut); code != ExitUsage {
			t.Errorf("exit code %d, want %d", code, ExitUsage)
		}
		if !strings.Contains(errOut.String(), "unknown command") {
			t.Errorf("the error did not explain the problem:\n%s", errOut.String())
		}
	})

	t.Run("help succeeds", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if code := RunWith([]string{"--help"}, &out, &errOut); code != ExitOK {
			t.Errorf("exit code %d, want %d", code, ExitOK)
		}
		if !strings.Contains(out.String(), "serve") {
			t.Errorf("the help does not list the commands:\n%s", out.String())
		}
	})

	t.Run("a command's own help succeeds", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if code := RunWith([]string{"serve", "--help"}, &out, &errOut); code != ExitOK {
			t.Errorf("exit code %d, want %d", code, ExitOK)
		}
		if !strings.Contains(errOut.String(), "wakil-mcp serve") {
			t.Errorf("the usage did not reach the stream a shell shows:\n%s", errOut.String())
		}
	})

	t.Run("version reports the build", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if code := RunWith([]string{"--version"}, &out, &errOut); code != ExitOK {
			t.Errorf("exit code %d, want %d", code, ExitOK)
		}
		if !strings.Contains(out.String(), "wakil-mcp") {
			t.Errorf("the version line is missing:\n%s", out.String())
		}
	})

	t.Run("a missing config is explained", func(t *testing.T) {
		var out, errOut bytes.Buffer
		missing := filepath.Join(t.TempDir(), "nothing.json")
		if code := RunWith([]string{"--config", missing, "list"}, &out, &errOut); code != ExitError {
			t.Errorf("exit code %d, want %d", code, ExitError)
		}
		if !strings.Contains(errOut.String(), "serve") {
			t.Errorf("the error does not say how to create one:\n%s", errOut.String())
		}
	})
}

// ---------------------------------------------------------------------------
// Editing the server list
// ---------------------------------------------------------------------------

func TestCLIServerListEditing(t *testing.T) {
	r := &runner{t: t, path: filepath.Join(t.TempDir(), "config.json")}
	if _, _, code := r.run("list"); code != ExitError {
		t.Fatalf("listing without a config should have failed")
	}

	// create the config the way "serve" would.
	if _, _, err := config.LoadOrCreate(r.path); err != nil {
		t.Fatalf("creating the config: %v", err)
	}

	t.Run("list is empty and says how to start", func(t *testing.T) {
		out := r.mustRun("list")
		if !strings.Contains(out, "No servers configured") {
			t.Errorf("an empty list should point at the next step:\n%s", out)
		}
	})

	t.Run("adding a stdio server", func(t *testing.T) {
		r.mustRun("add", "github",
			"--command", "npx",
			"--arg", "-y",
			"--arg", "@modelcontextprotocol/server-github",
			"--env", "GITHUB_TOKEN=${env:GITHUB_TOKEN}",
			"--description", "issues and pull requests",
			"--start-policy", "eager")

		cfg := loadConfig(t, r.path)
		up := cfg.MCPServers["github"]
		if up == nil {
			t.Fatal("the server was not written")
		}
		if up.Command != "npx" {
			t.Errorf("command = %q, want npx", up.Command)
		}
		if len(up.Args) != 2 || up.Args[1] != "@modelcontextprotocol/server-github" {
			t.Errorf("args = %v", up.Args)
		}
		if up.Env["GITHUB_TOKEN"] != "${env:GITHUB_TOKEN}" {
			t.Errorf("the secret reference was not preserved: %v", up.Env)
		}
		if up.EffectiveType() != config.TransportStdio {
			t.Errorf("type = %q, want stdio", up.EffectiveType())
		}
		if up.EffectiveStartPolicy(cfg.Defaults) != config.StartEager {
			t.Errorf("start policy = %q", up.EffectiveStartPolicy(cfg.Defaults))
		}
	})

	t.Run("adding a remote server", func(t *testing.T) {
		r.mustRun("add", "linear", "--url", "https://mcp.linear.app/mcp",
			"--header", "Authorization: Bearer ${env:LINEAR_KEY}")

		cfg := loadConfig(t, r.path)
		if got := cfg.MCPServers["linear"].EffectiveType(); got != config.TransportHTTP {
			t.Errorf("type = %q, want http", got)
		}
	})

	t.Run("list shows both", func(t *testing.T) {
		var rows []struct {
			Key     string `json:"key"`
			Enabled bool   `json:"enabled"`
			Type    string `json:"type"`
			Target  string `json:"target"`
		}
		decodeJSON(t, r.mustRun("--json", "list"), &rows)

		if len(rows) != 2 {
			t.Fatalf("listed %d servers, want 2: %+v", len(rows), rows)
		}
		if rows[0].Key != "github" || rows[1].Key != "linear" {
			t.Errorf("the list is not sorted by key: %+v", rows)
		}

		// A header value is a secret reference, so it must not be printed.
		if strings.Contains(rows[1].Target, "LINEAR_KEY") {
			t.Errorf("the target leaked a secret reference: %q", rows[1].Target)
		}
	})

	t.Run("disabling and enabling", func(t *testing.T) {
		r.mustRun("disable", "github")
		if loadConfig(t, r.path).MCPServers["github"].IsEnabled() {
			t.Error("the server is still enabled")
		}

		// Disabling twice is not an error; it is what a script would do.
		if out := r.mustRun("disable", "github"); !strings.Contains(out, "already") {
			t.Errorf("the second disable should say so:\n%s", out)
		}

		r.mustRun("enable", "github")
		if !loadConfig(t, r.path).MCPServers["github"].IsEnabled() {
			t.Error("the server is still disabled")
		}
	})

	t.Run("replacing keeps what was not overridden", func(t *testing.T) {
		r.mustRun("add", "github", "--command", "node", "--arg", "server.js")

		cfg := loadConfig(t, r.path)
		up := cfg.MCPServers["github"]
		if up.Command != "node" {
			t.Errorf("command = %q, want node", up.Command)
		}
		if len(up.Args) != 1 {
			t.Errorf("the new argument list was not applied: %v", up.Args)
		}
		if up.Description != "issues and pull requests" {
			t.Errorf("the description was lost: %q", up.Description)
		}
		if up.EffectiveStartPolicy(cfg.Defaults) != config.StartEager {
			t.Errorf("the start policy was lost: %q", up.EffectiveStartPolicy(cfg.Defaults))
		}
	})

	t.Run("removing", func(t *testing.T) {
		r.mustRun("remove", "linear")
		if _, ok := loadConfig(t, r.path).MCPServers["linear"]; ok {
			t.Error("the server was not removed")
		}
		r.mustFail("remove", "linear")
	})

	t.Run("bad input is refused", func(t *testing.T) {
		cases := []struct {
			name string
			args []string
		}{
			{"no destination", []string{"add", "thing"}},
			{"both destinations", []string{"add", "thing", "--command", "x", "--url", "http://x"}},
			{"reserved key", []string{"add", "_wakil", "--command", "x"}},
			{"reserved key prefix", []string{"add", "_wakil_thing", "--command", "x"}},
			{"bad start policy", []string{"add", "thing", "--command", "x", "--start-policy", "sometimes"}},
			{"malformed env", []string{"add", "thing", "--command", "x", "--env", "NOEQUALS"}},
			{"unknown flag", []string{"add", "thing", "--command", "x", "--nonsense"}},
			{"two keys", []string{"add", "one", "two", "--command", "x"}},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				message := r.mustFail(tc.args...)
				if strings.TrimSpace(message) == "" {
					t.Error("nothing was reported")
				}
			})
		}

		// Nothing above should have reached the file.
		cfg := loadConfig(t, r.path)
		if len(cfg.MCPServers) != 1 {
			t.Errorf("a rejected command changed the config: %v", sortedKeys(cfg.MCPServers))
		}
	})
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

func TestCLITokenLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if _, _, err := config.LoadOrCreate(path); err != nil {
		t.Fatalf("creating the config: %v", err)
	}
	r := &runner{t: t, path: path}

	r.mustRun("add", "github", "--command", "npx")
	r.mustRun("add", "linear", "--url", "https://mcp.linear.app/mcp")

	var token string
	t.Run("creating a client", func(t *testing.T) {
		out := r.mustRun("token", "create", "laptop")
		token = firstToken(t, out)

		cfg := loadConfig(t, path)
		profile := findProfile(t, cfg, "laptop")
		if !config.VerifyTokenHash(token, profile.TokenHash) {
			t.Error("the printed token does not match the stored hash")
		}
		if profile.TokenHash == token {
			t.Error("the token was stored in plaintext")
		}
	})

	t.Run("the token is not stored anywhere on disk", func(t *testing.T) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the config: %v", err)
		}
		if bytes.Contains(raw, []byte(token)) {
			t.Error("the plaintext token was written to the config file")
		}
	})

	t.Run("a scoped client", func(t *testing.T) {
		r.mustRun("token", "create", "ci",
			"--allow-server", "github",
			"--allow-tool", "github:get_*",
			"--deny-tool", "github:delete_*")

		profile := findProfile(t, loadConfig(t, path), "ci")
		if len(profile.Allow.Servers) != 1 || profile.Allow.Servers[0] != "github" {
			t.Errorf("allow servers = %v", profile.Allow.Servers)
		}
		if got := profile.Allow.Tools["github"]; len(got) != 1 || got[0] != "get_*" {
			t.Errorf("allow tools = %v", profile.Allow.Tools)
		}
		if got := profile.Deny.Tools["github"]; len(got) != 1 || got[0] != "delete_*" {
			t.Errorf("deny tools = %v", profile.Deny.Tools)
		}
	})

	t.Run("list shows the clients", func(t *testing.T) {
		var profiles []struct {
			Name         string              `json:"name"`
			Usable       bool                `json:"usable"`
			AllowServers []string            `json:"allowServers"`
			AllowTools   map[string][]string `json:"allowTools"`
		}
		decodeJSON(t, r.mustRun("--json", "token", "list"), &profiles)

		names := make([]string, 0, len(profiles))
		for _, p := range profiles {
			names = append(names, p.Name)
		}
		for _, want := range []string{"default", "laptop", "ci"} {
			if !contains(names, want) {
				t.Errorf("client %q is missing: %v", want, names)
			}
		}
	})

	t.Run("bad scope is refused", func(t *testing.T) {
		// A misspelled server would otherwise produce a token that reaches
		// nothing, which is hard to diagnose later.
		message := r.mustFail("token", "create", "typo", "--allow-server", "githubb")
		if !strings.Contains(message, "github") {
			t.Errorf("the error does not name the known servers: %s", message)
		}
		r.mustFail("token", "create", "pair", "--allow-tool", "github")
		r.mustFail("token", "create", "laptop")
	})

	t.Run("the admin token is replaced", func(t *testing.T) {
		before := loadConfig(t, path).Server.AdminTokenHash

		out := r.mustRun("token", "admin")
		after := loadConfig(t, path).Server.AdminTokenHash

		if after == before {
			t.Error("the admin hash did not change")
		}
		if !config.VerifyTokenHash(firstToken(t, out), after) {
			t.Error("the printed admin token does not match the stored hash")
		}
	})

	t.Run("removing a client", func(t *testing.T) {
		r.mustRun("token", "remove", "laptop")
		if contains(profileNames(loadConfig(t, path)), "laptop") {
			t.Error("the client was not removed")
		}
	})

	t.Run("the last client is protected", func(t *testing.T) {
		for _, name := range profileNames(loadConfig(t, path)) {
			if name == "default" {
				continue
			}
			r.mustRun("token", "remove", name)
		}

		// "default" is now the only client, and authentication is on.
		message := r.mustFail("token", "remove", "default")
		if !strings.Contains(message, "last client") {
			t.Errorf("the error does not explain the refusal: %s", message)
		}
	})
}

// ---------------------------------------------------------------------------
// Importing
// ---------------------------------------------------------------------------

func TestCLIImport(t *testing.T) {
	r := &runner{t: t, path: filepath.Join(t.TempDir(), "config.json")}
	if _, _, err := config.LoadOrCreate(r.path); err != nil {
		t.Fatalf("creating the config: %v", err)
	}

	// A Claude-Desktop-shaped file with an entry the importer has never heard
	// of, plus VS Code's spelling of a URL.
	other := filepath.Join(t.TempDir(), "claude_desktop_config.json")
	body := `{
	  "mcpServers": {
	    "filesystem": {
	      "command": "npx",
	      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"],
	      "env": {"HOME": "/home/me"}
	    },
	    "linear": {
	      "serverUrl": "https://mcp.linear.app/mcp",
	      "headers": {"Authorization": "Bearer abc"}
	    },
	    "off": {
	      "command": "sleep",
	      "disabled": true
	    },
	    "future": {
	      "command": "newthing",
	      "somethingNewer": {"nested": true}
	    }
	  }
	}`
	if err := os.WriteFile(other, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the other config: %v", err)
	}

	t.Run("a dry run changes nothing", func(t *testing.T) {
		out := r.mustRun("import", "--from", other, "--dry-run")
		if !strings.Contains(out, "filesystem") {
			t.Errorf("the dry run did not report what it would add:\n%s", out)
		}
		if len(loadConfig(t, r.path).MCPServers) != 0 {
			t.Error("a dry run wrote to the config")
		}
	})

	t.Run("importing with a prefix", func(t *testing.T) {
		r.mustRun("import", "--from", other, "--prefix", "old-")

		cfg := loadConfig(t, r.path)
		if len(cfg.MCPServers) != 4 {
			t.Fatalf("imported %d servers, want 4: %v", len(cfg.MCPServers), sortedKeys(cfg.MCPServers))
		}

		filesystem := cfg.MCPServers["old-filesystem"]
		if filesystem == nil {
			t.Fatal("the prefixed server is missing")
		}
		if filesystem.Command != "npx" || len(filesystem.Args) != 3 {
			t.Errorf("the stdio server was not imported faithfully: %+v", filesystem)
		}
		if filesystem.Env["HOME"] != "/home/me" {
			t.Errorf("the environment was lost: %v", filesystem.Env)
		}

		if got := cfg.MCPServers["old-linear"].URL; got != "https://mcp.linear.app/mcp" {
			t.Errorf("serverUrl was not understood: %q", got)
		}
		if cfg.MCPServers["old-off"].IsEnabled() {
			t.Error("a server marked disabled imported as enabled")
		}
		if cfg.MCPServers["old-future"] == nil {
			t.Error("an entry with unknown fields was dropped")
		}

		for _, up := range cfg.MCPServers {
			if up.EffectiveType() == "" {
				t.Errorf("an imported server has no usable transport: %+v", up)
			}
		}
	})

	t.Run("importing twice replaces", func(t *testing.T) {
		r.mustRun("import", "--from", other, "--prefix", "old-")
		if got := len(loadConfig(t, r.path).MCPServers); got != 4 {
			t.Errorf("a second import duplicated entries: %d servers", got)
		}
	})

	t.Run("a file with no servers is explained", func(t *testing.T) {
		empty := filepath.Join(t.TempDir(), "empty.json")
		if err := os.WriteFile(empty, []byte(`{"somethingElse": true}`), 0o600); err != nil {
			t.Fatalf("writing: %v", err)
		}
		if message := r.mustFail("import", "--from", empty); !strings.Contains(message, "no servers") {
			t.Errorf("the error does not explain the problem: %s", message)
		}
	})

	t.Run("a missing file is explained", func(t *testing.T) {
		if message := r.mustFail("import", "--from", filepath.Join(t.TempDir(), "nope.json")); !strings.Contains(message, "nope.json") {
			t.Errorf("the error does not name the file: %s", message)
		}
	})
}

// ---------------------------------------------------------------------------
// Probing and diagnosis
// ---------------------------------------------------------------------------

func TestCLITestAndDoctor(t *testing.T) {
	fixture := testutil.FixtureBinary(t)

	path, _, _ := newConfigFile(t, func(clientHash, adminHash string) string {
		return fmt.Sprintf(`{
		  "version": 1,
		  "server": {"bind": "127.0.0.1:0", "adminTokenHash": %q},
		  "defaults": {"startPolicy": "eager", "connectTimeout": "30s", "callTimeout": "30s"},
		  "clients": [{"name": "harness", "tokenHash": %q}],
		  "mcpServers": {
		    "modern": {
		      "command": %q,
		      "env": {"WAKIL_FIXTURE_MODE": "modern", "WAKIL_FIXTURE_SCHEMA": "rich"}
		    },
		    "broken": {
		      "command": "definitely-not-a-real-binary-wakil-test"
		    }
		  }
		}`, adminHash, clientHash, fixture)
	})
	r := &runner{t: t, path: path}

	t.Run("test reports what a server offers", func(t *testing.T) {
		var report struct {
			Key             string `json:"key"`
			Transport       string `json:"transport"`
			ProtocolVersion string `json:"protocolVersion"`
			Tools           []struct {
				Exposed  string `json:"exposed"`
				Upstream string `json:"upstream"`
			} `json:"tools"`
			ElapsedMS int64 `json:"elapsedMs"`
		}
		decodeJSON(t, r.mustRun("--json", "test", "modern"), &report)

		if report.Transport != config.TransportStdio {
			t.Errorf("transport = %q", report.Transport)
		}
		if report.ProtocolVersion == "" {
			t.Error("the handshake did not report a protocol version")
		}

		exposed := make([]string, 0, len(report.Tools))
		for _, tool := range report.Tools {
			exposed = append(exposed, tool.Exposed)
		}
		// The names are the ones a harness will see, not the upstream ones.
		for _, want := range []string{"modern.add", "modern.echo", "modern.error"} {
			if !contains(exposed, want) {
				t.Errorf("tool %q is missing: %v", want, exposed)
			}
		}
	})

	t.Run("test against a broken server fails with the reason", func(t *testing.T) {
		message := r.mustFail("test", "broken")
		if !strings.Contains(message, "broken") {
			t.Errorf("the error does not name the server: %s", message)
		}
	})

	t.Run("test against an unknown server fails", func(t *testing.T) {
		if message := r.mustFail("test", "nosuchserver"); !strings.Contains(message, "list") {
			t.Errorf("the error does not point at the list command: %s", message)
		}
	})

	t.Run("doctor reports the broken server and fails", func(t *testing.T) {
		var report struct {
			Checks  []struct{ Name, Status, Detail string } `json:"checks"`
			Servers []struct {
				Key    string `json:"key"`
				Status string `json:"status"`
				Tools  int    `json:"tools"`
			} `json:"servers"`
			Problems int `json:"problems"`
			Warnings int `json:"warnings"`
		}

		out, _, code := r.run("--json", "doctor")
		if code != ExitError {
			t.Errorf("doctor exited %d, want %d because one server is broken", code, ExitError)
		}
		decodeJSON(t, out, &report)

		if report.Problems == 0 {
			t.Error("a broken server was not counted as a problem")
		}

		status := map[string]string{}
		tools := map[string]int{}
		for _, s := range report.Servers {
			status[s.Key] = s.Status
			tools[s.Key] = s.Tools
		}
		if status["modern"] != statusOK {
			t.Errorf("the working server is reported as %q", status["modern"])
		}
		if tools["modern"] < 3 {
			t.Errorf("the working server reported %d tools", tools["modern"])
		}
		if status["broken"] != statusFail {
			t.Errorf("the broken server is reported as %q", status["broken"])
		}
	})

	t.Run("doctor without servers still checks the environment", func(t *testing.T) {
		out, _, _ := r.run("doctor", "--skip-servers")
		if !strings.Contains(out, "config file") {
			t.Errorf("the environment checks are missing:\n%s", out)
		}
		if !strings.Contains(out, "skipped") {
			t.Errorf("the report does not say the servers were skipped:\n%s", out)
		}
	})
}

// ---------------------------------------------------------------------------
// The daemon
// ---------------------------------------------------------------------------

// serve starts the daemon the way the command line does, in this process, with
// a context the test controls instead of a signal.
type served struct {
	runner  *runner
	out     *syncBuffer
	addr    string
	stopped chan error
	cancel  context.CancelFunc

	// The test body and the cleanup both want to know how the daemon exited,
	// so the answer is computed once.
	stopOnce sync.Once
	stopDone chan struct{}
	stopErr  error
}

func startServe(t *testing.T, configPath, bind string) *served {
	t.Helper()

	out := &syncBuffer{}
	errOut := &syncBuffer{}

	e := newEnv(out, errOut)
	e.configPath = configPath

	root, cancel := context.WithCancel(context.Background())
	e.signalContext = func() (context.Context, context.CancelFunc) {
		return context.WithCancel(root)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- e.serve([]string{"--bind", bind}) }()

	s := &served{
		runner:   &runner{t: t, path: configPath},
		out:      out,
		addr:     bind,
		stopped:  stopped,
		cancel:   cancel,
		stopDone: make(chan struct{}),
	}

	t.Cleanup(func() {
		if err := s.shutdown(); err != nil {
			t.Errorf("the daemon: %v", err)
		}
	})

	s.waitHealthy(t)
	return s
}

// waitHealthy blocks until the daemon answers its liveness probe.
func (s *served) waitHealthy(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + s.addr + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("the daemon never became healthy on %s\noutput:\n%s", s.addr, s.out.String())
}

// stop shuts the daemon down and checks that it exited cleanly. It is safe to
// call more than once, because the cleanup asks as well.
func (s *served) stop(t *testing.T) {
	t.Helper()

	if err := s.shutdown(); err != nil {
		t.Errorf("the daemon: %v", err)
	}
}

// shutdown stops the daemon and reports how it exited.
func (s *served) shutdown() error {
	s.cancel()

	s.stopOnce.Do(func() {
		defer close(s.stopDone)

		select {
		case err := <-s.stopped:
			s.stopErr = err
		case <-time.After(60 * time.Second):
			s.stopErr = errors.New("did not shut down within 60s")
		}
	})

	<-s.stopDone
	return s.stopErr
}

// adminURL builds a REST API URL.
func (s *served) adminURL(path string) string {
	return "http://" + s.addr + server.AdminPrefix + path
}

// mcpURL is the MCP endpoint.
func (s *served) mcpURL() string { return "http://" + s.addr + server.MCPEndpoint }

// callAdmin performs an authenticated REST request.
func (s *served) callAdmin(t *testing.T, method, path, token string, body io.Reader) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, s.adminURL(path), body)
	if err != nil {
		t.Fatalf("building a request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// TestServeBootstrapsCredentials covers the first run of a brand new install:
// the daemon creates the config, mints both credentials and shows them once.
func TestServeBootstrapsCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := startServe(t, path, testutil.FreeAddr(t))

	tokens := tokenPattern.FindAllString(s.out.String(), -1)
	if len(tokens) != 2 {
		t.Fatalf("the banner should show exactly two credentials, got %d:\n%s", len(tokens), s.out.String())
	}
	clientToken, adminToken := tokens[0], tokens[1]

	if !strings.Contains(s.out.String(), "shown once") {
		t.Error("the banner does not warn that the credentials are not recoverable")
	}

	cfg := loadConfig(t, path)
	if len(cfg.Clients) != 1 {
		t.Fatalf("expected one bootstrapped client, got %d", len(cfg.Clients))
	}
	if !config.VerifyTokenHash(clientToken, cfg.Clients[0].TokenHash) {
		t.Error("the client token does not match the stored hash")
	}
	if !config.VerifyTokenHash(adminToken, cfg.Server.AdminTokenHash) {
		t.Error("the admin token does not match the stored hash")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}
	for _, token := range tokens {
		if bytes.Contains(raw, []byte(token)) {
			t.Error("a plaintext credential was written to the config file")
		}
	}

	t.Run("the REST API needs the admin token", func(t *testing.T) {
		resp := s.callAdmin(t, http.MethodGet, "/status", "", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("an unauthenticated request returned %s, want 401", resp.Status)
		}
	})

	t.Run("the web UI loads without a token", func(t *testing.T) {
		// The browser has to be able to load the app before it can ask for a
		// token, so the shell is deliberately unauthenticated.
		resp, err := http.Get("http://" + s.addr + "/")
		if err != nil {
			t.Fatalf("loading the UI: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("the UI returned %s", resp.Status)
		}
	})

	// Shut down explicitly, so a failure here is reported against this test
	// rather than the cleanup.
	s.stop(t)
}

// TestServeEndToEnd is the whole product in one test: a daemon, a server added
// from the command line while it runs, a harness reaching it over HTTP, and the
// same daemon reached from a stdio-only harness through the bridge.
func TestServeEndToEnd(t *testing.T) {
	fixture := testutil.FixtureBinary(t)

	addr := testutil.FreeAddr(t)
	path, clientToken, adminToken := newConfigFile(t, func(clientHash, adminHash string) string {
		return fmt.Sprintf(`{
		  "version": 1,
		  "server": {
		    "bind": %q,
		    "adminTokenHash": %q,
		    "logLevel": "warn"
		  },
		  "defaults": {
		    "startPolicy": "eager",
		    "connectTimeout": "30s",
		    "callTimeout": "30s",
		    "idleTimeout": "10m"
		  },
		  "clients": [{"name": "harness", "tokenHash": %q}],
		  "mcpServers": {}
		}`, addr, adminHash, clientHash)
	})

	// The config already exists, so the bind address comes from the file and
	// nothing is bootstrapped. "--bind" is passed anyway to prove the override
	// is accepted.
	s := startServe(t, path, addr)

	t.Run("the admin API reports status", func(t *testing.T) {
		resp := s.callAdmin(t, http.MethodGet, "/status", adminToken, nil)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/status returned %s", resp.Status)
		}

		var status map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			t.Fatalf("the status is not JSON: %v", err)
		}
		if len(status) == 0 {
			t.Error("the status is empty")
		}
		t.Logf("status: %v", status)
	})

	t.Run("a client token cannot reach the admin API", func(t *testing.T) {
		resp := s.callAdmin(t, http.MethodGet, "/status", clientToken, nil)
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("a harness token was accepted for an admin request")
		}
	})

	t.Run("the MCP endpoint rejects an unknown token", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, s.mcpURL(), strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("building a request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer wakil_thisisnotarealtoken")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("calling the MCP endpoint: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("an unknown token returned %s, want 401", resp.Status)
		}
	})

	// One session for the rest of the test. It is dialled here rather than in
	// a subtest, because a session registered for cleanup inside a subtest is
	// closed as soon as that subtest ends.
	session := dialMCP(t, s.mcpURL(), clientToken)

	t.Run("an empty proxy still offers its own tools", func(t *testing.T) {
		names := mcpToolNames(t, session)
		if len(names) == 0 {
			t.Fatal("the proxy offers nothing at all")
		}
		if !contains(names, "_wakil.list_servers") {
			t.Errorf("the proxy's own tools are missing: %v", names)
		}
		if len(names) != 3 {
			t.Errorf("an empty proxy should expose only its own three tools: %v", names)
		}
	})

	t.Run("a server added from the command line appears", func(t *testing.T) {
		out := s.runner.mustRun("add", "modern",
			"--command", fixture,
			"--env", "WAKIL_FIXTURE_MODE=modern",
			"--env", "WAKIL_FIXTURE_SCHEMA=rich",
			"--test")

		if !strings.Contains(out, "Connected") {
			t.Errorf("--test did not report a successful connection:\n%s", out)
		}
		if !strings.Contains(out, "picks this up") {
			t.Errorf("the output does not say the daemon will notice:\n%s", out)
		}

		// Nothing tells the daemon; it has to notice the file change itself.
		testutil.WaitFor(t, 60*time.Second, func() bool {
			return contains(mcpToolNames(t, session), "modern.echo")
		}, "the daemon to expose the server added while it was running")

		names := mcpToolNames(t, session)
		for _, want := range []string{"modern.add", "modern.echo", "modern.error", "_wakil.list_servers"} {
			if !contains(names, want) {
				t.Errorf("tool %q is missing: %v", want, names)
			}
		}
	})

	t.Run("a tool call reaches the upstream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "modern.echo",
			Arguments: map[string]any{"text": "end to end"},
		})
		if err != nil {
			t.Fatalf("calling modern.echo: %v", err)
		}
		if got := textOfResult(t, res); !strings.Contains(got, "end to end") {
			t.Errorf("the echo came back as %q", got)
		}
	})

	t.Run("the web UI is served", func(t *testing.T) {
		base := "http://" + s.addr

		resp, err := http.Get(base + "/")
		if err != nil {
			t.Fatalf("loading the UI: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("the UI returned %s", resp.Status)
		}
		page := string(body)
		if !strings.Contains(page, `id="root"`) {
			t.Fatalf("the UI is not the app shell:\n%s", page)
		}

		// A build that shipped the shell but not the chunks it references is a
		// blank page in the browser, which compiling never catches. Fetch
		// every asset the shell names.
		assets := referencedAssets(page)
		if len(assets) < 3 {
			t.Fatalf("the shell references %d assets, want the script, the preload and the stylesheet: %v", len(assets), assets)
		}
		for _, asset := range assets {
			res, err := http.Get(base + asset)
			if err != nil {
				t.Errorf("loading %s: %v", asset, err)
				continue
			}
			assetBody, _ := io.ReadAll(res.Body)
			res.Body.Close()

			if res.StatusCode != http.StatusOK {
				t.Errorf("%s returned %s", asset, res.Status)
			}
			if len(assetBody) == 0 {
				t.Errorf("%s is empty", asset)
			}
			if cache := res.Header.Get("Cache-Control"); !strings.Contains(cache, "immutable") {
				t.Errorf("%s is served with Cache-Control %q, want the immutable hashed-asset policy", asset, cache)
			}
		}

		// The app is a single page, so a path that is not an asset hands back
		// the shell instead of a 404. That is what makes reloading a tab URL
		// such as /#/tools work.
		deep, err := http.Get(base + "/tools")
		if err != nil {
			t.Fatalf("loading a deep link: %v", err)
		}
		deepBody, _ := io.ReadAll(deep.Body)
		deep.Body.Close()

		if deep.StatusCode != http.StatusOK || !strings.Contains(string(deepBody), `id="root"`) {
			t.Errorf("a deep link returned %s and %d bytes, want the app shell", deep.Status, len(deepBody))
		}
		if cache := deep.Header.Get("Cache-Control"); strings.Contains(cache, "immutable") {
			t.Errorf("the app shell is cached hard (%q), so a new build would not be picked up", cache)
		}
	})

	t.Run("a stdio harness reaches the same daemon", func(t *testing.T) {
		runStdioHarness(t, s, clientToken, "modern.echo")
	})

	t.Run("doctor is satisfied", func(t *testing.T) {
		out, errOut, code := s.runner.run("doctor")
		if code != ExitOK {
			t.Errorf("doctor exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
		}
		if !strings.Contains(out, "modern") {
			t.Errorf("doctor did not report the configured server:\n%s", out)
		}
	})

	s.stop(t)
}

// runStdioHarness drives the real "wakil-mcp stdio" executable the way a
// harness does: it launches the binary, writes JSON-RPC to its stdin and reads
// the replies from its stdout.
func runStdioHarness(t *testing.T, s *served, token, wantTool string) {
	t.Helper()

	binary := testutil.Binary(t, "./cmd/wakil-mcp")

	// No --url: the bridge derives the endpoint from the config file, which is
	// the configuration a harness actually gets.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, "stdio", "--config", s.runner.path, "--poll", "200ms")
	cmd.Env = append(os.Environ(), "WAKIL_TOKEN="+token)
	// Anything the process logs has to stay out of the protocol stream.
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("opening the child's stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("opening the child's stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the bridge: %v", err)
	}

	replies := make(chan map[string]any, 16)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
		for scanner.Scan() {
			var message map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
				continue
			}
			replies <- message
		}
		close(replies)
	}()

	send := func(message map[string]any) {
		encoded, err := json.Marshal(message)
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		if _, err := stdin.Write(append(encoded, '\n')); err != nil {
			t.Fatalf("writing to the bridge: %v", err)
		}
	}

	await := func(id string) map[string]any {
		deadline := time.After(30 * time.Second)
		for {
			select {
			case message, open := <-replies:
				if !open {
					t.Fatalf("the bridge stopped responding while waiting for %s", id)
				}
				if fmt.Sprint(message["id"]) == id {
					return message
				}
			case <-deadline:
				t.Fatalf("the bridge never replied to %s", id)
			}
		}
	}

	send(map[string]any{
		"jsonrpc": "2.0",
		"id":      "init",
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "stdio-harness", "version": "1.0.0"},
		},
	})
	if init := await("init"); init["error"] != nil {
		t.Fatalf("the handshake failed: %v", init["error"])
	}

	send(map[string]any{"jsonrpc": "2.0", "id": "list", "method": "tools/list"})
	reply := await("list")
	if reply["error"] != nil {
		t.Fatalf("tools/list failed: %v", reply["error"])
	}

	result, _ := reply["result"].(map[string]any)
	raw, _ := result["tools"].([]any)

	var names []string
	for _, item := range raw {
		tool, _ := item.(map[string]any)
		if name, ok := tool["name"].(string); ok {
			names = append(names, name)
		}
	}
	if !contains(names, wantTool) {
		t.Errorf("the stdio harness cannot see %q: %v", wantTool, names)
	}

	// Closing stdin is how a harness shuts the bridge down, and it has to be
	// quick: a harness that waits on a hung child looks like a crash.
	_ = stdin.Close()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	select {
	case err := <-waited:
		if err != nil {
			t.Errorf("the bridge exited with an error: %v", err)
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Error("the bridge did not exit after its input closed")
	}
}

// dialMCP connects a real MCP client to the daemon.
func dialMCP(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()

	client := mcp.NewClient(&mcp.Implementation{Name: "cli-test", Version: "1.0.0"}, &mcp.ClientOptions{
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	})

	transport := &mcp.StreamableClientTransport{
		Endpoint: url,
		HTTPClient: &http.Client{
			Transport: bearerTransport{token: token, base: http.DefaultTransport},
			Timeout:   30 * time.Second,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connecting to %s: %v", url, err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// bearerTransport adds a bearer token to every request.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	if b.token != "" {
		clone.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(clone)
}

// mcpToolNames lists the tools the daemon is publishing. A failure is reported
// as an empty list so callers can poll while a reload settles.
func mcpToolNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Logf("listing tools: %v", err)
		return nil
	}

	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// textOfResult joins the text content of a call result.
func textOfResult(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()

	var parts []string
	for _, content := range res.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "")
}

// tokenPattern matches a credential in the command line's output.
var tokenPattern = regexp.MustCompile(`wakil_[A-Za-z0-9_\-]{16,}`)

// firstToken extracts the first credential from output.
func firstToken(t *testing.T, text string) string {
	t.Helper()

	found := tokenPattern.FindString(text)
	if found == "" {
		t.Fatalf("no credential in the output:\n%s", text)
	}
	return found
}

// referencedAssets returns the /assets/... URLs an index.html refers to. The
// shell is parsed by hand rather than with a full HTML parser: it is a file this
// repository generates, and the only thing worth extracting is which chunks the
// browser is going to ask for.
func referencedAssets(page string) []string {
	var out []string

	for _, attr := range []string{`src="`, `href="`} {
		rest := page
		for {
			start := strings.Index(rest, attr)
			if start < 0 {
				break
			}
			rest = rest[start+len(attr):]

			end := strings.Index(rest, `"`)
			if end < 0 {
				break
			}
			value := rest[:end]
			rest = rest[end:]

			if strings.HasPrefix(value, "/assets/") {
				out = append(out, value)
			}
		}
	}

	return out
}

// findProfile returns a named client profile.
func findProfile(t *testing.T, cfg *config.Config, name string) config.ClientProfile {
	t.Helper()

	for _, client := range cfg.Clients {
		if client.Name == name {
			return client
		}
	}
	t.Fatalf("no client named %q in %v", name, profileNames(cfg))
	return config.ClientProfile{}
}

// profileNames lists the configured client names.
func profileNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Clients))
	for _, client := range cfg.Clients {
		names = append(names, client.Name)
	}
	return names
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
