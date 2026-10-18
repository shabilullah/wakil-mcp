package aggregate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/upstream"
)

// tool is a terse helper for building upstream tools in tests.
func tool(name, description string, schema string) upstream.Tool {
	t := upstream.Tool{Name: name, Description: description}
	if schema != "" {
		t.InputSchema = json.RawMessage(schema)
	}
	return t
}

// buildIndex is a convenience wrapper for the common one-server case.
func buildIndex(t *testing.T, defaults config.ServerDefaults, key string, cfg *config.Upstream, tools ...upstream.Tool) *Index {
	t.Helper()
	b := NewBuilder(defaults)
	b.AddServer(key, cfg, tools)
	return b.Build()
}

func TestNamespacingDefault(t *testing.T) {
	ix := buildIndex(t, config.ServerDefaults{}, "github", nil,
		tool("create_issue", "Open an issue", `{"type":"object"}`))

	e, ok := ix.Lookup("github.create_issue")
	if !ok {
		t.Fatalf("expected github.create_issue, got %v", ix.Entries())
	}
	if e.ServerKey != "github" {
		t.Errorf("ServerKey = %q, want github", e.ServerKey)
	}
	if e.UpstreamName != "create_issue" {
		t.Errorf("UpstreamName = %q, want create_issue", e.UpstreamName)
	}
	if _, ok := ix.Lookup("create_issue"); ok {
		t.Error("the un-namespaced name should not resolve when namespacing is on")
	}
}

func TestNamespacingDisabled(t *testing.T) {
	no := false
	cfg := &config.Upstream{Namespace: &config.NamespaceConfig{Enabled: &no}}
	ix := buildIndex(t, config.ServerDefaults{}, "github", cfg,
		tool("create_issue", "", `{"type":"object"}`))

	if _, ok := ix.Lookup("create_issue"); !ok {
		t.Fatalf("expected the raw name, got %v", ix.Entries())
	}
}

func TestCustomPrefixAndSeparator(t *testing.T) {
	cfg := &config.Upstream{Namespace: &config.NamespaceConfig{Prefix: "gh", Separator: "_"}}
	ix := buildIndex(t, config.ServerDefaults{}, "github", cfg,
		tool("create_issue", "", `{"type":"object"}`))

	if _, ok := ix.Lookup("gh_create_issue"); !ok {
		t.Fatalf("expected gh_create_issue, got %v", ix.Entries())
	}
}

func TestGlobalSeparatorDefault(t *testing.T) {
	defaults := config.ServerDefaults{Separator: "::"}
	ix := buildIndex(t, defaults, "srv", nil, tool("do", "", `{"type":"object"}`))
	if _, ok := ix.Lookup("srv::do"); !ok {
		t.Fatalf("expected srv::do, got %v", ix.Entries())
	}
}

func TestLookupIsNotStringParsing(t *testing.T) {
	// A tool whose own name contains the separator must not be split apart.
	ix := buildIndex(t, config.ServerDefaults{}, "a", nil,
		tool("b.c.d", "dotted tool name", `{"type":"object"}`))

	e, ok := ix.Lookup("a.b.c.d")
	if !ok {
		t.Fatalf("expected a.b.c.d, got %v", ix.Entries())
	}
	if e.UpstreamName != "b.c.d" {
		t.Errorf("UpstreamName = %q, want b.c.d", e.UpstreamName)
	}
	if e.ServerKey != "a" {
		t.Errorf("ServerKey = %q, want a", e.ServerKey)
	}
}

