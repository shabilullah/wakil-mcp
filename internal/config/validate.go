package config

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
)

// serverKeyPattern constrains config keys for upstream servers. Dots are
// excluded because the key doubles as a tool name prefix, and a dot there
// makes the exposed name ambiguous to read even though lookups are exact.
var serverKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,48}$`)

// separatorPattern constrains namespace separators to characters that are
// legal in a tool name, so an operator cannot configure a separator that
// produces an invalid exposed name.
var separatorPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,8}$`)

// clientNamePattern constrains client profile names.
var clientNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// ReservedNamespace is the tool-name prefix the proxy reserves for itself, so
// no upstream server may claim it.
const ReservedNamespace = "_wakil"

// ValidationError collects every problem found in a config.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0]
	}
	return fmt.Sprintf("%d problems:\n  - %s", len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

// Add records a problem.
func (e *ValidationError) Add(format string, args ...any) {
	e.Problems = append(e.Problems, fmt.Sprintf(format, args...))
}

// Validate checks the config for structural problems. It returns a
// *ValidationError listing everything wrong rather than stopping at the first
// issue, so a bad edit can be fixed in one pass.
func (c *Config) Validate() error {
	errs := &ValidationError{}

	if c.Version != CurrentVersion {
		errs.Add("version: expected %d, got %d", CurrentVersion, c.Version)
	}

	if c.Server.Bind != "" {
		if _, _, err := net.SplitHostPort(c.Server.Bind); err != nil {
			errs.Add("server.bind: %q is not a valid host:port", c.Server.Bind)
		}
	}
	switch strings.ToLower(c.Server.LogLevel) {
	case "debug", "info", "warn", "error":
	case "":
	default:
		errs.Add("server.logLevel: %q is not one of debug, info, warn, error", c.Server.LogLevel)
	}
	if c.Server.PublicURL != "" && !strings.HasPrefix(c.Server.PublicURL, "http") {
		errs.Add("server.publicUrl: %q must start with http:// or https://", c.Server.PublicURL)
	}
	if c.Server.AuditRetentionDays < 0 {
		errs.Add("server.auditRetentionDays: must not be negative")
	}

	validateDefaults(c, errs)

	// Upstream servers.
	keys := make([]string, 0, len(c.MCPServers))
	for k := range c.MCPServers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		validateUpstream(key, c.MCPServers[key], c.Defaults, errs)
	}

	// Client profiles.
	seenClients := map[string]string{}
	seenTokens := map[string]string{}
	for i, cl := range c.Clients {
		where := fmt.Sprintf("clients[%d]", i)
		if cl.Name == "" {
			errs.Add("%s.name: must not be empty", where)
		} else {
			if !clientNamePattern.MatchString(cl.Name) {
				errs.Add("%s.name: %q must match %s", where, cl.Name, clientNamePattern)
			}
			if prev, dup := seenClients[cl.Name]; dup {
				errs.Add("%s.name: %q duplicates %s", where, cl.Name, prev)
			} else {
				seenClients[cl.Name] = where
			}
		}
		if cl.TokenHash != "" {
			if !validTokenHash(cl.TokenHash) {
				errs.Add("%s.tokenHash: must be \"sha256:\" followed by 64 lowercase hex characters", where)
			} else if prev, dup := seenTokens[cl.TokenHash]; dup {
				errs.Add("%s.tokenHash: duplicates %s", where, prev)
			} else {
				seenTokens[cl.TokenHash] = where
			}
		}
		if !c.Server.DisableAuth && cl.TokenHash == "" {
			errs.Add("%s.tokenHash: required unless server.disableAuth is true", where)
		}
		validateAllowRule(where+".allow", cl.Allow, c, errs)
		validateAllowRule(where+".deny", cl.Deny, c, errs)
	}

	// Using auth without any client configured locks everyone out.
	if !c.Server.DisableAuth && len(c.Clients) == 0 {
		errs.Add("clients: no client profiles defined; add one or set server.disableAuth to true")
	}
	if c.Server.AdminTokenHash != "" && !validTokenHash(c.Server.AdminTokenHash) {
		errs.Add("server.adminTokenHash: must be \"sha256:\" followed by 64 hex characters")
	}
	if !c.Server.DisableAuth && c.Server.AdminTokenHash == "" {
		errs.Add("server.adminTokenHash: required unless server.disableAuth is true")
	}

	if len(errs.Problems) > 0 {
		return errs
	}
	return nil
}

