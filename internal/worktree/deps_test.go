package worktree_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junqingyongyuanbusi/vouch/internal/worktree"
)

// writeInPlace rewrites an existing file through its own inode (O_WRONLY on the
// same path), which is what package managers and bundlers do when they refresh
// a cache entry. This is the only write shape that crosses a hardlink, so the
// isolation tests below must use it — a write-temp-then-rename would pass even
// against the unfixed implementation.
func writeInPlace(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open %s for in-place write: %v", path, err)
	}
	if _, err := f.Write([]byte("mutated")); err != nil {
		_ = f.Close()
		t.Fatalf("write %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

func readDepFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func writeDep(t *testing.T, root, rel, content string) string {
	t.Helper()
	path := filepath.Join(root, "node_modules", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReuseDeps_InPlaceWriteDoesNotLeak is the core isolation guarantee: a
// probe that rewrites a file inside its own node_modules must not be visible
// to the other side or to the user's repo.
//
// Under DepsCOW that holds for every file. Under the DepsHardlink fallback the
// inodes are shared and an arbitrary in-place rewrite does cross over — that is
// the documented limit of the mode, and the test asserts the honesty contract
// instead: the leak is only acceptable when reuse declared itself as hardlink.
// Silently leaking while reporting full isolation is the failure being guarded.
func TestReuseDeps_InPlaceWriteDoesNotLeak(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	srcFile := writeDep(t, src, filepath.Join("pkg", "state.json"), "original")

	mode, err := worktree.ReuseDepsContext(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("ReuseDepsContext: %v", err)
	}
	if mode == worktree.DepsNone {
		t.Fatal("same-filesystem reuse must not fall back to a full install")
	}

	dstFile := filepath.Join(dst, "node_modules", "pkg", "state.json")
	writeInPlace(t, dstFile)

	if got := readDepFile(t, dstFile); got != "mutated" {
		t.Fatalf("destination did not take the write: %q", got)
	}
	got := readDepFile(t, srcFile)
	if mode == worktree.DepsCOW && got != "original" {
		t.Fatalf("DepsCOW promises private writes, but the source changed to %q", got)
	}
	if mode == worktree.DepsHardlink && got == "original" {
		t.Skip("hardlink fallback did not share this inode; nothing to assert")
	}
}

// TestReuseDeps_KnownCachePathIsDelinked covers the paths tooling is known to
// rewrite in place (Vite's dep cache, pnpm/yarn state files). These must be
// isolated even when the filesystem cannot clone, because they are the ones
// that actually get written during a probe run.
func TestReuseDeps_KnownCachePathIsDelinked(t *testing.T) {
	for _, rel := range []string{
		filepath.Join(".vite", "deps.json"),
		filepath.Join(".cache", "entry"),
		".modules.yaml",
	} {
		t.Run(rel, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			srcFile := writeDep(t, src, rel, "original")
			// A plain dependency file alongside it, so the test cannot pass by
			// accident through a full (non-hardlink) copy of everything.
			writeDep(t, src, filepath.Join("pkg", "index.js"), "x")

			mode, err := worktree.ReuseDepsContext(context.Background(), src, dst)
			if err != nil {
				t.Fatalf("ReuseDepsContext: %v", err)
			}
			if mode == worktree.DepsNone {
				t.Fatal("same-filesystem reuse must not fall back to a full install")
			}

			writeInPlace(t, filepath.Join(dst, "node_modules", rel))

			if got := readDepFile(t, srcFile); got != "original" {
				t.Fatalf("writable state path %s leaked into the source: %q (mode=%s)", rel, got, mode)
			}
		})
	}
}

// TestReuseDeps_SymlinkedNodeModulesIsNotFakeReuse guards the worst case: when
// the repo's node_modules is a symlink (pnpm layouts, monorepos, and this
// project's own TypeScript fixtures), a copy that preserves the link leaves
// both worktrees pointing at one shared directory. That is zero isolation
// reported as success. Either the copy dereferences into a real, isolated tree,
// or reuse must decline and say so.
func TestReuseDeps_SymlinkedNodeModulesIsNotFakeReuse(t *testing.T) {
	shared := t.TempDir()
	if err := os.MkdirAll(filepath.Join(shared, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	sharedFile := filepath.Join(shared, "pkg", "state.json")
	if err := os.WriteFile(sharedFile, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	src, dst := t.TempDir(), t.TempDir()
	if err := os.Symlink(shared, filepath.Join(src, "node_modules")); err != nil {
		t.Fatal(err)
	}

	mode, err := worktree.ReuseDepsContext(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("ReuseDepsContext: %v", err)
	}

	if mode == worktree.DepsNone {
		return // declined: honest, probes will install their own deps
	}

	dstLink := filepath.Join(dst, "node_modules")
	if fi, lerr := os.Lstat(dstLink); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("reuse reported %s but the destination is still a symlink: zero isolation", mode)
	}
	writeInPlace(t, filepath.Join(dstLink, "pkg", "state.json"))
	if got := readDepFile(t, sharedFile); got != "original" {
		t.Fatalf("reuse reported %s but writes reach the shared directory: %q", mode, got)
	}
}

// TestReuseDeps_StillFastOnSameFilesystem pins the speed side of the trade:
// isolation must not be bought by degrading every same-filesystem reuse into a
// full install.
func TestReuseDeps_StillFastOnSameFilesystem(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeDep(t, src, filepath.Join("pkg", "index.js"), "x")

	mode, err := worktree.ReuseDepsContext(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("ReuseDepsContext: %v", err)
	}
	if mode == worktree.DepsNone {
		t.Fatal("a plain same-filesystem node_modules must be reused, not reinstalled")
	}
	if _, err := os.Stat(filepath.Join(dst, "node_modules", "pkg", "index.js")); err != nil {
		t.Fatalf("reuse reported %s but the dependency is missing: %v", mode, err)
	}
}

// TestReuseDeps_NoSourceIsNotAnError keeps the documented no-op: a repo without
// node_modules is not a failure, it just means probes install their own.
func TestReuseDeps_NoSourceIsNotAnError(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	mode, err := worktree.ReuseDepsContext(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("ReuseDepsContext: %v", err)
	}
	if mode != worktree.DepsNone {
		t.Fatalf("missing node_modules must report DepsNone, got %s", mode)
	}
}

// TestReuseDeps_HardlinkFallbackIsDeclared forces the no-clone path (by shimming
// cp to reject the clone flags) and pins the honesty contract: when isolation
// degrades to shared inodes, the pair must say so in DepsGaps. A silent
// downgrade would let a cross-side write look like a real regression.
func TestReuseDeps_HardlinkFallbackIsDeclared(t *testing.T) {
	realCp, err := exec.LookPath("cp")
	if err != nil {
		t.Skipf("cp not on PATH: %v", err)
	}
	shim := t.TempDir()
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  case \"$a\" in -c|--reflink=always) echo 'cp: clone not supported' >&2; exit 1;; esac\n" +
		"done\n" +
		"exec " + shQuote(realCp) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "cp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prependPath(t, shim)

	src, dst := t.TempDir(), t.TempDir()
	cacheFile := writeDep(t, src, filepath.Join(".vite", "deps.json"), "original")
	writeDep(t, src, filepath.Join("pkg", "index.js"), "x")

	mode, err := worktree.ReuseDepsContext(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("ReuseDepsContext: %v", err)
	}
	if mode != worktree.DepsHardlink {
		t.Fatalf("clone rejected, so reuse must report DepsHardlink, got %s", mode)
	}
	// Even in the degraded mode the known writable state paths are de-linked:
	// that is what keeps ordinary runs from polluting each other.
	writeInPlace(t, filepath.Join(dst, "node_modules", ".vite", "deps.json"))
	if got := readDepFile(t, cacheFile); got != "original" {
		t.Fatalf(".vite cache leaked under the hardlink fallback: %q", got)
	}
}

// TestCreate_HardlinkModeSurfacesGap is the end-to-end half of the contract:
// the degraded mode must reach the caller as a gap, not just as an enum value
// nobody reads.
func TestCreate_HardlinkModeSurfacesGap(t *testing.T) {
	realCp, err := exec.LookPath("cp")
	if err != nil {
		t.Skipf("cp not on PATH: %v", err)
	}
	shim := t.TempDir()
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  case \"$a\" in -c|--reflink=always) echo 'cp: clone not supported' >&2; exit 1;; esac\n" +
		"done\n" +
		"exec " + shQuote(realCp) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "cp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prependPath(t, shim)

	dir := newRepo(t)
	writeDep(t, dir, filepath.Join("pkg", "index.js"), "x")

	pair, err := worktree.CreateContext(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatalf("CreateContext: %v", err)
	}
	defer func() { _ = pair.Cleanup() }()

	if pair.DepsMode != worktree.DepsHardlink {
		t.Fatalf("clone rejected, so the pair must report DepsHardlink, got %s", pair.DepsMode)
	}
	var declared bool
	for _, gap := range pair.DepsGaps {
		if strings.Contains(gap, "hardlink") {
			declared = true
			break
		}
	}
	if !declared {
		t.Fatalf("hardlink reuse shares inodes but no gap declares it: %v", pair.DepsGaps)
	}
}
