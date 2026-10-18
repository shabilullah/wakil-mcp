package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shabilullah/wakil-mcp/internal/audit"
	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/server"
	"github.com/shabilullah/wakil-mcp/internal/upstream"
	"github.com/shabilullah/wakil-mcp/internal/version"
)

// Check statuses.
const (
	statusOK      = "ok"
	statusWarn    = "warn"
	statusFail    = "fail"
	statusSkipped = "skipped"
)

// check is one environment finding.
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// serverReport is the result of loading one configured server.
type serverReport struct {
	Key             string   `json:"key"`
	Type            string   `json:"type"`
	Target          string   `json:"target"`
	Status          string   `json:"status"`
	ProtocolVersion string   `json:"protocolVersion,omitempty"`
	Legacy          bool     `json:"legacy,omitempty"`
	Tools           int      `json:"tools"`
	ToolNames       []string `json:"toolNames,omitempty"`
	Detail          string   `json:"detail,omitempty"`
}

// doctorReport is the whole diagnosis.
type doctorReport struct {
	Version    string         `json:"version"`
	ConfigPath string         `json:"configPath"`
	Checks     []check        `json:"checks"`
	Servers    []serverReport `json:"servers"`
	Problems   int            `json:"problems"`
	Warnings   int            `json:"warnings"`
}

// doctorCmd checks a deployment and reports what is wrong with it.
//
// The point is to answer "why does my harness see no tools" without reading a
// log. Environment problems and per-server connection results are reported
// together, because in practice they are tangled.
func (e *env) doctorCmd(args []string) error {
	fs := e.flagSet("doctor", doctorUsage)
	skipServers := fs.Bool("skip-servers", false, "do not connect to the servers")
	timeout := fs.Duration("timeout", 30*time.Second, "per-server connection timeout")
	if err := e.parse(fs, args); err != nil {
		return err
	}

	report := doctorReport{Version: version.Version}

	path, err := e.resolvePath()
	if err != nil {
		return err
	}
	report.ConfigPath = path

	cfg, err := config.Load(path)
	if err != nil {
		report.Checks = append(report.Checks, check{
			Name:   "config file",
			Status: statusFail,
			Detail: err.Error(),
		})
		report.Problems = 1
		e.printDoctor(report)
		return fmt.Errorf("%d problem found", report.Problems)
	}
	report.Checks = append(report.Checks, check{
		Name:   "config file",
		Status: statusOK,
		Detail: path,
	})

	report.Checks = append(report.Checks, e.checkDataDir(cfg)...)
	report.Checks = append(report.Checks, e.checkBind(cfg)...)
	report.Checks = append(report.Checks, e.checkCredentials(cfg)...)
	report.Checks = append(report.Checks, e.checkTransport(cfg)...)

	if !*skipServers {
		report.Servers = e.checkServers(cfg, *timeout)
	}

	for _, c := range report.Checks {
		switch c.Status {
		case statusFail:
			report.Problems++
		case statusWarn:
			report.Warnings++
		}
	}
	for _, s := range report.Servers {
		switch s.Status {
		case statusFail:
			report.Problems++
		case statusWarn:
			report.Warnings++
		}
	}

	e.printDoctor(report)

	if report.Problems > 0 {
		return fmt.Errorf("%s found", plural(report.Problems, "problem"))
	}
	return nil
}

