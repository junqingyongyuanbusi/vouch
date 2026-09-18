package sandbox_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/sandbox"
)

// buildDialer compiles the test dialer helper into a temp dir.
func buildDialer(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "dialer")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/dialer")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build dialer helper: %v\n%s", err, out)
	}
	return bin
}

func TestSandbox_RunBasic(t *testing.T) {
	dir := t.TempDir()
	res, err := sandbox.Run(context.Background(), dir, "echo vouch", sandbox.Policy{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "vouch") {
		t.Fatalf("stdout=%q", res.Stdout)
	}
	if res.Reason == "" {
		t.Fatal("Result.Reason must document the isolation actually applied")
	}
}

func TestSandbox_NonZeroExitIsResult(t *testing.T) {
	dir := t.TempDir()
	res, err := sandbox.Run(context.Background(), dir, "exit 3", sandbox.Policy{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("want exit 3, got %d", res.ExitCode)
	}
}

// TestSandbox_LoopbackAllowed is the P1.2 acceptance test: localhost must be
// reachable under the default (network-denied) policy.
func TestSandbox_LoopbackAllowed(t *testing.T) {
	bin := buildDialer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	addr := ln.Addr().String()
	res, err := sandbox.Run(context.Background(), t.TempDir(), bin+" "+addr, sandbox.Policy{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("loopback dial must succeed under default policy: exit=%d stderr=%s reason=%s",
			res.ExitCode, res.Stderr, res.Reason)
	}
}

// TestSandbox_ExternalDeniedBestEffort verifies remote egress is blocked when
// the platform sandbox is actually applied. Skipped when no isolation mechanism
// exists (documented best-effort) so the suite stays green on unsupported hosts.
func TestSandbox_ExternalDeniedBestEffort(t *testing.T) {
	bin := buildDialer(t)
	dir := t.TempDir()
	// Probe sandbox availability first.
	probe, err := sandbox.Run(context.Background(), dir, "true", sandbox.Policy{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !probe.Sandboxed {
		t.Skipf("no OS isolation available: %s", probe.Reason)
	}
	// example.com:80 — must fail to connect when network is denied.
	res, err := sandbox.Run(context.Background(), dir, bin+" example.com:80", sandbox.Policy{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("remote egress must be denied, but dial succeeded (reason=%s)", res.Reason)
	}
}

func TestSandbox_NodeModulesBinOnPath(t *testing.T) {
	// detector emits script bodies like "vitest run"; the binary lives in
	// node_modules/.bin, so the sandbox must put it on PATH.
	dir := t.TempDir()
	binDir := filepath.Join(dir, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "vouchfixture"), []byte("#!/bin/sh\necho fixture-ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := sandbox.Run(context.Background(), dir, "vouchfixture", sandbox.Policy{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "fixture-ok") {
		t.Fatalf("node_modules/.bin not on PATH: exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
}

func TestSandbox_CommandEpermIsNotLauncherFailure(t *testing.T) {
	// A command that itself prints "Operation not permitted" is the sandbox
	// working (network denied), not a launcher failure — it must not be retried
	// unisolated (which would also re-run the command).
	dir := t.TempDir()
	probe, err := sandbox.Run(context.Background(), dir, "true", sandbox.Policy{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	res, err := sandbox.Run(context.Background(), dir, "echo 'Operation not permitted' >&2; exit 7", sandbox.Policy{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("command-level EPERM must be a Result, not an error: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("want exit 7, got %d (stderr=%q)", res.ExitCode, res.Stderr)
	}
	if res.Sandboxed != probe.Sandboxed {
		t.Fatalf("isolation must be kept on command-level EPERM: sandboxed=%v want %v", res.Sandboxed, probe.Sandboxed)
	}
}

func TestSandbox_TimeoutKillsProcessGroup(t *testing.T) {
	// A child that keeps running after the shell is killed must not keep the
	// pipes open: without process-group kill this takes the full sleep time.
	dir := t.TempDir()
	start := time.Now()
	res, err := sandbox.Run(context.Background(), dir, "sh -c 'sleep 30 & sleep 30'", sandbox.Policy{Timeout: 700 * time.Millisecond})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("expected TimedOut, got %+v", res)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout must kill the whole tree quickly, took %v", elapsed)
	}
}

func TestSandbox_FallbackCapturesOutput(t *testing.T) {
	// Simulate a launcher that exists but fails: the fallback path must still
	// capture the command's output (a nil Stdout assignment would silently
	// produce empty evidence for every probe).
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sandbox-exec", "unshare"} {
		script := "#!/bin/sh\necho \"" + name + ": Operation not permitted\" >&2\nexit 1\n"
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	res, err := sandbox.Run(context.Background(), dir, "echo fallback-output", sandbox.Policy{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Sandboxed {
		t.Skipf("launcher available on this host: %s", res.Reason)
	}
	if !strings.Contains(res.Stdout, "fallback-output") {
		t.Fatalf("fallback must capture stdout, got %q (stderr=%q, reason=%s)", res.Stdout, res.Stderr, res.Reason)
	}
}

func TestSandbox_LingeringIOEndsTheRunWithoutRerun(t *testing.T) {
	// A command that exits successfully while a grandchild still holds the
	// inherited pipes makes os/exec report either ErrWaitDelay or a bare
	// context error, depending on a race between its context watcher and its
	// process waiter. Both shapes must count as ONE completed run: classifying
	// them as a launcher failure would rerun the whole command without
	// isolation (double side effects, no sandbox) and misreport a passing test
	// as unmeasurable. Only the outcome is asserted here; the exact error shape
	// is an implementation detail of os/exec.
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		wantEnd bool // true when the budget is expected to cut the run off
	}{
		{"deadline after the child exits", 60 * time.Second, false},
		{"deadline during the lingering I/O", 1500 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "runs.log")
			// The background child outlives WaitDelay (2s) but not the test.
			cmdline := "echo run >> " + marker + "; sleep 4 & echo done"
			res, err := sandbox.Run(context.Background(), dir, cmdline, sandbox.Policy{Timeout: tc.timeout})
			if err != nil {
				t.Fatalf("a completed command must not be reported as a launcher failure: %v", err)
			}
			if res.TimedOut != tc.wantEnd {
				t.Fatalf("TimedOut=%v, want %v: %+v", res.TimedOut, tc.wantEnd, res)
			}
			if !tc.wantEnd && res.ExitCode != 0 {
				t.Fatalf("exit code must come from the finished process, got %+v", res)
			}
			data, readErr := os.ReadFile(marker)
			if readErr != nil {
				t.Fatalf("command did not run: %v", readErr)
			}
			if got := strings.Count(string(data), "run"); got != 1 {
				t.Fatalf("the command must run exactly once, got %d executions", got)
			}
		})
	}
}
