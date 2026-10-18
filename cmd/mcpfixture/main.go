// Command mcpfixture is a configurable MCP server used by the test suite.
//
// It is deliberately not part of the shipped binary. Its purpose is to speak
// both protocol eras over both transports, and to misbehave on demand, so the
// proxy can be tested against something that is not the same SDK it is built
// on.
//
// Behaviour is selected with environment variables:
//
//	WAKIL_FIXTURE_MODE        modern (default) | legacy | chaos
//	WAKIL_FIXTURE_ADDR        listen address; when set, the server speaks
//	                          Streamable HTTP instead of stdio
//	WAKIL_FIXTURE_NAME        serverInfo.name
//	WAKIL_FIXTURE_VERSION     serverInfo.version
//	WAKIL_FIXTURE_SCHEMA      rich (default) | missing | null | empty
//	WAKIL_FIXTURE_LONGNAME    when set, add a tool whose name exceeds 128 chars
//	WAKIL_FIXTURE_MRTR        when set, add a tool that requests user input
//	WAKIL_FIXTURE_STDERR      text written to stderr at startup
//	WAKIL_FIXTURE_SLEEP_MS    how long the "slow" tool sleeps
//	WAKIL_FIXTURE_CHANGED_MS  when set, add a tool and notify after this delay
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	// The fixture logs to stderr only, because stdout is the protocol channel
	// on stdio transports.
	log.SetOutput(os.Stderr)
	log.SetFlags(0)

	if msg := os.Getenv("WAKIL_FIXTURE_STDERR"); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}

	mode := env("WAKIL_FIXTURE_MODE", "modern")

	if mode == "legacy" {
		// The legacy fixture is hand written rather than built on the SDK, so
		// the proxy is tested against an independent implementation.
		runLegacy()
		return
	}

	server := buildSDKServer(mode)

	if addr := os.Getenv("WAKIL_FIXTURE_ADDR"); addr != "" {
		runHTTP(server, addr)
		return
	}

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("fixture: %v", err)
	}
}

// env returns an environment variable or a fallback.
func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// envInt returns an integer environment variable or a fallback.
func envInt(name string, fallback int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// ---------------------------------------------------------------------------
// Shared fixture definitions
// ---------------------------------------------------------------------------

// toolSpec describes one fixture tool in a way both the SDK server and the
// hand-written legacy server can serve.
type toolSpec struct {
	Name        string
	Description string
	// Schema is the exact bytes sent as inputSchema. A nil value means the
	// field is omitted entirely, which is what a sloppy server does and what
	// the proxy has to repair.
	Schema json.RawMessage
}

// richSchema exercises schema features a proxy must not lose: nested objects,
// arrays, $defs, anyOf, defaults, formats and an extra unknown keyword.
const richSchema = `{
  "type": "object",
  "title": "Echo input",
  "properties": {
    "text": {
      "type": "string",
      "description": "Text to echo back",
      "default": "hello",
      "examples": ["a", "b"],
      "x-vendor-extension": {"keep": true}
    },
    "options": {
      "type": "object",
      "properties": {
        "repeat": {"type": "integer", "minimum": 1, "maximum": 10},
        "transform": {"anyOf": [{"type": "string"}, {"type": "null"}]}
      },
      "additionalProperties": false
    },
    "tags": {
      "type": "array",
      "items": {"$ref": "#/$defs/tag"}
    }
  },
  "required": ["text"],
  "additionalProperties": false,
  "$defs": {
    "tag": {
      "type": "object",
      "properties": {"name": {"type": "string", "format": "uuid"}},
      "required": ["name"]
    }
  }
}`

// buildToolSpecs produces the fixture's tool list. The schema mode lets tests
// drive the proxy's schema repair path.
func buildToolSpecs(mode string) []toolSpec {
	schemaMode := env("WAKIL_FIXTURE_SCHEMA", "rich")

	var schema json.RawMessage
	switch schemaMode {
	case "missing":
		schema = nil
	case "null":
		schema = json.RawMessage(`null`)
	case "empty":
		schema = json.RawMessage(`{}`)
	default:
		schema = json.RawMessage(richSchema)
	}

	specs := []toolSpec{
		{
			Name:        "echo",
			Description: "Echo the provided text back, with a fixture prefix.",
			Schema:      schema,
		},
		{
			Name:        "add",
			Description: "Add two numbers.",
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {"a": {"type": "number"}, "b": {"type": "number"}},
			  "required": ["a", "b"]
			}`),
		},
		{
			Name:        "error",
			Description: "Always reports a tool-level error.",
			Schema:      json.RawMessage(`{"type":"object"}`),
		},
	}

	if mode == "chaos" {
		specs = append(specs,
			toolSpec{
				Name:        "big",
				Description: "Returns a large payload, to exercise size handling.",
				Schema:      json.RawMessage(`{"type":"object"}`),
			},
			toolSpec{
				Name:        "crash",
				Description: "Terminates the fixture process abruptly.",
				Schema:      json.RawMessage(`{"type":"object"}`),
			},
		)
	}

	if os.Getenv("WAKIL_FIXTURE_LONGNAME") != "" {
		specs = append(specs, toolSpec{
			Name:        "x" + strings.Repeat("verylongsegment", 12) + "end",
			Description: "A tool whose name far exceeds the 128 character limit.",
			Schema:      json.RawMessage(`{"type":"object"}`),
		})
	}

	if os.Getenv("WAKIL_FIXTURE_MRTR") != "" {
		specs = append(specs, toolSpec{
			Name:        "confirm",
			Description: "Asks the user to confirm before proceeding.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"}}}`),
		})
	}

	return specs
}

