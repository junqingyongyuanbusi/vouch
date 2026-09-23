// Package sandbox — P1.2 — minimal process isolation per PLAN-V2 §7.2.
//
// Default: network egress denied, loopback (127.0.0.1 / ::1) allowed —
// local integration tests that start localhost servers are the norm; blocking
// loopback would make most projects inconclusive.
//
// Platform support is best-effort and documented as such:
//   - darwin: sandbox-exec (deprecated by Apple) when available, else run direct.
//   - linux:  unshare -rn network namespace when available, else run direct.
//
// When a probe declares needs_network, P1 allows network (allowlist enforcement
// is deferred to P3); the Result records exactly what was enforced.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Policy describes the isolation intent for one command.
type Policy struct {
	// NeedsNetwork means the probe declared network access. P1: allow remote
	// (best-effort; per-host allowlist enforcement is P3).
	NeedsNetwork bool
	// AllowHosts is recorded for Result but not enforced in P1.
	AllowHosts []string
	// Timeout bounds the whole command; zero means no extra bound.
	Timeout time.Duration
}

// Result is the outcome of a sandboxed command.
type Result struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	TimedOut  bool   // true when the command was killed by the policy deadline (not a real failure)
	Sandboxed bool   // true when an OS isolation mechanism was actually applied
	Reason    string // why not sandboxed / what policy was applied
}

// Run executes command (via sh -c) in workdir under the given policy.
//
// Two distinct failure modes are kept apart:
//   - the command itself failed → Result with non-zero ExitCode, nil error
//   - the isolation launcher failed (e.g. unshare not permitted) → retried
//     without isolation, Sandboxed=false, Reason explains the fallback.
//
// This matters for the gate: a sandbox that cannot start must never be
// mistaken for a failing test (which would surface as a false BROKEN).
func Run(ctx context.Context, workdir, command string, policy Policy) (Result, error) {
	if policy.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, policy.Timeout)
		defer cancel()
	}

	res, launchErr := runOnce(ctx, workdir, command, policy)
	if launchErr == nil {
		return res, nil
	}
	// Isolation launcher failed — fall back to a direct run and say so.
	direct := exec.CommandContext(ctx, "sh", "-c", command)
	direct.Dir = workdir
	direct.Env = withNodeBin(workdir)
	var directOut, directErr bytes.Buffer
	direct.Stdout = &directOut
	direct.Stderr = &directErr
	runErr := runWatched(ctx, direct)
	fallback := Result{
		Stdout:    directOut.String(),
		Stderr:    directErr.String(),
		Sandboxed: false,
		Reason:    "isolation unavailable (" + launchErr.Error() + "); ran unisolated (best-effort)",
	}
	timedOut, exitCode, fatal := classify(ctx, direct, runErr)
	if fatal != nil {
		return fallback, fmt.Errorf("sandbox fallback run: %w", fatal)
	}
	fallback.TimedOut = timedOut
	fallback.ExitCode = exitCode
	return fallback, nil
}

// runOnce executes with isolation; returns launchErr only when the isolation
// launcher itself could not start (not when the wrapped command failed).
func runOnce(ctx context.Context, workdir, command string, policy Policy) (Result, error) {
	cmd, sandboxed, reason := buildCommand(ctx, workdir, command, policy)
	cmd.Env = withNodeBin(workdir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := runWatched(ctx, cmd)
	res := Result{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Sandboxed: sandboxed,
		Reason:    reason,
	}
	timedOut, exitCode, fatal := classify(ctx, cmd, err)
	if fatal != nil {
		return res, fatal
	}
	res.ExitCode = exitCode
	res.TimedOut = timedOut
	// ErrWaitDelay is already classified as a completed run above.
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		return res, nil
	}
	// Launcher-level failure markers (as opposed to the wrapped command failing).
	if sandboxed && looksLikeLauncherFailure(stderr.String()) {
		return res, fmt.Errorf("launcher failed: %s", firstLine(stderr.String()))
	}
	return res, nil
}

