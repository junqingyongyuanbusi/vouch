package selector_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/detector"
	"github.com/junqingyongyuanbusi/vouch/internal/selector"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func tsProfile() bundle.ProjectProfile {
	return bundle.ProjectProfile{
		Language:   []string{"typescript", "javascript"},
		Commands:   map[string]bundle.Command{"test": {Cmd: "vitest run", Source: "package.json#scripts.test"}},
		Confidence: bundle.ConfidenceHigh,
	}
}

func pyProfile() bundle.ProjectProfile {
	return bundle.ProjectProfile{
		Language:   []string{"python"},
		Commands:   map[string]bundle.Command{"test": {Cmd: "pytest -q", Source: "pyproject.toml#tool.pytest"}},
		Confidence: bundle.ConfidenceHigh,
	}
}

func goProfile() bundle.ProjectProfile {
	return bundle.ProjectProfile{
		Language:   []string{"go"},
		Commands:   map[string]bundle.Command{"test": {Cmd: "go test ./...", Source: "go.mod#default"}},
		Confidence: bundle.ConfidenceHigh,
	}
}

func TestSelect_TypeScript_VitestRelated(t *testing.T) {
	root := repoRoot(t)
	fixture := filepath.Join(root, "testdata/fixtures/ts-vitest")
	sel := selector.Select(fixture, []string{"src/add.ts"}, tsProfile())
	if sel.FullRun {
		t.Fatalf("expected narrowed selection, got full run: %s", sel.Reason)
	}
	if sel.Mechanism != "vitest related" {
		t.Fatalf("mechanism=%q", sel.Mechanism)
	}
	if len(sel.Targets) != 1 || sel.Targets[0] != "src/add.ts" {
		t.Fatalf("targets=%v", sel.Targets)
	}
	cmd := selector.Apply(tsProfile(), sel)
	if !strings.Contains(cmd, "vitest related src/add.ts") || !strings.Contains(cmd, "--run") {
		t.Fatalf("apply cmd=%q", cmd)
	}
}

func TestSelect_TypeScript_JestFallbackAndUnsupported(t *testing.T) {
	root := repoRoot(t)
	fixture := filepath.Join(root, "testdata/fixtures/ts-vitest")
	jest := tsProfile()
	jest.Commands["test"] = bundle.Command{Cmd: "jest", Source: "package.json#scripts.test"}
	sel := selector.Select(fixture, []string{"src/add.ts"}, jest)
	if sel.Mechanism != "jest --findRelatedTests" {
		t.Fatalf("mechanism=%q", sel.Mechanism)
	}
	if !strings.Contains(selector.Apply(jest, sel), "--findRelatedTests src/add.ts") {
		t.Fatalf("apply=%q", selector.Apply(jest, sel))
	}
	other := tsProfile()
	other.Commands["test"] = bundle.Command{Cmd: "mocha", Source: "package.json#scripts.test"}
	sel2 := selector.Select(fixture, []string{"src/add.ts"}, other)
	if !sel2.FullRun || sel2.Reason == "" {
		t.Fatalf("unsupported runner must fall back to FullRun with reason, got %+v", sel2)
	}
}

func TestSelect_Python_PathMap(t *testing.T) {
	root := repoRoot(t)
	fixture := filepath.Join(root, "testdata/fixtures/py-pytest")
	sel := selector.Select(fixture, []string{"src/add.py"}, pyProfile())
	if sel.FullRun {
		t.Fatalf("expected path map, got full run: %s", sel.Reason)
	}
	if sel.Mechanism != "pytest path map" || len(sel.Targets) != 1 || sel.Targets[0] != "tests/test_add.py" {
		t.Fatalf("selection=%+v", sel)
	}
	if cmd := selector.Apply(pyProfile(), sel); cmd != "pytest -q tests/test_add.py" {
		t.Fatalf("apply=%q", cmd)
	}
	// unmappable file → honest full run
	sel2 := selector.Select(fixture, []string{"src/nope.py"}, pyProfile())
	if !sel2.FullRun || !strings.Contains(sel2.Reason, "src/nope.py") {
		t.Fatalf("unmappable should full-run with file-named reason, got %+v", sel2)
	}
	// test file itself maps to itself
	sel3 := selector.Select(fixture, []string{"tests/test_add.py"}, pyProfile())
	if sel3.FullRun || sel3.Targets[0] != "tests/test_add.py" {
		t.Fatalf("test file should map to itself, got %+v", sel3)
	}
}

