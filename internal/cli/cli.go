// Package cli implements the wakil-mcp command line.
//
// The commands fall into three groups: running the daemon, editing the config
// file, and diagnosing a deployment. Everything the web UI can do to the server
// list has a command here too, so the tool is usable over SSH and from a
// script, not only from a browser.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/version"
)

// Exit codes.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// errHelp reports that the operator asked for usage rather than a run.
var errHelp = errors.New("help requested")

// env carries the options shared by every command.
type env struct {
	configPath string
	logLevel   string
	asJSON     bool
	stdout     io.Writer
	stderr     io.Writer

	// signalContext yields the context that stops a long-running command. It
	// is a field so a test can stop "serve" without raising a real signal.
	signalContext func() (context.Context, context.CancelFunc)
}

// command is one subcommand.
type command struct {
	name  string
	short string
	usage string
	run   func(*env, []string) error
}

// commands is the dispatch table, in the order shown by help.
var commands = []command{
	{name: "serve", short: "run the proxy daemon", usage: serveUsage, run: (*env).serve},
	{name: "stdio", short: "relay a stdio harness to a running daemon", usage: stdioUsage, run: (*env).stdioCmd},
	{name: "list", short: "list configured servers", usage: listUsage, run: (*env).listCmd},
	{name: "add", short: "add or replace a server", usage: addUsage, run: (*env).addCmd},
	{name: "remove", short: "delete a server", usage: removeUsage, run: (*env).removeCmd},
	{name: "enable", short: "enable a server", usage: toggleUsage, run: (*env).enableCmd},
	{name: "disable", short: "disable a server", usage: toggleUsage, run: (*env).disableCmd},
	{name: "import", short: "import another tool's mcpServers block", usage: importUsage, run: (*env).importCmd},
	{name: "test", short: "connect to one server and report what it offers", usage: testUsage, run: (*env).testCmd},
	{name: "token", short: "manage client credentials", usage: tokenUsage, run: (*env).tokenCmd},
	{name: "doctor", short: "check a deployment for problems", usage: doctorUsage, run: (*env).doctorCmd},
	{name: "version", short: "print the build identity", usage: versionUsage, run: (*env).versionCmd},
}

// Run executes the command line and returns the process exit code.
func Run(args []string) int {
	return RunWith(args, os.Stdout, os.Stderr)
}

// RunWith is Run with explicit output streams, which is what embedding and the
// tests use.
func RunWith(args []string, stdout, stderr io.Writer) int {
	return newEnv(stdout, stderr).dispatch(args)
}

// newEnv builds a command environment.
func newEnv(stdout, stderr io.Writer) *env {
	return &env{stdout: stdout, stderr: stderr}
}

// runContext returns the context that stops a long-running command.
func (e *env) runContext() (context.Context, context.CancelFunc) {
	if e.signalContext != nil {
		return e.signalContext()
	}
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// dispatch parses the command line and runs the matching command.
func (e *env) dispatch(args []string) int {
	rest, err := e.parseGlobals(args)
	if err != nil {
		if errors.Is(err, errHelp) {
			e.printHelp()
			return ExitOK
		}
		fmt.Fprintf(e.stderr, "wakil-mcp: %v\n", err)
		return ExitUsage
	}

	if len(rest) == 0 {
		e.printHelp()
		return ExitUsage
	}

	name, args := rest[0], rest[1:]
	for _, cmd := range commands {
		if cmd.name != name {
			continue
		}
		err := cmd.run(e, args)
		if err == nil || errors.Is(err, errHelp) {
			return ExitOK
		}
		fmt.Fprintf(e.stderr, "wakil-mcp %s: %v\n", name, err)
		return ExitError
	}

	fmt.Fprintf(e.stderr, "wakil-mcp: unknown command %q\n\n", name)
	e.printHelp()
	return ExitUsage
}

// parseGlobals pulls the flags that apply to every command out of the argument
// list, leaving the command and its own flags behind.
//
// The flag package cannot do this, because global flags may appear on either
// side of the subcommand, which is how people actually type them.
func (e *env) parseGlobals(args []string) ([]string, error) {
	var rest []string

	// Once the command name has gone by, -h belongs to that command, so each
	// command can show its own flags.
	commandSeen := false

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Everything after a bare "--" belongs to the command.
		if arg == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}

		name, inline, hasInline := strings.Cut(arg, "=")

		// take fetches the value for a flag, whether it was inline or separate.
		take := func(flagName string) (string, error) {
			if hasInline {
				return inline, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", flagName)
			}
			i++
			return args[i], nil
		}

		if !strings.HasPrefix(arg, "-") && !commandSeen {
			commandSeen = true
		}

		switch {
		case name == "-h" || name == "--help" || name == "help":
			if commandSeen {
				rest = append(rest, arg)
				continue
			}
			return nil, errHelp
		case name == "-c" || name == "--config":
			value, err := take(name)
			if err != nil {
				return nil, err
			}
			e.configPath = value
		case name == "--log-level":
			value, err := take(name)
			if err != nil {
				return nil, err
			}
			e.logLevel = value
		case name == "--json":
			e.asJSON = true
		case name == "-v" || name == "--version":
			// Let the version command handle it so there is one code path.
			rest = append(rest, "version")
		default:
			rest = append(rest, arg)
		}
	}

	return rest, nil
}

// flagSet builds a flag set that reports errors rather than exiting.
func (e *env) flagSet(name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() { fmt.Fprint(e.stderr, usage) }
	return fs
}

// parse parses a command's flags, mapping -h onto a clean exit.
func (e *env) parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errHelp
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return nil
}

