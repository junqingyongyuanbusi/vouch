//go:build unix

// Cancellation tests need POSIX process groups (syscall.Kill, /bin/sh shims);
// the non-unix process-group helpers are no-ops, so these assertions only hold
// on unix (see proc_unix.go / proc_other.go).
package worktree_test

import (
	"context"
	"errors"

	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/worktree"
)

// Resolve once before tests alter PATH; validation must never re-enter a shim.
var systemGit, systemGitErr = exec.LookPath("git")

func realGit(t *testing.T) string {
	t.Helper()
	p, err := systemGit, systemGitErr
	if err != nil {
		t.Fatalf("git not on PATH: %v", err)
	}
	return p
}

// writeStallShim installs an executable named `name` whose first action, when
// `cond` (a shell test) matches, is to record the pid of a grandchild and then
// block forever — simulating a helper subprocess that ignores SIGTERM. All
// other invocations are delegated to realBin.
func writeStallShim(t *testing.T, dir, name, realBin, cond, pidFile string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"if " + cond + "; then\n" +
		"  sh -c 'echo $$ > " + shQuote(pidFile) + "; sleep 300' &\n" +
		"  wait\n" +
		"fi\n" +
		"exec " + shQuote(realBin) + " \"$@\"\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func prependPath(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func readPid(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			pid, cerr := strconv.Atoi(strings.TrimSpace(string(b)))
			if cerr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("stalled child never recorded its pid in %s", pidFile)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitProcessGone asserts the stalled grandchild was actually killed (not just
// detached). ESRCH means the pid no longer exists.
func waitProcessGone(t *testing.T, pid int, timeout time.Duration) {
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

// gitOut runs real git in repo (bypassing any shim installed on PATH).
func gitOut(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command(realGit(t), args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func registrationCount(t *testing.T, repo string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(gitOut(t, repo, "worktree", "list", "--porcelain"), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			n++
		}
	}
	return n
}

func snapshotRefCount(t *testing.T, repo string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(gitOut(t, repo, "for-each-ref", "--format=%(refname)", worktree.SnapshotRefPrefix), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func tempWorktreeDirs(t *testing.T, repo string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(repo), filepath.Base(repo)+".vouch-worktrees-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func tempIndexCount(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "vouch-index-*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}

// --- cancellation tests ----------------------------------------------------

func TestSnapshotContext_CancelKillsGitAndRemovesTempIndex(t *testing.T) {
	dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "git-child.pid")
	shim := t.TempDir()
	writeStallShim(t, shim, "git", realGit(t), `[ "$1" = "add" ]`, pidFile)
	prependPath(t, shim)

	before := tempIndexCount(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	snap, err := worktree.SnapshotContext(ctx, dir)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("stalled git must surface cancellation, got snapshot %q", snap)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("cancellation took %v (child/pipe wait not bounded)", elapsed)
	}
	waitProcessGone(t, readPid(t, pidFile), 10*time.Second)
	if after := tempIndexCount(t); after != before {
		t.Fatalf("temp index not cleaned up after cancellation: %d -> %d", before, after)
	}
}

// TestCreateContext_CancelDuringWorktreeAddKillsChildAndRollsBack stalls git
// *after* the real `worktree add` has registered the worktree, which is the
// worst cancellation case: git never gets to clean up its admin entry. The
// rollback list must already contain the attempted path.
func TestCreateContext_CancelDuringWorktreeAddKillsChildAndRollsBack(t *testing.T) {
	dir := newRepo(t)
	pidFile := filepath.Join(t.TempDir(), "git-worktree-add.pid")
	shim := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"worktree\" ] && [ \"$2\" = \"add\" ]; then\n" +
		"  " + shQuote(realGit(t)) + " \"$@\" || exit $?\n" +
		"  sh -c 'echo $$ > " + shQuote(pidFile) + "; sleep 300' &\n" +
		"  wait\n" +
		"fi\n" +
		"exec " + shQuote(realGit(t)) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prependPath(t, shim)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	pair, err := worktree.CreateContext(ctx, dir, "HEAD")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("stalled git worktree add must surface cancellation, got pair %+v", pair)
	}
	if pair != nil {
		t.Fatalf("canceled CreateContext must not return a pair: %+v", pair)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("cancellation took %v", elapsed)
	}
	waitProcessGone(t, readPid(t, pidFile), 10*time.Second)

	if n := registrationCount(t, dir); n != 1 {
		t.Fatalf("canceled CreateContext left registrations behind: %d worktrees (want only main)", n)
	}
	if dirs := tempWorktreeDirs(t, dir); len(dirs) != 0 {
		t.Fatalf("canceled CreateContext left temp worktree dirs: %v", dirs)
	}
}

func TestCreateContext_PartialAddFailureDeregistersAndUnpins(t *testing.T) {
	dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shim := t.TempDir()
	counter := filepath.Join(shim, "adds")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"worktree\" ] && [ \"$2\" = \"remove\" ]; then\n" +
		"  echo 'shim: worktree remove refused (locked)' >&2\n" +
		"  exit 5\n" +
		"fi\n" +
		"if [ \"$1\" = \"worktree\" ] && [ \"$2\" = \"add\" ]; then\n" +
		"  n=$(cat " + shQuote(counter) + " 2>/dev/null || echo 0)\n" +
		"  n=$((n+1)); echo $n > " + shQuote(counter) + "\n" +
		"  if [ \"$n\" -ge 2 ]; then\n" +
		"    echo 'shim: candidate worktree add refused' >&2\n" +
		"    exit 3\n" +
		"  fi\n" +
		"fi\n" +
		"exec " + shQuote(realGit(t)) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prependPath(t, shim)

	pair, err := worktree.CreateContext(context.Background(), dir, "HEAD")
	if err == nil {
		t.Fatalf("second worktree add failure must fail CreateContext, got pair %+v", pair)
	}
	if pair != nil {
		t.Fatalf("failed CreateContext must not return a pair: %+v", pair)
	}
	if !strings.Contains(err.Error(), "candidate worktree") {
		t.Fatalf("error should name the failing step, got %v", err)
	}
	// `git worktree remove` is refused by the shim (locked worktree), so the
	// registration can only be cleared by the RemoveAll-then-prune fallback.
	// Prune before RemoveAll would be a no-op here and leave the entry behind.
	if strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("rollback should have succeeded via prune fallback, got %v", err)
	}
	// The base worktree was registered before the candidate add failed: it must
	// be deregistered, not left in `.git/worktrees`.
	if n := registrationCount(t, dir); n != 1 {
		t.Fatalf("failed CreateContext left registrations behind: %d (want only main)", n)
	}
	if dirs := tempWorktreeDirs(t, dir); len(dirs) != 0 {
		t.Fatalf("failed CreateContext left temp worktree dirs: %v", dirs)
	}
	// The snapshot ref this call pinned must be unpinned again (otherwise failed
	// runs accumulate refs that keep commits alive against gc).
	if n := snapshotRefCount(t, dir); n != 0 {
		t.Fatalf("failed CreateContext left %d pinned snapshot ref(s) behind", n)
	}
}

// TestCreateContext_KeepsPreExistingSnapshotRef covers ref ownership: a ref
// that already existed before the call must survive a later failure, because
// this call did not create it.
func TestCreateContext_KeepsPreExistingSnapshotRef(t *testing.T) {
	dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// First run pins the snapshot ref for this exact dirty tree...
	first, err := worktree.Create(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if first.SnapshotRef == "" {
		t.Fatal("expected a pinned snapshot ref")
	}
	if err := first.Cleanup(); err != nil {
		t.Fatal(err)
	}
	// ...second run produces the same commit sha, so the ref now pre-exists and
	// a failure during its worktree setup must not delete it.
	shim := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"worktree\" ] && [ \"$2\" = \"add\" ]; then\n" +
		"  exit 3\n" +
		"fi\n" +
		"exec " + shQuote(realGit(t)) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prependPath(t, shim)

	if _, err := worktree.CreateContext(context.Background(), dir, "HEAD"); err == nil {
		t.Fatal("expected CreateContext to fail")
	}
	if n := snapshotRefCount(t, dir); n != 1 {
		t.Fatalf("pre-existing snapshot ref was deleted by a failed run (%d refs left)", n)
	}
}

// TestCreateContext_CancelDuringRefLookupKeepsForeignRef covers the ambiguous
// branch: the ref-existence lookup fails because the context expired, so the
// call cannot tell "ref missing" from "lookup canceled". It must NOT claim
// ownership — otherwise cleanup would delete a ref this run never created.
func TestCreateContext_CancelDuringRefLookupKeepsForeignRef(t *testing.T) {
	dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-existing ref owned by a "previous verify" of the same dirty tree.
	first, err := worktree.Create(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if first.SnapshotRef == "" {
		t.Fatal("expected a pinned snapshot ref")
	}
	if err := first.Cleanup(); err != nil {
		t.Fatal(err)
	}
	ref := first.SnapshotRef

	// Stall only the snapshot-ref existence lookup, then let the context expire.
	pidFile := filepath.Join(t.TempDir(), "git-ref-lookup.pid")
	shim := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *refs/vouch/snapshots*)\n" +
		"    sh -c 'echo $$ > " + shQuote(pidFile) + "; sleep 300' &\n" +
		"    wait\n" +
		"    ;;\n" +
		"esac\n" +
		"exec " + shQuote(realGit(t)) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prependPath(t, shim)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := worktree.CreateContext(ctx, dir, "HEAD"); err == nil {
		t.Fatal("expected CreateContext to be canceled")
	}
	waitProcessGone(t, readPid(t, pidFile), 10*time.Second)
	if n := snapshotRefCount(t, dir); n != 1 {
		t.Fatalf("canceled run deleted a ref it never created: %d refs left (want %s)", n, ref)
	}
	if got := gitOut(t, dir, "rev-parse", ref); strings.TrimSpace(got) != first.Snapshot {
		t.Fatalf("pre-existing ref moved: %s -> %s", first.Snapshot, strings.TrimSpace(got))
	}
}

func TestReuseDepsContext_CancelKillsCpChild(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "node_modules", "pkg", "index.js"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "cp-child.pid")
	realCp, err := exec.LookPath("cp")
	if err != nil {
		t.Skipf("cp not on PATH: %v", err)
	}
	shim := t.TempDir()
	// Stall whichever copy runs first. The flags differ by platform (darwin
	// clones with -c, GNU with --reflink) and reuse may try a second copy as a
	// fallback, so matching on a specific flag would make this test silently
	// stop covering the cancellation path.
	writeStallShim(t, shim, "cp", realCp, `true`, pidFile)
	prependPath(t, shim)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	mode, err := worktree.ReuseDepsContext(ctx, src, dst)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("stalled cp must surface cancellation, got mode=%s", mode)
	}
	if mode != worktree.DepsNone {
		t.Fatalf("canceled ReuseDepsContext must not report reuse, got %s", mode)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("cancellation took %v", elapsed)
	}
	waitProcessGone(t, readPid(t, pidFile), 10*time.Second)
}

// TestCreateContext_CleanupSurvivesCanceledRunContext covers the requirement
// that cleanup uses a fresh bounded context: the caller cancels first, then
// cleanup must still deregister and remove everything.
func TestCreateContext_CleanupSurvivesCanceledRunContext(t *testing.T) {
	dir := newRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	pair, err := worktree.CreateContext(ctx, dir, "HEAD")
	if err != nil {
		t.Fatalf("CreateContext: %v", err)
	}
	cancel() // caller's run context is now dead
	if err := pair.Cleanup(); err != nil {
		t.Fatalf("cleanup after cancellation must succeed, got %v", err)
	}
	if n := registrationCount(t, dir); n != 1 {
		t.Fatalf("cleanup after cancelled run left %d registrations (want only main)", n)
	}
	if dirs := tempWorktreeDirs(t, dir); len(dirs) != 0 {
		t.Fatalf("cleanup after cancelled run left temp dirs: %v", dirs)
	}
	if _, err := os.Stat(pair.Dir); !os.IsNotExist(err) {
		t.Fatalf("worktree dir %s still exists: %v", pair.Dir, err)
	}
}

// TestCreateContext_OrdinaryPathStillWorks exercises the new context-aware API
// on the happy path, including hardlink dependency reuse and cleanup errors.
func TestCreateContext_OrdinaryPathStillWorks(t *testing.T) {
	dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	depFile := filepath.Join(dir, "node_modules", "pkg", "index.js")
	if err := os.WriteFile(depFile, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pair, err := worktree.CreateContext(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatalf("CreateContext: %v", err)
	}
	if !pair.DepsReused() {
		t.Fatalf("expected dependency reuse, gaps: %v", pair.DepsGaps)
	}
	for _, wt := range []string{pair.Base, pair.Candidate} {
		if _, err := os.Stat(filepath.Join(wt, "node_modules", "pkg", "index.js")); err != nil {
			t.Fatalf("dep missing in %s: %v", wt, err)
		}
	}
	// The base worktree is a clean HEAD checkout (no node_modules), so its copy
	// comes from the repo. Under DepsCOW it must be a distinct inode — that is
	// the whole point of the clone. Under the hardlink fallback the inode is
	// shared, which is why that mode has to be declared as a gap.
	src, err := os.Stat(depFile)
	if err != nil {
		t.Fatal(err)
	}
	baseDep, err := os.Stat(filepath.Join(pair.Base, "node_modules", "pkg", "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	switch pair.DepsMode {
	case worktree.DepsCOW:
		if os.SameFile(baseDep, src) {
			t.Fatalf("DepsCOW reported but %s shares an inode with %s", pair.Base, depFile)
		}
	case worktree.DepsHardlink:
		if !os.SameFile(baseDep, src) {
			t.Fatalf("DepsHardlink reported but %s is not linked to %s", pair.Base, depFile)
		}
		if len(pair.DepsGaps) == 0 {
			t.Fatal("hardlink reuse shares inodes: it must be declared as a gap")
		}
	default:
		t.Fatalf("unexpected deps mode %s", pair.DepsMode)
	}

	if pair.Snapshot == "" {
		t.Fatal("dirty repo must still produce a snapshot")
	}
	if err := pair.Cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	// Cleanup must be safe to call twice (idempotent).
	if err := pair.Cleanup(); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
	if n := registrationCount(t, dir); n != 1 {
		t.Fatalf("cleanup left %d registrations (want only main)", n)
	}
}

// TestWrappersMatchContextVersions guards the preserved legacy signatures.
func TestWrappersMatchContextVersions(t *testing.T) {
	dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := worktree.Snapshot(dir)
	if err != nil || snap == "" {
		t.Fatalf("Snapshot wrapper: %q %v", snap, err)
	}
	pair, err := worktree.Create(dir, "HEAD")
	if err != nil {
		t.Fatalf("Create wrapper: %v", err)
	}
	defer func() { _ = pair.Cleanup() }()
	// No node_modules in the repo: reuse is a documented no-op, not an error.
	mode, err := worktree.ReuseDeps(dir, pair.Base)
	if err != nil {
		t.Fatalf("ReuseDeps wrapper: %v", err)
	}
	if mode != worktree.DepsNone {
		t.Fatalf("ReuseDeps must report DepsNone when the source has no node_modules, got %s", mode)
	}
}
