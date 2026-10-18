package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shabilullah/wakil-mcp/internal/aggregate"
	"github.com/shabilullah/wakil-mcp/internal/auth"
	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/upstream"
)

// TestSDKToolSchemasAreNeverNull guards a bug that only a strict client finds.
//
// The SDK's schema fields are typed any. Assigning an empty json.RawMessage to
// one produces a non-nil interface holding a nil slice, which survives
// omitempty and marshals as the literal null. The official TypeScript SDK
// validates tools/list against the spec, so a single null outputSchema made it
// reject the whole list with "expected object, received null". The Go SDK's own
// client decodes into typed structs and cannot see the difference, which is
// exactly why this test asserts on the bytes.
func TestSDKToolSchemasAreNeverNull(t *testing.T) {
	tests := []struct {
		name       string
		tool       upstream.Tool
		wantInput  string
		wantOutput string
	}{
		{
			name:      "no schemas at all",
			tool:      upstream.Tool{Name: "bare"},
			wantInput: fallbackInputSchema,
		},
		{
			name:      "a null output schema stays absent",
			tool:      upstream.Tool{Name: "nulled", InputSchema: json.RawMessage(`{}`), OutputSchema: json.RawMessage(`null`)},
			wantInput: `{}`,
		},
		{
			name:      "an empty output schema stays absent",
			tool:      upstream.Tool{Name: "empty", InputSchema: json.RawMessage(`{}`), OutputSchema: json.RawMessage(`  `)},
			wantInput: `{}`,
		},
		{
			name: "a real output schema is passed through",
			tool: upstream.Tool{
				Name:         "rich",
				InputSchema:  json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"}}}`),
				OutputSchema: json.RawMessage(`{"type":"object","properties":{"sum":{"type":"number"}}}`),
			},
			wantInput:  `{"type":"object","properties":{"a":{"type":"number"}}}`,
			wantOutput: `{"type":"object","properties":{"sum":{"type":"number"}}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{}
			entry := aggregate.Entry{Exposed: "srv." + tc.tool.Name, ServerKey: "srv", UpstreamName: tc.tool.Name, Tool: tc.tool}

			raw, err := json.Marshal(p.sdkTool(entry))
			if err != nil {
				t.Fatalf("marshaling the tool: %v", err)
			}
			wire := string(raw)

			// No field may be present with a null value. The check is per
			// field rather than a substring search, because a tool called
			// "nulled" would defeat the latter.
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatalf("decoding the tool: %v", err)
			}
			for key, value := range fields {
				if string(bytes.TrimSpace(value)) == "null" {
					t.Errorf("%s is present and null, want the key left out:\n%s", key, wire)
				}
			}

			if got := string(fields["inputSchema"]); got != tc.wantInput {
				t.Errorf("inputSchema is %s, want %s", got, tc.wantInput)
			}
			if got := string(fields["outputSchema"]); got != tc.wantOutput {
				t.Errorf("outputSchema is %s, want %s", got, tc.wantOutput)
			}
		})
	}
}

// TestSDKToolKeepsUnmodelledFields checks that the parts of a tool definition
// the proxy does not interpret still reach the harness.
func TestSDKToolKeepsUnmodelledFields(t *testing.T) {
	p := &Proxy{}
	entry := aggregate.Entry{
		Exposed: "srv.echo",
		Tool: upstream.Tool{
			Name:        "echo",
			Title:       "Echo",
			Description: "Echoes text.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","x-vendor-extension":{"keep":true}}},"$defs":{"tag":{"type":"object"}}}`),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		},
	}

	raw, err := json.Marshal(p.sdkTool(entry))
	if err != nil {
		t.Fatalf("marshaling the tool: %v", err)
	}

	for _, want := range []string{`"$defs"`, `"x-vendor-extension"`, `"readOnlyHint":true`, `"title":"Echo"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the tool definition lost %s:\n%s", want, raw)
		}
	}
}

// TestSyncRenamesToolsThatWouldShadowTheProxysOwn covers a server configured
// with namespacing switched off: its tool names are exposed exactly as they
// arrive, so one can land on a synthetic name. The SDK replaces a tool
// registered under an existing name, so without the reservation the upstream
// would quietly take the place of the proxy's own tool.
func TestSyncRenamesToolsThatWouldShadowTheProxysOwn(t *testing.T) {
	p := New(Options{Version: "test"})

	namespacingOff := false
	snap := upstream.Snapshot{
		Key:     "sneaky",
		Enabled: true,
		Config:  &config.Upstream{Namespace: &config.NamespaceConfig{Enabled: &namespacingOff}},
		Tools: []upstream.Tool{
			{Name: ToolListServer, InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: ToolSearch, InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "harmless", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}

	ix := p.Sync([]upstream.Snapshot{snap})

	var exposed []string
	for _, e := range ix.Entries() {
		exposed = append(exposed, e.Exposed)
	}

	for _, reserved := range []string{ToolListServer, ToolSearch} {
		if _, ok := ix.Lookup(reserved); ok {
			t.Errorf("an upstream tool took the reserved name %q", reserved)
		}
		if _, ok := ix.Lookup(reserved + "-2"); !ok {
			t.Errorf("the clashing name %q was dropped instead of renamed; exposed: %v", reserved, exposed)
		}
	}
	if _, ok := ix.Lookup("harmless"); !ok {
		t.Errorf("an unaffected tool went missing; exposed: %v", exposed)
	}

	collisions := ix.Collisions()
	if len(collisions) != 2 {
		t.Fatalf("got %d collisions, want 2: %+v", len(collisions), collisions)
	}
	for _, c := range collisions {
		if c.HeldBy != syntheticHolder {
			t.Errorf("the collision blames %q, want %q", c.HeldBy, syntheticHolder)
		}
	}
}

// TestFilterToolsDoesNotTrustTheSyntheticPrefix keeps a name that merely looks
// synthetic from skipping the profile filter. Exact membership is the test.
func TestFilterToolsDoesNotTrustTheSyntheticPrefix(t *testing.T) {
	p := New(Options{Version: "test"})

	genuine := &mcp.Tool{Name: ToolListServer}
	impostor := &mcp.Tool{Name: SyntheticToolPrefix + ".something_else"}
	foreign := &mcp.Tool{Name: "modern.echo"}

	kept := p.filterTools(auth.Profile{Name: "nobody"}, []*mcp.Tool{impostor, genuine, foreign})

	if len(kept) != 1 || kept[0].Name != ToolListServer {
		var names []string
		for _, tool := range kept {
			names = append(names, tool.Name)
		}
		t.Errorf("a profile that may reach nothing was left with %v", names)
	}
}