// checkDataDir verifies the runtime directory is usable.
func (e *env) checkDataDir(cfg *config.Config) []check {
	results := []check{{Name: "data directory"}}

	dir, err := server.ResolveDataDir(cfg)
	if err != nil {
		results[0].Status = statusFail
		results[0].Detail = err.Error()
		return results
	}
	if err := server.EnsureDataDir(dir); err != nil {
		results[0].Status = statusFail
		results[0].Detail = fmt.Sprintf("%s: %v", dir, err)
		return results
	}

	// Creating a file is the only honest test of writability.
	probe, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		results[0].Status = statusFail
		results[0].Detail = fmt.Sprintf("%s is not writable: %v", dir, err)
		return results
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)

	results[0].Status = statusOK
	results[0].Detail = dir

	// The audit database is where the call history lives, so a failure to
	// open it is worth knowing about even though the proxy tolerates it.
	auditPath := server.DefaultAuditPath(dir)
	store, err := audit.Open(auditPath)
	if err != nil {
		results = append(results, check{
			Name:   "audit database",
			Status: statusWarn,
			Detail: fmt.Sprintf("%s could not be opened, so calls will not be recorded: %v", auditPath, err),
		})
		return results
	}
	store.Close()
	results = append(results, check{
		Name:   "audit database",
		Status: statusOK,
		Detail: auditPath,
	})
	return results
}

// checkBind verifies the listen address is usable.
func (e *env) checkBind(cfg *config.Config) []check {
	result := check{Name: "bind address"}

	if cfg.Server.Bind == "" {
		result.Status = statusFail
		result.Detail = "server.bind is empty"
		return []check{result}
	}

	listener, err := net.Listen("tcp", cfg.Server.Bind)
	if err != nil {
		if strings.Contains(err.Error(), "address already in use") ||
			strings.Contains(err.Error(), "Only one usage of each socket address") {
			// The usual cause is the daemon itself, which is not a problem.
			result.Status = statusWarn
			result.Detail = fmt.Sprintf("%s is already in use; the daemon may be running", cfg.Server.Bind)
			return []check{result}
		}
		result.Status = statusFail
		result.Detail = fmt.Sprintf("%s: %v", cfg.Server.Bind, err)
		return []check{result}
	}
	_ = listener.Close()

	result.Status = statusOK
	result.Detail = cfg.Server.Bind
	if !isLoopbackBind(cfg.Server.Bind) && cfg.Server.DisableAuth {
		result.Status = statusWarn
		result.Detail = fmt.Sprintf("%s is reachable from the network and authentication is off", cfg.Server.Bind)
	}
	return []check{result}
}

// checkCredentials verifies the tokens a deployment needs.
func (e *env) checkCredentials(cfg *config.Config) []check {
	var results []check

	if cfg.Server.DisableAuth {
		return []check{{
			Name:   "credentials",
			Status: statusWarn,
			Detail: "authentication is disabled; anyone who can reach the port can call every tool",
		}}
	}

	if cfg.Server.AdminTokenHash == "" {
		results = append(results, check{
			Name:   "admin token",
			Status: statusWarn,
			Detail: "not set, so the web UI and REST API are closed; create one with \"wakil-mcp token admin\"",
		})
	} else {
		results = append(results, check{Name: "admin token", Status: statusOK, Detail: "configured"})
	}

	active := 0
	for _, client := range cfg.Clients {
		if !client.Disabled && client.TokenHash != "" {
			active++
		}
	}
	switch {
	case active == 0:
		results = append(results, check{
			Name:   "client tokens",
			Status: statusFail,
			Detail: "no usable client, so every harness request will be rejected",
		})
	case active == 1:
		results = append(results, check{
			Name:   "client tokens",
			Status: statusWarn,
			Detail: "only one client; give each harness its own token so one can be revoked alone",
		})
	default:
		results = append(results, check{
			Name:   "client tokens",
			Status: statusOK,
			Detail: plural(active, "active client"),
		})
	}

	return results
}

// checkTransport reports the transport choices, which explain most
// harness-specific surprises.
func (e *env) checkTransport(cfg *config.Config) []check {
	detail := "session-less; required for protocol 2026-07-28"
	if !cfg.Server.HTTPStateless() {
		detail = "sessions enabled, so protocol 2026-07-28 is refused; older revisions still work"
	}
	results := []check{{Name: "http transport", Status: statusOK, Detail: detail}}

	if cfg.Server.LegacySSEEnabled() {
		results = append(results, check{
			Name:   "legacy SSE",
			Status: statusOK,
			Detail: "mounted at " + server.SSEEndpoint + " for clients that predate Streamable HTTP",
		})
	}

	return results
}

