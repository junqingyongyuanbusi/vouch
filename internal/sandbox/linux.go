//go:build linux

package sandbox

import (
	"context"
	"os/exec"
)

// buildCommand applies a network namespace via `unshare -rn` when available.
// `-r` maps the caller to root inside a new user namespace so the loopback
// interface can be raised without privileges; loopback is brought up
// explicitly because a fresh namespace starts with lo DOWN, and localhost
// integration tests are the norm (see the package comment).
//
// Raising lo needs a userspace tool (`ip` from iproute2 or `ifconfig` from
// net-tools). With neither, a namespace would break the "loopback allowed"
// policy — every localhost probe would die with connection refused, a
// sandbox-induced failure the command did not cause — so the run stays
// unisolated and says so instead. Missing tools are the norm on slim
// containers, not an exotic edge.
func buildCommand(ctx context.Context, workdir, command string, policy Policy) (*exec.Cmd, bool, string) {
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		return shCommand(ctx, workdir, command), false, "unshare not available; ran unisolated (best-effort)"
	}
	if policy.NeedsNetwork {
		// Namespace without network isolation would change nothing; run direct.
		return shCommand(ctx, workdir, command), false, "needs_network=true: P1 runs unisolated (allowlist deferred to P3)"
	}
	loUp, ok := loopbackUpCommand()
	if !ok {
		return shCommand(ctx, workdir, command), false, "no ip/ifconfig to raise loopback; ran unisolated (localhost must stay reachable)"
	}
	wrapped := loUp + "; " + command
	cmd := exec.CommandContext(ctx, unshare, "-rn", "sh", "-c", wrapped)
	cmd.Dir = workdir
	return cmd, true, "network namespace: remote denied, loopback up"
}

// loopbackUpCommand picks the first available tool for raising lo. Its
// stderr is silenced: a failed raise must not pollute the command's stderr,
// which probes parse (a stray "not found" line reads as a missing runner).
// Availability is probed with LookPath on the host — the namespace shares
// the filesystem, so what the host sees, the command can execute.
func loopbackUpCommand() (string, bool) {
	if _, err := exec.LookPath("ip"); err == nil {
		return "ip link set lo up 2>/dev/null", true
	}
	if _, err := exec.LookPath("ifconfig"); err == nil {
		return "ifconfig lo up 2>/dev/null", true
	}
	return "", false
}
