// Package worktree — P1.2 — base/candidate 双 worktree 管理 per PLAN-V2 §7.2.
//
// Strategy: dirty worktree → snapshot commit via a temporary index
// (GIT_INDEX_FILE + read-tree HEAD + add -A + write-tree + commit-tree).
// This includes untracked files and binary files (unlike `git stash create`
// without -u, and unlike base+patch which breaks on untracked/binary/LFS).
// Snapshot never touches the user's index, worktree, or stash refs.
//
// Worktrees are created via `git worktree add --detach` under the OS temp dir
// (never inside the repo — git refuses worktrees inside the repository).
// Cleanup removes worktree registrations; directories are recreated on demand.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Pair holds the two worktree paths.
type Pair struct {
	Base            string   // e.g. /tmp/vouch-worktree-xxx/base
	Candidate       string   // e.g. /tmp/vouch-worktree-xxx/candidate
	Dir             string   // parent tmp dir for cleanup
	Snapshot        string   // candidate snapshot commit ("" when clean)
	SnapshotRef     string   // refs/vouch/snapshots/<sha> pinning the snapshot ("" when clean)
	CandidateCommit string   // commit actually used as candidate (snapshot or HEAD)
	DepsMode        DepsMode // isolation level achieved when reusing node_modules
	DepsGaps        []string // non-fatal dependency-reuse problems (surface into profile.gaps)
	Cleanup         func() error
}

// DepsReused reports whether node_modules were reused at all, at any isolation
// level. Callers that care about isolation must read DepsMode instead: only
// DepsCOW keeps a probe's writes inside node_modules private to its own side.
func (p *Pair) DepsReused() bool { return p.DepsMode != DepsNone }

// Create builds base and candidate worktrees from baseRef.
// repoRoot is the original repo path. Dirty changes (including untracked and
// binary files) are snapshotted via a temporary git index.
//
// Create is CreateContext with a background context; use CreateContext when the
// operation must be cancellable (scheduler budgets, Ctrl-C, deadlines).
func Create(repoRoot, baseRef string, candidateRefs ...string) (*Pair, error) {
	return CreateContext(context.Background(), repoRoot, baseRef, candidateRefs...)
}

