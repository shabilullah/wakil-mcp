package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/shabilullah/wakil-mcp/internal/config"
)

// tokenCmd dispatches the credential subcommands.
func (e *env) tokenCmd(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(e.stdout, tokenUsage)
		return errHelp
	}

	switch args[0] {
	case "create", "new":
		return e.tokenCreate(args[1:])
	case "list", "ls":
		return e.tokenList(args[1:])
	case "remove", "delete", "rm":
		return e.tokenRemove(args[1:])
	case "admin":
		return e.tokenAdmin(args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(e.stdout, tokenUsage)
		return errHelp
	default:
		return fmt.Errorf("unknown subcommand %q; expected create, list, remove or admin", args[0])
	}
}

// tokenCreate mints a credential for one harness.
func (e *env) tokenCreate(args []string) error {
	fs := e.flagSet("token create", tokenUsage)
	var allowServers, denyServers, allowTools, denyTools stringList
	fs.Var(&allowServers, "allow-server", "servers this token may reach (repeatable)")
	fs.Var(&denyServers, "deny-server", "servers to exclude (repeatable)")
	fs.Var(&allowTools, "allow-tool", "SERVER:PATTERN the token may call (repeatable)")
	fs.Var(&denyTools, "deny-tool", "SERVER:PATTERN to exclude (repeatable)")

	positional, err := e.parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	name, err := oneValue(positional, "name for the new client")
	if err != nil {
		return fmt.Errorf("%w, for example \"wakil-mcp token create laptop\"", err)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("the client name cannot be empty")
	}

	cfg, err := e.load()
	if err != nil {
		return err
	}
	for _, client := range cfg.Clients {
		if client.Name == name {
			return fmt.Errorf("a client named %q already exists; remove it first with \"wakil-mcp token remove %s\"", name, name)
		}
	}

	allow, deny, err := buildRule(allowServers, denyServers, allowTools, denyTools)
	if err != nil {
		return err
	}
	if err := checkRuleAgainstServers(cfg, allow, deny); err != nil {
		return err
	}

	token, hash, err := config.GenerateToken()
	if err != nil {
		return err
	}

	cfg.Clients = append(cfg.Clients, config.ClientProfile{
		Name:      name,
		TokenHash: hash,
		Allow:     allow,
		Deny:      deny,
	})
	if err := cfg.Save(); err != nil {
		return err
	}

	report := struct {
		Name       string           `json:"name"`
		Token      string           `json:"token"`
		Allow      config.AllowRule `json:"allow,omitempty"`
		Deny       config.AllowRule `json:"deny,omitempty"`
		ConfigPath string           `json:"configPath"`
	}{Name: name, Token: token, Allow: allow, Deny: deny, ConfigPath: cfg.Path()}

	return e.emit(report, func() {
		e.printf("Created client %q.\n\n", name)
		e.printf("  %s\n\n", token)
		e.printf("This is the only time it is shown: only a hash is stored.\n\n")
		e.printf("Send it as a bearer token:\n")
		e.printf("  Authorization: Bearer <token>\n\n")
		e.printf("Scope: %s\n", describeScope(allow, deny))
		e.printf("Config: %s\n", cfg.Path())
	})
}

// tokenList shows the configured client profiles.
func (e *env) tokenList(args []string) error {
	fs := e.flagSet("token list", tokenUsage)
	if err := e.parse(fs, args); err != nil {
		return err
	}

	cfg, err := e.load()
	if err != nil {
		return err
	}

	type profile struct {
		Name         string              `json:"name"`
		Usable       bool                `json:"usable"`
		Disabled     bool                `json:"disabled,omitempty"`
		AllowServers []string            `json:"allowServers,omitempty"`
		DenyServers  []string            `json:"denyServers,omitempty"`
		AllowTools   map[string][]string `json:"allowTools,omitempty"`
		DenyTools    map[string][]string `json:"denyTools,omitempty"`
	}

	profiles := make([]profile, 0, len(cfg.Clients))
	for _, client := range cfg.Clients {
		profiles = append(profiles, profile{
			Name:         client.Name,
			Usable:       !client.Disabled && client.TokenHash != "",
			Disabled:     client.Disabled,
			AllowServers: client.Allow.Servers,
			DenyServers:  client.Deny.Servers,
			AllowTools:   client.Allow.Tools,
			DenyTools:    client.Deny.Tools,
		})
	}

	return e.emit(profiles, func() {
		adminConfigured := cfg.Server.AdminTokenHash != ""

		if len(profiles) == 0 {
			e.printf("No client profiles in %s.\n\n", cfg.Path())
			e.printf("Create one:\n  wakil-mcp token create laptop\n")
		} else {
			table := make([][]string, 0, len(profiles))
			for _, p := range profiles {
				table = append(table, []string{
					p.Name,
					yesNo(p.Usable),
					describeServerScope(p.AllowServers, p.DenyServers),
					describeToolScope(p.AllowTools, p.DenyTools),
				})
			}
			renderTable(e.stdout, []string{"NAME", "ACTIVE", "SERVERS", "TOOLS"}, table)
			e.printf("\n%s in %s\n", plural(len(profiles), "client"), cfg.Path())
		}

		if cfg.Server.DisableAuth {
			e.printf("\nAuthentication is off, so these profiles are not being checked.\n")
		}
		if adminConfigured {
			e.printf("An admin token is configured for the web UI and REST API.\n")
		} else if !cfg.Server.DisableAuth {
			e.printf("\nNo admin token is configured, so the web UI and REST API are closed.\n")
			e.printf("Create one with \"wakil-mcp token admin\".\n")
		}
	})
}

