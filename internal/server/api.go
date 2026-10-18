package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/shabilullah/wakil-mcp/internal/aggregate"
	"github.com/shabilullah/wakil-mcp/internal/audit"
	"github.com/shabilullah/wakil-mcp/internal/auth"
	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/harness"
	"github.com/shabilullah/wakil-mcp/internal/upstream"
)

// testTimeout bounds the "test this server" probe from the UI.
const testTimeout = 45 * time.Second

// registerConfigRoutes wires the configuration endpoints.
func (a *App) registerConfigRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/config", a.handleGetConfig)
	mux.HandleFunc("PUT /api/config", a.handlePutConfig)
	mux.HandleFunc("POST /api/config/validate", a.handleValidateConfig)
}

// registerServerRoutes wires the upstream server endpoints.
func (a *App) registerServerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/servers", a.handleListServers)
	mux.HandleFunc("GET /api/servers/{key}", a.handleGetServer)
	mux.HandleFunc("POST /api/servers/{key}/start", a.handleStartServer)
	mux.HandleFunc("POST /api/servers/{key}/stop", a.handleStopServer)
	mux.HandleFunc("POST /api/servers/{key}/restart", a.handleRestartServer)
	mux.HandleFunc("GET /api/servers/{key}/stderr", a.handleServerStderr)
	mux.HandleFunc("POST /api/servers/test", a.handleTestServer)
}

// registerToolRoutes wires the aggregated tool endpoints.
func (a *App) registerToolRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tools", a.handleListTools)
	mux.HandleFunc("GET /api/tools/one", a.handleGetTool)
}

// registerClientRoutes wires the client profile endpoints.
func (a *App) registerClientRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/clients", a.handleListClients)
	mux.HandleFunc("POST /api/clients", a.handleCreateClient)
	mux.HandleFunc("DELETE /api/clients/{name}", a.handleDeleteClient)
	mux.HandleFunc("POST /api/clients/{name}/rotate", a.handleRotateClient)
	mux.HandleFunc("POST /api/clients/{name}/enabled", a.handleSetClientEnabled)
}

// registerAuditRoutes wires the audit endpoints.
func (a *App) registerAuditRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/audit", a.handleAuditEvents)
	mux.HandleFunc("POST /api/audit/prune", a.handleAuditPrune)
}

// registerMetaRoutes wires the status and harness-configuration endpoints.
func (a *App) registerMetaRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("GET /api/harness", a.handleHarnessList)
	mux.HandleFunc("GET /api/harness/{client}", a.handleHarness)
}

// ---------------------------------------------------------------------------
// Status and configuration
// ---------------------------------------------------------------------------

// handleGetConfig returns the raw config text plus where it lives.
//
// The editor edits the file itself rather than a form-shaped projection of it,
// so nothing is lost to a round trip and any field the UI does not know about
// still survives an edit.
func (a *App) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := a.Config()
	raw, err := cfg.Marshal()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":    cfg.Path(),
		"version": cfg.Version,
		"json":    string(raw),
	})
}

// configRequest is the body of a config edit.
type configRequest struct {
	JSON string `json:"json"`
}

// handleValidateConfig parses and validates a candidate config without saving.
func (a *App) handleValidateConfig(w http.ResponseWriter, r *http.Request) {
	var req configRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	problems := validateConfigText(req.JSON)
	writeJSON(w, http.StatusOK, map[string]any{
		"valid":    len(problems) == 0,
		"problems": problems,
	})
}

// handlePutConfig replaces the configuration, saving it and applying it live.
func (a *App) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var req configRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	candidate, err := config.Parse([]byte(req.JSON))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{
			Error:  "the JSON could not be parsed",
			Detail: []string{err.Error()},
		})
		return
	}

	// One writer at a time: the file is read-modify-written, and the daemon's
	// own watcher must not see a half-finished edit as an operator change.
	a.saveMu.Lock()
	defer a.saveMu.Unlock()

	path := a.Config().Path()
	candidate.SetPath(path)

	if err := candidate.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := candidate.SaveTo(path); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	a.noteStamp(path)

	// Applying can fail for reasons validation cannot see, such as a port
	// already in use by another process. The file is already written, so
	// report the problem rather than rolling back silently.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := a.Reload(ctx, candidate); err != nil {
		writeJSON(w, http.StatusConflict, apiError{
			Error:  "the configuration was saved but could not be applied",
			Detail: []string{err.Error()},
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// validateConfigText parses and validates raw config text, returning a list of
// human-readable problems. An empty list means the config is usable.
func validateConfigText(text string) []string {
	candidate, err := config.Parse([]byte(text))
	if err != nil {
		return []string{err.Error()}
	}
	if err := candidate.Validate(); err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			return ve.Problems
		}
		return []string{err.Error()}
	}
	return nil
}