// callFixtureTool executes a fixture tool by name, returning the text result and
// whether the result represents a tool-level error.
func callFixtureTool(ctx context.Context, name string, args json.RawMessage, hasResponses bool, requestState string) (text string, isError bool) {
	switch name {
	case "echo":
		var in struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(args, &in)
		return "echo: " + in.Text, false

	case "add":
		var in struct {
			A float64 `json:"a"`
			B float64 `json:"b"`
		}
		_ = json.Unmarshal(args, &in)
		return strconv.FormatFloat(in.A+in.B, 'f', -1, 64), false

	case "error":
		return "deliberate failure from the fixture", true

	case "big":
		return strings.Repeat("payload ", 20000), false

	case "crash":
		fmt.Fprintln(os.Stderr, "fixture: crashing on request")
		os.Exit(3)
		return "", false

	case "confirm":
		if !hasResponses {
			return "", false
		}
		return "confirmed with state " + requestState, false

	default:
		if strings.HasPrefix(name, "xverylongsegment") {
			return "long name ok", false
		}
		return "unknown fixture tool " + name, true
	}
}

// ---------------------------------------------------------------------------
// SDK-backed server (modern and chaos modes)
// ---------------------------------------------------------------------------

// buildSDKServer assembles the SDK-based fixture.
func buildSDKServer(mode string) *mcp.Server {
	name := env("WAKIL_FIXTURE_NAME", "fixture")
	ver := env("WAKIL_FIXTURE_VERSION", "1.0.0")

	opts := &mcp.ServerOptions{
		Instructions: "Fixture server for wakil-mcp tests.",
		Capabilities: &mcp.ServerCapabilities{
			Tools: &mcp.ToolCapabilities{ListChanged: true},
		},
	}

	if mode == "chaos" {
		// Chaos reports a server name that would be unusable as a tool prefix.
		// The proxy must use the operator's config key instead, so this proves
		// the namespacing source is right.
		name = env("WAKIL_FIXTURE_NAME", "spaces and /slashes/")
	} else {
		// Pin the revision so the proxy's negotiation has a definite answer.
		opts.SupportedProtocolVersions = []string{"2026-07-28"}
	}

	server := mcp.NewServer(&mcp.Implementation{Name: name, Version: ver}, opts)

	for _, spec := range buildToolSpecs(mode) {
		spec := spec
		tool := &mcp.Tool{
			Name:        spec.Name,
			Description: spec.Description,
		}
		if spec.Schema != nil {
			tool.InputSchema = spec.Schema
		} else {
			// The SDK refuses a nil schema, so a schema-less tool sends the
			// empty object schema; the "missing" mode is exercised by the
			// legacy fixture instead, which is not bound by that check.
			tool.InputSchema = json.RawMessage(`{"type":"object"}`)
		}

		server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if spec.Name == "confirm" && len(req.Params.InputResponses) == 0 {
				// First round: ask the client for input and hand back opaque
				// state that the client must echo on the retry.
				return &mcp.CallToolResult{
					InputRequests: mcp.InputRequestMap{
						"confirm-1": &mcp.ElicitParams{
							Mode:    "form",
							Message: "Proceed with the action?",
							RequestedSchema: map[string]any{
								"type": "object",
								"properties": map[string]any{
									"approve": map[string]any{"type": "boolean"},
								},
								"required": []string{"approve"},
							},
						},
					},
					RequestState: "fixture-state-abc123",
				}, nil
			}

			if ms := envInt("WAKIL_FIXTURE_SLEEP_MS", 0); ms > 0 && spec.Name == "echo" {
				select {
				case <-time.After(time.Duration(ms) * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}

			text, isError := callFixtureTool(ctx, spec.Name, req.Params.Arguments,
				len(req.Params.InputResponses) > 0, req.Params.RequestState)

			res := &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: text}},
				IsError: isError,
			}
			if !isError && spec.Name != "confirm" {
				res.StructuredContent = map[string]any{"result": text}
			}
			return res, nil
		})
	}

	if delay := envInt("WAKIL_FIXTURE_CHANGED_MS", 0); delay > 0 {
		go func() {
			time.Sleep(time.Duration(delay) * time.Millisecond)
			server.AddTool(&mcp.Tool{
				Name:        "arrived_later",
				Description: "Added after startup, announced by a list-changed notification.",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "late arrival"}},
				}, nil
			})
			log.Println("fixture: added arrived_later")
		}()
	}

	return server
}

