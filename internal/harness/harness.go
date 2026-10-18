// Package harness renders ready-to-paste MCP client configuration for the
// harnesses people actually use.
//
// The point is that adding a new client is a copy and paste rather than a hunt
// through documentation for the right JSON shape. Each snippet carries a note
// describing what to check, because client configuration formats move and this
// package cannot track every release.
package harness

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Input describes the deployment being documented.
type Input struct {
	// Harness selects the client. Empty renders every supported harness.
	Harness string
	// BaseURL is the Streamable HTTP endpoint, for example
	// http://127.0.0.1:8765/mcp.
	BaseURL string
	// SSEURL is the deprecated HTTP+SSE endpoint.
	SSEURL string
	// LegacySSE reports whether the SSE endpoint is actually mounted.
	LegacySSE bool
	// Token, when set, is written literally into the snippets. When empty each
	// harness gets a placeholder in its own secret-reference syntax.
	Token string
	// Binary is the command name of this program, used by the stdio bridge
	// form.
	Binary string
}

// Snippet is one configuration fragment.
type Snippet struct {
	Label    string `json:"label"`
	Language string `json:"language"`
	Filename string `json:"filename,omitempty"`
	Content  string `json:"content"`
	Note     string `json:"note,omitempty"`
	// Deprecated marks a snippet kept only for backwards compatibility.
	Deprecated bool `json:"deprecated,omitempty"`
}

// supported lists every harness, newest convention first, with the renderer.
var supported = map[string]func(Input) []Snippet{
	"opencode":       openCode,
	"claude-code":    claudeCode,
	"claude-desktop": claudeDesktop,
	"cursor":         cursor,
	"vscode":         vscode,
	"codex":          codex,
	"windsurf":       windsurf,
	"generic":        generic,
}