// mutateConfig applies a change to a copy of the config, then saves and
// applies it. Validation runs before anything is written.
//
// saveMu is held across the read-modify-write so two editors cannot lose each
// other's change. It is deliberately not held across Reload, which takes cfgMu
// and can block on enumeration.
func (a *App) mutateConfig(change func(*config.Config) error) error {
	clone := a.Config().Clone()
	if clone == nil {
		return errors.New("could not copy the configuration")
	}
	if err := change(clone); err != nil {
		return err
	}
	if err := clone.Validate(); err != nil {
		return err
	}

	a.saveMu.Lock()
	path := a.Config().Path()
	clone.SetPath(path)
	err := clone.SaveTo(path)
	a.saveMu.Unlock()

	if err != nil {
		return err
	}
	a.noteStamp(path)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return a.Reload(ctx, clone)
}

// ---------------------------------------------------------------------------
// Upstream servers
// ---------------------------------------------------------------------------

// handleListServers returns every configured upstream and its state.
func (a *App) handleListServers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"servers": a.Manager().Snapshots(),
	})
}

// handleGetServer returns one upstream.
func (a *App) handleGetServer(w http.ResponseWriter, r *http.Request) {
	snap, ok := a.Manager().Snapshot(r.PathValue("key"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no server named %q", r.PathValue("key")))
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// handleStartServer connects an upstream now.
func (a *App) handleStartServer(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if _, err := a.Manager().Acquire(ctx, key); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	a.Manager().Release(key)
	a.Resync()
	a.respondServer(w, key)
}

// handleStopServer shuts an upstream down.
func (a *App) handleStopServer(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if err := a.Manager().Stop(key); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	a.Resync()
	a.respondServer(w, key)
}

// handleRestartServer drops and re-establishes an upstream connection.
func (a *App) handleRestartServer(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if err := a.Manager().Restart(ctx, key); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	a.Resync()
	a.respondServer(w, key)
}

// handleServerStderr returns the retained stderr of a stdio upstream. This is
// where a crashing npx server's actual error message shows up.
func (a *App) handleServerStderr(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	snap, ok := a.Manager().Snapshot(key)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no server named %q", key))
		return
	}

	stderr := snap.Stderr
	if stderr == "" {
		if conn, live := a.Manager().Get(key); live {
			stderr = conn.Stderr()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stderr": stderr})
}

// testRequest is the body of a connectivity probe.
type testRequest struct {
	// Key is the name to test under. Defaults to "test".
	Key string `json:"key"`
	// Server is the definition to probe. It is never saved.
	Server *config.Upstream `json:"server"`
}

// handleTestServer connects to an unsaved definition and reports what the
// server offers. This backs the UI's "Test" button, so a bad server is found
// before it is saved.
func (a *App) handleTestServer(w http.ResponseWriter, r *http.Request) {
	var req testRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Server == nil {
		writeError(w, http.StatusBadRequest, errors.New("server is required"))
		return
	}

	key := req.Key
	if key == "" {
		key = "test"
	}

	// Validate the probe the same way a saved server would be, so the UI
	// catches mistakes here rather than on save.
	probe := a.Config().Clone()
	probe.MCPServers = map[string]*config.Upstream{key: req.Server}
	probe.Clients = nil
	probe.Server.DisableAuth = true
	if err := probe.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), testTimeout)
	defer cancel()

	info, err := a.Manager().Test(ctx, upstream.Def{
		Key:      key,
		Cfg:      req.Server,
		Defaults: a.Config().Defaults,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"info": info,
	})
}

// respondServer writes one upstream's snapshot.
func (a *App) respondServer(w http.ResponseWriter, key string) {
	snap, ok := a.Manager().Snapshot(key)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no server named %q", key))
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// ---------------------------------------------------------------------------
// Aggregated tools
// ---------------------------------------------------------------------------

// handleListTools returns the exposed tool index.
func (a *App) handleListTools(w http.ResponseWriter, r *http.Request) {
	ix := a.Index()

	tools := ix.Entries()
	if server := r.URL.Query().Get("server"); server != "" {
		tools = ix.ForServer(server)
	}
	if q := r.URL.Query().Get("q"); q != "" {
		tools = ix.Find(q)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"tools":      tools,
		"count":      len(tools),
		"total":      ix.Len(),
		"servers":    ix.Servers(),
		"collisions": ix.Collisions(),
	})
}

// handleGetTool returns one exposed tool by name. The name is a query
// parameter rather than a path segment so dotted names need no escaping.
func (a *App) handleGetTool(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, errors.New("the name query parameter is required"))
		return
	}
	entry, ok := a.Index().Lookup(name)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no tool named %q", name))
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// ---------------------------------------------------------------------------
// Client profiles
// ---------------------------------------------------------------------------

// clientView is the safe projection of a client profile. Token hashes are
// never returned; only whether one is set.
type clientView struct {
	Name      string           `json:"name"`
	HasToken  bool             `json:"hasToken"`
	TokenShow string           `json:"tokenHint,omitempty"`
	Disabled  bool             `json:"disabled"`
	Allow     config.AllowRule `json:"allow"`
	Deny      config.AllowRule `json:"deny"`
	ToolCount int              `json:"visibleTools"`
}

// handleListClients returns every client profile.
func (a *App) handleListClients(w http.ResponseWriter, r *http.Request) {
	cfg := a.Config()
	views := make([]clientView, 0, len(cfg.Clients))
	for _, c := range cfg.Clients {
		views = append(views, a.viewClient(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": views})
}

// viewClient projects a profile, counting how many tools it can reach.
func (a *App) viewClient(c config.ClientProfile) clientView {
	v := clientView{
		Name:     c.Name,
		HasToken: c.TokenHash != "",
		Disabled: c.Disabled,
		Allow:    c.Allow,
		Deny:     c.Deny,
	}
	if c.TokenHash != "" {
		// The hash is not reusable as a credential, but there is no reason to
		// publish it either.
		v.TokenShow = "(set)"
	}

	profile := auth.ProfileForClient(c)
	for _, e := range a.Index().Entries() {
		if profile.AllowsTool(e.ServerKey, e.Exposed, e.UpstreamName) {
			v.ToolCount++
		}
	}
	return v
}

// createClientRequest is the body of a client creation.
type createClientRequest struct {
	Name  string           `json:"name"`
	Allow config.AllowRule `json:"allow"`
	Deny  config.AllowRule `json:"deny"`
}

// handleCreateClient adds a profile and returns its token exactly once.
func (a *App) handleCreateClient(w http.ResponseWriter, r *http.Request) {
	var req createClientRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}

	token, hash, err := config.GenerateToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	err = a.mutateConfig(func(c *config.Config) error {
		for _, existing := range c.Clients {
			if existing.Name == req.Name {
				return fmt.Errorf("a client named %q already exists", req.Name)
			}
		}
		c.Clients = append(c.Clients, config.ClientProfile{
			Name:      req.Name,
			TokenHash: hash,
			Allow:     req.Allow,
			Deny:      req.Deny,
		})
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	view, _ := a.findClient(req.Name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"client": view,
		"token":  token,
		"note":   "Store this token now. Only its hash is kept, so it cannot be shown again.",
	})
}

// handleDeleteClient removes a profile.
func (a *App) handleDeleteClient(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	err := a.mutateConfig(func(c *config.Config) error {
		out := c.Clients[:0]
		found := false
		for _, existing := range c.Clients {
			if existing.Name == name {
				found = true
				continue
			}
			out = append(out, existing)
		}
		if !found {
			return fmt.Errorf("no client named %q", name)
		}
		c.Clients = out
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRotateClient issues a new token for a profile.
func (a *App) handleRotateClient(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	token, hash, err := config.GenerateToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	err = a.mutateConfig(func(c *config.Config) error {
		for i := range c.Clients {
			if c.Clients[i].Name == name {
				c.Clients[i].TokenHash = hash
				return nil
			}
		}
		return fmt.Errorf("no client named %q", name)
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	view, _ := a.findClient(name)
	writeJSON(w, http.StatusOK, map[string]any{
		"client": view,
		"token":  token,
		"note":   "Store this token now. The previous token no longer works.",
	})
}

// enabledRequest is the body of an enable or disable call.
type enabledRequest struct {
	Enabled bool `json:"enabled"`
}

// handleSetClientEnabled suspends or resumes a profile without deleting it.
func (a *App) handleSetClientEnabled(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var req enabledRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	err := a.mutateConfig(func(c *config.Config) error {
		for i := range c.Clients {
			if c.Clients[i].Name == name {
				c.Clients[i].Disabled = !req.Enabled
				return nil
			}
		}
		return fmt.Errorf("no client named %q", name)
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	view, _ := a.findClient(name)
	writeJSON(w, http.StatusOK, view)
}

// findClient projects one profile by name.
func (a *App) findClient(name string) (clientView, bool) {
	for _, c := range a.Config().Clients {
		if c.Name == name {
			return a.viewClient(c), true
		}
	}
	return clientView{}, false
}

// ---------------------------------------------------------------------------
// Audit log
// ---------------------------------------------------------------------------

// handleAuditEvents returns recent tool calls plus a summary.
func (a *App) handleAuditEvents(w http.ResponseWriter, r *http.Request) {
	if a.Audit() == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"events":  []audit.Event{},
			"stats":   audit.Stats{ByStatus: map[string]int64{}, ByServer: map[string]int64{}},
			"enabled": false,
		})
		return
	}

	q := r.URL.Query()
	filter := audit.Filter{
		Client: q.Get("client"),
		Server: q.Get("server"),
		Tool:   q.Get("tool"),
		Status: q.Get("status"),
		Limit:  queryInt(r, "limit", 100),
		Offset: queryInt(r, "offset", 0),
	}
	if since := q.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			filter.Since = t
		}
	}

	events, err := a.Audit().Query(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	stats, err := a.Audit().Stats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"events":  events,
		"stats":   stats,
		"enabled": true,
	})
}

// pruneRequest is the body of an audit prune.
type pruneRequest struct {
	Days int `json:"days"`
}

// handleAuditPrune deletes events older than a cutoff.
func (a *App) handleAuditPrune(w http.ResponseWriter, r *http.Request) {
	if a.Audit() == nil {
		writeError(w, http.StatusConflict, errors.New("auditing is disabled"))
		return
	}

	var req pruneRequest
	if r.ContentLength > 0 {
		if err := readJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	if req.Days <= 0 {
		req.Days = 30
	}

	removed, err := a.Audit().Prune(r.Context(), time.Now().AddDate(0, 0, -req.Days))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed, "days": req.Days})
}

// ---------------------------------------------------------------------------
// Status and harness snippets
// ---------------------------------------------------------------------------

// handleStatus summarises the whole proxy for the UI header.
func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	snaps := a.Manager().Snapshots()
	states := map[string]int{}
	for _, s := range snaps {
		states[string(s.State)]++
	}
	ix := a.Index()

	var collisions []aggregate.Collision
	if len(ix.Collisions()) > 0 {
		collisions = ix.Collisions()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":     a.ver,
		"publicUrl":   a.PublicURL(),
		"mcpEndpoint": MCPEndpoint,
		"dataDir":     a.Config().Server.DataDir,
		"authEnabled": !a.Config().Server.DisableAuth,
		"serverCount": len(snaps),
		"states":      states,
		"toolCount":   ix.Len(),
		"clientCount": len(a.Config().Clients),
		"collisions":  collisions,
	})
}

// handleHarness renders ready-to-paste configuration for one harness, or for
// every harness when the name is "all", so setting up a new client is a copy
// and paste rather than a hunt for the right JSON shape.
func (a *App) handleHarness(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("client")
	cfg := a.Config()
	base := a.PublicURL()

	snippets, err := harness.Snippets(harness.Input{
		Harness:   name,
		BaseURL:   base + MCPEndpoint,
		SSEURL:    base + SSEEndpoint,
		LegacySSE: cfg.Server.LegacySSEEnabled(),
		Binary:    "wakil-mcp",
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"client":   name,
		"baseUrl":  base,
		"snippets": snippets,
	})
}

// handleHarnessList lists the harnesses that have snippets.
func (a *App) handleHarnessList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"clients": harness.Names()})
}
