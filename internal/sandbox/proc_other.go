//go:build !unix

package sandbox

import "os/exec"

func setupProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(pid int) {}
