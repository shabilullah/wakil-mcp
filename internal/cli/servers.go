package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/secrets"
	"github.com/shabilullah/wakil-mcp/internal/upstream"
)

// listCmd prints the configured servers.
func (e *env) listCmd(args []string) error {
	fs := e.flagSet("list", listUsage)
	if err := e.parse(fs, args); err != nil {
		return err
	}

	cfg, err := e.load()
	if err != nil {
		return err
	}

	type row struct {
		Key         string `json:"key"`
		Enabled     bool   `json:"enabled"`
		Type        string `json:"type"`
		Target      string `json:"target"`
		StartPolicy string `json:"startPolicy"`
		Description string `json:"description,omitempty"`
	}

	keys := sortedKeys(cfg.MCPServers)
	rows := make([]row, 0, len(keys))
	for _, key := range keys {
		up := cfg.MCPServers[key]
		if up == nil {
			continue
		}
		rows = append(rows, row{
			Key:         key,
			Enabled:     up.IsEnabled(),
			Type:        orDash(up.EffectiveType()),
			Target:      describeTarget(up),
			StartPolicy: up.EffectiveStartPolicy(cfg.Defaults),
			Description: up.Description,
		})
	}

	return e.emit(rows, func() {
		if len(rows) == 0 {
			e.printf("No servers configured in %s.\n\n", cfg.Path())
			e.printf("Add one:\n")
			e.printf("  wakil-mcp add github --command npx --arg -y --arg @modelcontextprotocol/server-github\n\n")
			e.printf("Or copy what you already have:\n")
			e.printf("  wakil-mcp import --from ~/.cursor/mcp.json\n")
			return
		}

		table := make([][]string, 0, len(rows))
		for _, r := range rows {
			table = append(table, []string{r.Key, yesNo(r.Enabled), r.Type, r.StartPolicy, r.Target})
		}
		renderTable(e.stdout, []string{"KEY", "ON", "TYPE", "START", "TARGET"}, table)

		e.printf("\n%s in %s\n", plural(len(rows), "server"), cfg.Path())
	})
}

// addCmd adds or replaces one server.
func (e *env) addCmd(args []string) error {
	fs := e.flagSet("add", addUsage)
	command := fs.String("command", "", "stdio: the program to run")
	endpoint := fs.String("url", "", "http or sse: the endpoint")
	transport := fs.String("type", "", "stdio, http, sse or auto")
	description := fs.String("description", "", "free text shown in the UI")
	cwd := fs.String("cwd", "", "stdio: working directory")
	startPolicy := fs.String("start-policy", "", "lazy, eager or never")
	disabled := fs.Bool("disabled", false, "add the server switched off")
	test := fs.Bool("test", false, "connect once before saving")

	var extraArgs stringList
	var envVars, headers keyValues
	fs.Var(&extraArgs, "arg", "stdio: an argument (repeatable)")
	fs.Var(&envVars, "env", "stdio: KEY=VALUE (repeatable)")
	fs.Var(&headers, "header", "http: KEY=VALUE (repeatable)")

	positional, err := e.parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	key, err := oneValue(positional, "server key")
	if err != nil {
		return fmt.Errorf("%w, for example \"wakil-mcp add github --command npx\"", err)
	}

	switch {
	case *command == "" && *endpoint == "":
		return fmt.Errorf("give either --command (for a local program) or --url (for a remote server)")
	case *command != "" && *endpoint != "":
		return fmt.Errorf("--command and --url are mutually exclusive; a server is one or the other")
	}

	cfg, err := e.load()
	if err != nil {
		return err
	}
	if err := checkKey(key); err != nil {
		return err
	}

	up := &config.Upstream{
		Type:        *transport,
		Command:     *command,
		Args:        extraArgs,
		Env:         envVars,
		Cwd:         *cwd,
		URL:         *endpoint,
		Headers:     headers,
		Description: *description,
	}
	if *startPolicy != "" {
		if err := checkStartPolicy(*startPolicy); err != nil {
			return err
		}
		up.StartPolicy = *startPolicy
	}
	if *disabled {
		up.Enabled = boolPointer(false)
	}
	if up.Type == "" {
		up.Type = up.EffectiveType()
	}
	if up.Type == "" {
		return fmt.Errorf("could not tell which transport to use; pass --type")
	}

	replacing := false
	if existing, ok := cfg.MCPServers[key]; ok {
		replacing = true
		// Keep what the operator set earlier unless the flags override it.
		if up.Description == "" {
			up.Description = existing.Description
		}
		if up.StartPolicy == "" {
			up.StartPolicy = existing.StartPolicy
		}
		if existing.Enabled != nil && !*disabled {
			up.Enabled = existing.Enabled
		}
		up.Tools = existing.Tools
		up.Namespace = existing.Namespace
	}

	if *test {
		if err := e.checkServer(cfg, key, up, 30*time.Second); err != nil {
			return fmt.Errorf("the server did not answer, so nothing was saved: %w", err)
		}
	}

	cfg.MCPServers[key] = up
	if err := cfg.Save(); err != nil {
		return err
	}

	if replacing {
		e.printf("Replaced %q.\n", key)
	} else {
		e.printf("Added %q.\n", key)
	}
	e.printf("  type      %s\n", up.Type)
	e.printf("  target    %s\n", describeTarget(up))
	e.printf("  enabled   %s\n", yesNo(up.IsEnabled()))
	e.printf("  config    %s\n\n", cfg.Path())
	e.printf("A running daemon picks this up within a couple of seconds.\n")
	return nil
}

