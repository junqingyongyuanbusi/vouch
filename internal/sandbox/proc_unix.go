//go:build unix

package sandbox

import (
	"os/exec"
	"syscall"
)

// setupProcessGroup puts the command in its own process group so the whole
// tree can be killed on timeout. Without this, killing `sh -c` leaves the
// grandchildren (npm → node → ava/tsc) running and holding the pipes, so
// cmd.Wait() blocks far past the budget.
func setupProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup kills the whole group led by pid. The pid is captured after
// Start so no other goroutine touches exec.Cmd state concurrently.
func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
