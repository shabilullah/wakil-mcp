// Package server ties the configuration, upstream connections, aggregated tool
// index and HTTP surface into one runnable proxy.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shabilullah/wakil-mcp/internal/aggregate"
	"github.com/shabilullah/wakil-mcp/internal/audit"
	"github.com/shabilullah/wakil-mcp/internal/auth"
	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/proxy"
	"github.com/shabilullah/wakil-mcp/internal/secrets"
	"github.com/shabilullah/wakil-mcp/internal/upstream"
)

// Options configures an App.
type Options struct {
	// Logger receives all diagnostics. Defaults to slog.Default().
	Logger *slog.Logger
	// Version is reported as serverInfo.version. Defaults to "dev".
	Version string
	// AuditPath is the audit database location. Empty disables auditing.
	AuditPath string
	// SkipEnumeration suppresses the startup connection pass. Tests that want
	// to observe lazy startup set this.
	SkipEnumeration bool
	// ReapInterval overrides how often idle upstreams are checked. Tests use a
	// short interval so they do not have to wait for the default.
	ReapInterval time.Duration
}

// App is a running proxy.
type App struct {
	log  *slog.Logger
	ver  string
	cfg  *config.Config
	mgr  *upstream.Manager
	prx  *proxy.Proxy
	stop *audit.Store
	reg  *auth.Registry

	// cfgMu guards the config pointer, the credentials derived from it and the
	// file stamp. A reload holds it for the whole swap, so two reloads cannot
	// interleave and leave the manager running one configuration while the
	// proxy describes another.
	cfgMu sync.Mutex

	// saveMu serialises writes to the config file, so two editors cannot
	// interleave a read-modify-write and lose one of the changes.
	saveMu sync.Mutex

	// stamp identifies the config file contents already applied, so the
	// watcher can tell an operator's edit from the daemon's own write.
	stamp string

	skipEnumeration bool

	// ready is closed once the first enumeration pass has finished.
	ready     chan struct{}
	readyOnce sync.Once

	handlerOnce sync.Once
	handler     http.Handler

	resourceStop context.CancelFunc
	wg           sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
}

// New builds an App from a validated config.
func New(cfg *config.Config, opts Options) (*App, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	ver := opts.Version
	if ver == "" {
		ver = "dev"
	}

	app := &App{
		log:             log,
		ver:             ver,
		cfg:             cfg,
		reg:             auth.NewRegistry(cfg.Server.DisableAuth),
		skipEnumeration: opts.SkipEnumeration,
		ready:           make(chan struct{}),
	}

	// Audit log. A failure here is reported but does not stop the proxy: a
	// read-only data directory should not take tool serving down.
	auditRecorder := audit.Recorder(audit.NopRecorder{})
	if opts.AuditPath != "" {
		store, err := audit.Open(opts.AuditPath)
		if err != nil {
			log.Warn("audit log disabled", "error", err, "path", opts.AuditPath)
		} else {
			app.stop = store
			auditRecorder = store
		}
	}

	app.mgr = upstream.NewManager(upstream.Options{
		Logger:       log,
		Resolver:     secrets.New(),
		ReapInterval: opts.ReapInterval,
		// Any change to a server's tool list must reach the aggregate, so the
		// manager calls back here and a single resync handles it.
		OnToolsChanged: func(key string) {
			log.Debug("upstream tool list changed", "server", key)
			app.Resync()
		},
	})

	app.prx = proxy.New(proxy.Options{
		Manager:  app.mgr,
		Defaults: cfg.Defaults,
		Logger:   log,
		Version:  ver,
		Registry: app.reg,
		Recorder: auditRecorder,
		// Hold the first tools/list until the upstreams have been enumerated,
		// so a harness that connects during start-up still gets the full list
		// rather than a partial one it may cache indefinitely.
		Ready: app.WaitReady,
	})

	app.applyAuth()
	app.mgr.SetServers(defsFromConfig(cfg))
	app.Resync()
	app.stamp = configStamp(cfg.Path())

	return app, nil
}

// applyAuth pushes the current credentials into the registry. It assumes the
// caller holds cfgMu.
func (a *App) applyAuth() {
	a.reg.Set(a.cfg.Clients, a.cfg.Server.AdminTokenHash, a.cfg.Server.DisableAuth)
}

