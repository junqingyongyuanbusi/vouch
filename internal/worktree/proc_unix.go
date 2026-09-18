//go:build unix

package worktree

import (
	"os/exec"
	"syscall"
)

// setupProcessGroup puts the command in its own process group so the whole
// tree can be killed on cancellation. Without this, killing a helper shell
// leaves its grandchildren (git hooks, cp, sleep) running and holding the
// captured pipes open, so Wait blocks far past the caller's budget.
func setupProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup kills the whole group led by pid. The pid is read from
// exec.Cmd only while os/exec owns it (inside Cancel, before Wait returns).
func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
