//go:build linux

package sandbox

import (
	"context"
	"os/exec"
)

// buildCommand applies a network namespace via `unshare -rn` when available.
// `-r` maps the caller to root inside a new user namespace so `ip link set lo up`
// works without privileges; loopback is brought up explicitly so localhost
// integration tests keep working while remote egress stays blocked.
func buildCommand(ctx context.Context, workdir, command string, policy Policy) (*exec.Cmd, bool, string) {
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		return shCommand(ctx, workdir, command), false, "unshare not available; ran unisolated (best-effort)"
	}
	if policy.NeedsNetwork {
		// Namespace without network isolation would change nothing; run direct.
		return shCommand(ctx, workdir, command), false, "needs_network=true: P1 runs unisolated (allowlist deferred to P3)"
	}
	wrapped := "ip link set lo up 2>/dev/null; " + command
	cmd := exec.CommandContext(ctx, unshare, "-rn", "sh", "-c", wrapped)
	cmd.Dir = workdir
	return cmd, true, "network namespace: remote denied, loopback up"
}
