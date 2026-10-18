package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// syntheticSchema builds an object schema from a property list, so the
// definitions stay readable rather than being wall-to-wall escaped JSON.
func syntheticSchema(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// registerSyntheticTools adds the proxy's own tools to the server.
func (p *Proxy) registerSyntheticTools() {
	p.server.AddTool(&mcp.Tool{
		Name:  ToolListServer,
		Title: "List MCP servers",
		Description: "List every MCP server configured in this proxy, with its " +
			"connection state and how many tools it contributes.",
		InputSchema: syntheticSchema(map[string]any{}),
	}, p.handleListServers)

	p.server.AddTool(&mcp.Tool{
		Name:  ToolSearch,
		Title: "Search tools",
		Description: "Search the aggregated tool list by name or description. " +
			"Use this when you know what you want to do but not which tool does it.",
		InputSchema: syntheticSchema(map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Substring to match against tool names, descriptions and server names.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum number of matches to return. Defaults to 25.",
				"minimum":     1,
			},
		}, "query"),
	}, p.handleSearchTools)

	p.server.AddTool(&mcp.Tool{
		Name:  ToolGetTool,
		Title: "Describe a tool",
		Description: "Return the full definition of one tool, including its input " +
			"schema, without invoking it.",
		InputSchema: syntheticSchema(map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "The exposed tool name, for example \"github.create_issue\".",
			},
		}, "name"),
	}, p.handleDescribeTool)
}

// handleListServers reports configured servers and their state.
//
// Both the server list and the tool count are filtered to what the caller may
// reach. Without that, a harness holding a narrow token could read the names,
// error messages and stderr excerpts of servers it has no business seeing, and
// learn how many tools exist behind the ones it can use.
func (p *Proxy) handleListServers(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	profile := profileFromContext(ctx)
	ix := p.Index()

	type serverView struct {
		Name            string   `json:"name"`
		State           string   `json:"state"`
		Transport       string   `json:"transport,omitempty"`
		Enabled         bool     `json:"enabled"`
		ToolCount       int      `json:"toolCount"`
		ProtocolVersion string   `json:"protocolVersion,omitempty"`
		Legacy          bool     `json:"legacy,omitempty"`
		StartPolicy     string   `json:"startPolicy,omitempty"`
		LastError       string   `json:"lastError,omitempty"`
		SampleTools     []string `json:"sampleTools,omitempty"`
	}

	var (
		views     []serverView
		toolCount int
	)
	for _, s := range p.opts.Manager.Snapshots() {
		if !profile.AllowsServer(s.Key) {
			continue
		}

		v := serverView{
			Name:            s.Key,
			State:           string(s.State),
			Transport:       s.Transport,
			Enabled:         s.Enabled,
			ProtocolVersion: s.ProtocolVersion,
			Legacy:          s.Legacy,
			StartPolicy:     s.StartPolicy,
		}

		var samples []string
		for _, e := range ix.ForServer(s.Key) {
			if !profile.AllowsTool(e.ServerKey, e.Exposed, e.UpstreamName) {
				continue
			}
			v.ToolCount++
			if len(samples) < 5 {
				samples = append(samples, e.Exposed)
			}
		}
		v.SampleTools = samples

		// Error detail can carry credentials out of a connection string, so it
		// is only reported for a server the caller can actually use.
		if v.ToolCount == 0 {
			v.LastError = s.LastError
		}
		toolCount += v.ToolCount
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })

	return jsonResult(map[string]any{
		"servers":     views,
		"serverCount": len(views),
		"toolCount":   toolCount,
	})
}

// searchArgs is the input to _wakil.search_tools.
type searchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

// searchResult is one hit.
type searchResult struct {
	Name        string `json:"name"`
	Server      string `json:"server"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// handleSearchTools searches names and descriptions.
func (p *Proxy) handleSearchTools(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args searchArgs
	if err := decodeArgs(req, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Query) == "" {
		return nil, errBadRequest("query must not be empty")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 25
	}
	if limit > 200 {
		limit = 200
	}

	profile := profileFromContext(ctx)
	matches := p.Index().Find(args.Query)

	results := make([]searchResult, 0, min(limit, len(matches)))
	for _, e := range matches {
		if !profile.AllowsTool(e.ServerKey, e.Exposed, e.UpstreamName) {
			continue
		}
		if len(results) >= limit {
			break
		}
		results = append(results, searchResult{
			Name:        e.Exposed,
			Server:      e.ServerKey,
			Title:       e.Tool.Title,
			Description: firstLine(e.Tool.Description, 300),
		})
	}

	return jsonResult(map[string]any{
		"query":      args.Query,
		"matches":    results,
		"matchCount": len(results),
		"truncated":  len(results) >= limit,
	})
}

// describeArgs is the input to _wakil.describe_tool.
type describeArgs struct {
	Name string `json:"name"`
}

// handleDescribeTool returns one tool's full definition.
func (p *Proxy) handleDescribeTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args describeArgs
	if err := decodeArgs(req, &args); err != nil {
		return nil, err
	}
	if args.Name == "" {
		return nil, errBadRequest("name must not be empty")
	}

	e, ok := p.Index().Lookup(args.Name)
	if !ok {
		return nil, errUnknownTool(args.Name)
	}
	// Answer with the same denial a call would get, so the tool list, the
	// description and the call all agree about what the caller may reach.
	if !profileFromContext(ctx).AllowsTool(e.ServerKey, e.Exposed, e.UpstreamName) {
		return nil, errUnknownTool(args.Name)
	}

	out := map[string]any{
		"name":         e.Exposed,
		"server":       e.ServerKey,
		"upstreamName": e.UpstreamName,
		"description":  e.Tool.Description,
	}
	if e.Tool.Title != "" {
		out["title"] = e.Tool.Title
	}
	if len(e.Tool.InputSchema) > 0 {
		out["inputSchema"] = json.RawMessage(e.Tool.InputSchema)
	}
	if len(e.Tool.OutputSchema) > 0 {
		out["outputSchema"] = json.RawMessage(e.Tool.OutputSchema)
	}
	if e.Tool.Annotations != nil {
		out["annotations"] = e.Tool.Annotations
	}
	if e.Truncated || e.Renamed || e.Disambiguated {
		out["naming"] = map[string]any{
			"truncated":     e.Truncated,
			"renamed":       e.Renamed,
			"disambiguated": e.Disambiguated,
		}
	}
	return jsonResult(out)
}

// decodeArgs unmarshals a tool call's arguments.
func decodeArgs(req *mcp.CallToolRequest, dst any) error {
	if req == nil || req.Params == nil || len(req.Params.Arguments) == 0 {
		return nil
	}
	if err := json.Unmarshal(req.Params.Arguments, dst); err != nil {
		return errBadRequest("invalid arguments: %v", err)
	}
	return nil
}

// jsonResult renders a structured result, including the matching text block so
// harnesses that only read content still show something useful.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: rendering result: %v", errInternal, err)
	}
	res := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}
	var structured any
	if err := json.Unmarshal(b, &structured); err == nil {
		res.StructuredContent = structured
	}
	return res, nil
}

// firstLine collapses a description to a single bounded line.
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
