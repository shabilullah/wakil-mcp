package server

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shabilullah/wakil-mcp/internal/logging"
)

// maxRequestBodyBytes bounds an inbound MCP request body. The SDK default is
// 4 MiB, which is tight for tools that accept a document; 32 MiB is generous
// while still bounded.
const maxRequestBodyBytes = 32 << 20

// buildHandler assembles the HTTP surface.
func (a *App) buildHandler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated liveness probe, for container health checks.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return a.prx.Server() },
		&mcp.StreamableHTTPOptions{
			// Session-less operation is what makes protocol 2026-07-28
			// possible: the SDK rejects that revision on a server with
			// sessions, and its own client would otherwise be told the
			// version is supported and then refused. See ServerSettings.
			Stateless: a.cfg.Server.HTTPStateless(),
			// The default localhost DNS-rebinding guard is kept: it costs
			// nothing on a LAN bind and closes a real hole on a workstation.
			DisableLocalhostProtection: false,
			MaxRequestBodyBytes:        maxRequestBodyBytes,
			// Per-request transport chatter is dropped: see logging.Quiet.
			Logger: logging.Quiet(a.log, slog.LevelWarn),
			// Tie handler context to the HTTP request so a harness that
			// disconnects also cancels the upstream call it started.
			PropagateRequestCancellation: true,
		},
	)
	mux.Handle(MCPEndpoint, a.reg.Middleware()(mcpHandler))

	if a.cfg.Server.LegacySSEEnabled() {
		sseHandler := mcp.NewSSEHandler(
			func(*http.Request) *mcp.Server { return a.prx.Server() },
			&mcp.SSEOptions{MaxRequestBodyBytes: maxRequestBodyBytes},
		)
		mux.Handle(SSEEndpoint, a.reg.Middleware()(sseHandler))
		a.log.Info("deprecated HTTP+SSE transport is enabled", "path", SSEEndpoint)
	}

	// Admin REST API and the web UI, both behind admin credentials.
	adminAPI := a.reg.Middleware()(a.reg.RequireAdmin(a.adminMux()))
	mux.Handle(AdminPrefix+"/", adminAPI)

	// The UI itself is served without credentials so the browser can load it
	// and prompt for a token. Every API call it makes is authenticated.
	mux.Handle("/", a.uiHandler())

	return recoverPanics(a.log, requestLogger(a.log, mux))
}

// adminMux routes the REST API. The prefix is stripped by the caller.
func (a *App) adminMux() http.Handler {
	mux := http.NewServeMux()

	// Auth check: the middleware has already validated the token, so reaching
	// here means the credential is an admin credential.
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"version": a.ver,
		})
	})

	a.registerConfigRoutes(mux)
	a.registerServerRoutes(mux)
	a.registerToolRoutes(mux)
	a.registerClientRoutes(mux)
	a.registerAuditRoutes(mux)
	a.registerMetaRoutes(mux)

	return mux
}

// requestLogger records method, path and duration, and recovers nothing: the
// panics middleware handles that.
func requestLogger(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Long-lived streams would log a duration equal to the session
		// lifetime, which is noise; only log the start for those.
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Health checks every few seconds would drown the log.
		if r.URL.Path == "/healthz" {
			return
		}
		log.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"ms", time.Since(started).Milliseconds())
	})
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.wroteHeader = true
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush lets streaming responses pass through the recorder.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// recoverPanics turns a handler panic into a 500 instead of killing the
// listener, and logs the stack.
func recoverPanics(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("panic serving request",
					"method", r.Method,
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()))
				// A partial response may already be written, in which case
				// this header write is a no-op and the connection is closed.
				if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}
