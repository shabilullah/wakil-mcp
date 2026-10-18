package secrets

import (
	"errors"
	"testing"
)

func testResolver(env map[string]string, files map[string]string) *Resolver {
	return &Resolver{
		LookupEnv: func(k string) (string, bool) {
			v, ok := env[k]
			return v, ok
		},
		ReadFile: func(name string) ([]byte, error) {
			v, ok := files[name]
			if !ok {
				return nil, errors.New("no such file")
			}
			return []byte(v), nil
		},
		AllowFileRefs: true,
	}
}

func TestResolveEnv(t *testing.T) {
	r := testResolver(map[string]string{"GH_TOKEN": "s3cret"}, nil)

	got, err := r.Resolve("${env:GH_TOKEN}")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "s3cret" {
		t.Errorf("got %q, want s3cret", got)
	}
}

func TestResolveEmbedded(t *testing.T) {
	r := testResolver(map[string]string{"TOK": "abc123"}, nil)

	got, err := r.Resolve("Bearer ${env:TOK}")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "Bearer abc123" {
		t.Errorf("got %q, want \"Bearer abc123\"", got)
	}

	got, err = r.Resolve("${env:TOK}-${env:TOK}")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "abc123-abc123" {
		t.Errorf("got %q, want \"abc123-abc123\"", got)
	}
}

func TestResolveFileTrimsTrailingNewline(t *testing.T) {
	r := testResolver(nil, map[string]string{"/run/secrets/tok": "line1\n"})

	got, err := r.Resolve("${file:/run/secrets/tok}")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "line1" {
		t.Errorf("got %q, want \"line1\"", got)
	}
}

func TestResolvePassthroughWithoutReferences(t *testing.T) {
	r := testResolver(nil, nil)
	got, err := r.Resolve("plain value $ and {braces}")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "plain value $ and {braces}" {
		t.Errorf("got %q", got)
	}
}

func TestResolveLeavesUnknownSchemeAlone(t *testing.T) {
	r := testResolver(nil, nil)
	got, err := r.Resolve("${vault:secret/foo}")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "${vault:secret/foo}" {
		t.Errorf("got %q, want the literal reference", got)
	}
}

func TestResolveLeavesUnterminatedAlone(t *testing.T) {
	r := testResolver(nil, nil)
	got, err := r.Resolve("${env:MISSING")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "${env:MISSING" {
		t.Errorf("got %q, want the literal text", got)
	}
}

func TestResolveMissingIsAnError(t *testing.T) {
	r := testResolver(nil, nil)
	_, err := r.Resolve("${env:NOPE}")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsMissing(err) {
		t.Errorf("expected IsMissing to be true, got %v", err)
	}
	var me *MissingReferenceError
	if !errors.As(err, &me) {
		t.Fatalf("expected *MissingReferenceError, got %T", err)
	}
	if me.Kind != "env" || me.Name != "NOPE" {
		t.Errorf("got (%q, %q), want (env, NOPE)", me.Kind, me.Name)
	}
}

func TestResolveFileRefsDisabled(t *testing.T) {
	r := testResolver(nil, map[string]string{"/etc/passwd": "root"})
	r.AllowFileRefs = false
	if _, err := r.Resolve("${file:/etc/passwd}"); err == nil {
		t.Fatal("expected file references to be refused")
	}
}

func TestResolveMap(t *testing.T) {
	r := testResolver(map[string]string{"A": "1", "B": "2"}, nil)
	got, err := r.ResolveMap(map[string]string{"x": "${env:A}", "y": "lit${env:B}"})
	if err != nil {
		t.Fatalf("ResolveMap: %v", err)
	}
	if got["x"] != "1" || got["y"] != "lit2" {
		t.Errorf("got %#v", got)
	}
}

func TestResolveMapReportsTheKey(t *testing.T) {
	r := testResolver(nil, nil)
	_, err := r.ResolveMap(map[string]string{"BAD_KEY": "${env:NOPE}"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); !contains(got, "BAD_KEY") {
		t.Errorf("error should name the key, got: %v", got)
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"${env:TOKEN}":                "${env:TOKEN}",
		"Bearer ${env:TOKEN}":         "Bearer ${env:TOKEN}",
		"${file:/run/secrets/x}":      "${file:/run/secrets/x}",
		"plain":                       "plain",
		"":                            "",
		"${vault:thing}":              "${vault:thing}",
		"a ${env:X} b ${file:/tmp/y}": "a ${env:X} b ${file:/tmp/y}",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactMapAndSlice(t *testing.T) {
	m := RedactMap(map[string]string{"AUTH": "Bearer ${env:TOKEN}"})
	if m["AUTH"] != "Bearer ${env:TOKEN}" {
		t.Errorf("RedactMap = %#v", m)
	}
	s := RedactSlice([]string{"${env:A}", "plain"})
	if s[0] != "${env:A}" || s[1] != "plain" {
		t.Errorf("RedactSlice = %#v", s)
	}
}

func TestReferenceSecretValues(t *testing.T) {
	r := testResolver(map[string]string{"TOKEN": "supersecretvalue"}, map[string]string{"/s/t": "filesecretvalue\n"})

	vals := r.ReferenceSecretValues(
		map[string]string{"A": "${env:TOKEN}", "B": "literal"},
		map[string]string{"Authorization": "Bearer ${file:/s/t}"},
	)
	if len(vals) != 2 {
		t.Fatalf("got %d secret values, want 2: %#v", len(vals), vals)
	}
}

func TestRedactSecrets(t *testing.T) {
	s := "error: token supersecretvalue was rejected"
	got := RedactSecrets(s, []string{"supersecretvalue"})
	if got != "error: token [REDACTED] was rejected" {
		t.Errorf("got %q", got)
	}

	// Short values are left alone: replacing them would mangle ordinary text.
	got = RedactSecrets("a short token abc", []string{"abc"})
	if got != "a short token abc" {
		t.Errorf("short secrets should not be redacted, got %q", got)
	}
}

func TestHasReference(t *testing.T) {
	if !HasReference("${env:X}") {
		t.Error("expected true")
	}
	if HasReference("plain") {
		t.Error("expected false")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 ||
		indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