// CreateContext is Create with caller cancellation.
//
// Cancellation is honored between steps and inside every subprocess: git and
// cp run in their own process group, which is killed as a whole and reaped
// before CreateContext returns (helpers' grandchildren cannot leak). On
// cancellation — or any later failure — the worktrees this call registered are
// deregistered, the temp dir is removed, and the snapshot ref this call pinned
// is deleted, all under a fresh, independently bounded cleanup context so
// cleanup still runs when the caller's context is already canceled. Cleanup
// errors are returned alongside the original failure instead of being
// swallowed.
func CreateContext(ctx context.Context, repoRoot, baseRef string, candidateRefs ...string) (*Pair, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Resolve to an absolute path first: callers use "." (README's `vouch verify`),
	// and Dir(".")/Base(".") would create the worktrees *inside* the repo — which
	// git refuses and which Snapshot's `git add -A` would stage as a fake diff.
	absRepo, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repo root: %w", err)
	}
	repoRoot = absRepo
	// Worktrees must live on the same volume as the repo for cp -al hardlink
	// dependency reuse (tmpfs /tmp would force a full install every run).
	// Sibling directory keeps them out of the repository (git refuses worktrees
	// inside the working tree) while staying on the same filesystem.
	parent := filepath.Dir(repoRoot)
	base := filepath.Base(repoRoot)
	tmpDir, err := os.MkdirTemp(parent, base+".vouch-worktrees-*")
	crossDevice := false
	if err != nil {
		tmpDir, err = os.MkdirTemp("", "vouch-worktree-*")
		if err != nil {
			return nil, err
		}
		crossDevice = true
	}
	if resolved, err := filepath.EvalSymlinks(tmpDir); err == nil {
		// Canonical paths: macOS reports /private/var while TempDir gives /var,
		// which breaks downstream path comparisons (selector/go list, parsers).
		tmpDir = resolved
	}
	baseDir := filepath.Join(tmpDir, "base")
	candDir := filepath.Join(tmpDir, "candidate")

	// registered tracks the worktree paths actually handed to `git worktree add`
	// so a later failure deregisters exactly what this call created (and the
	// initial MkdirTemp failure path deregisters nothing).
	var registered []string
	// snapshotRef is tracked separately: only the ref pinned by *this* call is
	// ever deleted on failure, never one a caller passed in.
	var snapshotRef string

	fail := func(step string, err error) (*Pair, error) {
		// Fresh, independently bounded context: rollback must make progress even
		// when the run context is canceled or already expired.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		var cleanupErrs []error
		if snapshotRef != "" {
			if derr := gitUpdateRefDelete(cleanupCtx, repoRoot, snapshotRef); derr != nil {
				cleanupErrs = append(cleanupErrs, derr)
			}
		}
		if cerr := rollbackContext(cleanupCtx, repoRoot, tmpDir, registered); cerr != nil {
			cleanupErrs = append(cleanupErrs, cerr)
		}
		if cerr := errors.Join(cleanupErrs...); cerr != nil {
			return nil, fmt.Errorf("%s: %w (cleanup failed: %v)", step, err, cerr)
		}
		return nil, fmt.Errorf("%s: %w", step, err)
	}

	explicit := ""
	if len(candidateRefs) > 0 {
		explicit = candidateRefs[0]
	}
	var snapshot string
	if explicit == "" {
		// Only snapshot when the working tree IS the candidate. With an explicit
		// --to ref the snapshot would be dead weight (and pin an unused ref).
		snapshot, err = SnapshotContext(ctx, repoRoot)
		if err != nil {
			return fail("snapshot", err)
		}
	}
	if snapshot != "" {
		// Pin the snapshot under refs/vouch/ so `vouch rerun <id>` keeps working
		// after cleanup: an unreferenced commit is pruned by git gc.
		ref := "refs/vouch/snapshots/" + snapshot
		// Only a ref that did not exist before this call may be deleted again on
		// failure: the same snapshot content yields the same commit sha, so a
		// concurrent verify (or a caller) may already own this ref. The
		// ctx.Err() guard matters: a canceled or expired lookup fails without
		// telling us whether the ref exists, and claiming ownership then would
		// delete someone else's pin — so we never claim it on cancellation.
		if _, err := gitRevParse(ctx, repoRoot, ref); err != nil && ctx.Err() == nil {
			snapshotRef = ref
		}
		if err := gitUpdateRef(ctx, repoRoot, ref, snapshot); err != nil {
			return fail("pin snapshot ref", err)
		}
	}

	// Candidate is what we actually verify: an explicit ref when given (branch
	// range mode), else the dirty snapshot, else the current HEAD. Falling back
	// to baseRef would verify base and still report the range as verified.
	candidateRef := explicit
	if candidateRef == "" {
		candidateRef = snapshot
	}
	if candidateRef == "" {
		head, err := gitRevParse(ctx, repoRoot, "HEAD")
		if err != nil {
			return fail("resolve HEAD", err)
		}
		candidateRef = head
	}
	if err := ctx.Err(); err != nil {
		return fail("canceled before base worktree", err)
	}
	// Register optimistically, BEFORE running the command: a canceled or killed
	// `git worktree add` cannot roll back its own .git/worktrees admin entry, so
	// the path must already be on the rollback list or cleanup would leave a
	// phantom registration behind. Removal is safe for a path that was never
	// registered (remove fails, directory removal is a no-op, prune clears any
	// partial admin entry).
	registered = append(registered, baseDir)
	if err := gitWorktreeAdd(ctx, repoRoot, baseDir, baseRef); err != nil {
		return fail("base worktree", err)
	}
	if err := ctx.Err(); err != nil {
		return fail("canceled before candidate worktree", err)
	}
	registered = append(registered, candDir)
	if err := gitWorktreeAdd(ctx, repoRoot, candDir, candidateRef); err != nil {
		return fail("candidate worktree", err)
	}

	var depsGaps []string
	if crossDevice {
		depsGaps = append(depsGaps, "worktrees on a different filesystem than the repo: dependency hardlink reuse disabled")
	}
	// Reuse the repo's node_modules into both worktrees, preferring a
	// copy-on-write clone so each side's writes stay private. Best effort, but
	// never silent: when the repo has node_modules and reuse did not happen, or
	// happened only at hardlink strength, say so in DepsGaps.
	modeBase, errBase := ReuseDepsContext(ctx, repoRoot, baseDir)
	modeCandidate, errCandidate := ReuseDepsContext(ctx, repoRoot, candDir)
	if errBase != nil {
		depsGaps = append(depsGaps, "base node_modules reuse failed: "+errBase.Error())
	}
	if errCandidate != nil {
		depsGaps = append(depsGaps, "candidate node_modules reuse failed: "+errCandidate.Error())
	}
	// A canceled cp is not a "gap": the worktrees are being torn down, so report
	// the cancellation instead of returning a half-populated pair.
	if err := ctx.Err(); err != nil {
		return fail("canceled during dependency reuse", err)
	}
	// The pair is only as isolated as its weaker side.
	depsMode := weakestDepsMode(modeBase, modeCandidate)
	if depsMode == DepsHardlink {
		depsGaps = append(depsGaps, "dependencies reused via hardlinks (no copy-on-write support on this filesystem): in-place writes inside node_modules may cross between base and candidate")
	}
	if !crossDevice && depsMode == DepsNone {
		if _, err := os.Stat(filepath.Join(repoRoot, "node_modules")); err == nil {
			depsGaps = append(depsGaps, "node_modules present but not reused (cross-device, symlinked without clone support, or partial failure): probes may need a full install")
		}
	}

	// Cleanup is reusable after a successful Create: it uses its own bounded
	// context, so a canceled caller context cannot skip deregistration.
	cleanup := func() error {
		return rollback(repoRoot, tmpDir, registered)
	}
	return &Pair{
		Base:            baseDir,
		Candidate:       candDir,
		Dir:             tmpDir,
		Snapshot:        snapshot,
		SnapshotRef:     snapshotRef,
		CandidateCommit: candidateRef,
		DepsMode:        depsMode,
		DepsGaps:        depsGaps,
		Cleanup:         cleanup,
	}, nil
}

