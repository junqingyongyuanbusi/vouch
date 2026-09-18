package worktree_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junqingyongyuanbusi/vouch/internal/worktree"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newRepo creates a temp git repo with one commit.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "test@test")
	git(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "init")
	return dir
}

func TestSnapshot_CleanReturnsEmpty(t *testing.T) {
	dir := newRepo(t)
	snap, err := worktree.Snapshot(dir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap != "" {
		t.Fatalf("clean repo should snapshot to empty, got %q", snap)
	}
}

func TestSnapshot_UntrackedAndBinary(t *testing.T) {
	dir := newRepo(t)
	// dirty tracked
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("base\ndirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// untracked
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// binary
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{0x00, 0x01, 0x02, 0xff}, 0o644); err != nil {
		t.Fatal(err)
	}
	// ignored file must NOT be in snapshot
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap, err := worktree.Snapshot(dir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap == "" {
		t.Fatal("dirty repo must produce a snapshot commit")
	}
	// Verify snapshot tree content
	out := exec.Command("git", "ls-tree", "-r", "--name-only", snap)
	out.Dir = dir
	b, err := out.Output()
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	names := string(b)
	for _, want := range []string{"a.txt", "untracked.txt", "bin.dat", ".gitignore"} {
		if !containsLine(names, want) {
			t.Fatalf("snapshot missing %q:\n%s", want, names)
		}
	}
	if containsLine(names, "ignored.txt") {
		t.Fatalf("snapshot must not contain ignored file:\n%s", names)
	}
	// User state must be untouched: index still clean for tracked file
	st := exec.Command("git", "status", "--porcelain")
	st.Dir = dir
	sb, _ := st.Output()
	if !strings.Contains(string(sb), "M a.txt") {
		t.Fatalf("user index/worktree modified by Snapshot:\n%s", sb)
	}
}

func TestCreate_BaseAndCandidate(t *testing.T) {
	dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("base\ndirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pair, err := worktree.Create(dir, "HEAD")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = pair.Cleanup() }()

	// base: original content, no untracked
	baseA, err := os.ReadFile(filepath.Join(pair.Base, "a.txt"))
	if err != nil {
		t.Fatalf("base a.txt: %v", err)
	}
	if string(baseA) != "base\n" {
		t.Fatalf("base should have original content, got %q", baseA)
	}
	if _, err := os.Stat(filepath.Join(pair.Base, "untracked.txt")); err == nil {
		t.Fatal("base must not contain untracked file")
	}
	// candidate: dirty content + untracked present (the P1.2 regression this prevents)
	candA, err := os.ReadFile(filepath.Join(pair.Candidate, "a.txt"))
	if err != nil {
		t.Fatalf("candidate a.txt: %v", err)
	}
	if string(candA) != "base\ndirty\n" {
		t.Fatalf("candidate should have dirty content, got %q", candA)
	}
	if _, err := os.Stat(filepath.Join(pair.Candidate, "untracked.txt")); err != nil {
		t.Fatalf("candidate must contain untracked file: %v", err)
	}
	if pair.Snapshot == "" {
		t.Fatal("pair.Snapshot should be non-empty for dirty repo")
	}

	// Cleanup removes registrations
	if err := pair.Cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	out := exec.Command("git", "worktree", "list", "--porcelain")
	out.Dir = dir
	b, _ := out.Output()
	if containsLine(string(b), pair.Base) || containsLine(string(b), pair.Candidate) {
		t.Fatalf("worktrees still registered after cleanup:\n%s", b)
	}
}

func containsLine(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func TestCreate_PinsSnapshotRef(t *testing.T) {
	dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pair, err := worktree.Create(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pair.Cleanup() }()
	if pair.Snapshot == "" || pair.SnapshotRef == "" {
		t.Fatalf("dirty repo must pin a snapshot ref: %+v", pair)
	}
	out := exec.Command("git", "rev-parse", pair.SnapshotRef)
	out.Dir = dir
	got, err := out.Output()
	if err != nil {
		t.Fatalf("snapshot ref not resolvable after Create: %v", err)
	}
	if strings.TrimSpace(string(got)) != pair.Snapshot {
		t.Fatalf("ref points elsewhere: %s vs %s", got, pair.Snapshot)
	}
	// After cleanup the ref must still resolve (durability for `vouch rerun`).
	if err := pair.Cleanup(); err != nil {
		t.Fatal(err)
	}
	out2 := exec.Command("git", "rev-parse", pair.SnapshotRef)
	out2.Dir = dir
	if _, err := out2.Output(); err != nil {
		t.Fatalf("snapshot must survive cleanup for reproduce: %v", err)
	}
}

func TestPruneSnapshotRefs(t *testing.T) {
	dir := newRepo(t)
	// create 3 snapshots with distinct content
	for i := 0; i < 3; i++ {
		content := "dirty-" + string(rune('a'+i)) + "\n"
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		pair, err := worktree.Create(dir, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if err := pair.Cleanup(); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		out := exec.Command("git", "for-each-ref", "--format=%(refname)", worktree.SnapshotRefPrefix)
		out.Dir = dir
		b, _ := out.Output()
		n := 0
		for _, l := range splitLinesTests(string(b)) {
			if l != "" {
				n++
			}
		}
		return n
	}
	if got := count(); got < 2 {
		t.Fatalf("expected multiple snapshot refs, got %d", got)
	}
	removed, err := worktree.PruneSnapshotRefs(dir, 1)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed == 0 {
		t.Fatal("prune must remove older refs")
	}
	if got := count(); got != 1 {
		t.Fatalf("prune must keep exactly 1 ref, got %d", got)
	}
}

func splitLinesTests(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		out = append(out, strings.TrimSpace(l))
	}
	return out
}
