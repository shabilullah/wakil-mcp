//go:build !windows

package upstream

import (
	"os/exec"
	"syscall"
	"time"
)

// killGracePollInterval is how often killTree checks whether the process group
// has gone away before escalating to SIGKILL.
const killGracePollInterval = 50 * time.Millisecond

// killGracePolls bounds how long to wait for a graceful exit before SIGKILL.
const killGracePolls = 20

// setProcAttrs puts the child in its own process group so killTree can signal
// the whole group at once.
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree signals the child's process group, escalating to SIGKILL. The
// negative pid targets the group, which catches grandchildren such as the
// `node` process behind an `npx` wrapper.
func killTree(pid int) {
	if pid <= 0 {
		return
	}
	// Best effort TERM first so the server can flush and clean up.
	_ = syscall.Kill(-pid, syscall.SIGTERM)

	for range killGracePolls {
		if err := syscall.Kill(-pid, 0); err != nil {
			return // the group is gone
		}
		time.Sleep(killGracePollInterval)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