// removeCmd deletes a server.
func (e *env) removeCmd(args []string) error {
	fs := e.flagSet("remove", removeUsage)

	positional, err := e.parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	key, err := oneValue(positional, "server key")
	if err != nil {
		return err
	}

	cfg, err := e.load()
	if err != nil {
		return err
	}
	if _, ok := cfg.MCPServers[key]; !ok {
		return fmt.Errorf("no server named %q in %s", key, cfg.Path())
	}

	delete(cfg.MCPServers, key)
	if err := cfg.Save(); err != nil {
		return err
	}
	e.printf("Removed %q. A running daemon drops its tools within a couple of seconds.\n", key)
	return nil
}

// enableCmd switches a server on.
func (e *env) enableCmd(args []string) error { return e.setEnabled(args, true) }

// disableCmd switches a server off without deleting it.
func (e *env) disableCmd(args []string) error { return e.setEnabled(args, false) }

// setEnabled writes the enabled flag for one server.
func (e *env) setEnabled(args []string, enabled bool) error {
	verb := "enable"
	if !enabled {
		verb = "disable"
	}

	fs := e.flagSet(verb, toggleUsage)

	positional, err := e.parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	key, err := oneValue(positional, "server key")
	if err != nil {
		return err
	}

	cfg, err := e.load()
	if err != nil {
		return err
	}
	up, ok := cfg.MCPServers[key]
	if !ok {
		return fmt.Errorf("no server named %q in %s", key, cfg.Path())
	}
	if up.IsEnabled() == enabled {
		e.printf("%q is already %sd.\n", key, verb)
		return nil
	}

	up.Enabled = boolPointer(enabled)
	if err := cfg.Save(); err != nil {
		return err
	}

	if enabled {
		e.printf("Enabled %q. A running daemon starts it within a couple of seconds.\n", key)
	} else {
		e.printf("Disabled %q. A running daemon drops its tools within a couple of seconds.\n", key)
	}
	return nil
}