func TestSelect_Go_ImportGraph(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/graph\n\ngo 1.24\n")
	write("b/b.go", "package b\n\nfunc B() int { return 1 }\n")
	write("b/b_test.go", "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\n")
	write("a/a.go", "package a\n\nimport \"example.com/graph/b\"\n\nfunc A() int { return b.B() }\n")
	write("a/a_test.go", "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n")

	sel := selector.Select(dir, []string{"b/b.go"}, goProfile())
	if sel.FullRun {
		t.Fatalf("expected import-graph narrowing, got %s", sel.Reason)
	}
	if sel.Mechanism != "go list import graph" {
		t.Fatalf("mechanism=%q", sel.Mechanism)
	}
	got := strings.Join(sel.Targets, ",")
	// b changed → b and its importer a are affected
	if !strings.Contains(got, "./b") || !strings.Contains(got, "./a") {
		t.Fatalf("targets=%v", sel.Targets)
	}
	if cmd := selector.Apply(goProfile(), sel); !strings.HasPrefix(cmd, "go test ") || !strings.Contains(cmd, "./b") {
		t.Fatalf("apply=%q", cmd)
	}

	// unmapped (new file not in any package list) → full run
	write("c/new.go", "package c\n\nfunc C() {}\n")
	_ = exec.Command("go", "mod", "tidy").Run() // keep module tidy; best effort
	sel2 := selector.Select(dir, []string{"nonexistent.go"}, goProfile())
	if !sel2.FullRun {
		t.Fatalf("unmapped file must full-run, got %+v", sel2)
	}
}

func TestSelect_EmptyDiffAndUnsupported(t *testing.T) {
	sel := selector.Select(t.TempDir(), nil, tsProfile())
	if !sel.FullRun || sel.Mechanism != "empty diff" {
		t.Fatalf("empty diff: %+v", sel)
	}
	unknown := bundle.ProjectProfile{Language: []string{"haskell"}, Commands: map[string]bundle.Command{"test": {Cmd: "cabal test", Source: "x"}}}
	sel2 := selector.Select(t.TempDir(), []string{"Main.hs"}, unknown)
	if !sel2.FullRun || sel2.Reason == "" {
		t.Fatalf("unsupported ecosystem must full-run with reason: %+v", sel2)
	}
}

func TestProfileWithSelection(t *testing.T) {
	sel := selector.Selection{Targets: []string{"src/add.ts"}, Mechanism: "vitest related"}
	p := selector.NewProfileWithSelection(tsProfile(), sel)
	if p.TestSelection == nil || !p.TestSelection.Supported || p.TestSelection.Mechanism != "vitest related" {
		t.Fatalf("profile.test_selection=%+v", p.TestSelection)
	}
}

func TestApply_UsesRealDetectorCommandShape(t *testing.T) {
	// detector.parsePackageJSON emits the script body ("vitest run"), so Apply
	// must transform that exact shape — not a hypothetical "pnpm vitest run".
	root := repoRoot(t)
	fixture := filepath.Join(root, "testdata/fixtures/ts-vitest")
	profile, err := detector.Detect(fixture)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if got := profile.Commands["test"].Cmd; got != "vitest run" {
		t.Fatalf("fixture detector command=%q (test assumes script-body shape)", got)
	}
	sel := selector.Selection{Targets: []string{"src/a.ts"}, Mechanism: "vitest related"}
	if got := selector.Apply(profile, sel); got != "vitest related src/a.ts --run" {
		t.Fatalf("apply=%q", got)
	}
	// An explicit package-manager prefix (other detectors may emit it) is preserved.
	pmProfile := profile
	pmProfile.Commands["test"] = bundle.Command{Cmd: "pnpm vitest run", Source: "package.json#scripts.test"}
	if got := selector.Apply(pmProfile, sel); got != "pnpm vitest related src/a.ts --run" {
		t.Fatalf("pm apply=%q", got)
	}
}

func TestApply_NarrowsTheDetectedCommand(t *testing.T) {
	// Go: keep flags, swap the package pattern.
	goProf := goProfile()
	goProf.Commands["test"] = bundle.Command{Cmd: "go test -race -count=1 ./...", Source: "Makefile#test"}
	sel := selector.Selection{Targets: []string{"./b", "./a"}, Mechanism: "go list import graph"}
	if got := selector.Apply(goProf, sel); got != "go test -race -count=1 ./b ./a" {
		t.Fatalf("go apply=%q", got)
	}
	// Python: keep the detected interpreter (e.g. profile override) and append cases.
	py := pyProfile()
	py.Commands["test"] = bundle.Command{Cmd: "python3 -m pytest -q", Source: ".vouch/profile.json"}
	selPy := selector.Selection{Targets: []string{"tests/test_add.py"}, Mechanism: "pytest path map"}
	if got := selector.Apply(py, selPy); got != "python3 -m pytest -q tests/test_add.py" {
		t.Fatalf("pytest apply=%q", got)
	}
}