// Names returns the supported harness names, sorted.
func Names() []string {
	names := make([]string, 0, len(supported))
	for name := range supported {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Snippets renders the configuration for one harness, or for every harness when
// in.Harness is empty.
func Snippets(in Input) ([]Snippet, error) {
	if in.Binary == "" {
		in.Binary = "wakil-mcp"
	}
	if in.BaseURL == "" {
		return nil, fmt.Errorf("a base URL is required")
	}

	if in.Harness == "" {
		var all []Snippet
		for _, name := range Names() {
			all = append(all, supported[name](in)...)
		}
		return all, nil
	}

	render, ok := supported[strings.ToLower(strings.TrimSpace(in.Harness))]
	if !ok {
		return nil, fmt.Errorf("unknown harness %q; supported: %s",
			in.Harness, strings.Join(Names(), ", "))
	}
	return render(in), nil
}

// ---------------------------------------------------------------------------
// Secret references
// ---------------------------------------------------------------------------
//
// Each client substitutes environment variables with its own syntax, and none
// of them agree. Emitting the right one is the difference between a snippet
// that works when pasted and one that silently sends the literal text
// "WAKIL_TOKEN" as a credential.

func (in Input) token() string {
	if in.Token != "" {
		return in.Token
	}
	return "REPLACE_WITH_TOKEN"
}

// openCodeRef is opencode's substitution syntax.
func (in Input) openCodeRef() string {
	if in.Token != "" {
		return in.Token
	}
	return "{env:WAKIL_TOKEN}"
}

// cursorRef is Cursor's substitution syntax.
func (in Input) cursorRef() string {
	if in.Token != "" {
		return in.Token
	}
	return "${env:WAKIL_TOKEN}"
}

// shellRef is plain shell expansion.
func (in Input) shellRef() string { return "${WAKIL_TOKEN}" }

// ---------------------------------------------------------------------------
// HTTP-style harnesses
// ---------------------------------------------------------------------------

// openCode renders the opencode configuration block.
func openCode(in Input) []Snippet {
	body := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"mcp": map[string]any{
			"wakil": map[string]any{
				"type":    "remote",
				"url":     in.BaseURL,
				"enabled": true,
				"headers": map[string]string{
					"Authorization": "Bearer " + in.openCodeRef(),
				},
			},
		},
	}
	return []Snippet{{
		Label:    "opencode",
		Language: "json",
		Filename: "opencode.json",
		Content:  mustJSON(body),
		Note: "Merge the \"mcp\" block into your opencode.json. " +
			"opencode substitutes {env:WAKIL_TOKEN} from the environment.",
	}}
}

// claudeCode renders the Claude Code command and its .mcp.json form.
func claudeCode(in Input) []Snippet {
	command := fmt.Sprintf(
		"claude mcp add --transport http wakil %s \\\n  --header \"Authorization: Bearer $WAKIL_TOKEN\"",
		in.BaseURL)

	body := map[string]any{
		"mcpServers": map[string]any{
			"wakil": map[string]any{
				"type": "http",
				"url":  in.BaseURL,
				"headers": map[string]string{
					"Authorization": "Bearer " + in.shellRef(),
				},
			},
		},
	}
	return []Snippet{
		{
			Label:    "Claude Code (command)",
			Language: "shell",
			Content:  command,
			Note:     "Run once. The header value is expanded by your shell, so export WAKIL_TOKEN first.",
		},
		{
			Label:    "Claude Code (project file)",
			Language: "json",
			Filename: ".mcp.json",
			Content:  mustJSON(body),
			Note:     "Commit this to the project root to share the server with the whole repository.",
		},
	}
}

// claudeDesktop renders the desktop app's configuration.
//
// The desktop app reads a stdio-only mcpServers block, so it goes through the
// stdio bridge rather than connecting over HTTP.
func claudeDesktop(in Input) []Snippet {
	body := map[string]any{
		"mcpServers": map[string]any{
			"wakil": map[string]any{
				"command": in.Binary,
				"args":    []string{"stdio", "--url", in.BaseURL},
				"env": map[string]string{
					"WAKIL_TOKEN": in.token(),
				},
			},
		},
	}
	return []Snippet{{
		Label:    "Claude Desktop",
		Language: "json",
		Filename: "claude_desktop_config.json",
		Content:  mustJSON(body),
		Note: "Goes through the stdio bridge, so no HTTP client support is needed. " +
			"Set WAKIL_TOKEN to a real token, or export it and drop the env block. " +
			"Some builds now accept a remote \"url\" entry directly.",
	}}
}

// cursor renders Cursor's configuration.
func cursor(in Input) []Snippet {
	body := map[string]any{
		"mcpServers": map[string]any{
			"wakil": map[string]any{
				"url": in.BaseURL,
				"headers": map[string]string{
					"Authorization": "Bearer " + in.cursorRef(),
				},
			},
		},
	}
	return []Snippet{{
		Label:    "Cursor",
		Language: "json",
		Filename: ".cursor/mcp.json",
		Content:  mustJSON(body),
		Note: "Put this in ~/.cursor/mcp.json for every project, or .cursor/mcp.json " +
			"inside one project. Cursor expands ${env:WAKIL_TOKEN} from the environment.",
	}}
}

// vscode renders VS Code's workspace configuration.
func vscode(in Input) []Snippet {
	body := map[string]any{
		"inputs": []map[string]any{{
			"type":        "promptString",
			"id":          "wakil-token",
			"description": "wakil-mcp bearer token",
			"password":    true,
		}},
		"servers": map[string]any{
			"wakil": map[string]any{
				"type": "http",
				"url":  in.BaseURL,
				"headers": map[string]string{
					"Authorization": "Bearer ${input:wakil-token}",
				},
			},
		},
	}
	return []Snippet{{
		Label:    "VS Code",
		Language: "json",
		Filename: ".vscode/mcp.json",
		Content:  mustJSON(body),
		Note: "VS Code prompts for the token once and stores it, so the secret never " +
			"lands in the file.",
	}}
}

// windsurf renders Windsurf's configuration.
func windsurf(in Input) []Snippet {
	body := map[string]any{
		"mcpServers": map[string]any{
			"wakil": map[string]any{
				"serverUrl": in.BaseURL,
				"headers": map[string]string{
					"Authorization": "Bearer " + in.cursorRef(),
				},
			},
		},
	}
	return []Snippet{{
		Label:    "Windsurf",
		Language: "json",
		Filename: "mcp_config.json",
		Content:  mustJSON(body),
		Note:     "Lives at ~/.codeium/windsurf/mcp_config.json.",
	}}
}

// codex renders Codex's TOML configuration.
func codex(in Input) []Snippet {
	content := fmt.Sprintf(`[mcp_servers.wakil]
command = %q
args = ["stdio", "--url", %q]
env = { WAKIL_TOKEN = %q }
`, in.Binary, in.BaseURL, in.token())

	return []Snippet{{
		Label:    "Codex CLI",
		Language: "toml",
		Filename: "~/.codex/config.toml",
		Content:  content,
		Note: "Uses the stdio bridge. Newer builds can also connect directly with " +
			"url = " + quote(in.BaseURL) + " plus an http_headers block.",
	}}
}

// generic is the always-correct fallback: the raw endpoint and header a client
// needs, with no assumption about its configuration format.
func generic(in Input) []Snippet {
	content := fmt.Sprintf(`Transport:  Streamable HTTP
Endpoint:   %s
Method:     POST
Headers:    Authorization: Bearer <your token>
            Content-Type: application/json
            Accept: application/json, text/event-stream
`, in.BaseURL)

	snippets := []Snippet{{
		Label:    "Any MCP client",
		Language: "text",
		Content:  content,
		Note: "A modern client will handle the handshake itself. This is what you " +
			"need if your client asks for an endpoint and a header.",
	}}

	if in.LegacySSE && in.SSEURL != "" {
		snippets = append(snippets, Snippet{
			Label:    "Any MCP client (deprecated SSE)",
			Language: "text",
			Filename: "",
			Content: fmt.Sprintf(`Transport:  HTTP+SSE (protocol 2024-11-05, deprecated)
Endpoint:   %s
Headers:    Authorization: Bearer <your token>
`, in.SSEURL),
			Note:       "Only use this if your client predates Streamable HTTP.",
			Deprecated: true,
		})
	}

	snippets = append(snippets, Snippet{
		Label:    "Direct connection test",
		Language: "shell",
		Content: fmt.Sprintf(
			"curl -sS -X POST %s \\\n  -H \"Authorization: Bearer $WAKIL_TOKEN\" \\\n"+
				"  -H \"Content-Type: application/json\" \\\n"+
				"  -H \"Accept: application/json, text/event-stream\" \\\n"+
				"  -d '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}'",
			in.BaseURL),
		Note: "Confirms the endpoint, the token and reachability without involving a client.",
	})

	return snippets
}

// mustJSON renders an indented JSON document. The values here are all
// JSON-marshalable, so a failure would be a programming error rather than a
// runtime condition.
func mustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("{\"error\": %q}", err.Error())
	}
	return string(b)
}

// quote renders a value as a JSON string literal, for notes that embed one.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `"` + s + `"`
	}
	return string(b)
}
