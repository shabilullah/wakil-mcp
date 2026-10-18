package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDurationParsing(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{`"15m"`, 15 * time.Minute, false},
		{`"1h30m"`, 90 * time.Minute, false},
		{`"500ms"`, 500 * time.Millisecond, false},
		{`"0s"`, 0, false},
		{`""`, 0, false},
		{`30`, 30 * time.Second, false},
		{`"45"`, 45 * time.Second, false},
		{`"nonsense"`, 0, true},
		{`true`, 0, true},
	}
	for _, tc := range cases {
		var d Duration
		err := d.UnmarshalJSON([]byte(tc.in))
		if tc.wantErr {
			if err == nil {
				t.Errorf("UnmarshalJSON(%s): expected an error, got %v", tc.in, d.D())
			}
			continue
		}
		if err != nil {
			t.Errorf("UnmarshalJSON(%s): unexpected error: %v", tc.in, err)
			continue
		}
		if d.D() != tc.want {
			t.Errorf("UnmarshalJSON(%s) = %v, want %v", tc.in, d.D(), tc.want)
		}
	}
}

func TestDurationRoundTrip(t *testing.T) {
	d := Duration(15 * time.Minute)
	b, err := d.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if string(b) != `"15m0s"` {
		t.Fatalf("MarshalJSON = %s, want \"15m0s\"", b)
	}
	var back Duration
	if err := back.UnmarshalJSON(b); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if back != d {
		t.Fatalf("round trip = %v, want %v", back, d)
	}
}

const validConfig = `{
  "version": 1,
  "server": { "bind": "127.0.0.1:8765", "disableAuth": true },
  "defaults": { "idleTimeout": "10m", "callTimeout": "60s" },
  "mcpServers": {
    "github": {
      "command": "npx",
      "args": ["-y", "server-github"],
      "env": { "TOKEN": "${env:GH_TOKEN}" }
    },
    "linear": {
      "url": "https://mcp.linear.app/mcp",
      "headers": { "Authorization": "Bearer ${file:/run/secrets/linear}" }
    }
  }
}`

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := c.MCPServers["github"].EffectiveType(); got != TransportStdio {
		t.Errorf("github type = %q, want stdio", got)
	}
	if got := c.MCPServers["linear"].EffectiveType(); got != TransportHTTP {
		t.Errorf("linear type = %q, want http", got)
	}
	if got := c.MCPServers["github"].EffectiveIdleTimeout(c.Defaults); got != 10*time.Minute {
		t.Errorf("github idleTimeout = %v, want 10m", got)
	}
	if got := c.MCPServers["linear"].EffectiveIdleTimeout(c.Defaults); got != 10*time.Minute {
		t.Errorf("linear idleTimeout = %v, want the default 10m", got)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(`{"version":1,"nope":true,"mcpServers":{}}`))
	if err == nil {
		t.Fatal("expected an error for an unknown field")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
}

func TestParseRejectsTrailingContent(t *testing.T) {
	_, err := Parse([]byte(`{"version":1,"mcpServers":{}} {"extra":true}`))
	if err == nil {
		t.Fatal("expected an error for trailing content")
	}
}

