//go:build !unix

package worktree

import "os/exec"

// setupProcessGroup is a no-op on platforms without process groups.
func setupProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup is a no-op on platforms without process groups; the
// context path still relies on os/exec's WaitDelay to bound Wait.
func killProcessGroup(pid int) {}