func TestTruncationKeepsNamesDistinct(t *testing.T) {
	// Two names sharing a long prefix must stay distinguishable after
	// truncation, and the result must respect the 128 character limit.
	long1 := "p" + strings.Repeat("a", 200) + "one"
	long2 := "p" + strings.Repeat("a", 200) + "two"

	ix := buildIndex(t, config.ServerDefaults{}, "s", nil,
		tool(long1, "", `{"type":"object"}`),
		tool(long2, "", `{"type":"object"}`))

	if ix.Len() != 2 {
		t.Fatalf("expected 2 tools, got %d: %v", ix.Len(), ix.Entries())
	}
	seen := map[string]bool{}
	for _, e := range ix.Entries() {
		if len(e.Exposed) > MaxToolNameLen {
			t.Errorf("exposed name is %d characters, limit is %d: %q", len(e.Exposed), MaxToolNameLen, e.Exposed)
		}
		if !e.Truncated {
			t.Errorf("%q should be marked truncated", e.Exposed)
		}
		if seen[e.Exposed] {
			t.Errorf("duplicate exposed name %q", e.Exposed)
		}
		seen[e.Exposed] = true
	}

	// Both must still resolve to their original upstream names.
	found := map[string]bool{}
	for _, e := range ix.Entries() {
		got, ok := ix.Lookup(e.Exposed)
		if !ok || got.UpstreamName == "" {
			t.Errorf("lookup failed for %q", e.Exposed)
		}
		found[got.UpstreamName] = true
	}
	if !found[long1] || !found[long2] {
		t.Errorf("both long names should be reachable, got %v", found)
	}
}

func TestCollisionWithinServerGetsSuffix(t *testing.T) {
	// Sanitisation folds these two distinct names onto one.
	ix := buildIndex(t, config.ServerDefaults{}, "s", nil,
		tool("do thing", "with a space", `{"type":"object"}`),
		tool("do/thing", "with a slash", `{"type":"object"}`))

	if ix.Len() != 2 {
		t.Fatalf("expected both tools to survive, got %d: %v", ix.Len(), ix.Entries())
	}
	names := []string{ix.Entries()[0].Exposed, ix.Entries()[1].Exposed}
	if names[0] == names[1] {
		t.Fatalf("names collided: %v", names)
	}
	if len(ix.Collisions()) != 1 {
		t.Fatalf("expected 1 recorded collision, got %v", ix.Collisions())
	}
	c := ix.Collisions()[0]
	if !strings.HasSuffix(c.Given, "-2") {
		t.Errorf("expected a -2 suffix, got %q", c.Given)
	}
}

func TestCrossServerCollisionWhenNamespacingOff(t *testing.T) {
	no := false
	cfg := &config.Upstream{Namespace: &config.NamespaceConfig{Enabled: &no}}

	b := NewBuilder(config.ServerDefaults{})
	b.AddServer("alpha", cfg, []upstream.Tool{tool("shared", "from alpha", `{"type":"object"}`)})
	b.AddServer("beta", cfg, []upstream.Tool{tool("shared", "from beta", `{"type":"object"}`)})
	ix := b.Build()

	if ix.Len() != 2 {
		t.Fatalf("expected 2 tools, got %d: %v", ix.Len(), ix.Entries())
	}
	if len(ix.Collisions()) != 1 {
		t.Fatalf("expected 1 collision, got %v", ix.Collisions())
	}
	c := ix.Collisions()[0]
	if c.HeldBy == "" || c.HeldBy == c.ServerKey {
		t.Errorf("collision should name a different holder, got %+v", c)
	}

	// Each name must route back to the server that supplied it.
	for _, e := range ix.Entries() {
		got, ok := ix.Lookup(e.Exposed)
		if !ok {
			t.Fatalf("lookup failed for %q", e.Exposed)
		}
		if got.ServerKey != e.ServerKey {
			t.Errorf("lookup for %q returned server %q, want %q", e.Exposed, got.ServerKey, e.ServerKey)
		}
	}
}

func TestIncludeAndExclude(t *testing.T) {
	cfg := &config.Upstream{Tools: &config.ToolFilter{
		Exclude: []string{"delete_*"},
	}}
	ix := buildIndex(t, config.ServerDefaults{}, "s", cfg,
		tool("create_thing", "", `{"type":"object"}`),
		tool("delete_thing", "", `{"type":"object"}`),
		tool("delete_all", "", `{"type":"object"}`))

	if ix.Len() != 1 {
		t.Fatalf("expected only create_thing, got %v", ix.Entries())
	}
	if ix.Entries()[0].UpstreamName != "create_thing" {
		t.Errorf("wrong tool survived: %v", ix.Entries())
	}
}

func TestIncludeOnly(t *testing.T) {
	cfg := &config.Upstream{Tools: &config.ToolFilter{
		Include: []string{"get_*", "list_*"},
	}}
	ix := buildIndex(t, config.ServerDefaults{}, "s", cfg,
		tool("get_thing", "", `{"type":"object"}`),
		tool("list_things", "", `{"type":"object"}`),
		tool("delete_thing", "", `{"type":"object"}`))

	if ix.Len() != 2 {
		t.Fatalf("expected 2 tools, got %v", ix.Entries())
	}
}

