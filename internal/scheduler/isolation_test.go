package scheduler_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/scheduler"
)

// treeDigest fingerprints a directory's regular files by path and content. It
// follows the tree as-is (no symlink dereferencing at the root) so it can prove
// a shared fixture was left untouched by a run.
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	type entry struct{ path, sum string }
	var entries []entry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			return nil // non-regular entries carry no content to compare
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		entries = append(entries, entry{rel, hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	if err != nil {
		t.Fatalf("digest %s: %v", root, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s %s\n", e.path, e.sum)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestScheduler_BaseRunDoesNotPolluteCandidateOrRepo is the end-to-end form of
// the isolation guarantee. The test command rewrites a file inside
// node_modules in place — the write shape that crosses a hardlink — on every
// run. If the sides share that file, one run's write lands in the other's
// inputs and the delta grows a change nobody made.
//
// The repo's node_modules is a symlink to a shared directory, which is the
// layout pnpm and monorepos produce and the one this suite uses throughout: it
// is also the layout where a link-preserving copy would write straight through
// to a directory other checkouts depend on.
func TestScheduler_BaseRunDoesNotPolluteCandidateOrRepo(t *testing.T) {
	// A private stand-in for an installed dependency tree, shared by the repo
	// through a symlink exactly like the TypeScript fixtures do.
	shared := t.TempDir()
	if err := os.MkdirAll(filepath.Join(shared, ".vite"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(shared, ".vite", "deps.json")
	if err := os.WriteFile(stateFile, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	sharedBefore := treeDigest(t, shared)

	// A Go repo whose test also refreshes the dependency cache in place,
	// standing in for what a bundler or package manager does during a run.
	files := map[string]string{
		"go.mod":      "module example.com/iso\n\ngo 1.21\n",
		"add.go":      "package iso\n\nfunc Add(a, b int) int { return a + b }\n",
		"add_test.go": mutatingTest,
	}
	repo := scenarioRepoWithSetup(t, files, func(dir string) {
		if err := os.Symlink(shared, filepath.Join(dir, "node_modules")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	res := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if res.Verdict == bundle.Broken {
		t.Fatalf("unchanged repo must not be BROKEN: %+v", res.Evidences)
	}

	// The shared dependency tree the user's repo points at must be byte-identical.
	if got := treeDigest(t, shared); got != sharedBefore {
		t.Fatalf("a verify run mutated the shared node_modules: %s != %s", got, sharedBefore)
	}
	if got, err := os.ReadFile(stateFile); err != nil || string(got) != "original" {
		t.Fatalf("shared dependency state rewritten by the run: %q (%v)", got, err)
	}
}

// mutatingTest rewrites node_modules/.vite/deps.json through its own inode on
// every run, then passes. Writing in place (not temp+rename) is deliberate:
// that is the only write shape that crosses a hardlink.
const mutatingTest = `package iso

import (
	"os"
	"testing"
)

func TestAdd(t *testing.T) {
	if f, err := os.OpenFile("node_modules/.vite/deps.json", os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		_, _ = f.Write([]byte("mutated-by-probe"))
		_ = f.Close()
	}
	if Add(1, 2) != 3 {
		t.Fatal("bad sum")
	}
}
`

// TestScenarioFixturesAreNotMutated guards against the suite poisoning its own
// inputs: the TypeScript fixture's node_modules is shared by every scenario
// through a symlink, so one run writing into it would silently change what
// later runs measure.
func TestScenarioFixturesAreNotMutated(t *testing.T) {
	fixture := filepath.Join(repoRootOf(t), "testdata", "fixtures", "ts-vitest")
	nodeModules := filepath.Join(fixture, "node_modules")
	if _, err := os.Stat(nodeModules); err != nil {
		t.Skipf("fixture node_modules missing (run npm install in %s): %v", fixture, err)
	}

	before := treeDigest(t, nodeModules)

	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(fixture, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	files := map[string]string{
		"package.json":    read("package.json"),
		"tsconfig.json":   read("tsconfig.json"),
		"src/add.ts":      read("src/add.ts"),
		"src/add.test.ts": read("src/add.test.ts"),
	}
	repo := scenarioRepoWithSetup(t, files, func(dir string) {
		if err := os.Symlink(nodeModules, filepath.Join(dir, "node_modules")); err != nil {
			t.Fatal(err)
		}
	})
	run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})

	if after := treeDigest(t, nodeModules); after != before {
		t.Fatalf("a scheduler run modified the shared ts-vitest fixture:\n before %s\n after  %s", before, after)
	}
}