// importCmd copies servers out of another tool's configuration.
func (e *env) importCmd(args []string) error {
	fs := e.flagSet("import", importUsage)
	from := fs.String("from", "", "the configuration file to read")
	prefix := fs.String("prefix", "", "prefix each imported key")
	dryRun := fs.Bool("dry-run", false, "report what would be added without saving")
	if err := e.parse(fs, args); err != nil {
		return err
	}
	if *from == "" {
		return fmt.Errorf("--from is required; give the configuration file to read")
	}

	raw, err := os.ReadFile(*from)
	if err != nil {
		return fmt.Errorf("reading %s: %w", *from, err)
	}

	imported, err := parseHarnessConfig(raw)
	if err != nil {
		return fmt.Errorf("reading %s: %w", *from, err)
	}
	if len(imported) == 0 {
		return fmt.Errorf("no servers found in %s; expected an \"mcpServers\" block", *from)
	}

	cfg, err := e.load()
	if err != nil {
		return err
	}

	keys := sortedKeys(imported)
	type result struct {
		Key    string `json:"key"`
		Action string `json:"action"`
		Type   string `json:"type"`
		Target string `json:"target"`
	}

	results := make([]result, 0, len(keys))
	for _, key := range keys {
		name := *prefix + key
		up := imported[key]

		action := "added"
		if _, exists := cfg.MCPServers[name]; exists {
			action = "replaced"
		}
		if err := checkKey(name); err != nil {
			results = append(results, result{Key: name, Action: "skipped: " + err.Error()})
			continue
		}
		if up.EffectiveType() == "" {
			results = append(results, result{Key: name, Action: "skipped: no command or url"})
			continue
		}

		if !*dryRun {
			cfg.MCPServers[name] = up
		}
		results = append(results, result{
			Key:    name,
			Action: action,
			Type:   up.EffectiveType(),
			Target: describeTarget(up),
		})
	}

	if !*dryRun {
		if err := cfg.Save(); err != nil {
			return err
		}
	}

	return e.emit(results, func() {
		table := make([][]string, 0, len(results))
		for _, r := range results {
			table = append(table, []string{r.Key, r.Action, orDash(r.Type), r.Target})
		}
		renderTable(e.stdout, []string{"KEY", "ACTION", "TYPE", "TARGET"}, table)
		if *dryRun {
			e.printf("\nDry run: nothing was written.\n")
			return
		}
		e.printf("\nImported from %s into %s\n", *from, cfg.Path())
	})
}

