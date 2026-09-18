//go:build !darwin && !linux

package sandbox

import (
	"context"
	"os/exec"
)

// buildCommand falls back to an unisolated run on unsupported platforms.
func buildCommand(ctx context.Context, workdir, command string, policy Policy) (*exec.Cmd, bool, string) {
	return shCommand(ctx, workdir, command), false, "unsupported platform; ran unisolated (best-effort)"
}