// runHTTP serves the fixture over Streamable HTTP.
func runHTTP(server *mcp.Server, addr string) {
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			// The fixture pins the newest revision, which is only valid on a
			// session-less server.
			Stateless: true,
		},
	)
	log.Printf("fixture: listening on %s", addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("fixture: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Hand-written legacy server
// ---------------------------------------------------------------------------

// legacyProtocolVersion is the revision the legacy fixture speaks. It predates
// the removal of the initialize handshake.
const legacyProtocolVersion = "2025-06-18"

// rpcRequest is the subset of a JSON-RPC request the fixture needs.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is the reply shape. A nil ID means the message is a
// notification and carries no id field.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// runLegacy speaks newline-delimited JSON-RPC over stdio, implementing only
// what a minimal legacy MCP server must: initialize, tools/list and tools/call.
func runLegacy() {
	specs := buildToolSpecs("legacy")

	out := bufio.NewWriter(os.Stdout)
	var mu sync.Mutex
	send := func(v any) {
		mu.Lock()
		defer mu.Unlock()
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}

	// A legacy server announces list changes on the shared stream.
	if delay := envInt("WAKIL_FIXTURE_CHANGED_MS", 0); delay > 0 {
		go func() {
			time.Sleep(time.Duration(delay) * time.Millisecond)
			mu.Lock()
			specs = append(specs, toolSpec{
				Name:        "arrived_later",
				Description: "Added after startup.",
				Schema:      json.RawMessage(`{"type":"object"}`),
			})
			mu.Unlock()
			send(rpcResponse{
				JSONRPC: "2.0",
				Method:  "notifications/tools/list_changed",
			})
		}()
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}

		// Notifications carry no id and expect no reply.
		expectsReply := len(req.ID) > 0 && string(req.ID) != "null"

		switch req.Method {
		case "initialize":
			if expectsReply {
				send(rpcResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Result: map[string]any{
						"protocolVersion": legacyProtocolVersion,
						"capabilities": map[string]any{
							"tools": map[string]any{"listChanged": true},
						},
						"serverInfo": map[string]any{
							"name":    env("WAKIL_FIXTURE_NAME", "legacy-fixture"),
							"version": env("WAKIL_FIXTURE_VERSION", "0.9.0"),
						},
						"instructions": "Legacy fixture server.",
					},
				})
			}

		case "ping":
			if expectsReply {
				send(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})
			}

		case "tools/list":
			mu.Lock()
			tools := make([]map[string]any, 0, len(specs))
			for _, s := range specs {
				entry := map[string]any{
					"name":        s.Name,
					"description": s.Description,
				}
				// In "missing" schema mode the field is omitted entirely, which
				// is the case the proxy has to repair.
				if s.Schema != nil {
					entry["inputSchema"] = s.Schema
				}
				tools = append(tools, entry)
			}
			mu.Unlock()
			if expectsReply {
				send(rpcResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Result:  map[string]any{"tools": tools},
				})
			}

		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			text, isError := callFixtureTool(context.Background(), params.Name, params.Arguments, false, "")
			if expectsReply {
				send(rpcResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Result: map[string]any{
						"content": []map[string]any{{"type": "text", "text": text}},
						"isError": isError,
					},
				})
			}

		default:
			if expectsReply {
				send(rpcResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Error: &rpcError{
						Code:    -32601,
						Message: "method not found: " + req.Method,
					},
				})
			}
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("fixture: stdin: %v", err)
	}
}