// parseInterspersed parses flags that may appear before, after or between
// positional arguments, and returns the positional ones.
//
// The flag package stops at the first positional argument, which would make the
// natural spelling "add github --command npx" reject --command as unexpected.
// Re-parsing what is left after each positional is the standard way around it.
func (e *env) parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string

	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, errHelp
			}
			return nil, err
		}

		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// oneValue requires exactly one positional argument.
func oneValue(positional []string, what string) (string, error) {
	if len(positional) != 1 {
		return "", fmt.Errorf("expected exactly one %s, got %d", what, len(positional))
	}
	return positional[0], nil
}

// resolvePath returns the config file to operate on.
func (e *env) resolvePath() (string, error) {
	if e.configPath != "" {
		return e.configPath, nil
	}
	return config.DefaultPath()
}

// load reads the config, with a hint when it has never been created.
func (e *env) load() (*config.Config, error) {
	path, err := e.resolvePath()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if errors.Is(err, config.ErrNotFound) {
		return nil, fmt.Errorf("no config at %s; run \"wakil-mcp serve\" once to create one", path)
	}
	return cfg, err
}

// save writes the config back with a summary, which is what makes the mutating
// commands safe to run without a confirmation prompt.
func (e *env) save(cfg *config.Config) error {
	if err := cfg.Save(); err != nil {
		return err
	}
	e.printf("saved %s\n", cfg.Path())
	return nil
}

// logger builds the process logger from a level name.
func (e *env) logger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: lvl}))
}

// printf writes to stdout.
func (e *env) printf(format string, args ...any) {
	fmt.Fprintf(e.stdout, format, args...)
}

// emit renders a value as JSON when --json was given, and otherwise calls the
// human renderer.
func (e *env) emit(v any, human func()) error {
	if !e.asJSON {
		human()
		return nil
	}
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printHelp writes the top-level usage.
func (e *env) printHelp() {
	e.printf("wakil-mcp %s\n", version.String())
	e.printf("\nOne MCP server in front of all of them.\n")
	e.printf("Add each upstream once, then point every harness at wakil-mcp.\n")
	e.printf("\nUsage:\n  wakil-mcp [global flags] <command> [flags]\n")
	e.printf("\nCommands:\n")

	width := 0
	for _, cmd := range commands {
		if len(cmd.name) > width {
			width = len(cmd.name)
		}
	}
	for _, cmd := range commands {
		e.printf("  %-*s  %s\n", width, cmd.name, cmd.short)
	}

	e.printf("\nGlobal flags:\n")
	e.printf("  -c, --config <path>     config file (default %s)\n", defaultPathHint())
	e.printf("      --log-level <level> override the configured log level\n")
	e.printf("      --json              machine-readable output where supported\n")
	e.printf("  -v, --version           print the build identity\n")
	e.printf("\nRun \"wakil-mcp <command> --help\" for the flags of one command.\n")
}

// defaultPathHint renders the default config location for help text.
func defaultPathHint() string {
	path, err := config.DefaultPath()
	if err != nil {
		return "in the user config directory"
	}
	return path
}

// versionCmd prints the build identity.
func (e *env) versionCmd([]string) error {
	if e.asJSON {
		return e.emit(map[string]string{
			"version": version.Version,
			"commit":  version.Commit,
			"date":    version.Date,
		}, func() {})
	}
	e.printf("wakil-mcp %s\n", version.String())
	return nil
}

// ---------------------------------------------------------------------------
// Flag types and output helpers
// ---------------------------------------------------------------------------

// stringList collects a flag that may be repeated.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ", ") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// keyValues collects repeated KEY=VALUE flags into a map.
//
// Both "KEY=VALUE" and "KEY: VALUE" are accepted, because HTTP headers are
// conventionally written the second way and that is how an operator types them.
type keyValues map[string]string

func (k *keyValues) String() string {
	if *k == nil {
		return ""
	}
	keys := make([]string, 0, len(*k))
	for key := range *k {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+(*k)[key])
	}
	return strings.Join(parts, ", ")
}

func (k *keyValues) Set(value string) error {
	key, val, ok := strings.Cut(value, "=")
	if !ok {
		// A colon is the other spelling, and the one headers use.
		if key, val, ok = strings.Cut(value, ":"); ok {
			val = strings.TrimSpace(val)
		}
	}
	if !ok || strings.TrimSpace(key) == "" {
		return fmt.Errorf("expected KEY=VALUE or KEY: VALUE, got %q", value)
	}
	if *k == nil {
		*k = map[string]string{}
	}
	(*k)[key] = val
	return nil
}

// duration is a flag that accepts Go duration syntax and stores config.Duration.
type duration struct {
	value *config.Duration
}

func (d duration) String() string {
	if d.value == nil {
		return ""
	}
	return d.value.String()
}

func (d duration) Set(text string) error {
	parsed, err := parseDuration(text)
	if err != nil {
		return err
	}
	*d.value = parsed
	return nil
}

// renderTable prints aligned columns, which is how every human-readable command
// reports its result.
func renderTable(w io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, header := range headers {
		widths[i] = len(header)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	line := func(cells []string) {
		parts := make([]string, len(cells))
		for i, cell := range cells {
			if i == len(cells)-1 {
				parts[i] = cell
				continue
			}
			parts[i] = cell + strings.Repeat(" ", widths[i]-len(cell))
		}
		fmt.Fprintln(w, strings.TrimRight(strings.Join(parts, "  "), " "))
	}

	line(headers)

	separators := make([]string, len(headers))
	for i := range separators {
		separators[i] = strings.Repeat("-", widths[i])
	}
	line(separators)

	for _, row := range rows {
		line(row)
	}
}

// yesNo renders a boolean for a table.
func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

// orDash renders an empty string as a dash, so table columns stay readable.
func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