// defsFromConfig turns the config's servers into manager definitions.
func defsFromConfig(cfg *config.Config) map[string]upstream.Def {
	defs := make(map[string]upstream.Def, len(cfg.MCPServers))
	for key, up := range cfg.MCPServers {
		defs[key] = upstream.Def{Key: key, Cfg: up, Defaults: cfg.Defaults}
	}
	return defs
}

// Config returns the live config. Callers must not mutate it.
func (a *App) Config() *config.Config {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	return a.cfg
}

// configStamp identifies a config file's contents, so the daemon can tell its
// own write from an operator's edit.
func configStamp(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", info.ModTime().UnixNano(), info.Size())
}

// noteStamp records the config file as applied.
func (a *App) noteStamp(path string) {
	stamp := configStamp(path)
	a.cfgMu.Lock()
	a.stamp = stamp
	a.cfgMu.Unlock()
}

// Manager returns the upstream manager.
func (a *App) Manager() *upstream.Manager { return a.mgr }

// Proxy returns the aggregated MCP server.
func (a *App) Proxy() *proxy.Proxy { return a.prx }

// Registry returns the credential registry.
func (a *App) Registry() *auth.Registry { return a.reg }

// Audit returns the audit store, or nil when auditing is disabled.
func (a *App) Audit() *audit.Store { return a.stop }

// Index returns the current exposed tool index.
func (a *App) Index() *aggregate.Index { return a.prx.Index() }

// Version returns the reported version string.
func (a *App) Version() string { return a.ver }

// Resync rebuilds the exposed tool set from the current upstream snapshots. It
// is the single place the aggregate is recomputed, so a config change, a new
// connection and a tool-list notification all converge here.
func (a *App) Resync() *aggregate.Index {
	return a.prx.Sync(a.mgr.Snapshots())
}

// Start begins enumeration and the background maintenance loop.
//
// Enumeration runs in the background so the HTTP listener can come up
// immediately, and WaitReady reports when it has finished. Anything that needs
// a complete tool list waits on that rather than on a timer.
func (a *App) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	a.resourceStop = cancel

	if !a.skipEnumeration {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.mgr.Enumerate(ctx)
			a.Resync()
			a.markReady()
		}()
	} else {
		// Tests that suppress enumeration want readers to proceed at once.
		a.markReady()
	}

	a.wg.Add(1)
	go a.maintain(ctx)

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.watchConfig(ctx)
	}()
}

// configPollInterval is how often the config file is checked for an out-of-band
// edit. Frequent enough to feel immediate, cheap enough to ignore.
const configPollInterval = 2 * time.Second

// watchConfig reloads the configuration when the file changes underneath the
// daemon.
//
// Without this, adding a server with the command line while the daemon runs
// would appear to do nothing. The API path reloads synchronously, so this only
// covers edits made outside it.
func (a *App) watchConfig(ctx context.Context) {
	ticker := time.NewTicker(configPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.pollConfig(ctx)
		}
	}
}

// pollConfig reloads the config when the file no longer matches what is
// running.
func (a *App) pollConfig(ctx context.Context) {
	a.cfgMu.Lock()
	path := a.cfg.Path()
	current := a.stamp
	a.cfgMu.Unlock()

	if path == "" {
		return
	}
	observed := configStamp(path)
	if observed == "" || observed == current {
		return
	}

	cfg, err := config.Load(path)
	if err != nil {
		// Claim the stamp so a broken edit is reported once rather than on
		// every tick, and keep serving the last good configuration.
		a.cfgMu.Lock()
		a.stamp = observed
		a.cfgMu.Unlock()
		a.log.Error("config file changed but is not usable; keeping the running configuration",
			"path", path, "error", err)
		return
	}

	a.log.Info("config file changed; reloading", "path", path)

	reloadCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := a.Reload(reloadCtx, cfg); err != nil {
		a.log.Error("reload failed", "error", err)
	}
}

// markReady signals that the first enumeration pass has finished.
func (a *App) markReady() {
	a.readyOnce.Do(func() { close(a.ready) })
}

