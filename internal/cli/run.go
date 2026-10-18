package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/shabilullah/wakil-mcp/internal/bridge"
	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/server"
	"github.com/shabilullah/wakil-mcp/internal/version"
)

// serve runs the proxy daemon.
func (e *env) serve(args []string) error {
	fs := e.flagSet("serve", serveUsage)
	bind := fs.String("bind", "", "override server.bind from the config")
	noEnumerate := fs.Bool("no-enumerate", false, "listen without connecting to upstreams")
	if err := e.parse(fs, args); err != nil {
		return err
	}

	path, err := e.resolvePath()
	if err != nil {
		return err
	}

	// LoadOrCreate mints the first credentials, so a brand new install is
	// usable without a separate setup step.
	cfg, bootstrap, err := config.LoadOrCreate(path)
	if err != nil {
		return err
	}
	if *bind != "" {
		cfg.Server.Bind = *bind
	}

	level := cfg.Server.LogLevel
	if e.logLevel != "" {
		level = e.logLevel
	}
	log := e.logger(level)

	dataDir, err := server.ResolveDataDir(cfg)
	if err != nil {
		return err
	}
	if err := server.EnsureDataDir(dataDir); err != nil {
		return fmt.Errorf("creating the data directory: %w", err)
	}

	app, err := server.New(cfg, server.Options{
		Logger:          log,
		Version:         version.Version,
		AuditPath:       server.DefaultAuditPath(dataDir),
		SkipEnumeration: *noEnumerate,
	})
	if err != nil {
		return err
	}
	defer app.Close()

	// Bind before waiting on upstreams, so a port clash is reported in
	// milliseconds rather than after a minute of connecting.
	listener, err := net.Listen("tcp", cfg.Server.Bind)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.Server.Bind, err)
	}

	ctx, stop := e.runContext()
	defer stop()

	app.Start(ctx)

	httpSrv := &http.Server{
		Handler: app.Handler(),
		// A header timeout protects against a stuck client. There is
		// deliberately no write timeout: a tool call or a notification stream
		// legitimately stays open for a long time, and the call timeout is
		// enforced against the upstream instead.
		ReadHeaderTimeout: 15 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		err := httpSrv.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	e.printBanner(app, listener)

	if bootstrap != nil {
		e.printBootstrap(cfg, bootstrap)
	}

	readyCtx, cancelReady := context.WithTimeout(ctx, 3*time.Minute)
	err = app.WaitReady(readyCtx)
	cancelReady()
	if err != nil {
		e.printf("upstreams   still starting; see the log for details\n\n")
	} else {
		e.summariseUpstreams(app)
	}

	select {
	case <-ctx.Done():
		e.printf("\nshutting down\n")
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serving http: %w", err)
		}
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelShutdown()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		e.printf("forced shutdown: %v\n", err)
	}
	return app.Close()
}

// printBanner reports where the daemon is reachable.
func (e *env) printBanner(app *server.App, listener net.Listener) {
	base := app.PublicURL()

	e.printf("wakil-mcp %s\n", version.String())
	e.printf("config      %s\n", app.Config().Path())
	e.printf("listening   %s\n", listener.Addr())
	e.printf("mcp         %s%s\n", base, server.MCPEndpoint)
	if app.Config().Server.LegacySSEEnabled() {
		e.printf("sse         %s%s  (deprecated transport)\n", base, server.SSEEndpoint)
	}
	e.printf("web ui      %s/\n", base)

	if app.Config().Server.DisableAuth {
		e.printf("auth        DISABLED: anyone who can reach this port can call every tool\n")
		if !isLoopbackBind(app.Config().Server.Bind) {
			e.printf("            and it is not bound to loopback\n")
		}
	}
	e.printf("\n")
}

// printBootstrap shows the credentials minted for a fresh config. They are
// printed once because only hashes are stored.
func (e *env) printBootstrap(cfg *config.Config, boot *config.Bootstrap) {
	e.printf("A new config was created at %s.\n\n", cfg.Path())
	e.printf("  client token for %q, to put in your harnesses:\n", boot.ClientName)
	e.printf("    %s\n\n", boot.ClientToken)
	e.printf("  admin token, for the web UI and the REST API:\n")
	e.printf("    %s\n\n", boot.AdminToken)
	e.printf("Both are shown once; only hashes are stored. Save them now.\n")
	e.printf("Replace either later with \"wakil-mcp token create <name>\" or \"wakil-mcp token admin\".\n\n")
}

// summariseUpstreams reports what the upstreams contributed, and names the ones
// that did not work, which is the first thing an operator needs to know.
func (e *env) summariseUpstreams(app *server.App) {
	index := app.Index()
	snapshots := app.Manager().Snapshots()

	enabled, failed := 0, 0
	for _, s := range snapshots {
		if !s.Enabled {
			continue
		}
		enabled++
		if s.ToolCount == 0 {
			failed++
		}
	}

	e.printf("upstreams   %d enabled, %d contributing tools\n", enabled, enabled-failed)
	e.printf("tools       %d exposed\n", index.Len())

	for _, s := range snapshots {
		if s.ToolCount > 0 || s.LastError == "" {
			continue
		}
		e.printf("  ! %-16s %s\n", s.Key, s.LastError)
	}

	if collisions := index.Collisions(); len(collisions) > 0 {
		e.printf("  ! %d tool names collided and were renamed; see the UI for the mapping\n",
			len(collisions))
	}
	e.printf("\n")
}

// stdioCmd runs the bridge that lets a stdio-only harness reach the daemon.
func (e *env) stdioCmd(args []string) error {
	fs := e.flagSet("stdio", stdioUsage)
	url := fs.String("url", "", "daemon MCP endpoint")
	token := fs.String("token", "", "bearer token (defaults to $WAKIL_TOKEN)")
	pinnedVersion := fs.String("protocol-version", "", "pin a protocol revision")
	poll := fs.Duration("poll", 15*time.Second, "how often to check for tool-list changes")
	if err := e.parse(fs, args); err != nil {
		return err
	}

	daemonURL := *url
	if daemonURL == "" {
		// A harness configured with nothing but the command still has to find
		// the daemon, so fall back to the config's own bind address.
		cfg, err := e.load()
		if err != nil {
			return fmt.Errorf("no --url given and the config could not be read: %w", err)
		}
		daemonURL = server.PublicURLFor(cfg) + server.MCPEndpoint
	}

	bearer := *token
	if bearer == "" {
		bearer = os.Getenv("WAKIL_TOKEN")
	}

	level := e.logLevel
	if level == "" {
		// Quiet by default: this process sits between a harness and the
		// daemon, and its diagnostics go to stderr where they may still be
		// read by a human.
		level = "warn"
	}

	ctx, stop := e.runContext()
	defer stop()

	return bridge.Run(ctx, bridge.Options{
		URL:             daemonURL,
		Token:           bearer,
		ProtocolVersion: *pinnedVersion,
		PollEvery:       *poll,
		Logger:          e.logger(level),
		Out:             e.stdout,
	})
}
