// Package aggregate turns the tools reported by many upstream servers into a
// single flat, namespaced namespace.
//
// Tool names are only unique within one server, so a proxy that merges several
// servers must disambiguate. The MCP specification calls for exactly this:
// aggregators "SHOULD implement a disambiguation strategy such as prefixing
// tool names with a server identifier", and warns that serverInfo.name is not
// reliably unique. Prefixing uses the operator's config key instead.
package aggregate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/upstream"
)

// MaxToolNameLen is the longest tool name the MCP specification allows.
const MaxToolNameLen = 128

// emptyObjectSchema is substituted for a missing input schema. The spec
// requires inputSchema to be a valid JSON Schema object and explicitly not
// null, and the SDK panics on a nil schema, so a server that sends nothing
// gets the recommended empty-parameter schema.
const emptyObjectSchema = `{"type":"object","additionalProperties":false}`

// Entry is one tool exposed to harnesses.
type Entry struct {
	// Exposed is the name harnesses call.
	Exposed string `json:"exposed"`
	// ServerKey is the config key of the upstream providing the tool.
	ServerKey string `json:"serverKey"`
	// UpstreamName is the tool name the upstream knows.
	UpstreamName string `json:"upstreamName"`
	// Tool is the upstream's definition, with the input schema normalized.
	Tool upstream.Tool `json:"tool"`
	// Truncated reports that the name was shortened to fit the length limit.
	Truncated bool `json:"truncated,omitempty"`
	// Renamed reports that the operator's alias map changed the name.
	Renamed bool `json:"renamed,omitempty"`
	// Disambiguated reports that a suffix was added to resolve a collision.
	Disambiguated bool `json:"disambiguated,omitempty"`
}

// Collision records two tools that wanted the same exposed name.
type Collision struct {
	// ServerKey is the server whose tool lost the name.
	ServerKey string `json:"serverKey"`
	// Wanted is the name that was already taken.
	Wanted string `json:"wanted"`
	// Given is the name actually assigned.
	Given string `json:"given"`
	// HeldBy is the server that already owned Wanted.
	HeldBy string `json:"heldBy"`
}

// Index is an immutable lookup table from exposed name to origin.
type Index struct {
	entries    []Entry
	byName     map[string]int
	collisions []Collision
	servers    []string
}

// Len returns how many tools are exposed.
func (ix *Index) Len() int { return len(ix.entries) }

// Entries returns every exposed tool, ordered by exposed name.
func (ix *Index) Entries() []Entry { return ix.entries }

// Collisions returns every name that had to be disambiguated.
func (ix *Index) Collisions() []Collision { return ix.collisions }

// Servers returns the config keys contributing tools, sorted.
func (ix *Index) Servers() []string { return ix.servers }

// Lookup resolves an exposed name to its origin.
func (ix *Index) Lookup(name string) (Entry, bool) {
	if ix == nil {
		return Entry{}, false
	}
	i, ok := ix.byName[name]
	if !ok {
		return Entry{}, false
	}
	return ix.entries[i], true
}

// ForServer returns every exposed tool belonging to one upstream.
func (ix *Index) ForServer(key string) []Entry {
	var out []Entry
	for _, e := range ix.entries {
		if e.ServerKey == key {
			out = append(out, e)
		}
	}
	return out
}

// Find returns tools whose name or description contains the query, used by the
// synthetic search tool.
func (ix *Index) Find(query string) []Entry {
	if query == "" {
		return nil
	}
	q := strings.ToLower(query)
	var out []Entry
	for _, e := range ix.entries {
		if strings.Contains(strings.ToLower(e.Exposed), q) ||
			strings.Contains(strings.ToLower(e.Tool.Description), q) ||
			strings.Contains(strings.ToLower(e.UpstreamName), q) ||
			strings.Contains(strings.ToLower(e.ServerKey), q) {
			out = append(out, e)
		}
	}
	return out
}

// Builder accumulates tools from several servers and produces an Index.
type Builder struct {
	defaults   config.ServerDefaults
	entries    []Entry
	taken      map[string]string // exposed name -> owning server key
	collisions []Collision
	servers    map[string]bool
}

// NewBuilder starts a builder using the given defaults for namespace settings.
func NewBuilder(defaults config.ServerDefaults) *Builder {
	return &Builder{
		defaults: defaults,
		taken:    map[string]string{},
		servers:  map[string]bool{},
	}
}

// Reserve marks exposed names as already taken by holder.
//
// Tools are registered by name, and a second registration with the same name
// replaces the first. The proxy reserves its own _wakil namespace this way: an
// upstream server with namespacing switched off can offer a tool under any name
// at all, and a prefix test is not a defence. Reserving routes the clash
// through the ordinary disambiguation path instead, so the upstream tool stays
// reachable under a suffixed name and the rename is reported like any other
// collision.
func (b *Builder) Reserve(holder string, names ...string) {
	for _, name := range names {
		if _, ok := b.taken[name]; !ok {
			b.taken[name] = holder
		}
	}
}

