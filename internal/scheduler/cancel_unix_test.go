//go:build unix

// Cancellation/cleanup integration tests need POSIX process groups and shell
// shims on PATH; assertions would not hold on platforms without them (see
// internal/worktree/proc_unix.go for the same split).
package scheduler_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/scheduler"
)

func shQ(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// waitPidFile polls until the stalled grandchild has recorded its pid.
func waitPidFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if b, err := os.ReadFile(path); err == nil {
			if pid, cerr := strconv.Atoi(strings.TrimSpace(string(b))); cerr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("stalled child never recorded its pid in %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitPidGone asserts the stalled grandchild was actually killed, not leaked.
func waitPidGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stalled child pid %d still alive after %v (process group not reaped)", pid, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func worktreeList(t *testing.T, realGit, repo string) int {
	t.Helper()
	out, err := exec.Command(realGit, "-C", repo, "worktree", "list", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree list: %v\n%s", err, out)
	}
	return strings.Count(string(out), "worktree ")
}

// TestScheduler_WorktreeSetupCancelIsUnverifiedAndReapsChildren proves the
// run's context reaches worktree setup: canceling mid-`git worktree add`
// must (a) kill and reap the stalled child, (b) return UNVERIFIED with the
// stage named, (c) roll back the registrations this call created.
func TestScheduler_WorktreeSetupCancelIsUnverifiedAndReapsChildren(t *testing.T) {
	realGit, err := exec.LookPath("git") // resolve BEFORE the shim enters PATH
	if err != nil {
		t.Fatal(err)
	}
	repo := scenarioRepo(t, goFiles()) // clean tree → stall lands on worktree add
	pidFile := filepath.Join(t.TempDir(), "stalled.pid")
	shim := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in\n  *\"worktree add\"*)\n" +
		"    sh -c 'echo $$ > " + shQ(pidFile) + "; sleep 300' &\n" +
		"    wait\n" +
		"    ;;\nesac\nexec " + shQ(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		res scheduler.Result
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		r, e := scheduler.Run(ctx, scheduler.Config{RepoRoot: repo, BaseRef: "HEAD", DisableCache: true})
		ch <- outcome{r, e}
	}()

	pid := waitPidFile(t, pidFile, 15*time.Second)
	cancel()
	select {
	case out := <-ch:
		if out.err != nil {
			t.Fatalf("cancellation must surface as a verdict, not an error: %v", out.err)
		}
		if out.res.Verdict != bundle.Unverified {
			t.Fatalf("canceled worktree setup must be UNVERIFIED, got %s", out.res.Verdict)
		}
		joined := strings.Join(out.res.Unverified, "\n")
		if !strings.Contains(joined, "canceled") || !strings.Contains(joined, "worktree setup") {
			t.Fatalf("claim must attribute the cancellation to worktree setup: %v", out.res.Unverified)
		}
		if len(out.res.Evidences) != 0 {
			t.Fatalf("no evidence may exist for an unobserved diff: %+v", out.res.Evidences)
		}
		if out.res.Bundle.BundleID != "" {
			t.Fatalf("an unobserved diff must not be persisted: %s", out.res.Bundle.BundleID)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Run did not return after cancellation (worktree setup not ctx-aware?)")
	}
	waitPidGone(t, pid, 10*time.Second)
	if n := worktreeList(t, realGit, repo); n != 1 {
		t.Fatalf("canceled run left %d worktree registrations (want main only)", n)
	}
}

// TestScheduler_CleanupFailureIsSurfaced makes git worktree remove AND prune
// fail (RemoveAll still succeeds), so the only trace of the failure is the
// leftover .git/worktrees registration. The warning must surface instead of
// being swallowed.
func TestScheduler_CleanupFailureIsSurfaced(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	repo := scenarioRepo(t, goFiles())
	shim := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in\n  *\"worktree remove\"*|*\"worktree prune\"*) exit 1 ;;\nesac\nexec " + shQ(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() { _ = exec.Command(realGit, "-C", repo, "worktree", "prune").Run() })

	res, err := scheduler.Run(context.Background(), scheduler.Config{RepoRoot: repo, BaseRef: "HEAD", DisableCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != bundle.Verified {
		t.Fatalf("the verification itself must succeed: %s", res.Verdict)
	}
	surfaced := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "worktree cleanup failed") {
			surfaced = true
		}
	}
	if !surfaced {
		t.Fatalf("cleanup failure must surface as a warning: %+v", res.Warnings)
	}
	// The warning must be real, not decorative: the registration is left behind.
	if n := worktreeList(t, realGit, repo); n < 2 {
		t.Fatalf("expected leftover registrations to justify the warning, got %d", n)
	}
}
