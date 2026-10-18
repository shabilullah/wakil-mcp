package server

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// webuiFS holds the built single-page app. The placeholder file in the source
// tree keeps go:embed satisfied even before the front end is built, so a plain
// `go build` always works.
//
//go:embed all:webui/dist
var webuiFS embed.FS

// uiHandler serves the web UI with single-page-app fallback: any path that is
// not a real asset returns index.html so client-side routes work on reload.
func (a *App) uiHandler() http.Handler {
	sub, err := fs.Sub(webuiFS, "webui/dist")
	if err != nil {
		// The embed is compile-time checked, so this cannot happen outside a
		// broken build.
		a.log.Error("web UI assets are missing", "error", err)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "web UI is not available", http.StatusNotFound)
		})
	}

	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}

		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}

		if f, err := sub.Open(name); err == nil {
			info, statErr := f.Stat()
			f.Close()
			if statErr == nil && !info.IsDir() {
				// Hashed asset names are safe to cache hard; index.html must
				// not be, so a new build is picked up immediately.
				//
				// The name has already had its leading slash trimmed, so this
				// has to be a prefix test: "/assets/" would never match.
				if name != "index.html" && strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				files.ServeHTTP(w, r)
				return
			}
		}

		// Unknown path: hand back the app shell.
		serveIndex(w, r, sub)
	})
}

// serveIndex writes index.html with headers that prevent stale caching.
func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	f, err := sub.Open("index.html")
	if err != nil {
		http.Error(w, "web UI is not available", http.StatusNotFound)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}
