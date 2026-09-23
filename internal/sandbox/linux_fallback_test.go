//go:build linux

package sandbox_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/sandbox"
)

// TestSandbox_UsernsRestrictedFallback covers the failure shape seen on
// GitHub's ubuntu-24.04 runners: AppArmor blocks unprivileged user namespaces
// and util-linux 2.39+ unshare dies with "unshare: write failed
// /proc/self/uid_map: Operation not permitted". The launcher failed and the
// wrapped command never ran, so Run must fall back to an unisolated run —
// reporting that failure as a failed command turned eight probe tests red.
//
// Linux-only because macOS routes through sandbox-exec and never executes
// unshare (see the corresponding table test for the launcher-shape rules,
// which run on every platform).
func TestSandbox_UsernsRestrictedFallback(t *testing.T) {
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare not installed")
	}
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho 'unshare: write failed /proc/self/uid_map: Operation not permitted' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "unshare"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := sandbox.Run(context.Background(), dir, "echo fell-back", sandbox.Policy{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Sandboxed {
		t.Fatalf("expected the unisolated fallback, got reason=%s", res.Reason)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "fell-back") {
		t.Fatalf("fallback must run the command, got %+v", res)
	}
	if !strings.Contains(res.Reason, "isolation unavailable") {
		t.Fatalf("Reason must explain the fallback, got %q", res.Reason)
	}
}