func TestValidateProblems(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name: "stdio without command",
			mutate: func(c *Config) {
				c.MCPServers["bad"] = &Upstream{Type: "stdio"}
			},
			want: "command: required",
		},
		{
			name: "http without url",
			mutate: func(c *Config) {
				c.MCPServers["bad"] = &Upstream{Type: "http"}
			},
			want: "url: required",
		},
		{
			name: "no transport at all",
			mutate: func(c *Config) {
				c.MCPServers["bad"] = &Upstream{}
			},
			want: "set either command (stdio) or url (http)",
		},
		{
			name: "invalid server key",
			mutate: func(c *Config) {
				c.MCPServers["has space"] = &Upstream{Command: "x"}
			},
			want: "key must match",
		},
		{
			name: "key with a dot",
			mutate: func(c *Config) {
				c.MCPServers["has.dot"] = &Upstream{Command: "x"}
			},
			want: "key must match",
		},
		{
			name: "bad transport type",
			mutate: func(c *Config) {
				c.MCPServers["bad"] = &Upstream{Type: "carrier-pigeon", Command: "x"}
			},
			want: "is not one of stdio, http, sse, auto",
		},
		{
			name: "url on a stdio server",
			mutate: func(c *Config) {
				c.MCPServers["bad"] = &Upstream{Command: "x", URL: "http://x"}
			},
			want: "url: not valid for a stdio server",
		},
		{
			name: "bad bind",
			mutate: func(c *Config) {
				c.Server.Bind = "not-a-host-port"
			},
			want: "not a valid host:port",
		},
		{
			name: "bad log level",
			mutate: func(c *Config) {
				c.Server.LogLevel = "verbose"
			},
			want: "is not one of debug, info, warn, error",
		},
		{
			name: "bad start policy",
			mutate: func(c *Config) {
				c.MCPServers["bad"] = &Upstream{Command: "x", StartPolicy: "whenever"}
			},
			want: "is not one of lazy, eager, never",
		},
		{
			name: "backoff factor too small",
			mutate: func(c *Config) {
				c.Defaults.RestartBackoff.Factor = 0.5
			},
			want: "must be greater than 1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(validConfig))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			tc.mutate(c)
			err = c.Validate()
			if err == nil {
				t.Fatal("expected a validation error")
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("expected a *ValidationError, got %T", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidateAuthRequiresClients(t *testing.T) {
	c, err := Parse([]byte(`{"version":1,"mcpServers":{}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c.Server.DisableAuth = false
	if err := c.Validate(); err == nil {
		t.Fatal("expected an error when auth is on and no clients exist")
	}

	// Both a client profile and an admin token are required once auth is on.
	c.Clients = []ClientProfile{{Name: "x", TokenHash: "sha256:" + strings.Repeat("a", 64)}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "adminTokenHash") {
		t.Fatalf("expected the admin token to still be required, got: %v", err)
	}

	c.Server.AdminTokenHash = "sha256:" + strings.Repeat("b", 64)
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error with both credentials defined: %v", err)
	}
}

func TestValidateAdminTokenShape(t *testing.T) {
	c, err := Parse([]byte(`{"version":1,"mcpServers":{}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c.Server.DisableAuth = true

	for _, bad := range []string{
		"nothashed",
		"sha256:tooshort",
		"sha256:" + strings.Repeat("z", 64), // not hex
		"md5:" + strings.Repeat("a", 64),
	} {
		c.Server.AdminTokenHash = bad
		if err := c.Validate(); err == nil {
			t.Errorf("adminTokenHash %q should have been rejected", bad)
		}
	}

	c.Server.AdminTokenHash = "sha256:" + strings.Repeat("0", 64)
	if err := c.Validate(); err != nil {
		t.Errorf("a well-formed hash should be accepted: %v", err)
	}
}

func TestValidateReservedNamespace(t *testing.T) {
	c, err := Parse([]byte(`{"version":1,"server":{"disableAuth":true},"mcpServers":{}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, key := range []string{"_wakil", "_wakil_thing", "_wakil-other"} {
		c.MCPServers = map[string]*Upstream{key: {Command: "x"}}
		err := c.Validate()
		if err == nil {
			t.Fatalf("server key %q should have been rejected as reserved", key)
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("expected a reservation complaint for %q, got: %v", key, err)
		}
	}

	// A key that merely contains the word is fine.
	c.MCPServers = map[string]*Upstream{"my_wakil_thing": {Command: "x"}}
	if err := c.Validate(); err != nil {
		t.Errorf("a key containing the reserved word should be allowed: %v", err)
	}
}

func TestValidateClientRules(t *testing.T) {
	c, err := Parse([]byte(`{"version":1,"mcpServers":{}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c.Server.DisableAuth = true
	c.Clients = []ClientProfile{
		{Name: "dup"},
		{Name: "dup"},
	}
	err = c.Validate()
	if err == nil {
		t.Fatal("expected a duplicate name error")
	}
	if !strings.Contains(err.Error(), "duplicates") {
		t.Errorf("expected a duplicate complaint, got: %v", err)
	}

	c.Clients = []ClientProfile{{Name: "ok", TokenHash: "sha256:tooshort"}}
	err = c.Validate()
	if err == nil || !strings.Contains(err.Error(), "64 lowercase hex characters") {
		t.Errorf("expected a tokenHash format error, got: %v", err)
	}

	same := "sha256:" + strings.Repeat("b", 64)
	c.Clients = []ClientProfile{{Name: "a", TokenHash: same}, {Name: "b", TokenHash: same}}
	err = c.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Errorf("expected a duplicate tokenHash error, got: %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	c, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := c.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Path() != path {
		t.Errorf("Path() = %q, want %q", got.Path(), path)
	}
	if len(got.MCPServers) != 2 {
		t.Errorf("got %d servers, want 2", len(got.MCPServers))
	}
	if got.MCPServers["github"].Env["TOKEN"] != "${env:GH_TOKEN}" {
		t.Errorf("secret reference did not survive the round trip: %q", got.MCPServers["github"].Env["TOKEN"])
	}
}

func TestSaveCreatesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	c := Default()
	c.Server.DisableAuth = true
	if err := c.SaveTo(path); err != nil {
		t.Fatalf("first SaveTo: %v", err)
	}
	c.Server.LogLevel = "debug"
	if err := c.SaveTo(path); err != nil {
		t.Fatalf("second SaveTo: %v", err)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("expected a .bak file: %v", err)
	}
	if !strings.Contains(string(bak), `"info"`) {
		t.Errorf("backup should hold the previous contents, got: %s", bak)
	}
}

func TestSaveRefusesInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	c := Default()
	c.Server.DisableAuth = true
	c.MCPServers["broken"] = &Upstream{Type: "stdio"} // no command
	err := c.SaveTo(filepath.Join(dir, "config.json"))
	if err == nil {
		t.Fatal("expected SaveTo to refuse an invalid config")
	}
	if !strings.Contains(err.Error(), "refusing to write") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestLoadOrCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	c, boot, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if c.Version != CurrentVersion {
		t.Errorf("version = %d, want %d", c.Version, CurrentVersion)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected the config file to be created: %v", err)
	}
	if boot == nil {
		t.Fatal("first run should return bootstrap credentials")
	}
	if boot.ClientToken == "" || boot.AdminToken == "" {
		t.Fatalf("both tokens should be minted, got %#v", boot)
	}
	if boot.ClientToken == boot.AdminToken {
		t.Error("the client and admin tokens must differ")
	}
	if len(c.Clients) != 1 {
		t.Fatalf("got %d clients, want 1", len(c.Clients))
	}
	if c.Clients[0].Name != boot.ClientName {
		t.Errorf("profile name = %q, want %q", c.Clients[0].Name, boot.ClientName)
	}
	if !VerifyTokenHash(boot.ClientToken, c.Clients[0].TokenHash) {
		t.Error("the bootstrap client token does not match the stored hash")
	}
	if !VerifyTokenHash(boot.AdminToken, c.Server.AdminTokenHash) {
		t.Error("the bootstrap admin token does not match the stored hash")
	}
	if err := c.Validate(); err != nil {
		t.Errorf("a bootstrapped config must be valid: %v", err)
	}

	// A second load must not mint new credentials.
	c2, boot2, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if boot2 != nil {
		t.Errorf("second load returned bootstrap credentials %#v, want nil", boot2)
	}
	if len(c2.Clients) != 1 {
		t.Errorf("second load has %d clients, want 1", len(c2.Clients))
	}
	if c2.Server.AdminTokenHash != c.Server.AdminTokenHash {
		t.Error("the admin token hash changed on reload")
	}
}

func TestTokenHashAndVerify(t *testing.T) {
	token, hash, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if !strings.HasPrefix(token, TokenPrefix) {
		t.Errorf("token %q should carry the %q prefix", token, TokenPrefix)
	}
	if !LooksLikeToken(token) {
		t.Error("LooksLikeToken should accept a generated token")
	}
	if !VerifyTokenHash(token, hash) {
		t.Error("VerifyTokenHash should accept the matching token")
	}
	if VerifyTokenHash(token+"x", hash) {
		t.Error("VerifyTokenHash should reject a modified token")
	}
	if VerifyTokenHash("", hash) {
		t.Error("VerifyTokenHash should reject an empty token")
	}
	if VerifyTokenHash(token, "") {
		t.Error("VerifyTokenHash should reject an empty hash")
	}
	if VerifyTokenHash(token, "sha256:deadbeef") {
		t.Error("VerifyTokenHash should reject a wrong-length hash")
	}

	// Two tokens must never collide.
	t2, h2, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if token == t2 || hash == h2 {
		t.Error("generated tokens should be distinct")
	}
}

func TestMaskToken(t *testing.T) {
	got := MaskToken("wakil_abcdefghijklmnop")
	if !strings.HasPrefix(got, "wakil_abcd") {
		t.Errorf("masked token should keep a readable prefix, got %q", got)
	}
	if !strings.HasSuffix(got, "mnop") {
		t.Errorf("masked token should keep the last four characters, got %q", got)
	}
	if strings.Contains(got, "efghijkl") {
		t.Errorf("masked token leaked the middle: %q", got)
	}
}

func TestNamespaceResolution(t *testing.T) {
	c, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	u := c.MCPServers["github"]

	prefix, sep, enabled := u.NamespaceFor("github", c.Defaults)
	if !enabled || prefix != "github" || sep != "." {
		t.Fatalf("defaults = (%q, %q, %v), want (github, ., true)", prefix, sep, enabled)
	}

	no := false
	u.Namespace = &NamespaceConfig{Enabled: &no}
	if _, _, enabled := u.NamespaceFor("github", c.Defaults); enabled {
		t.Fatal("expected namespacing to be disabled")
	}

	u.Namespace = &NamespaceConfig{Prefix: "gh", Separator: "_"}
	prefix, sep, enabled = u.NamespaceFor("github", c.Defaults)
	if !enabled || prefix != "gh" || sep != "_" {
		t.Fatalf("override = (%q, %q, %v), want (gh, _, true)", prefix, sep, enabled)
	}
}

func TestCloneIsDeep(t *testing.T) {
	c, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	clone := c.Clone()
	if clone == nil {
		t.Fatal("Clone returned nil")
	}
	clone.MCPServers["github"].Args[0] = "changed"
	if c.MCPServers["github"].Args[0] == "changed" {
		t.Fatal("mutating the clone affected the original")
	}
}

func TestIsEnabled(t *testing.T) {
	u := &Upstream{}
	if !u.IsEnabled() {
		t.Error("an upstream with no enabled field should default to enabled")
	}
	no := false
	u.Enabled = &no
	if u.IsEnabled() {
		t.Error("enabled=false should report disabled")
	}
}