// Snapshot creates a commit object representing the current working tree
// (tracked modifications + untracked non-ignored files + binary files) without
// touching the user's index, HEAD, or stash. Returns "" when there is nothing
// to snapshot beyond HEAD and HEAD tree equals the snapshot tree.
func Snapshot(repoRoot string) (string, error) {
	return SnapshotContext(context.Background(), repoRoot)
}

// SnapshotContext is Snapshot with caller cancellation.
//
// Every git step runs in its own process group killed as a whole on
// cancellation, and the temporary index file is always removed (deferred), so a
// canceled snapshot leaves neither a stray index file nor a running git child.
func SnapshotContext(ctx context.Context, repoRoot string) (string, error) {
	// Temporary index file (git requires it not to exist yet).
	f, err := os.CreateTemp("", "vouch-index-*")
	if err != nil {
		return "", err
	}
	idx := f.Name()
	_ = f.Close()
	_ = os.Remove(idx)
	// Deferred: cleanup runs on every exit path, including cancellation, so the
	// temp index never survives a canceled run.
	defer func() { _ = os.Remove(idx) }()

	// Deterministic identity for the temporary snapshot commit (overrides user config
	// only for these git invocations; the commit is never stored in a ref).
	env := append(os.Environ(),
		"GIT_INDEX_FILE="+idx,
		"GIT_AUTHOR_NAME=vouch", "GIT_AUTHOR_EMAIL=vouch@localhost",
		"GIT_COMMITTER_NAME=vouch", "GIT_COMMITTER_EMAIL=vouch@localhost",
	)
	git := func(args ...string) (string, error) {
		// Re-check between steps: a canceled snapshot must not start another git.
		if err := ctx.Err(); err != nil {
			return "", err
		}
		stdout, _, err := runCommand(ctx, repoRoot, env, "git", args...)
		if err != nil {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(stdout), nil
	}

	// Seed temp index from HEAD when it exists (unborn HEAD → empty index).
	headEmpty := false
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		headEmpty = true
	} else if _, err := git("read-tree", "HEAD"); err != nil {
		return "", err
	}

	// Stage everything (tracked + untracked, respecting .gitignore). The caller
	// adds .vouch/ to .git/info/exclude first, so vouch's own runtime state is
	// never staged: otherwise byte-identical runs would get different ids.
	if _, err := git("add", "-A"); err != nil {
		return "", err
	}
	tree, err := git("write-tree")
	if err != nil {
		return "", err
	}

	if !headEmpty {
		// Nothing new relative to HEAD? Compare trees; skip snapshot to keep
		// clean repos using baseRef directly.
		headTree, err := git("rev-parse", "HEAD^{tree}")
		if err == nil && headTree == tree {
			return "", nil
		}
	}

	args := []string{"commit-tree", tree, "-m", "vouch snapshot (temporary, not stored in refs)"}
	if !headEmpty {
		args = append(args, "-p", head)
	}
	snap, err := git(args...)
	if err != nil {
		return "", err
	}
	return snap, nil
}