// tokenRemove deletes a client profile.
func (e *env) tokenRemove(args []string) error {
	fs := e.flagSet("token remove", tokenUsage)

	positional, err := e.parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	name, err := oneValue(positional, "client name")
	if err != nil {
		return err
	}

	cfg, err := e.load()
	if err != nil {
		return err
	}

	kept := make([]config.ClientProfile, 0, len(cfg.Clients))
	found := false
	for _, client := range cfg.Clients {
		if client.Name == name {
			found = true
			continue
		}
		kept = append(kept, client)
	}
	if !found {
		return fmt.Errorf("no client named %q in %s", name, cfg.Path())
	}
	if len(kept) == 0 && !cfg.Server.DisableAuth {
		return fmt.Errorf("%q is the last client, and authentication is on; "+
			"create a replacement first, or set server.disableAuth to true", name)
	}

	cfg.Clients = kept
	if err := cfg.Save(); err != nil {
		return err
	}
	e.printf("Removed %q. Its token stopped working.\n", name)
	return nil
}

// tokenAdmin mints a new admin credential.
func (e *env) tokenAdmin(args []string) error {
	fs := e.flagSet("token admin", tokenUsage)
	if err := e.parse(fs, args); err != nil {
		return err
	}

	cfg, err := e.load()
	if err != nil {
		return err
	}

	token, hash, err := config.GenerateToken()
	if err != nil {
		return err
	}

	previous := cfg.Server.AdminTokenHash != ""
	cfg.Server.AdminTokenHash = hash
	if err := cfg.Save(); err != nil {
		return err
	}

	report := struct {
		Token      string `json:"token"`
		Replaced   bool   `json:"replaced"`
		ConfigPath string `json:"configPath"`
	}{Token: token, Replaced: previous, ConfigPath: cfg.Path()}

	return e.emit(report, func() {
		if previous {
			e.printf("Replaced the admin token. The previous one stopped working.\n\n")
		} else {
			e.printf("Created an admin token.\n\n")
		}
		e.printf("  %s\n\n", token)
		e.printf("This is the only time it is shown: only a hash is stored.\n")
		e.printf("Use it to sign in to the web UI, or as a bearer token for %s/api.\n", "http://"+cfg.Server.Bind)
	})
}

// buildRule turns the scope flags into allow and deny rules.
func buildRule(allowServers, denyServers, allowTools, denyTools []string) (config.AllowRule, config.AllowRule, error) {
	collect := func(entries []string) (map[string][]string, error) {
		if len(entries) == 0 {
			return nil, nil
		}
		out := map[string][]string{}
		for _, entry := range entries {
			server, pattern, err := splitPair(entry)
			if err != nil {
				return nil, err
			}
			out[server] = append(out[server], pattern)
		}
		return out, nil
	}

	allowToolsMap, err := collect(allowTools)
	if err != nil {
		return config.AllowRule{}, config.AllowRule{}, err
	}
	denyToolsMap, err := collect(denyTools)
	if err != nil {
		return config.AllowRule{}, config.AllowRule{}, err
	}

	return config.AllowRule{Servers: allowServers, Tools: allowToolsMap},
		config.AllowRule{Servers: denyServers, Tools: denyToolsMap},
		nil
}

// checkRuleAgainstServers catches a typo in a server name, which would
// otherwise produce a token that quietly reaches nothing.
func checkRuleAgainstServers(cfg *config.Config, rules ...config.AllowRule) error {
	for _, rule := range rules {
		names := append([]string(nil), rule.Servers...)
		for server := range rule.Tools {
			names = append(names, server)
		}

		for _, name := range names {
			if strings.ContainsAny(name, "*?[") {
				continue // a pattern, nothing to check against
			}
			if _, ok := cfg.MCPServers[name]; !ok {
				known := sortedKeys(cfg.MCPServers)
				if len(known) == 0 {
					return fmt.Errorf("no servers are configured, so %q cannot be scoped", name)
				}
				return fmt.Errorf("no server named %q; configured servers: %s",
					name, strings.Join(known, ", "))
			}
		}
	}
	return nil
}

// describeScope renders an allow and deny pair as one sentence.
func describeScope(allow, deny config.AllowRule) string {
	parts := []string{describeServerScope(allow.Servers, deny.Servers)}
	if len(allow.Tools) > 0 || len(deny.Tools) > 0 {
		parts = append(parts, "tools "+describeToolScope(allow.Tools, deny.Tools))
	}
	return strings.Join(parts, ", ")
}

// describeServerScope renders which servers a rule permits.
func describeServerScope(allow, deny []string) string {
	switch {
	case len(allow) == 0 && len(deny) == 0:
		return "all servers"
	case len(allow) == 0:
		return "all servers except " + strings.Join(deny, ", ")
	case len(deny) == 0:
		return strings.Join(allow, ", ")
	default:
		return strings.Join(allow, ", ") + " except " + strings.Join(deny, ", ")
	}
}

// describeToolScope renders which tools a rule permits.
func describeToolScope(allow, deny map[string][]string) string {
	switch {
	case len(allow) == 0 && len(deny) == 0:
		return "all tools"
	case len(allow) == 0:
		return "all except " + describeToolMap(deny)
	case len(deny) == 0:
		return describeToolMap(allow)
	default:
		return describeToolMap(allow) + " except " + describeToolMap(deny)
	}
}

// describeToolMap renders a server-to-patterns map compactly.
func describeToolMap(m map[string][]string) string {
	if len(m) == 0 {
		return "none"
	}
	servers := sortedKeys(m)
	parts := make([]string, 0, len(servers))
	for _, server := range servers {
		patterns := append([]string(nil), m[server]...)
		parts = append(parts, server+":"+strings.Join(patterns, "|"))
	}
	return strings.Join(parts, " ")
}
