package worktree

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// ReuseDeps hardlinks node_modules from src to dst via cp -al (same filesystem).
//
// Returns:
//   - (true, nil)  hardlink copy succeeded, or dst already present
//   - (false, nil) nothing to reuse / cross-device / cp unavailable → full install
//   - (false, err) unexpected failure (surfaced so callers can record a gap)
//
// ReuseDeps is ReuseDepsContext with a background context.
func ReuseDeps(srcWorktree, dstWorktree string) (bool, error) {
	return ReuseDepsContext(context.Background(), srcWorktree, dstWorktree)
}

// ReuseDepsContext is ReuseDeps with caller cancellation. The cp runs in its own
// process group, killed as a whole on cancellation and reaped before return, so
// a canceled reuse cannot leave a cp writing into the destination worktree
// after the call has returned.
func ReuseDepsContext(ctx context.Context, srcWorktree, dstWorktree string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	src := filepath.Join(srcWorktree, "node_modules")
	dst := filepath.Join(dstWorktree, "node_modules")
	if _, err := os.Stat(src); err != nil {
		return false, nil // nothing to reuse → full install
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := os.Stat(dst); err == nil {
		return true, nil // already present (e.g. installed by an earlier step)
	}
	_, stderr, err := runCommand(ctx, "", nil, "cp", "-al", src, dst)
	if err != nil {
		// Cancellation outranks the string classification below: the copy was
		// interrupted, so reporting "cross-device" would be wrong.
		if cerr := ctx.Err(); cerr != nil {
			return false, fmt.Errorf("cp -al canceled: %w", cerr)
		}
		msg := stderr
		if bytes.Contains([]byte(msg), []byte("cross-device")) || bytes.Contains([]byte(msg), []byte("EXDEV")) {
			return false, nil // cross-device: fall back to full install
		}
		if bytes.Contains([]byte(msg), []byte("No such file or directory")) || isNotFound(err) {
			return false, nil // cp missing/unavailable: full install
		}
		return false, fmt.Errorf("cp -al failed: %v: %s", err, msg)
	}
	return true, nil
}
