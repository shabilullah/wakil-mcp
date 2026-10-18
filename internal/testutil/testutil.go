// Package testutil provides helpers shared by the integration tests. It is a
// normal package, not a _test.go file, so several packages can use it.
package testutil

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// ModuleRoot walks up from the working directory until it finds go.mod.
func ModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate the module root (no go.mod above the working directory)")
		}
		dir = parent
	}
}

// fixturePath caches the built fixture binary for the whole test run.
var (
	fixtureOnce sync.Once
	fixturePath string
	fixtureErr  error
)

// FixtureBinary builds cmd/mcpfixture once per test run and returns its path.
// The binary is the upstream MCP server the proxy connects to in tests.
func FixtureBinary(t *testing.T) string {
	t.Helper()
	fixtureOnce.Do(func() {
		root := ModuleRoot(t)
		dir := filepath.Join(os.TempDir(), "wakil-mcp-tests")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fixtureErr = err
			return
		}
		name := "mcpfixture"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		out := filepath.Join(dir, name)

		cmd := exec.Command("go", "build", "-o", out, "./cmd/mcpfixture")
		cmd.Dir = root
		cmd.Env = os.Environ()
		if combined, err := cmd.CombinedOutput(); err != nil {
			fixtureErr = &buildError{output: string(combined), err: err}
			return
		}
		fixturePath = out
	})
	if fixtureErr != nil {
		t.Fatalf("building the fixture binary: %v", fixtureErr)
	}
	return fixturePath
}

// buildError carries compiler output so a build failure is readable.
type buildError struct {
	output string
	err    error
}

func (e *buildError) Error() string {
	return e.err.Error() + "\n" + e.output
}

// Binary builds a command package once per test run and returns its path. It is
// how tests exercise the real executable rather than an in-process stand-in.
func Binary(t *testing.T, pkg string) string {
	t.Helper()

	binsMu.Lock()
	defer binsMu.Unlock()

	if path, ok := bins[pkg]; ok {
		return path
	}

	root := ModuleRoot(t)
	dir := filepath.Join(os.TempDir(), "wakil-mcp-tests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating the build directory: %v", err)
	}

	name := filepath.Base(pkg)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(dir, name)

	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = root
	cmd.Env = os.Environ()
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", pkg, err, combined)
	}

	bins[pkg] = out
	return out
}

var (
	binsMu sync.Mutex
	bins   = map[string]string{}
)

// WaitFor polls cond until it returns true or the timeout passes.
func WaitFor(t *testing.T, timeout time.Duration, cond func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for: %s", timeout, message)
}

// FreeAddr returns a loopback address that was free a moment ago. The port is
// released before being returned, so there is an unavoidable race with anything
// else on the machine; tests should treat a bind failure as a flake and retry.
func FreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}
