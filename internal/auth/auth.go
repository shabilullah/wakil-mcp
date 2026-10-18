// Package auth maps bearer tokens to authorization profiles.
//
// Two kinds of credential exist. Harness tokens identify a client profile and
// carry an optional allowlist. The admin token identifies an operator and
// gates the REST API and web UI. They are stored as SHA-256 hashes, so a
// leaked config file does not hand over working credentials.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/shabilullah/wakil-mcp/internal/config"
)

// ErrUnauthorized reports a missing or unrecognised credential.
var ErrUnauthorized = errors.New("unauthorized")

// profileKey is the context key under which a resolved Profile is stored.
type profileKey struct{}

// Profile is the resolved authorization scope for one request.
type Profile struct {
	// Name identifies the harness, and is recorded in the audit log.
	Name string
	// Allow restricts reachable servers and tools. Empty means unrestricted.
	Allow config.AllowRule
	// Deny removes from what Allow permits.
	Deny config.AllowRule
	// Admin reports whether this credential may use the admin API.
	Admin bool
}

// Registry holds the current credentials. It is swapped wholesale when config
// changes, so a request always sees a consistent snapshot.
type Registry struct {
	mu         sync.RWMutex
	byHash     map[string]Profile
	adminHash  string
	authOff    bool
	configSnap []config.ClientProfile
}

// NewRegistry builds an empty registry. When disabled is true every request is
// treated as a local administrator.
func NewRegistry(disabled bool) *Registry {
	return &Registry{
		byHash:  map[string]Profile{},
		authOff: disabled,
	}
}

// Set replaces the credential set. It is safe to call while requests are in
// flight.
func (r *Registry) Set(clients []config.ClientProfile, adminTokenHash string, disabled bool) {
	byHash := make(map[string]Profile, len(clients))
	for _, c := range clients {
		if c.Disabled || c.TokenHash == "" {
			continue
		}
		byHash[c.TokenHash] = Profile{
			Name:  c.Name,
			Allow: c.Allow,
			Deny:  c.Deny,
		}
	}

	r.mu.Lock()
	r.byHash = byHash
	r.adminHash = adminTokenHash
	r.authOff = disabled
	r.configSnap = append([]config.ClientProfile(nil), clients...)
	r.mu.Unlock()
}

// ProfileForClient builds the authorization scope a client profile describes.
// It is used to preview what a profile can reach without needing a token.
func ProfileForClient(c config.ClientProfile) Profile {
	return Profile{
		Name:  c.Name,
		Allow: c.Allow,
		Deny:  c.Deny,
	}
}

// AuthDisabled reports whether token checking is switched off.
func (r *Registry) AuthDisabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.authOff
}

// ProfileNames lists the configured client profile names, sorted.
func (r *Registry) ProfileNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.configSnap))
	for _, c := range r.configSnap {
		out = append(out, c.Name)
	}
	return out
}

// VerifyToken resolves a token to a profile. This is the SDK's TokenVerifier
// signature, so the result can be threaded through the protocol stack and
// read back from a request.
func (r *Registry) VerifyToken(_ context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.authOff {
		return &sdkauth.TokenInfo{
			UserID: localUserName,
			Extra:  map[string]any{profileExtraKey: Profile{Name: localUserName, Admin: true}},
		}, nil
	}

	hash := config.HashToken(token)
	if hash == r.adminHash {
		p := Profile{Name: "admin", Admin: true}
		return &sdkauth.TokenInfo{
			UserID: p.Name,
			Extra:  map[string]any{profileExtraKey: p},
		}, nil
	}
	p, ok := r.byHash[hash]
	if !ok {
		return nil, sdkauth.ErrInvalidToken
	}
	return &sdkauth.TokenInfo{
		UserID: p.Name,
		Extra:  map[string]any{profileExtraKey: p},
	}, nil
}

// localUserName identifies requests that bypassed auth entirely.
const localUserName = "local"

// profileExtraKey is the TokenInfo.Extra key holding the resolved Profile.
const profileExtraKey = "wakil.profile"

// ProfileFromTokenInfo recovers the Profile stashed by VerifyToken.
func ProfileFromTokenInfo(ti *sdkauth.TokenInfo) (Profile, bool) {
	if ti == nil || ti.Extra == nil {
		return Profile{}, false
	}
	p, ok := ti.Extra[profileExtraKey].(Profile)
	return p, ok
}

// WithProfile attaches a profile to a context.
func WithProfile(ctx context.Context, p Profile) context.Context {
	return context.WithValue(ctx, profileKey{}, p)
}

// ProfileFromContext recovers a profile attached by WithProfile.
func ProfileFromContext(ctx context.Context) (Profile, bool) {
	p, ok := ctx.Value(profileKey{}).(Profile)
	return p, ok
}

// LocalProfile is the identity used when auth is switched off.
func LocalProfile() Profile {
	return Profile{Name: localUserName, Admin: true}
}

// AllowsServer reports whether the profile may use a given upstream.
func (p Profile) AllowsServer(serverKey string) bool {
	if p.Admin {
		return true
	}
	if len(p.Deny.Servers) > 0 && matchAny(p.Deny.Servers, serverKey) {
		return false
	}
	if len(p.Allow.Servers) == 0 {
		return true
	}
	return matchAny(p.Allow.Servers, serverKey)
}

// AllowsTool reports whether the profile may call a given tool. Both the
// exposed name and the original upstream name are checked so that a rule
// written either way behaves as the operator expects.
func (p Profile) AllowsTool(serverKey, exposedName, upstreamName string) bool {
	if p.Admin {
		return true
	}
	if !p.AllowsServer(serverKey) {
		return false
	}

	if patterns, ok := lookupServerPatterns(p.Deny.Tools, serverKey); ok {
		if matchAny(patterns, exposedName) || matchAny(patterns, upstreamName) {
			return false
		}
	}

	if len(p.Allow.Tools) == 0 {
		return true
	}
	patterns, ok := lookupServerPatterns(p.Allow.Tools, serverKey)
	if !ok {
		// The allow map exists but says nothing about this server, so this
		// server was not granted.
		return false
	}
	return matchAny(patterns, exposedName) || matchAny(patterns, upstreamName)
}

// lookupServerPatterns finds the tool patterns for a server, preferring an
// exact key match and falling back to a glob over the map keys.
func lookupServerPatterns(m map[string][]string, serverKey string) ([]string, bool) {
	if m == nil {
		return nil, false
	}
	if patterns, ok := m[serverKey]; ok {
		return patterns, true
	}
	for key, patterns := range m {
		if key == "*" || matchPattern(key, serverKey) {
			return patterns, true
		}
	}
	return nil, false
}

// matchAny reports whether name matches any pattern.
func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if matchPattern(p, name) {
			return true
		}
	}
	return false
}

// matchPattern reports whether name matches a glob pattern, case-insensitively.
// '*' matches any run of characters and '?' matches exactly one.
func matchPattern(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if !strings.ContainsAny(pattern, "*?") {
		return strings.EqualFold(pattern, name)
	}
	return globFold([]rune(strings.ToLower(pattern)), []rune(strings.ToLower(name)))
}

// globFold matches a lowercased pattern with backtracking on '*'.
func globFold(pattern, name []rune) bool {
	p, n := 0, 0
	starP, starN := -1, 0

	for n < len(name) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == name[n]):
			p++
			n++
		case p < len(pattern) && pattern[p] == '*':
			starP = p
			starN = n
			p++
		case starP >= 0:
			starN++
			n = starN
			p = starP + 1
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
