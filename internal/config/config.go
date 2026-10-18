// Package config defines the wakil-mcp configuration file format and the
// loading, validation and persistence rules around it.
//
// The file is deliberately shaped like the mcpServers blocks that harnesses
// already use, so an existing snippet can be pasted in with minimal edits.
package config

import (
	"time"
)

// CurrentVersion is the config schema version this build writes.
const CurrentVersion = 1

// Transport kinds accepted for an upstream server.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
	TransportSSE   = "sse"
	TransportAuto  = "auto"
)

// Start policies for upstream servers.
const (
	StartLazy  = "lazy"
	StartEager = "eager"
	StartNever = "never"
)

// Config is the root of config.json.
type Config struct {
	Version    int                  `json:"version"`
	Server     ServerSettings       `json:"server"`
	Defaults   ServerDefaults       `json:"defaults"`
	Clients    []ClientProfile      `json:"clients,omitempty"`
	MCPServers map[string]*Upstream `json:"mcpServers"`

	// path records where this config was loaded from. It is not serialized.
	path string
}

// ServerSettings configures the daemon itself.
type ServerSettings struct {
	// Bind is the host:port the daemon listens on.
	Bind string `json:"bind"`
	// PublicURL is the externally reachable base URL, used when generating
	// ready-to-paste harness config snippets.
	PublicURL string `json:"publicUrl,omitempty"`
	// DataDir holds the audit database and other runtime state. Defaults to a
	// platform-appropriate directory under the user config dir.
	DataDir string `json:"dataDir,omitempty"`
	// LogLevel is one of debug, info, warn, error.
	LogLevel string `json:"logLevel,omitempty"`
	// DisableAuth turns off token checking on the MCP and admin endpoints.
	// Only sensible on a fully trusted machine.
	DisableAuth bool `json:"disableAuth,omitempty"`
	// AdminTokenHash authorises the REST API and web UI. It is separate from
	// the client profiles so that revoking a harness token cannot lock an
	// operator out of the UI, and vice versa.
	AdminTokenHash string `json:"adminTokenHash,omitempty"`
	// AllowLegacySSE keeps the deprecated HTTP+SSE transport mounted for
	// harnesses that have not moved to Streamable HTTP.
	AllowLegacySSE *bool `json:"allowLegacySSE,omitempty"`
	// StatelessHTTP makes Streamable HTTP requests session-less.
	//
	// This matters more than it looks. Protocol revision 2026-07-28 removed
	// the session concept, and the SDK enforces that: a server with sessions
	// enabled rejects 2026-07-28 requests outright. Turning this off therefore
	// gives up that revision, and in exchange the server can originate
	// requests to the client (elicitation and sampling) and hold long-lived
	// notification streams open.
	//
	// Defaults to true, because a client that asks for 2026-07-28 is the case
	// that fails hard, while session-less operation still serves every older
	// revision.
	StatelessHTTP *bool `json:"statelessHTTP,omitempty"`
	// AuditRetentionDays controls pruning of the tool-call audit log.
	AuditRetentionDays int `json:"auditRetentionDays,omitempty"`
}

// ServerDefaults holds per-upstream defaults, overridable on each server.
type ServerDefaults struct {
	// StartPolicy is lazy, eager or never.
	StartPolicy string `json:"startPolicy,omitempty"`
	// IdleTimeout is how long an upstream may sit unused before being stopped.
	IdleTimeout Duration `json:"idleTimeout,omitempty"`
	// CallTimeout bounds a single tools/call against an upstream.
	CallTimeout Duration `json:"callTimeout,omitempty"`
	// ConnectTimeout bounds connection and initialization.
	ConnectTimeout Duration `json:"connectTimeout,omitempty"`
	// RestartBackoff configures crash recovery.
	RestartBackoff Backoff `json:"restartBackoff,omitempty"`
	// Separator joins the server prefix and the tool name. Defaults to ".".
	Separator string `json:"separator,omitempty"`
	// MaxToolsPerServer caps how many tools one upstream may contribute.
	// Zero means no cap. Protects against a runaway server flooding harnesses.
	MaxToolsPerServer int `json:"maxToolsPerServer,omitempty"`
	// Namespace controls whether tool names are prefixed with the server key.
	Namespace *NamespaceConfig `json:"namespace,omitempty"`
}

// Backoff describes exponential restart backoff.
type Backoff struct {
	Initial Duration `json:"initial,omitempty"`
	Max     Duration `json:"max,omitempty"`
	Factor  float64  `json:"factor,omitempty"`
}

// NamespaceConfig controls tool name prefixing.
type NamespaceConfig struct {
	// Enabled defaults to true. Set false to expose upstream tool names as-is.
	Enabled *bool `json:"enabled,omitempty"`
	// Prefix overrides the config key used as the namespace prefix.
	Prefix string `json:"prefix,omitempty"`
	// Separator overrides the default join separator.
	Separator string `json:"separator,omitempty"`
}

// ToolFilter selects which of an upstream's tools are exposed.
type ToolFilter struct {
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
	// Aliases renames individual upstream tools after namespacing.
	Aliases map[string]string `json:"aliases,omitempty"`
}

