package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// DepsMode is the isolation level a dependency reuse actually achieved. It is
// deliberately not a bool: callers must be able to tell "base and candidate
// have independent copies" apart from "they share inodes", because only the
// first one lets a probe write inside node_modules without the write showing up
// on the other side. Reporting the weaker case as plain success would make the
// evidence dishonest.
type DepsMode string

const (
	// DepsCOW is a copy-on-write clone (APFS clonefile, Btrfs/XFS reflink).
	// Writes on either side are private: full isolation at hardlink speed.
	DepsCOW DepsMode = "cow"
	// DepsHardlink shares inodes with the source. New files and
	// write-temp-then-rename are private, but an in-place rewrite of an existing
	// file crosses to the other side and to the user's repo. Callers must record
	// a gap.
	DepsHardlink DepsMode = "hardlink"
	// DepsNone means nothing was reused: probes install their own dependencies.
	DepsNone DepsMode = "none"
)

// writableStatePaths are the entries inside node_modules that tooling rewrites
// in place during a run — the ones that actually leak under DepsHardlink. After
// a hardlink copy they are broken out into private copies (same content, new
// inode) so the common case is isolated even without reflink support.
//
// Deleting them instead of copying would be wrong: a missing .package-lock.json
// makes npm decide the tree is stale and reinstall it.
var writableStatePaths = []string{
	".vite",
	".vite-temp",
	".cache",
	".package-lock.json",
	".modules.yaml",
	".yarn-state.yml",
}

// delinkBudget caps how much a single writable state path may be un-shared.
// Past this the copy costs more than the isolation is worth, so it is skipped
// and reported as a gap rather than silently blowing up the run time.
const delinkBudget = 200 << 20 // 200 MiB

// weakestDepsMode reduces the two sides of a pair to the isolation the pair
// actually has. A pair is only as isolated as its weaker side: a cloned base
// next to a hardlinked candidate can still leak, so it must report hardlink.
func weakestDepsMode(a, b DepsMode) DepsMode {
	rank := func(m DepsMode) int {
		switch m {
		case DepsCOW:
			return 2
		case DepsHardlink:
			return 1
		default:
			return 0
		}
	}
	if rank(a) <= rank(b) {
		return a
	}
	return b
}

// ReuseDeps copies node_modules from src to dst, preferring copy-on-write.
//
// Returns the isolation level actually achieved (see DepsMode) — DepsNone when
// there is nothing to reuse or the copy could not be made in a way worth
// trusting. An error means an unexpected failure the caller should surface as a
// gap; it is never used for the ordinary "fall back to a full install" path.
//
// Isolating the two sides makes a verify run on a caching test runner roughly
// twice as slow, because both sides now actually run. Sharing the cache back
// would restore the old speed and re-break the evidence: the candidate would
// read the base's results and a real regression could pass. See
// docs/agent-loop.md, "Why isolation costs time".
//
// ReuseDeps is ReuseDepsContext with a background context.
func ReuseDeps(srcWorktree, dstWorktree string) (DepsMode, error) {
	return ReuseDepsContext(context.Background(), srcWorktree, dstWorktree)
}

// ReuseDepsContext is ReuseDeps with caller cancellation. Each cp runs in its
// own process group, killed as a whole on cancellation and reaped before
// return, so a canceled reuse cannot leave a cp writing into the destination
// worktree after the call has returned.
func ReuseDepsContext(ctx context.Context, srcWorktree, dstWorktree string) (DepsMode, error) {
	if err := ctx.Err(); err != nil {
		return DepsNone, err
	}
	src := filepath.Join(srcWorktree, "node_modules")
	dst := filepath.Join(dstWorktree, "node_modules")

	info, err := os.Lstat(src)
	if err != nil {
		return DepsNone, nil // nothing to reuse → full install
	}
	// A symlinked node_modules (pnpm layouts, monorepos) must be dereferenced:
	// copying the link itself would point both worktrees at one shared
	// directory, which is no isolation at all while looking like success.
	symlinked := info.Mode()&fs.ModeSymlink != 0

	if err := ctx.Err(); err != nil {
		return DepsNone, err
	}
	if dstInfo, err := os.Lstat(dst); err == nil {
		// Something is already there. A real directory is the worktree's own
		// (installed by an earlier step, or checked out from the snapshot's
		// untracked node_modules) and shares nothing. A symlink is not: git
		// recreates a committed link on checkout, so both worktrees end up
		// pointing at one directory — including the user's. Replace it with an
		// isolated copy rather than trusting its mere presence.
		if dstInfo.Mode()&fs.ModeSymlink == 0 {
			return DepsCOW, nil
		}
		if err := os.Remove(dst); err != nil {
			return DepsNone, fmt.Errorf("replacing symlinked node_modules at %s: %w", dst, err)
		}
	}

	// ① Copy-on-write: independent inodes, hardlink-class speed.
	mode, err := cowCopy(ctx, src, dst, symlinked)
	if err != nil {
		return DepsNone, err
	}
	if mode == DepsCOW {
		return DepsCOW, nil
	}

	// ② Hardlink fallback. For a symlinked source this would still share every
	// byte with the shared directory even after dereferencing, so decline
	// instead: an honest "not reused" beats a reuse that silently writes
	// through to a directory other checkouts also use.
	if symlinked {
		return DepsNone, nil
	}
	if err := ctx.Err(); err != nil {
		return DepsNone, err
	}
	_, stderr, err := runCommand(ctx, "", nil, "cp", "-al", src, dst)
	if err != nil {
		// Cancellation outranks the string classification below: the copy was
		// interrupted, so reporting "cross-device" would be wrong.
		if cerr := ctx.Err(); cerr != nil {
			return DepsNone, fmt.Errorf("cp -al canceled: %w", cerr)
		}
		msg := stderr
		if bytes.Contains([]byte(msg), []byte("cross-device")) || bytes.Contains([]byte(msg), []byte("EXDEV")) {
			return DepsNone, nil // cross-device: fall back to full install
		}
		if bytes.Contains([]byte(msg), []byte("No such file or directory")) || isNotFound(err) {
			return DepsNone, nil // cp missing/unavailable: full install
		}
		return DepsNone, fmt.Errorf("cp -al failed: %v: %s", err, msg)
	}
	// Break the inode sharing for the paths tooling actually rewrites in place.
	// Best effort: a failure here narrows isolation, it does not invalidate the
	// copy, and DepsHardlink already tells the caller to record a gap.
	delinkWritableState(ctx, dst)
	return DepsHardlink, nil
}