func TestExcludeWinsOverInclude(t *testing.T) {
	cfg := &config.Upstream{Tools: &config.ToolFilter{
		Include: []string{"*_thing"},
		Exclude: []string{"delete_thing"},
	}}
	ix := buildIndex(t, config.ServerDefaults{}, "s", cfg,
		tool("get_thing", "", `{"type":"object"}`),
		tool("delete_thing", "", `{"type":"object"}`))

	if ix.Len() != 1 || ix.Entries()[0].UpstreamName != "get_thing" {
		t.Fatalf("expected only get_thing, got %v", ix.Entries())
	}
}

func TestFiltersMatchOriginalNamesNotAliases(t *testing.T) {
	cfg := &config.Upstream{Tools: &config.ToolFilter{
		Include: []string{"original"},
		Aliases: map[string]string{"original": "renamed"},
	}}
	ix := buildIndex(t, config.ServerDefaults{}, "s", cfg,
		tool("original", "", `{"type":"object"}`),
		tool("renamed", "", `{"type":"object"}`))

	// "original" passes the filter and is renamed; the pre-existing "renamed"
	// tool is filtered out because its own name does not match.
	if ix.Len() != 1 {
		t.Fatalf("expected 1 tool, got %v", ix.Entries())
	}
	e := ix.Entries()[0]
	if e.Exposed != "s.renamed" {
		t.Errorf("Exposed = %q, want s.renamed", e.Exposed)
	}
	if !e.Renamed {
		t.Error("the entry should be flagged as renamed")
	}
	if e.UpstreamName != "original" {
		t.Errorf("UpstreamName = %q; routing must use the original name", e.UpstreamName)
	}
}

func TestMaxToolsPerServer(t *testing.T) {
	defaults := config.ServerDefaults{MaxToolsPerServer: 2}
	ix := buildIndex(t, defaults, "s", nil,
		tool("one", "", `{"type":"object"}`),
		tool("two", "", `{"type":"object"}`),
		tool("three", "", `{"type":"object"}`))

	if ix.Len() != 2 {
		t.Fatalf("expected the cap to hold, got %v", ix.Entries())
	}
}

func TestMissingSchemaIsRepaired(t *testing.T) {
	cases := map[string]string{
		"absent": "",
		"null":   "null",
		"blank":  "   ",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			ix := buildIndex(t, config.ServerDefaults{}, "s", nil, tool("t", "", raw))
			if ix.Len() != 1 {
				t.Fatalf("expected 1 tool, got %d", ix.Len())
			}
			got := string(ix.Entries()[0].Tool.InputSchema)

			var schema map[string]any
			if err := json.Unmarshal([]byte(got), &schema); err != nil {
				t.Fatalf("repaired schema is not valid JSON: %v (%s)", err, got)
			}
			if schema["type"] != "object" {
				t.Errorf("repaired schema should be an object schema, got %s", got)
			}
			if schema["additionalProperties"] != false {
				t.Errorf("repaired schema should forbid extra properties, got %s", got)
			}
		})
	}
}

func TestRichSchemaPassesThroughUnchanged(t *testing.T) {
	schema := `{"type":"object","properties":{"a":{"type":"string","x-custom":[1,2,{"deep":true}]}},"$defs":{"d":{"type":"number"}},"additionalProperties":false}`
	ix := buildIndex(t, config.ServerDefaults{}, "s", nil, tool("t", "", schema))

	got := string(ix.Entries()[0].Tool.InputSchema)

	var want, have any
	if err := json.Unmarshal([]byte(schema), &want); err != nil {
		t.Fatalf("test schema is invalid: %v", err)
	}
	if err := json.Unmarshal([]byte(got), &have); err != nil {
		t.Fatalf("passthrough schema is invalid: %v", err)
	}
	if !jsonEqual(want, have) {
		t.Errorf("schema changed during the round trip:\n want %s\n got  %s", schema, got)
	}
}

// jsonEqual compares two decoded JSON values structurally.
func jsonEqual(a, b any) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ab) == string(bb)
}