// checkServers loads every enabled server and records what it offered.
func (e *env) checkServers(cfg *config.Config, timeout time.Duration) []serverReport {
	keys := sortedKeys(cfg.MCPServers)

	manager := upstream.NewManager(upstream.Options{Logger: e.logger("error")})
	defer manager.Close()

	reports := make([]serverReport, len(keys))
	var wg sync.WaitGroup

	// Concurrently, because one unreachable server should not add its timeout
	// to every other server's diagnosis.
	for i, key := range keys {
		up := cfg.MCPServers[key]
		if up == nil {
			reports[i] = serverReport{Key: key, Status: statusSkipped, Detail: "no definition"}
			continue
		}

		report := serverReport{
			Key:    key,
			Type:   orDash(up.EffectiveType()),
			Target: describeTarget(up),
		}
		if !up.IsEnabled() {
			report.Status = statusSkipped
			report.Detail = "disabled"
			reports[i] = report
			continue
		}

		wg.Add(1)
		go func(slot int, key string, up *config.Upstream, report serverReport) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			info, err := manager.Test(ctx, upstream.Def{Key: key, Cfg: up, Defaults: cfg.Defaults})
			if err != nil {
				report.Status = statusFail
				report.Detail = err.Error()
				reports[slot] = report
				return
			}

			report.Status = statusOK
			report.ProtocolVersion = info.ProtocolVersion
			report.Legacy = info.Legacy
			report.Tools = len(info.Tools)

			preview := previewIndex(cfg.Defaults, key, up, info.Tools)

			names := make([]string, 0, preview.Len())
			for _, entry := range preview.Entries() {
				names = append(names, entry.Exposed)
			}
			report.ToolNames = names

			if collisions := preview.Collisions(); len(collisions) > 0 {
				report.Detail = fmt.Sprintf("%s renamed after name collisions", plural(len(collisions), "tool"))
				report.Status = statusWarn
			}
			if len(names) == 0 {
				report.Status = statusWarn
				report.Detail = "connected, but it offers no tools"
			}
			reports[slot] = report
		}(i, key, up, report)
	}

	wg.Wait()
	return reports
}

// printDoctor renders the report for a human.
func (e *env) printDoctor(report doctorReport) {
	if e.asJSON {
		_ = e.emit(report, func() {})
		return
	}

	e.printf("wakil-mcp %s\n\n", report.Version)
	e.printf("Config: %s\n\n", report.ConfigPath)

	e.printf("Environment\n")
	rows := make([][]string, 0, len(report.Checks))
	for _, c := range report.Checks {
		rows = append(rows, []string{c.Status, c.Name, c.Detail})
	}
	renderTable(e.stdout, []string{"", "CHECK", "DETAIL"}, rows)

	if len(report.Servers) == 0 {
		e.printf("\nServers were skipped.\n")
		return
	}

	e.printf("\nServers\n")
	rows = rows[:0]
	for _, s := range report.Servers {
		detail := s.Detail
		if detail == "" {
			detail = fmt.Sprintf("%s, %s", orDash(s.ProtocolVersion), plural(s.Tools, "tool"))
		}
		rows = append(rows, []string{s.Status, s.Key, s.Type, detail})
	}
	renderTable(e.stdout, []string{"", "KEY", "TYPE", "DETAIL"}, rows)

	total := 0
	for _, s := range report.Servers {
		total += s.Tools
	}
	e.printf("\n%s contributed by %s\n", plural(total, "tool"), plural(len(report.Servers), "server"))

	switch {
	case report.Problems > 0:
		e.printf("\n%s and %s.\n", plural(report.Problems, "problem"), plural(report.Warnings, "warning"))
	case report.Warnings > 0:
		e.printf("\nNo problems, %s.\n", plural(report.Warnings, "warning"))
	default:
		e.printf("\nEverything checked out.\n")
	}
}
