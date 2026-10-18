package upstream

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shabilullah/wakil-mcp/internal/ringbuf"
)

// stderrRingSize bounds how much upstream stderr is retained for diagnostics.
const stderrRingSize = 64 * 1024

// defaultStopGrace is how long a process gets to exit on its own before it is
// killed.
const defaultStopGrace = 5 * time.Second

// Proc is a supervised child process running a stdio MCP server.
//
// It is deliberately not built on the SDK's CommandTransport: a proxy needs to
// capture stderr for the UI, and needs to kill whole process trees, which
// matters because `npx foo` leaves a `node` grandchild behind if only the
// immediate child is signalled.
type Proc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *ringbuf.Ring

	done    chan struct{}
	waitErr error

	stopped  atomic.Bool
	stopOnce sync.Once
}

// startProcess launches command with the given extra environment, merging it
// over the current process environment so PATH and friends still resolve.
func startProcess(command string, args []string, extraEnv map[string]string, dir string) (*Proc, error) {
	cmd := exec.Command(command, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(extraEnv) > 0 {
		cmd.Env = mergeEnv(os.Environ(), extraEnv)
	}

	stderr := ringbuf.New(stderrRingSize)
	cmd.Stderr = stderr

	// Use explicit pipes rather than StdoutPipe/StdinPipe. Wait closes pipes
	// created by StdoutPipe while readers may still be draining them; owning
	// both ends avoids that race entirely.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("creating stdin pipe: %w", err)
	}

	cmd.Stdout = stdoutW
	cmd.Stdin = stdinR

	setProcAttrs(cmd)

	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stdinR.Close()
		stdinW.Close()
		return nil, fmt.Errorf("starting %s: %w", command, err)
	}

	// The parent must drop the child's ends, or reads on the parent's end of
	// stdout never see EOF once the child exits.
	stdoutW.Close()
	stdinR.Close()

	p := &Proc{
		cmd:    cmd,
		stdin:  stdinW,
		stdout: stdoutR,
		stderr: stderr,
		done:   make(chan struct{}),
	}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// Stderr returns the retained stderr output.
func (p *Proc) Stderr() string { return p.stderr.String() }

// StderrLen returns how many stderr bytes have been retained.
func (p *Proc) StderrLen() int { return p.stderr.Len() }

// Pid returns the child process ID, or 0 if it never started.
func (p *Proc) Pid() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Exited reports whether the process has finished.
func (p *Proc) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// WaitErr returns the error from waiting on the process. It is only meaningful
// after Done is closed.
func (p *Proc) WaitErr() error { return p.waitErr }

// Done returns a channel closed when the process exits.
func (p *Proc) Done() <-chan struct{} { return p.done }

// Stop shuts the process down. It first closes stdin, which many MCP servers
// treat as a shutdown request, then waits for grace, then kills the process
// tree. It is safe to call more than once.
func (p *Proc) Stop(grace time.Duration) error {
	if grace <= 0 {
		grace = defaultStopGrace
	}
	var killErr error
	p.stopOnce.Do(func() {
		p.stopped.Store(true)

		// Closing stdin signals EOF; well-behaved servers exit on their own.
		_ = p.stdin.Close()
		_ = p.stdout.Close()

		select {
		case <-p.done:
			return
		case <-time.After(grace):
		}

		killTree(p.Pid())

		select {
		case <-p.done:
		case <-time.After(grace):
			killErr = fmt.Errorf("process %d did not exit after being killed", p.Pid())
		}
	})
	if killErr != nil {
		return killErr
	}
	// A non-zero exit after a deliberate stop is expected. Only surface wait
	// failures that are not ordinary process exits.
	if p.waitErr != nil && !isExitError(p.waitErr) {
		return p.waitErr
	}
	return nil
}

// Stopped reports whether Stop was called. The manager uses it to tell a
// deliberate shutdown apart from a crash.
func (p *Proc) Stopped() bool { return p.stopped.Load() }

// isExitError reports whether err is an ordinary non-zero process exit.
func isExitError(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}

// mergeEnv overlays extra on top of base, dropping any base entry whose name
// is overridden. Name comparison is case-insensitive on Windows.
func mergeEnv(base []string, extra map[string]string) []string {
	fold := runtime.GOOS == "windows"
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		if _, override := lookupFold(extra, name, fold); override {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// lookupFold finds key in m, honouring case-insensitivity when fold is set.
func lookupFold(m map[string]string, key string, fold bool) (string, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	if !fold {
		return "", false
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}
