//go:build windows

package upstream

import (
	"os/exec"
	"strconv"
	"syscall"
)

// Windows has no process groups in the Unix sense and no SIGTERM. Putting the
// child in a new process group at least detaches it from Ctrl-C handling, and
// killTree uses taskkill /T to walk grandchildren, which is what actually
// matters for `npx` wrappers.
const createNewProcessGroup = 0x00000200

func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup,
	}
}

// killTree terminates pid and every descendant. taskkill is used rather than
// Process.Kill because a Node-based MCP server is normally a grandchild of the
// command we spawned, and killing only the direct child orphans it.
func killTree(pid int) {
	if pid <= 0 {
		return
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = kill.Run()
}
