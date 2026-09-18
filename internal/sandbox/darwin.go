//go:build darwin

package sandbox

import (
	"context"
	"os/exec"
)

// buildCommand applies macOS sandbox-exec when available.
//
// Verified profile syntax (sbpl on macOS 14/15): `(allow network* (local))` is
// rejected by the parser; the working loopback form is
// `(allow network* (remote ip "localhost:*"))`.
//
// sandbox-exec is deprecated by Apple but still present; when missing we run
// direct and report Sandboxed=false (best-effort, documented).
func buildCommand(ctx context.Context, workdir, command string, policy Policy) (*exec.Cmd, bool, string) {
	path, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return shCommand(ctx, workdir, command), false, "sandbox-exec not available; ran unisolated (best-effort)"
	}
	profile := "(version 1)\n(allow default)\n" + networkRules(policy)
	cmd := exec.CommandContext(ctx, path, "-p", profile, "sh", "-c", command)
	cmd.Dir = workdir
	reason := "network denied, loopback allowed"
	if policy.NeedsNetwork {
		reason = "network allowed (needs_network=true; per-host allowlist deferred to P3)"
	}
	return cmd, true, reason
}

func networkRules(policy Policy) string {
	if policy.NeedsNetwork {
		return "(allow network*)\n"
	}
	return "(deny network*)\n(allow network* (remote ip \"localhost:*\"))\n"
}