// validTokenHash reports whether h has the expected storage-hash shape.
func validTokenHash(h string) bool {
	if !strings.HasPrefix(h, TokenHashPrefix) {
		return false
	}
	hex := h[len(TokenHashPrefix):]
	if len(hex) != 64 {
		return false
	}
	for _, r := range hex {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

func validateDefaults(c *Config, errs *ValidationError) {
	switch c.Defaults.StartPolicy {
	case StartLazy, StartEager, StartNever, "":
	default:
		errs.Add("defaults.startPolicy: %q is not one of lazy, eager, never", c.Defaults.StartPolicy)
	}
	if c.Defaults.IdleTimeout < 0 {
		errs.Add("defaults.idleTimeout: must not be negative")
	}
	if c.Defaults.CallTimeout < 0 {
		errs.Add("defaults.callTimeout: must not be negative")
	}
	if c.Defaults.ConnectTimeout < 0 {
		errs.Add("defaults.connectTimeout: must not be negative")
	}
	if c.Defaults.MaxToolsPerServer < 0 {
		errs.Add("defaults.maxToolsPerServer: must not be negative")
	}
	if c.Defaults.Separator != "" && !separatorPattern.MatchString(c.Defaults.Separator) {
		errs.Add("defaults.separator: %q must match %s", c.Defaults.Separator, separatorPattern)
	}
	if c.Defaults.Namespace != nil {
		validateNamespace("defaults.namespace", c.Defaults.Namespace, errs)
	}
	b := c.Defaults.RestartBackoff
	if b.Initial < 0 || b.Max < 0 {
		errs.Add("defaults.restartBackoff: durations must not be negative")
	}
	if b.Factor != 0 && b.Factor <= 1 {
		errs.Add("defaults.restartBackoff.factor: must be greater than 1")
	}
}

func validateUpstream(key string, u *Upstream, def ServerDefaults, errs *ValidationError) {
	where := fmt.Sprintf("mcpServers.%s", key)
	if u == nil {
		errs.Add("%s: must not be null", where)
		return
	}
	if !serverKeyPattern.MatchString(key) {
		errs.Add("%s: key must match %s", where, serverKeyPattern)
	}
	// The proxy owns this namespace for its own tools, so a server may not
	// claim a prefix that would collide with them.
	if key == ReservedNamespace || strings.HasPrefix(key, ReservedNamespace+"_") || strings.HasPrefix(key, ReservedNamespace+"-") {
		errs.Add("%s: key must not begin with %q, which is reserved for the proxy's own tools", where, ReservedNamespace)
	}

	switch u.EffectiveType() {
	case TransportStdio:
		if u.Command == "" {
			errs.Add("%s.command: required for a stdio server", where)
		}
		if u.URL != "" {
			errs.Add("%s.url: not valid for a stdio server", where)
		}
		if len(u.Headers) > 0 {
			errs.Add("%s.headers: not valid for a stdio server", where)
		}
	case TransportHTTP, TransportSSE:
		if u.URL == "" {
			errs.Add("%s.url: required for an http or sse server", where)
		} else if !strings.HasPrefix(u.URL, "http://") && !strings.HasPrefix(u.URL, "https://") {
			errs.Add("%s.url: %q must start with http:// or https://", where, u.URL)
		}
		if u.Command != "" {
			errs.Add("%s.command: not valid for an http or sse server", where)
		}
	default:
		errs.Add("%s: set either command (stdio) or url (http), or set type explicitly", where)
	}

	if u.Type != "" {
		switch u.Type {
		case TransportStdio, TransportHTTP, TransportSSE, TransportAuto:
		default:
			errs.Add("%s.type: %q is not one of stdio, http, sse, auto", where, u.Type)
		}
	}

	switch u.StartPolicy {
	case StartLazy, StartEager, StartNever, "":
	default:
		errs.Add("%s.startPolicy: %q is not one of lazy, eager, never", where, u.StartPolicy)
	}

	for name, v := range u.Env {
		if name == "" {
			errs.Add("%s.env: contains an empty variable name", where)
		}
		_ = v
	}

	// A namespace prefix must itself be a legal tool-name fragment.
	if u.Namespace != nil {
		validateNamespace(where+".namespace", u.Namespace, errs)
	}
	if _, _, enabled := u.NamespaceFor(key, def); enabled {
		prefix, sep, _ := u.NamespaceFor(key, def)
		if prefix == "" {
			errs.Add("%s.namespace.prefix: must not be empty when namespacing is on", where)
		}
		if !separatorPattern.MatchString(sep) {
			errs.Add("%s.namespace.separator: %q must match %s", where, sep, separatorPattern)
		}
		if strings.ContainsAny(prefix, " \t") {
			errs.Add("%s.namespace.prefix: must not contain whitespace", where)
		}
	}

	if u.Tools != nil {
		for from := range u.Tools.Aliases {
			if from == "" {
				errs.Add("%s.tools.aliases: contains an empty source name", where)
			}
		}
	}
}

// validateNamespace checks the namespace block's shape.
func validateNamespace(where string, ns *NamespaceConfig, errs *ValidationError) {
	if ns == nil {
		return
	}
	if ns.Prefix != "" && !serverKeyPattern.MatchString(ns.Prefix) {
		errs.Add("%s.prefix: %q must match %s", where, ns.Prefix, serverKeyPattern)
	}
	if ns.Separator != "" && !separatorPattern.MatchString(ns.Separator) {
		errs.Add("%s.separator: %q must match %s", where, ns.Separator, separatorPattern)
	}
}

func validateAllowRule(where string, r AllowRule, c *Config, errs *ValidationError) {
	for _, s := range r.Servers {
		if s == "" {
			errs.Add("%s.servers: contains an empty pattern", where)
		}
	}
	for server, patterns := range r.Tools {
		if server == "" {
			errs.Add("%s.tools: contains an empty server pattern", where)
			continue
		}
		for _, p := range patterns {
			if p == "" {
				errs.Add("%s.tools[%s]: contains an empty tool pattern", where, server)
			}
		}
	}
}