// testCmd connects to one server and reports what it offers.
func (e *env) testCmd(args []string) error {
	fs := e.flagSet("test", testUsage)
	timeout := fs.Duration("timeout", 30*time.Second, "connection timeout")

	positional, err := e.parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	key, err := oneValue(positional, "server key")
	if err != nil {
		return err
	}

	cfg, err := e.load()
	if err != nil {
		return err
	}
	up, ok := cfg.MCPServers[key]
	if !ok {
		return fmt.Errorf("no server named %q in %s; try \"wakil-mcp list\"", key, cfg.Path())
	}

	started := time.Now()
	info, err := e.connect(cfg, key, up, *timeout)
	if err != nil {
		return fmt.Errorf("connecting to %q: %w", key, err)
	}
	elapsed := time.Since(started)

	// Render the names the way the daemon will, so the report can be compared
	// directly against what a harness sees.
	index := previewIndex(cfg.Defaults, key, up, info.Tools)

	type toolView struct {
		Exposed  string `json:"exposed"`
		Upstream string `json:"upstream"`
		Title    string `json:"title,omitempty"`
	}
	tools := make([]toolView, 0, len(index.Entries()))
	for _, entry := range index.Entries() {
		tools = append(tools, toolView{
			Exposed:  entry.Exposed,
			Upstream: entry.UpstreamName,
			Title:    entry.Tool.Title,
		})
	}

	report := struct {
		Key             string     `json:"key"`
		Transport       string     `json:"transport"`
		Endpoint        string     `json:"endpoint,omitempty"`
		ProtocolVersion string     `json:"protocolVersion"`
		Legacy          bool       `json:"legacy,omitempty"`
		ServerName      string     `json:"serverName,omitempty"`
		ServerVersion   string     `json:"serverVersion,omitempty"`
		Instructions    string     `json:"instructions,omitempty"`
		ElapsedMS       int64      `json:"elapsedMs"`
		Tools           []toolView `json:"tools"`
	}{
		Key:             key,
		Transport:       info.Transport,
		Endpoint:        info.Endpoint,
		ProtocolVersion: info.ProtocolVersion,
		Legacy:          info.Legacy,
		Instructions:    info.Instructions,
		ElapsedMS:       elapsed.Milliseconds(),
		Tools:           tools,
	}
	if info.ServerInfo != nil {
		report.ServerName = info.ServerInfo.Name
		report.ServerVersion = info.ServerInfo.Version
	}

	return e.emit(report, func() {
		e.printf("%s is reachable.\n\n", key)
		e.printf("  transport   %s\n", report.Transport)
		if report.Endpoint != "" {
			e.printf("  endpoint    %s\n", report.Endpoint)
		}
		e.printf("  protocol    %s", report.ProtocolVersion)
		if report.Legacy {
			e.printf("  (older revision; the proxy still serves it)")
		}
		e.printf("\n")
		if report.ServerName != "" {
			e.printf("  server      %s %s\n", report.ServerName, report.ServerVersion)
		}
		e.printf("  handshake   %dms\n", report.ElapsedMS)
		e.printf("  tools       %d\n", len(report.Tools))
		if report.Instructions != "" {
			e.printf("\n  instructions:\n")
			for _, line := range wrap(report.Instructions, 76) {
				e.printf("    %s\n", line)
			}
		}

		if len(report.Tools) == 0 {
			return
		}

		e.printf("\n")
		table := make([][]string, 0, len(report.Tools))
		for _, tool := range report.Tools {
			table = append(table, []string{tool.Exposed, orDash(tool.Upstream), orDash(firstLine(tool.Title))})
		}
		renderTable(e.stdout, []string{"EXPOSED NAME", "UPSTREAM NAME", "TITLE"}, table)

		if collisions := index.Collisions(); len(collisions) > 0 {
			e.printf("\n%d names collided and were renamed.\n", len(collisions))
		}
	})
}

// connect opens a one-off connection to a server and closes it again.
func (e *env) connect(cfg *config.Config, key string, up *config.Upstream, timeout time.Duration) (*upstream.Info, error) {
	manager := upstream.NewManager(upstream.Options{
		Logger:   e.logger("warn"),
		Resolver: secrets.New(),
	})
	defer manager.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return manager.Test(ctx, upstream.Def{Key: key, Cfg: up, Defaults: cfg.Defaults})
}

// checkServer runs the connection test and reports a one-line summary, which is
// what "add --test" prints.
func (e *env) checkServer(cfg *config.Config, key string, up *config.Upstream, timeout time.Duration) error {
	info, err := e.connect(cfg, key, up, timeout)
	if err != nil {
		return err
	}
	e.printf("Connected to %q: %s, protocol %s, %s.\n",
		key, orDash(info.ServerInfo.Name), info.ProtocolVersion, plural(len(info.Tools), "tool"))
	return nil
}

// describeTarget renders a server's destination for display, with any secret
// material masked.
func describeTarget(up *config.Upstream) string {
	switch up.EffectiveType() {
	case config.TransportHTTP, config.TransportSSE:
		return secrets.Redact(up.URL)
	default:
		parts := make([]string, 0, len(up.Args)+1)
		parts = append(parts, up.Command)
		parts = append(parts, secrets.RedactSlice(up.Args)...)
		return strings.Join(parts, " ")
	}
}

// checkKey rejects a server key the aggregator cannot use.
func checkKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("a server key cannot be empty")
	}
	switch {
	case key == config.ReservedNamespace:
		return fmt.Errorf("%q is reserved for the proxy's own tools; pick another name", key)
	case strings.HasPrefix(key, config.ReservedNamespace+"."),
		strings.HasPrefix(key, config.ReservedNamespace+"_"),
		strings.HasPrefix(key, config.ReservedNamespace+"-"):
		return fmt.Errorf("a server key cannot start with %q, which is reserved", config.ReservedNamespace)
	}
	return nil
}