// Upstream is one configured MCP server.
type Upstream struct {
	// Type is stdio, http or sse. When empty it is inferred: command implies
	// stdio, url implies http.
	Type string `json:"type,omitempty"`

	// stdio fields
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`

	// http and sse fields
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// Description is free text shown in the UI.
	Description string `json:"description,omitempty"`

	// Enabled defaults to true.
	Enabled *bool `json:"enabled,omitempty"`

	// StartPolicy overrides the default.
	StartPolicy string `json:"startPolicy,omitempty"`

	// IdleTimeout overrides the default.
	IdleTimeout Duration `json:"idleTimeout,omitempty"`

	// Namespace overrides the default prefixing behaviour.
	Namespace *NamespaceConfig `json:"namespace,omitempty"`

	// Tools filters and renames this server's tools.
	Tools *ToolFilter `json:"tools,omitempty"`

	// Timeout bounds connect and initialize for this server.
	Timeout Duration `json:"timeout,omitempty"`
}

// ClientProfile maps a bearer token to an authorization scope.
type ClientProfile struct {
	// Name identifies the harness, and is what shows up in the audit log.
	Name string `json:"name"`
	// TokenHash is "sha256:<hex>". Empty disables auth for this profile.
	TokenHash string `json:"tokenHash,omitempty"`
	// Allow, when non-empty, restricts which servers and tools are reachable.
	Allow AllowRule `json:"allow,omitempty"`
	// Deny removes servers and tools from what Allow permits. Deny wins.
	Deny AllowRule `json:"deny,omitempty"`
	// Disabled suspends the token without deleting it.
	Disabled bool `json:"disabled,omitempty"`
}

// AllowRule is a set of server and tool patterns.
type AllowRule struct {
	// Servers lists server keys or glob patterns. Empty means all servers.
	Servers []string `json:"servers,omitempty"`
	// Tools maps a server key or glob to a list of tool name patterns.
	Tools map[string][]string `json:"tools,omitempty"`
}

// IsEnabled reports whether the upstream should be started. Absent means yes.
func (u *Upstream) IsEnabled() bool { return u.Enabled == nil || *u.Enabled }

// EffectiveType resolves the transport, inferring it when unset.
func (u *Upstream) EffectiveType() string {
	if u.Type != "" && u.Type != TransportAuto {
		return u.Type
	}
	if u.Command != "" {
		return TransportStdio
	}
	if u.URL != "" {
		return TransportHTTP
	}
	return ""
}

// EffectiveStartPolicy resolves the start policy for this server.
func (u *Upstream) EffectiveStartPolicy(def ServerDefaults) string {
	if u.StartPolicy != "" {
		return u.StartPolicy
	}
	return def.EffectiveStartPolicy()
}

// EffectiveStartPolicy resolves the default start policy.
func (d ServerDefaults) EffectiveStartPolicy() string {
	if d.StartPolicy != "" {
		return d.StartPolicy
	}
	return StartLazy
}

// EffectiveIdleTimeout resolves the idle timeout for a server.
func (u *Upstream) EffectiveIdleTimeout(def ServerDefaults) time.Duration {
	if u.IdleTimeout != 0 {
		return time.Duration(u.IdleTimeout)
	}
	return d1(def.IdleTimeout, 15*time.Minute)
}

// EffectiveConnectTimeout resolves the connect timeout for a server.
func (u *Upstream) EffectiveConnectTimeout(def ServerDefaults) time.Duration {
	if u.Timeout != 0 {
		return time.Duration(u.Timeout)
	}
	return d1(def.ConnectTimeout, 30*time.Second)
}

// EffectiveCallTimeout returns the configured call timeout.
func (d ServerDefaults) EffectiveCallTimeout() time.Duration {
	return d1(d.CallTimeout, 120*time.Second)
}

// EffectiveSeparator returns the namespace join separator.
func (d ServerDefaults) EffectiveSeparator() string {
	if d.Separator != "" {
		return d.Separator
	}
	return "."
}

// EffectiveBackoff returns the restart backoff settings.
func (d ServerDefaults) EffectiveBackoff() Backoff {
	b := d.RestartBackoff
	if b.Initial == 0 {
		b.Initial = Duration(time.Second)
	}
	if b.Max == 0 {
		b.Max = Duration(time.Minute)
	}
	if b.Factor <= 1 {
		b.Factor = 2
	}
	return b
}

// LegacySSEEnabled reports whether the deprecated SSE transport is mounted.
func (s ServerSettings) LegacySSEEnabled() bool {
	return s.AllowLegacySSE != nil && *s.AllowLegacySSE
}

// HTTPStateless reports whether Streamable HTTP requests are session-less.
// Absent means yes.
func (s ServerSettings) HTTPStateless() bool {
	return s.StatelessHTTP == nil || *s.StatelessHTTP
}

// NamespaceFor resolves the namespace settings for a server key.
func (u *Upstream) NamespaceFor(key string, def ServerDefaults) (prefix, sep string, enabled bool) {
	enabled = true
	prefix = key
	sep = def.EffectiveSeparator()
	if def.Namespace != nil {
		if def.Namespace.Enabled != nil {
			enabled = *def.Namespace.Enabled
		}
		if def.Namespace.Prefix != "" {
			prefix = def.Namespace.Prefix
		}
		if def.Namespace.Separator != "" {
			sep = def.Namespace.Separator
		}
	}
	if u.Namespace != nil {
		if u.Namespace.Enabled != nil {
			enabled = *u.Namespace.Enabled
		}
		if u.Namespace.Prefix != "" {
			prefix = u.Namespace.Prefix
		}
		if u.Namespace.Separator != "" {
			sep = u.Namespace.Separator
		}
	}
	return prefix, sep, enabled
}

func d1(d Duration, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return time.Duration(d)
}
