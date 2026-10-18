package upstream

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/secrets"
)

// Snapshot is a point-in-time view of one upstream, shaped for the REST API
// and the web UI. Resolved secret values never appear here.
type Snapshot struct {
	Key       string `json:"key"`
	Enabled   bool   `json:"enabled"`
	State     State  `json:"state"`
	Transport string `json:"transport,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`

	StartPolicy string `json:"startPolicy,omitempty"`
	IdleTimeout string `json:"idleTimeout,omitempty"`

	LastError     string    `json:"lastError,omitempty"`
	FailureCount  int       `json:"failureCount"`
	NextAttemptAt time.Time `json:"nextAttemptAt,omitempty"`
	ConnectedAt   time.Time `json:"connectedAt,omitempty"`
	LastUsedAt    time.Time `json:"lastUsedAt,omitempty"`
	Pid           int       `json:"pid,omitempty"`
	InFlight      int       `json:"inFlight"`

	ProtocolVersion string         `json:"protocolVersion,omitempty"`
	Legacy          bool           `json:"legacy,omitempty"`
	ServerName      string         `json:"serverName,omitempty"`
	ServerVersion   string         `json:"serverVersion,omitempty"`
	Instructions    string         `json:"instructions,omitempty"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
	Tools           []Tool         `json:"tools,omitempty"`
	ToolCount       int            `json:"toolCount"`
	Stderr          string         `json:"stderr,omitempty"`

	Config *config.Upstream `json:"config,omitempty"`
}

// Snapshot returns the current state of one upstream.
func (m *Manager) Snapshot(key string) (Snapshot, bool) {
	e, err := m.entry(key)
	if err != nil {
		return Snapshot{}, false
	}
	return m.snapshotOf(e), true
}

// Snapshots returns every configured upstream, sorted by key.
func (m *Manager) Snapshots() []Snapshot {
	m.mu.Lock()
	entries := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.mu.Unlock()

	out := make([]Snapshot, 0, len(entries))
	for _, e := range entries {
		out = append(out, m.snapshotOf(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Keys returns every configured upstream key, sorted.
func (m *Manager) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.entries))
	for k := range m.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// snapshotOf builds a Snapshot from an entry, holding the entry lock only long
// enough to read its fields.
func (m *Manager) snapshotOf(e *entry) Snapshot {
	e.mu.Lock()
	snap := Snapshot{
		Key:           e.key,
		Enabled:       defEnabled(e.def),
		State:         e.state,
		LastError:     e.lastErr,
		FailureCount:  e.failures,
		NextAttemptAt: e.nextAttempt,
		LastUsedAt:    e.lastUsed,
		InFlight:      e.inFlight,
	}
	def := e.def
	conn := e.conn
	cachedTools := e.cachedTools
	cachedInfo := e.cachedInfo
	e.mu.Unlock()

	if def.Cfg != nil {
		snap.Transport = def.Cfg.EffectiveType()
		snap.Endpoint = endpointFor(def.Cfg)
		snap.StartPolicy = def.Cfg.EffectiveStartPolicy(def.Defaults)
		snap.IdleTimeout = def.Cfg.EffectiveIdleTimeout(def.Defaults).String()
		snap.Config = redactedUpstream(def.Cfg)
	}

	// Prefer live data, but fall back to the cached view so a server that has
	// been reaped still contributes its tools to the aggregate.
	info := cachedInfo
	if conn != nil {
		info = conn.Info()
	}

	snap.ProtocolVersion = info.ProtocolVersion
	snap.Legacy = info.Legacy
	snap.ConnectedAt = info.ConnectedAt
	snap.ToolCount = len(info.Tools)
	snap.Tools = info.Tools
	snap.Instructions = info.Instructions
	snap.Capabilities = capabilitiesToMap(info.Capabilities)
	if info.ServerInfo != nil {
		snap.ServerName = info.ServerInfo.Name
		snap.ServerVersion = info.ServerInfo.Version
	}
	if len(snap.Tools) == 0 {
		snap.Tools = cachedTools
		snap.ToolCount = len(cachedTools)
	}

	if conn != nil {
		snap.Pid = conn.Pid()
		// Stderr is diagnostic output, only worth surfacing when something is
		// wrong or we got nothing usable from the server.
		if snap.State == StateError || snap.ToolCount == 0 {
			snap.Stderr = conn.Stderr()
		}
	}
	return snap
}

// capabilitiesToMap renders server capabilities as plain JSON. Passing the
// SDK struct through a JSON round trip keeps the API honest without a
// hand-written field mapping that can drift when the SDK adds fields.
func capabilitiesToMap(c *mcp.ServerCapabilities) map[string]any {
	if c == nil {
		return nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// redactedUpstream returns a copy of the upstream config holding only secret
// references, never resolved values.
func redactedUpstream(cfg *config.Upstream) *config.Upstream {
	clone := *cfg
	clone.Env = secrets.RedactMap(cfg.Env)
	clone.Headers = secrets.RedactMap(cfg.Headers)
	clone.Args = secrets.RedactSlice(cfg.Args)
	clone.Command = secrets.Redact(cfg.Command)
	if cfg.Cwd != "" {
		clone.Cwd = secrets.Redact(cfg.Cwd)
	}
	return &clone
}

// sameUpstream reports whether two upstream configs are equivalent. Comparing
// the JSON form keeps this correct as fields are added.
func sameUpstream(a, b *config.Upstream) bool {
	if a == nil || b == nil {
		return a == b
	}
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ab) == string(bb)
}