// checkStartPolicy rejects an unknown start policy.
func checkStartPolicy(policy string) error {
	switch policy {
	case config.StartLazy, config.StartEager, config.StartNever:
		return nil
	default:
		return fmt.Errorf("start policy %q is not one of %s, %s or %s",
			policy, config.StartLazy, config.StartEager, config.StartNever)
	}
}

// parseHarnessConfig reads another tool's MCP configuration.
//
// The shapes differ more than they look: Claude Desktop writes "mcpServers" with
// command and args, VS Code writes "servers" with a transport type, Windsurf
// calls the endpoint "serverUrl", and several write a bare map. Rather than
// model each one, this reads the fields that exist and ignores the rest, so a
// configuration from a tool newer than this build still imports.
func parseHarnessConfig(raw []byte) (map[string]*config.Upstream, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}

	// Unwrap the two container keys clients use, and otherwise treat the
	// document itself as the map.
	source := document
	for _, wrapper := range []string{"mcpServers", "servers", "mcp"} {
		nested, ok := document[wrapper]
		if !ok {
			continue
		}
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(nested, &inner); err == nil && len(inner) > 0 {
			source = inner
			break
		}
	}

	out := map[string]*config.Upstream{}
	for name, body := range source {
		var fields map[string]any
		if err := json.Unmarshal(body, &fields); err != nil {
			continue
		}
		if len(fields) == 0 {
			continue
		}

		up := &config.Upstream{}
		up.Type = stringField(fields, "type")
		up.Command = stringField(fields, "command")
		up.Args = stringSliceField(fields, "args")
		up.Env = stringMapField(fields, "env")
		up.Cwd = stringField(fields, "cwd")
		up.URL = firstNonEmpty(
			stringField(fields, "url"),
			stringField(fields, "serverUrl"),
			stringField(fields, "httpUrl"),
		)
		up.Headers = stringMapField(fields, "headers")
		up.Description = stringField(fields, "description")

		// A disabled entry stays disabled; anything else is left to the
		// daemon's default, which is enabled.
		if disabled, ok := fields["disabled"].(bool); ok && disabled {
			up.Enabled = boolPointer(false)
		}

		// "type": "remote" is opencode's spelling for a Streamable HTTP URL.
		switch up.Type {
		case "remote", "streamable-http", "streamablehttp":
			up.Type = config.TransportHTTP
		case "sse":
			up.Type = config.TransportSSE
		case "stdio", "local":
			up.Type = config.TransportStdio
		}

		// Some configurations nest the real definition, as opencode does with
		// "environment" for env vars.
		if up.Env == nil {
			up.Env = stringMapField(fields, "environment")
		}
		if up.Type == "" {
			up.Type = up.EffectiveType()
		}

		out[name] = up
	}

	return out, nil
}

// stringField reads a string field, tolerating absence and wrong types.
func stringField(fields map[string]any, key string) string {
	value, _ := fields[key].(string)
	return strings.TrimSpace(value)
}

// firstNonEmpty returns the first non-empty value.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// stringSliceField reads a list of strings.
func stringSliceField(fields map[string]any, key string) []string {
	raw, ok := fields[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// stringMapField reads a map of strings.
func stringMapField(fields map[string]any, key string) map[string]string {
	raw, ok := fields[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for name, value := range raw {
		if text, ok := value.(string); ok {
			out[name] = text
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// boolPointer returns a pointer to a bool, for the tri-state config fields.
func boolPointer(value bool) *bool { return &value }

// firstLine shortens text to its first line and a maximum length.
func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	if len(text) > 80 {
		return text[:77] + "..."
	}
	return text
}