// cowCopy attempts a copy-on-write clone, returning DepsNone (without error)
// when the platform or filesystem does not support one.
//
// A symlinked source is resolved to the directory it points at and copied by
// content (trailing separator), never with cp's -L: dereferencing recursively
// would also flatten the relative links inside node_modules/.bin into plain
// files, and package managers need those to stay links. Resolving only the root
// gives an isolated tree whose internals are unchanged.
func cowCopy(ctx context.Context, src, dst string, symlinked bool) (DepsMode, error) {
	from := src
	if symlinked {
		resolved, err := filepath.EvalSymlinks(src)
		if err != nil {
			return DepsNone, nil // dangling link: nothing usable to reuse
		}
		from = resolved + string(filepath.Separator)
	}

	args := []string{"-c", "-R"} // darwin: clonefile, fail if unsupported
	if runtime.GOOS != "darwin" {
		args = []string{"-a", "--reflink=always"} // GNU coreutils
	}
	args = append(args, from, dst)

	if _, _, err := runCommand(ctx, "", nil, "cp", args...); err == nil {
		return DepsCOW, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return DepsNone, fmt.Errorf("cp clone canceled: %w", cerr)
	}
	// Unsupported filesystem / flag / missing cp: not an error, just no clone.
	// A partial destination would make the hardlink fallback fail on "already
	// exists", so clear it before falling through.
	if err := os.RemoveAll(dst); err != nil {
		return DepsNone, fmt.Errorf("clearing partial clone at %s: %w", dst, err)
	}
	return DepsNone, nil
}

// delinkWritableState replaces hardlinked copies of known in-place-rewritten
// paths with private ones: read the content, write a sibling temp file, rename
// over the original. The rename is what breaks the link — writing through the
// existing path would change the shared inode, which is the bug being fixed.
func delinkWritableState(ctx context.Context, nodeModules string) {
	for _, name := range writableStatePaths {
		if ctx.Err() != nil {
			return
		}
		path := filepath.Join(nodeModules, name)
		info, err := os.Lstat(path)
		if err != nil {
			continue // not present in this layout
		}
		if !info.IsDir() {
			if info.Mode().IsRegular() && info.Size() <= delinkBudget {
				_ = delinkFile(path, info)
			}
			continue
		}
		if size, err := treeSize(path, delinkBudget); err != nil || size > delinkBudget {
			continue // too large to un-share affordably; the DepsHardlink gap covers it
		}
		_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return nil //nolint:nilerr // a partially readable cache is still worth de-linking
			}
			if ctx.Err() != nil {
				return filepath.SkipAll
			}
			info, ierr := d.Info()
			if ierr != nil || !info.Mode().IsRegular() {
				return nil
			}
			_ = delinkFile(p, info)
			return nil
		})
	}
}

// delinkFile gives path its own inode while preserving content and mode.
func delinkFile(path string, info fs.FileInfo) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	tmp, err := os.CreateTemp(filepath.Dir(path), ".vouch-delink-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // renamed: nothing left to clean up
	return nil
}

// treeSize sums regular file sizes under root, stopping early once the limit is
// exceeded so an oversized cache is cheap to reject.
func treeSize(root string, limit int64) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil // skip entries that vanished mid-walk
		}
		if info.Mode().IsRegular() {
			total += info.Size()
			if total > limit {
				return errTreeTooLarge
			}
		}
		return nil
	})
	if errors.Is(err, errTreeTooLarge) {
		return total, nil
	}
	return total, err
}

var errTreeTooLarge = errors.New("tree exceeds budget")