// AddServer contributes one upstream's tools.
//
// Tools are filtered against the server's include and exclude rules using the
// original upstream names, so filters stay stable when an alias is present.
func (b *Builder) AddServer(key string, cfg *config.Upstream, tools []upstream.Tool) {
	prefix, sep, namespaced := namespaceFor(key, cfg, b.defaults)

	max := b.defaults.MaxToolsPerServer
	added := 0

	for _, t := range tools {
		if !included(cfg, t.Name) {
			continue
		}
		if max > 0 && added >= max {
			break
		}

		sourceName := t.Name
		if cfg != nil && cfg.Tools != nil {
			if alias, ok := cfg.Tools.Aliases[sourceName]; ok && alias != "" {
				sourceName = alias
			}
		}

		// Sanitise the two halves separately and join with the separator,
		// which is operator-supplied and must survive intact. Sanitising the
		// joined name would mangle a separator like "::" into "__".
		toolPart := sanitize(sourceName)
		if toolPart == "" {
			toolPart = "tool"
		}

		base := toolPart
		if namespaced {
			if p := sanitize(prefix); p != "" {
				base = p + sep + toolPart
			}
		}
		base, truncated := truncateName(base)

		final, disambiguated, heldBy := b.claim(key, base)
		if final == "" {
			continue
		}

		tool := t
		tool.InputSchema = normalizeInputSchema(t.InputSchema)
		tool.OutputSchema = normalizeOutputSchema(t.OutputSchema)

		b.entries = append(b.entries, Entry{
			Exposed:       final,
			ServerKey:     key,
			UpstreamName:  t.Name,
			Tool:          tool,
			Truncated:     truncated,
			Renamed:       sourceName != t.Name,
			Disambiguated: disambiguated,
		})
		if disambiguated {
			b.collisions = append(b.collisions, Collision{
				ServerKey: key,
				Wanted:    base,
				Given:     final,
				HeldBy:    heldBy,
			})
		}
		b.servers[key] = true
		added++
	}
}

// claim reserves an exposed name, appending a numeric suffix when it is
// already held by another server. It returns the name assigned, whether a
// suffix was needed, and the server that held the original name.
func (b *Builder) claim(serverKey, base string) (name string, disambiguated bool, heldBy string) {
	if holder, ok := b.taken[base]; !ok {
		b.taken[base] = serverKey
		return base, false, ""
	} else {
		heldBy = holder
	}

	for n := 2; n <= 1000; n++ {
		suffix := "-" + strconv.Itoa(n)
		candidate := fitWithSuffix(base, suffix)
		if _, ok := b.taken[candidate]; !ok {
			b.taken[candidate] = serverKey
			return candidate, true, heldBy
		}
	}
	// Pathological case: a thousand tools want the same name. Drop the extra
	// rather than emit something malformed.
	return "", true, heldBy
}

// Build finalizes the index. After this call the builder must not be reused.
func (b *Builder) Build() *Index {
	sort.Slice(b.entries, func(i, j int) bool { return b.entries[i].Exposed < b.entries[j].Exposed })

	byName := make(map[string]int, len(b.entries))
	for i, e := range b.entries {
		byName[e.Exposed] = i
	}
	servers := make([]string, 0, len(b.servers))
	for k := range b.servers {
		servers = append(servers, k)
	}
	sort.Strings(servers)

	return &Index{
		entries:    b.entries,
		byName:     byName,
		collisions: b.collisions,
		servers:    servers,
	}
}

// namespaceFor resolves the prefix, separator and enablement for a server.
func namespaceFor(key string, cfg *config.Upstream, defaults config.ServerDefaults) (prefix, sep string, enabled bool) {
	if cfg == nil {
		return key, defaults.EffectiveSeparator(), true
	}
	return cfg.NamespaceFor(key, defaults)
}

// included applies a server's include and exclude rules.
func included(cfg *config.Upstream, name string) bool {
	if cfg == nil || cfg.Tools == nil {
		return true
	}
	f := cfg.Tools
	// Exclude wins over include, which matches how people expect a deny list
	// to behave when they add a broad allow.
	if len(f.Exclude) > 0 && matchAny(f.Exclude, name) {
		return false
	}
	if len(f.Include) > 0 {
		return matchAny(f.Include, name)
	}
	return true
}

// sanitize replaces characters that are not allowed in tool names. The
// specification permits letters, digits, underscore, hyphen and dot.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// truncateName shortens a name that exceeds the limit, appending a short hash
// of the original so two long names sharing a prefix remain distinct.
func truncateName(name string) (string, bool) {
	if len(name) <= MaxToolNameLen {
		return name, false
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "~" + hex.EncodeToString(sum[:4])
	keep := MaxToolNameLen - len(suffix)
	return name[:keep] + suffix, true
}

// fitWithSuffix appends a suffix, trimming the base if that would exceed the
// limit.
func fitWithSuffix(base, suffix string) string {
	if len(base)+len(suffix) <= MaxToolNameLen {
		return base + suffix
	}
	room := MaxToolNameLen - len(suffix)
	if room < 1 {
		room = 1
	}
	return base[:room] + suffix
}

// normalizeInputSchema replaces a missing or null schema with the spec's
// recommended empty-object schema.
func normalizeInputSchema(raw json.RawMessage) json.RawMessage {
	if isNullish(raw) {
		return json.RawMessage(emptyObjectSchema)
	}
	return raw
}

// normalizeOutputSchema drops a null output schema. Unlike inputSchema it is
// optional, so there is no correct substitute.
func normalizeOutputSchema(raw json.RawMessage) json.RawMessage {
	if isNullish(raw) {
		return nil
	}
	return raw
}

// isNullish reports whether a raw schema is absent, empty, or JSON null.
func isNullish(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null"
}
