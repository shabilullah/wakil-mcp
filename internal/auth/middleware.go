package auth

import (
	"net/http"
	"strings"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// tokenQueryParam is accepted alongside headers because some harnesses cannot
// set headers on the GET request that opens an SSE stream.
const tokenQueryParam = "token"

// apiKeyHeader is a second accepted header spelling. Harness configuration
// dialogs vary in which one they expose.
const apiKeyHeader = "X-Api-Key"

// Middleware enforces token authentication on an HTTP handler.
//
// The SDK's RequireBearerToken only reads the Authorization header, so this
// wrapper normalises the other accepted spellings first, then converts the
// verified TokenInfo into a Profile for downstream handlers.
func (r *Registry) Middleware() func(http.Handler) http.Handler {
	require := sdkauth.RequireBearerToken(
		r.VerifyToken,
		&sdkauth.RequireBearerTokenOptions{
			// Static operator-issued tokens carry no expiry claim.
			AllowMissingExpiration: true,
		},
	)

	// inner runs after verification, with TokenInfo already in the context.
	inner := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if p, ok := ProfileFromTokenInfo(sdkauth.TokenInfoFromContext(req.Context())); ok {
				req = req.WithContext(WithProfile(req.Context(), p))
			}
			next.ServeHTTP(w, req)
		})
	}

	return func(next http.Handler) http.Handler {
		guarded := require(inner(next))
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if r.AuthDisabled() {
				// Auth is off, so every request acts as a local operator. The
				// profile is still attached so handlers need no special case.
				ctx := WithProfile(req.Context(), LocalProfile())
				next.ServeHTTP(w, req.WithContext(ctx))
				return
			}
			guarded.ServeHTTP(w, normalizeToken(req))
		})
	}
}

// RequireAdmin wraps a handler so only admin credentials may reach it.
func (r *Registry) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.AuthDisabled() {
			next.ServeHTTP(w, req)
			return
		}
		p, ok := ProfileFromContext(req.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !p.Admin {
			http.Error(w, "admin credentials required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, req)
	})
}

// normalizeToken copies a token supplied via X-Api-Key or a query parameter
// into the Authorization header, which is the only place the SDK verifier
// looks.
func normalizeToken(req *http.Request) *http.Request {
	if hasBearer(req) {
		return req
	}

	token := req.Header.Get(apiKeyHeader)
	if token == "" {
		token = req.URL.Query().Get(tokenQueryParam)
	}
	if token == "" {
		return req
	}

	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+token)
	return clone
}

// hasBearer reports whether an Authorization bearer header is already present.
func hasBearer(req *http.Request) bool {
	h := req.Header.Get("Authorization")
	if len(h) < 7 {
		return false
	}
	return strings.EqualFold(h[:7], "Bearer ")
}
