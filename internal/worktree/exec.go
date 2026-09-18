package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// commandWaitDelay bounds Wait after a kill. A child that ignores the signal
// (or a grandchild holding the stdout/stderr pipes open) would otherwise pin
// Wait forever, and the pipe-copying goroutines would keep writing into the
// buffers this function returns. With WaitDelay set, os/exec closes the pipes
// and Wait returns bounded.
const commandWaitDelay = 3 * time.Second

// cleanupTimeout bounds the *fresh* context used for rollback/cleanup, which
// must make progress even when the caller's run context is already canceled.
//
// This is a budget for the cleanup commands, not a promise about the OS: the
// final os.RemoveAll is a plain filesystem call and is not deadline-bounded.
const cleanupTimeout = 30 * time.Second

// cmdError carries the command identity and captured stderr so callers can
// classify failures (cross-device, missing binary) without re-parsing.
type cmdError struct {
	name   string
	args   []string
	stderr string
	err    error
}

func (e *cmdError) Error() string {
	msg := fmt.Sprintf("%s %s: %v", e.name, strings.Join(e.args, " "), e.err)
	if s := strings.TrimSpace(e.stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

// Unwrap keeps errors.Is/errors.As working: a cancelled command unwraps to the
// context error, a missing binary to exec.ErrNotFound.
func (e *cmdError) Unwrap() error { return e.err }

// runCommand runs name with args under ctx, in its own process group.
//
// Cancellation kills the whole process group (not just the direct child), so
// helper shells and their descendants cannot outlive the run. stdout/stderr
// are captured into buffers; Wait is bounded by commandWaitDelay.
func runCommand(ctx context.Context, dir string, env []string, name string, args ...string) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	setupProcessGroup(cmd)
	// Kill via os/exec's own cancel hook: the runtime stops calling Cancel once
	// Wait returns, so a deadline can never SIGKILL a recycled process group.
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			killProcessGroup(cmd.Process.Pid)
		}
		return nil
	}
	cmd.WaitDelay = commandWaitDelay

	runErr := cmd.Run()
	stdout, stderr := outBuf.String(), errBuf.String()
	if runErr == nil {
		return stdout, stderr, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		// Report the cancellation reason, not the derived "signal: killed".
		return stdout, stderr, &cmdError{name: name, args: args, stderr: stderr, err: cerr}
	}
	return stdout, stderr, &cmdError{name: name, args: args, stderr: stderr, err: runErr}
}

// isNotFound reports whether err means the executable is unavailable.
func isNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound)
}

// rollback deregisters the worktree paths this call registered and removes the
// temp dir. It always uses a fresh, independently bounded context so cleanup
// still runs when the caller's run context was canceled; errors are returned
// instead of swallowed.
func rollback(repoRoot, tmpDir string, registered []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	return rollbackContext(ctx, repoRoot, tmpDir, registered)
}