// classify maps a runWatched error onto (timedOut, exitCode, fatal):
//
//	fatal != nil  → the sandbox itself failed and Run may fall back
//	ErrWaitDelay  → the command finished; orphaned grandchildren just held the
//	                pipes open. Treating this as a launcher failure would rerun
//	                the whole command unisolated (double execution + no sandbox).
func classify(ctx context.Context, cmd *exec.Cmd, err error) (bool, int, error) {
	switch {
	case err == nil:
		return false, 0, nil
	case errors.Is(err, exec.ErrWaitDelay):
		if ctx.Err() != nil {
			return true, -1, nil
		}
		// os/exec only reports ErrWaitDelay when the command "otherwise exited
		// with a successful status".
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0 {
			return false, cmd.ProcessState.ExitCode(), nil
		}
		return false, 0, nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		// os/exec can report a bare context error (Cancel returning nil makes
		// watchCtx surface ctx.Err()) or a Start failure. Whatever the shape: if
		// the context ended, the run was cut off by the budget — rerunning it
		// unisolated would double the side effects and bypass the sandbox.
		if ctx.Err() != nil {
			return true, -1, nil
		}
		return false, 0, fmt.Errorf("launch: %w", err)
	}
	if ctx.Err() != nil || ee.ExitCode() < 0 {
		// Killed by the deadline/policy: this is "could not measure", not a
		// failing command. Callers must not read it as a regression.
		return true, -1, nil
	}
	return false, ee.ExitCode(), nil
}

func looksLikeLauncherFailure(stderr string) bool {
	// The launcher's own failure is always its first stderr line, prefixed
	// with the executable name (`unshare: ...`, `sandbox-exec: ...`). Matching
	// that prefix — instead of enumerating message variants — survives
	// wording changes across util-linux versions: runners that restrict
	// unprivileged user namespaces turned "unshare failed" into
	// "write failed /proc/self/uid_map", which the old marker list missed and
	// misreported as a failing command (eight red tests on ubuntu-24.04 CI).
	//
	// A generic "Operation not permitted" mid-stream is the wrapped command
	// itself being denied network access (that is the sandbox working);
	// retrying it unisolated would re-run the command and silently open the
	// network, so it must not match. The launcher prefix is only trusted on
	// the first line: when the launcher dies, it is the only writer of stderr
	// (the wrapped command never started).
	if first := firstLine(stderr); strings.HasPrefix(first, "unshare:") ||
		strings.HasPrefix(first, "sandbox-exec:") {
		return true
	}
	// Some sandbox-exec failures surface the library marker without the
	// executable prefix.
	return strings.Contains(stderr, "sandbox_init")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// shCommand builds the base `sh -c` command for the platform.
//
// For JS projects the detected command is the package.json script body
// (e.g. "vitest run"), whose binary lives in <workdir>/node_modules/.bin —
// not on PATH. Prepending it makes detector output directly executable without
// forcing every ecosystem through a package-manager wrapper.
func shCommand(ctx context.Context, workdir, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = workdir
	return cmd
}

func withNodeBin(workdir string) []string {
	env := os.Environ()
	bin := filepath.Join(workdir, "node_modules", ".bin")
	if fi, err := os.Stat(bin); err == nil && fi.IsDir() {
		for i, kv := range env {
			if strings.HasPrefix(kv, "PATH=") {
				env[i] = "PATH=" + bin + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
				return env
			}
		}
		env = append(env, "PATH="+bin)
	}
	return env
}

// runWatched runs cmd in its own process group and kills the whole group when
// ctx expires, so a deadline bounds the entire process tree (not just the shell).
//
// The kill goes through os/exec's own cancel hook: the runtime stops invoking
// Cancel once Wait returns, so a deadline can never deliver SIGKILL to a process
// group the OS has already recycled. WaitDelay bounds Wait even when a child
// ignores the signal (and stops its pipes from pinning Wait forever).
func runWatched(ctx context.Context, cmd *exec.Cmd) error {
	setupProcessGroup(cmd)
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			killProcessGroup(cmd.Process.Pid)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Wait()
}