func gitWorktreeAdd(ctx context.Context, repoRoot, path, ref string) error {
	if _, _, err := runCommand(ctx, repoRoot, nil, "git", "worktree", "add", "--detach", path, ref); err != nil {
		return err
	}
	return nil
}

// gitWorktreeRemove deregisters path and removes its directory.
//
// Ordering matters for "no stale registrations": `git worktree prune` only
// drops entries whose working-tree directory is already gone, so the directory
// is removed BEFORE pruning. When worktree remove fails (locked worktree, git
// killed mid-operation) the prune is what clears the .git/worktrees entry; if
// both deregistration attempts fail, that is a real error, not a silent no-op.
func gitWorktreeRemove(ctx context.Context, repoRoot, path string) error {
	_, _, removeErr := runCommand(ctx, repoRoot, nil, "git", "worktree", "remove", "--force", path)
	rmErr := os.RemoveAll(path)
	if removeErr == nil {
		return rmErr
	}
	// Directory is gone now; prune drops any leftover admin entry.
	_, _, pruneErr := runCommand(ctx, repoRoot, nil, "git", "worktree", "prune")
	if pruneErr == nil {
		return rmErr
	}
	return errors.Join(removeErr, pruneErr, rmErr)
}

func gitUpdateRef(ctx context.Context, repoRoot, ref, sha string) error {
	if _, _, err := runCommand(ctx, repoRoot, nil, "git", "update-ref", ref, sha); err != nil {
		return err
	}
	return nil
}

// gitUpdateRefDelete removes ref only if the command actually ran; used by the
// failure path to unpin a snapshot ref this call created.
func gitUpdateRefDelete(ctx context.Context, repoRoot, ref string) error {
	if _, _, err := runCommand(ctx, repoRoot, nil, "git", "update-ref", "-d", ref); err != nil {
		return fmt.Errorf("delete snapshot ref %s: %w", ref, err)
	}
	return nil
}

func gitRevParse(ctx context.Context, repoRoot, ref string) (string, error) {
	out, _, err := runCommand(ctx, repoRoot, nil, "git", "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// rollbackContext is rollback's body under a caller-owned cleanup context.
func rollbackContext(ctx context.Context, repoRoot, tmpDir string, registered []string) error {
	var errs []error
	for i := len(registered) - 1; i >= 0; i-- {
		if err := gitWorktreeRemove(ctx, repoRoot, registered[i]); err != nil {
			errs = append(errs, err)
		}
	}
	if err := os.RemoveAll(tmpDir); err != nil {
		errs = append(errs, fmt.Errorf("remove worktree dir %s: %w", tmpDir, err))
	}
	return errors.Join(errs...)
}

// SnapshotRefPrefix is the namespace for pinned candidate snapshots.
const SnapshotRefPrefix = "refs/vouch/snapshots/"

// PruneSnapshotRefs keeps at most `keep` snapshot refs (newest by commit date)
// and deletes the rest. verify calls this after a bundle is stored: without it
// every dirty-tree run would pin another commit forever.
func PruneSnapshotRefs(repoRoot string, keep int) (int, error) {
	return PruneSnapshotRefsExcept(repoRoot, keep, nil)
}

// PruneSnapshotRefsExcept is PruneSnapshotRefs with a protected set: refs in
// `protected` are always kept, so a stored bundle can still be reproduced even
// when it falls outside the retention window.
func PruneSnapshotRefsExcept(repoRoot string, keep int, protected map[string]bool) (int, error) {
	if keep < 1 {
		keep = 1
	}
	cmd := exec.Command("git", "for-each-ref", "--sort=-committerdate", "--format=%(refname)", SnapshotRefPrefix)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	var refs []string
	for _, line := range strings.Split(string(out), "\n") {
		if r := strings.TrimSpace(line); r != "" {
			refs = append(refs, r)
		}
	}
	removed := 0
	keptCount := 0
	for _, ref := range refs {
		if protected[ref] {
			continue
		}
		if keptCount < keep {
			keptCount++
			continue
		}
		del := exec.Command("git", "update-ref", "-d", ref)
		del.Dir = repoRoot
		if err := del.Run(); err == nil {
			removed++
		}
	}
	return removed, nil
}

// DeleteSnapshotRef removes one snapshot ref (used by gc).
func DeleteSnapshotRef(repoRoot, ref string) error {
	if !strings.HasPrefix(ref, SnapshotRefPrefix) {
		return fmt.Errorf("refusing to delete non-snapshot ref %q", ref)
	}
	cmd := exec.Command("git", "update-ref", "-d", ref)
	cmd.Dir = repoRoot
	return cmd.Run()
}