func TestNullOutputSchemaDropped(t *testing.T) {
	tt := tool("t", "", `{"type":"object"}`)
	tt.OutputSchema = json.RawMessage(`null`)
	ix := buildIndex(t, config.ServerDefaults{}, "s", nil, tt)

	if got := ix.Entries()[0].Tool.OutputSchema; len(got) != 0 {
		t.Errorf("a null output schema should be dropped, got %q", got)
	}
}

func TestFindSearchesNameAndDescription(t *testing.T) {
	ix := buildIndex(t, config.ServerDefaults{}, "github", nil,
		tool("create_issue", "Open a new issue", `{"type":"object"}`),
		tool("list_repos", "Enumerate repositories", `{"type":"object"}`))

	if got := ix.Find("issue"); len(got) != 1 || got[0].UpstreamName != "create_issue" {
		t.Errorf("searching by name found %v", got)
	}
	if got := ix.Find("repositories"); len(got) != 1 || got[0].UpstreamName != "list_repos" {
		t.Errorf("searching by description found %v", got)
	}
	if got := ix.Find("github"); len(got) != 2 {
		t.Errorf("searching by server should match all its tools, got %d", len(got))
	}
	if got := ix.Find("nothing-matches-this"); len(got) != 0 {
		t.Errorf("expected no matches, got %v", got)
	}
}

func TestEmptyToolNameIsGivenAFallback(t *testing.T) {
	ix := buildIndex(t, config.ServerDefaults{}, "s", nil, tool("", "unnamed", `{"type":"object"}`))
	if ix.Len() != 1 {
		t.Fatalf("expected the tool to be kept, got %d", ix.Len())
	}
	e := ix.Entries()[0]
	if e.Exposed == "" || e.Exposed == "s." {
		t.Errorf("a fallback name should have been generated, got %q", e.Exposed)
	}
	if _, ok := ix.Lookup(e.Exposed); !ok {
		t.Errorf("the fallback name must be resolvable, got %q", e.Exposed)
	}
}

func TestForServerAndServers(t *testing.T) {
	b := NewBuilder(config.ServerDefaults{})
	b.AddServer("alpha", nil, []upstream.Tool{tool("a1", "", `{"type":"object"}`)})
	b.AddServer("beta", nil, []upstream.Tool{tool("b1", "", `{"type":"object"}`), tool("b2", "", `{"type":"object"}`)})
	ix := b.Build()

	if got := ix.ForServer("beta"); len(got) != 2 {
		t.Errorf("beta has %d tools, want 2", len(got))
	}
	if got := ix.ForServer("missing"); len(got) != 0 {
		t.Errorf("an unknown server should have no tools, got %v", got)
	}
	servers := ix.Servers()
	if len(servers) != 2 || servers[0] != "alpha" || servers[1] != "beta" {
		t.Errorf("Servers() = %v, want [alpha beta]", servers)
	}
}

func TestEntriesAreSortedByExposedName(t *testing.T) {
	ix := buildIndex(t, config.ServerDefaults{}, "s", nil,
		tool("zebra", "", `{"type":"object"}`),
		tool("apple", "", `{"type":"object"}`),
		tool("mango", "", `{"type":"object"}`))

	var names []string
	for _, e := range ix.Entries() {
		names = append(names, e.Exposed)
	}
	want := []string{"s.apple", "s.mango", "s.zebra"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("entries = %v, want %v", names, want)
		}
	}
}

func TestNilIndexLookupIsSafe(t *testing.T) {
	var ix *Index
	if _, ok := ix.Lookup("anything"); ok {
		t.Error("a nil index should resolve nothing")
	}
}

func TestGlobMatching(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"*", "anything", true},
		{"exact", "exact", true},
		{"exact", "EXACT", true}, // case-insensitive
		{"exact", "other", false},
		{"get_*", "get_thing", true},
		{"get_*", "set_thing", false},
		{"*_thing", "get_thing", true},
		{"*thing*", "get_thing_here", true},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"a?c", "abbc", false},
		{"*a*b*c*", "xxaxxbxxcxx", true},
		{"*a*b*c*", "xxaxxcxxbxx", false},
		{"", "anything", false},
	}
	for _, tc := range cases {
		if got := matchPattern(tc.pattern, tc.name); got != tc.want {
			t.Errorf("matchPattern(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}