// WaitReady blocks until the first enumeration pass has finished, or ctx
// expires. A harness's first tools/list waits on this, so it never sees a
// half-populated tool list just because it connected during start-up.
func (a *App) WaitReady(ctx context.Context) error {
	select {
	case <-a.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// maintain prunes the audit log on a schedule.
func (a *App) maintain(ctx context.Context) {
	defer a.wg.Done()

	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.pruneAudit(ctx)
		}
	}
}

// pruneAudit drops audit rows past the retention window.
func (a *App) pruneAudit(ctx context.Context) {
	if a.stop == nil {
		return
	}
	days := a.cfg.Server.AuditRetentionDays
	if days <= 0 {
		days = 30
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	removed, err := a.stop.Prune(ctx, cutoff)
	if err != nil {
		a.log.Warn("audit prune failed", "error", err)
		return
	}
	if removed > 0 {
		a.log.Info("audit log pruned", "removed", removed, "olderThan", cutoff)
	}
}

// Reload swaps in a new configuration. Upstreams whose settings changed are
// dropped so the new settings take effect; new servers are added; removed ones
// are stopped.
//
// The swap itself is atomic from a reader's point of view. Enumeration happens
// afterwards, outside the lock, because connecting to upstreams can take a
// while and nothing needs to wait on it to see a consistent configuration.
func (a *App) Reload(ctx context.Context, cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	a.cfgMu.Lock()
	a.cfg = cfg
	a.applyAuth()
	// The namespace settings are the proxy's, not the manager's, so a reload
	// that changes them has to reach the aggregator explicitly.
	a.prx.SetDefaults(cfg.Defaults)
	a.mgr.SetServers(defsFromConfig(cfg))
	a.stamp = configStamp(cfg.Path())
	a.cfgMu.Unlock()

	a.Resync()
	a.mgr.Enumerate(ctx)
	a.Resync()
	return nil
}

// Close shuts everything down.
func (a *App) Close() error {
	a.closeOnce.Do(func() {
		if a.resourceStop != nil {
			a.resourceStop()
		}
		a.wg.Wait()
		if err := a.mgr.Close(); err != nil {
			a.closeErr = err
		}
		if a.stop != nil {
			if err := a.stop.Close(); err != nil && a.closeErr == nil {
				a.closeErr = err
			}
		}
	})
	return a.closeErr
}

// Handler returns the HTTP surface. It is built once and cached.
func (a *App) Handler() http.Handler {
	a.handlerOnce.Do(func() { a.handler = a.buildHandler() })
	return a.handler
}

// DefaultAuditPath returns the audit database location for a data directory.
func DefaultAuditPath(dataDir string) string {
	return filepath.Join(dataDir, "audit.db")
}

// ResolveDataDir picks the runtime data directory.
func ResolveDataDir(cfg *config.Config) (string, error) {
	if cfg.Server.DataDir != "" {
		return cfg.Server.DataDir, nil
	}
	dir, err := config.DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "data"), nil
}

// EnsureDataDir creates a data directory with owner-only permissions.
func EnsureDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return nil
}

// PublicURLFor derives the base URL harnesses should use from a config,
// preferring the configured value and falling back to the bind address.
func PublicURLFor(cfg *config.Config) string {
	if cfg.Server.PublicURL != "" {
		return cfg.Server.PublicURL
	}
	host := cfg.Server.Bind
	if host == "" {
		host = "127.0.0.1:8765"
	}
	// A wildcard bind is not an address anyone can connect to, so it is
	// reported as loopback and left for the operator to correct.
	if strings.HasPrefix(host, "0.0.0.0:") || strings.HasPrefix(host, "[::]:") {
		_, port, err := net.SplitHostPort(host)
		if err == nil {
			host = net.JoinHostPort("127.0.0.1", port)
		}
	}
	return "http://" + host
}

// PublicURL returns the base URL harnesses should use for this proxy.
func (a *App) PublicURL() string {
	return PublicURLFor(a.cfg)
}

// MCPEndpoint is the path harnesses connect to.
const MCPEndpoint = "/mcp"

// SSEEndpoint is the deprecated HTTP+SSE path, served only when enabled.
const SSEEndpoint = "/sse"

// AdminPrefix is the REST API path prefix.
const AdminPrefix = "/api"
