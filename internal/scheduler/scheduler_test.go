package scheduler_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/scheduler"
)

// scenarioRepo builds a git repo from an inline file set, commits it, then
// applies candidate mutations to the working tree.
func scenarioRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "t@t")
	gitRun(t, dir, "config", "user.name", "t")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "base")
	return dir
}

// scenarioRepoWithSetup is scenarioRepo plus a hook that runs before the initial
// commit (e.g. staging node_modules so both worktrees can run the runner).
func scenarioRepoWithSetup(t *testing.T, files map[string]string, setup func(dir string)) string {
	t.Helper()
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if setup != nil {
		setup(dir)
	}
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "t@t")
	gitRun(t, dir, "config", "user.name", "t")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "base")
	return dir
}

// stageNodeModules puts a REAL node_modules into the scenario repo and keeps
// it out of git. The earlier symlink approach was dereferenced by the deps
// isolator and, on reflink-less filesystems (ext4), its reuse was declined
// outright — the worktrees ended up without a runner and every TypeScript
// scenario reported "runner not available" on Linux. A real directory takes
// the normal reuse path on every platform (COW on APFS, hardlinks on ext4).
func stageNodeModules(t *testing.T, fixture, dir string) {
	t.Helper()
	gitignore := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gitignore, []byte("node_modules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// cp -R (not os.CopyFS): the fixture tree is full of relative symlinks
	// (node_modules/.bin) and os.CopyFS cannot recreate symlinks on the
	// go1.24 toolchain (ReadLinkFS support landed in 1.25 — it fails with
	// "CopyFS .bin/<tool>: invalid argument"). cp preserves symlinks by
	// default on both BSD and GNU.
	if out, err := exec.Command("cp", "-R", "-p",
		filepath.Join(fixture, "node_modules"),
		filepath.Join(dir, "node_modules"),
	).CombinedOutput(); err != nil {
		t.Fatalf("copy node_modules: %v\n%s", err, out)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func goFiles() map[string]string {
	return map[string]string{
		"go.mod":       "module example.com/fixture\n\ngo 1.24\n",
		"calc.go":      "package fixture\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"calc_test.go": "package fixture\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatalf(\"Add(1,2) = %d, want 3\", Add(1, 2))\n\t}\n}\n",
	}
}

func run(t *testing.T, repo string, cfg scheduler.Config) scheduler.Result {
	t.Helper()
	cfg.RepoRoot = repo
	if cfg.ProbeBudgetMs == nil {
		cfg.ProbeBudgetMs = map[string]int{"test": 60000, "build": 60000, "typecheck": 30000}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := scheduler.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("scheduler.Run: %v", err)
	}
	return res
}

// ---------- Go scenarios ----------

func TestScenarioGo_NoChangeVerified(t *testing.T) {
	repo := scenarioRepo(t, goFiles())
	res := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if res.Verdict != bundle.Verified {
		t.Fatalf("verdict=%s warnings=%v unverified=%v", res.Verdict, res.Warnings, res.Unverified)
	}
	if len(res.Evidences) == 0 {
		t.Fatal("expected test evidence")
	}
	if res.Bundle.BaselineSummary == nil {
		t.Fatal("VERIFIED must carry baseline_summary")
	}
}

func TestScenarioGo_InjectedRegressionBroken(t *testing.T) {
	repo := scenarioRepo(t, goFiles())
	write(t, repo, "calc.go", "package fixture\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n")
	res := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if res.Verdict != bundle.Broken {
		t.Fatalf("verdict=%s ev=%+v", res.Verdict, res.Evidences)
	}
	if len(res.Evidences[0].Delta.Regressions) == 0 {
		t.Fatalf("regressions must be recorded: %+v", res.Evidences[0].Delta)
	}
}

func TestScenarioGo_RemovedFailingTestIsVerifiedWithWarning(t *testing.T) {
	// base has a failing test (existing failure), candidate deletes it: no
	// regression on diff semantics, but the deletion must be surfaced.
	files := goFiles()
	files["extra_test.go"] = "package fixture\n\nimport \"testing\"\n\nfunc TestKnownBad(t *testing.T) {\n\tt.Fatal(\"pre-existing failure\")\n}\n"
	repo := scenarioRepo(t, files)
	if err := os.Remove(filepath.Join(repo, "extra_test.go")); err != nil {
		t.Fatal(err)
	}
	res := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if res.Verdict != bundle.Verified {
		t.Fatalf("verdict=%s (deleting a base-failing test is not a regression)", res.Verdict)
	}
	joined := strings.Join(res.Warnings, "\n")
	if !strings.Contains(joined, "removed_tests") {
		t.Fatalf("removed_tests warning missing: warnings=%v delta=%+v", res.Warnings, res.Evidences[0].Delta)
	}
	if res.Evidences[0].Baseline.Fail == 0 {
		t.Fatalf("baseline failure must be recorded: %+v", res.Evidences[0].Baseline)
	}
}

func TestScenarioGo_BaseCacheHitSkipsBase(t *testing.T) {
	repo := scenarioRepo(t, goFiles())
	first := run(t, repo, scheduler.Config{BaseRef: "HEAD"})
	if first.Verdict != bundle.Verified {
		t.Fatalf("first verdict=%s", first.Verdict)
	}
	// second run: base side must come from cache and still produce the same verdict
	second := run(t, repo, scheduler.Config{BaseRef: "HEAD"})
	if second.Verdict != bundle.Verified {
		t.Fatalf("second verdict=%s", second.Verdict)
	}
	// A cache file must exist under .vouch/cache/baseline
	entries, err := os.ReadDir(filepath.Join(repo, ".vouch", "cache", "baseline"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("baseline cache not written: err=%v entries=%d", err, len(entries))
	}
}

// ---------- Python scenario ----------

func TestScenarioPython_RegressionAndClean(t *testing.T) {
	if cmd, ok := pythonTestCommand(); !ok {
		t.Skip("pytest unavailable (neither pytest nor python3 -m pytest)")
	} else if !strings.HasPrefix(cmd, "python3") {
		_ = cmd
	}
	files := map[string]string{
		"pyproject.toml":    "[tool.pytest.ini_options]\ntestpaths=[\"tests\"]\n",
		"src/__init__.py":   "",
		"src/add.py":        "def add(a, b):\n    return a + b\n",
		"tests/__init__.py": "",
		"tests/test_add.py": "from src.add import add\n\n\ndef test_add():\n    assert add(1, 2) == 3\n",
	}
	repo := scenarioRepo(t, files)
	clean := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if clean.Verdict != bundle.Verified {
		t.Fatalf("clean verdict=%s warnings=%v unverified=%v", clean.Verdict, clean.Warnings, clean.Unverified)
	}
	write(t, repo, "src/add.py", "def add(a, b):\n    return a - b\n")
	broken := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if broken.Verdict != bundle.Broken {
		t.Fatalf("broken verdict=%s ev=%+v", broken.Verdict, broken.Evidences)
	}
	if !strings.Contains(broken.Evidences[0].Delta.Regressions[0], "test_add.py::test_add") {
		t.Fatalf("regression must name the case: %+v", broken.Evidences[0].Delta.Regressions)
	}
}

// ---------- TypeScript scenario ----------

func TestScenarioTypeScript_RegressionAndClean(t *testing.T) {
	root := repoRootOf(t)
	fixture := filepath.Join(root, "testdata/fixtures/ts-vitest")
	if _, err := os.Stat(filepath.Join(fixture, "node_modules")); err != nil {
		t.Skip("ts fixture node_modules not installed (CI installs it)")
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(fixture, rel))
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
		stageNodeModules(t, fixture, dir)
	})
	clean := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if clean.Verdict != bundle.Verified {
		t.Fatalf("clean verdict=%s ev=%+v unverified=%v", clean.Verdict, clean.Evidences, clean.Unverified)
	}
	write(t, repo, "src/add.ts", "export function add(a: number, b: number) {\n  return a - b\n}\n")
	broken := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if broken.Verdict != bundle.Broken {
		t.Fatalf("broken verdict=%s ev=%+v", broken.Verdict, broken.Evidences)
	}
}

func repoRootOf(t *testing.T) string {
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

// TestScenarioGo_FlakyAbsorbedViaRerun is the P1.5 "introduced flaky" case:
// the candidate's first execution of one case fails, its rerun (driven by
// differ.Arbitrate through rerun.go) passes → absorbed, not BROKEN.
func TestScenarioGo_FlakyAbsorbedViaRerun(t *testing.T) {
	t.Setenv("VOUCH_FLAKY_TOKEN", fmt.Sprintf("%s-%d", t.Name(), os.Getpid()))
	files := map[string]string{
		"go.mod":  "module example.com/fixture\n\ngo 1.24\n",
		"calc.go": "package fixture\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"flaky_test.go": `package fixture

import (
	"os"
	"path/filepath"
	"testing"
)

// The base worktree always passes; the candidate fails on its first execution
// (marker absent) and passes afterwards — a deterministic flake simulation.
func TestFlaky(t *testing.T) {
	wd, _ := os.Getwd()
	if filepath.Base(wd) == "base" {
		return
	}
	marker := filepath.Join(os.TempDir(), "vouch-flaky-"+os.Getenv("VOUCH_FLAKY_TOKEN")+"-"+filepath.Base(filepath.Dir(wd)))
	if _, err := os.Stat(marker); err != nil {
		_ = os.WriteFile(marker, []byte("1"), 0o644)
		t.Fatal("simulated flake (first run in this worktree)")
	}
}
`,
		"calc_test.go": "package fixture\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatalf(\"bad\")\n\t}\n}\n",
	}
	repo := scenarioRepo(t, files)
	res := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if res.Verdict != bundle.Unverified {
		t.Fatalf("single-side instability must remain UNVERIFIED, got %s", res.Verdict)
	}
	for _, ev := range res.Evidences {
		if len(ev.Delta.FlakyAbsorbed) != 0 {
			t.Fatalf("single-side instability must not be absorbed: %+v", ev)
		}
	}
	if len(res.Unverified) == 0 {
		t.Fatal("unresolved instability must include an explanation")
	}
}

// ---------- pure addition ----------

func TestScenarioGo_PureAdditionVerified(t *testing.T) {
	repo := scenarioRepo(t, goFiles())
	write(t, repo, "mul.go", "package fixture\n\nfunc Mul(a, b int) int {\n\treturn a * b\n}\n")
	write(t, repo, "mul_test.go", "package fixture\n\nimport \"testing\"\n\nfunc TestMul(t *testing.T) {\n\tif Mul(2, 3) != 6 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n")
	res := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if res.Verdict != bundle.Verified {
		t.Fatalf("pure addition must verify: %s warnings=%v unverified=%v", res.Verdict, res.Warnings, res.Unverified)
	}
	if len(res.Evidences) == 0 || len(res.Evidences[0].Delta.NewPassing) == 0 {
		t.Fatalf("new passing cases must be recorded: %+v", res.Evidences)
	}
}

func TestScenarioGo_CacheMissAfterNewCommit(t *testing.T) {
	repo := scenarioRepo(t, goFiles())
	if res := run(t, repo, scheduler.Config{BaseRef: "HEAD"}); res.Verdict != bundle.Verified {
		t.Fatalf("first run: %s", res.Verdict)
	}
	// A new commit moves HEAD: the cache key must change with it.
	write(t, repo, "note.txt", "hello\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "advance")
	if res := run(t, repo, scheduler.Config{BaseRef: "HEAD"}); res.Verdict != bundle.Verified {
		t.Fatalf("second run: %s", res.Verdict)
	}
	entries, err := os.ReadDir(filepath.Join(repo, ".vouch", "cache", "baseline"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("new commit must produce a distinct baseline cache entry, got %d", len(entries))
	}
}

func TestScheduler_ZeroEvidenceHasNoAllGreenBaseline(t *testing.T) {
	// A repo with no detectable commands: zero evidence → UNVERIFIED with no
	// baseline claim at all.
	repo := scenarioRepo(t, map[string]string{"README.md": "no build system here\n"})
	cfg := scheduler.Config{RepoRoot: repo, BaseRef: "HEAD", DisableCache: true,
		ProbeBudgetMs: map[string]int{"test": 5000, "build": 5000, "typecheck": 5000}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := scheduler.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Verdict != bundle.Unverified {
		t.Fatalf("verdict=%s", res.Verdict)
	}
	if res.Bundle.BaselineSummary != nil && res.Bundle.BaselineSummary.Status == bundle.BaselineAllGreen {
		t.Fatalf("zero evidence must not claim all_green: %+v", res.Bundle.BaselineSummary)
	}
}

// TestScheduler_DiffIdentity_IndependentOfCwd locks the evidence identity:
// diff files / diff sha / bundle id must describe the target repo even when the
// process cwd is a different repository.
func TestScheduler_DiffIdentity_IndependentOfCwd(t *testing.T) {
	repo := scenarioRepo(t, goFiles())
	clean := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})

	// pure untracked addition: must change diff identity
	write(t, repo, "mul.go", "package fixture\n\nfunc Mul(a, b int) int { return a * b }\n")
	added := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})

	if added.Bundle.Subject.DiffSHA256 == clean.Bundle.Subject.DiffSHA256 {
		t.Fatalf("untracked addition must change diff identity (clean=%s added=%s)",
			clean.Bundle.Subject.DiffSHA256, added.Bundle.Subject.DiffSHA256)
	}
	if added.Bundle.BundleID == clean.Bundle.BundleID {
		t.Fatalf("untracked addition must not reuse the empty-diff bundle id: %s", added.Bundle.BundleID)
	}
	sel := added.Selection
	if sel.FullRun && len(sel.Targets) == 0 {
		t.Fatalf("addition should be visible to the selector, got FullRun reason=%q", sel.Reason)
	}
	if added.Bundle.Subject.CandidateRef == nil || *added.Bundle.Subject.CandidateRef == "" {
		t.Fatal("bundle must record the candidate snapshot ref for rerun")
	}
}

func TestScenarioPython_PureAdditionAndRemovedTest(t *testing.T) {
	if cmd, ok := pythonTestCommand(); !ok {
		t.Skip("pytest unavailable (neither pytest nor python3 -m pytest)")
	} else if !strings.HasPrefix(cmd, "python3") {
		_ = cmd
	}
	base := map[string]string{
		"pyproject.toml":    "[tool.pytest.ini_options]\ntestpaths=[\"tests\"]\n",
		"src/__init__.py":   "",
		"src/add.py":        "def add(a, b):\n    return a + b\n",
		"tests/__init__.py": "",
		"tests/test_add.py": "from src.add import add\n\n\ndef test_add():\n    assert add(1, 2) == 3\n",
	}
	repo := scenarioRepo(t, base)
	// pure addition: new module + new test
	write(t, repo, "src/mul.py", "def mul(a, b):\n    return a * b\n")
	write(t, repo, "tests/test_mul.py", "from src.mul import mul\n\n\ndef test_mul():\n    assert mul(2, 3) == 6\n")
	addRes := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if addRes.Verdict != bundle.Verified {
		t.Fatalf("pure addition must verify: %s unverified=%v", addRes.Verdict, addRes.Unverified)
	}
	if addRes.Bundle.Subject.CandidateRef == nil {
		t.Fatal("candidate ref must be recorded")
	}
	// A newly added test must never look like someone deleted the old ones.
	if joined := strings.Join(addRes.Warnings, "\n"); strings.Contains(joined, "removed_tests") {
		t.Fatalf("pure addition must not warn about removed_tests: %v", addRes.Warnings)
	}
	for _, ev := range addRes.Evidences {
		if len(ev.Delta.RemovedTests) != 0 {
			t.Fatalf("pure addition must not report removed tests: %+v", ev.Delta)
		}
	}

	// removed failing test: base has a failing test, candidate deletes it
	repo2 := scenarioRepo(t, map[string]string{
		"pyproject.toml":          "[tool.pytest.ini_options]\ntestpaths=[\"tests\"]\n",
		"src/__init__.py":         "",
		"src/add.py":              "def add(a, b):\n    return a + b\n",
		"tests/__init__.py":       "",
		"tests/test_add.py":       "from src.add import add\n\n\ndef test_add():\n    assert add(1, 2) == 3\n",
		"tests/test_known_bad.py": "def test_known_bad():\n    assert False, 'pre-existing failure'\n",
	})
	if err := os.Remove(filepath.Join(repo2, "tests", "test_known_bad.py")); err != nil {
		t.Fatal(err)
	}
	removed := run(t, repo2, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if removed.Verdict != bundle.Verified {
		t.Fatalf("removing a base-failing test is not a regression: %s ev=%+v", removed.Verdict, removed.Evidences)
	}
	joined := strings.Join(removed.Warnings, "\n")
	if !strings.Contains(joined, "removed_tests") {
		t.Fatalf("removed_tests warning missing: %v delta=%+v", removed.Warnings, removed.Evidences[0].Delta)
	}
}

func TestScenarioTypeScript_PureAdditionAndFlaky(t *testing.T) {
	root := repoRootOf(t)
	fixture := filepath.Join(root, "testdata/fixtures/ts-vitest")
	if _, err := os.Stat(filepath.Join(fixture, "node_modules")); err != nil {
		t.Skip("ts fixture node_modules not installed (CI installs it)")
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(fixture, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	repo := scenarioRepoWithSetup(t, map[string]string{
		"package.json":    read("package.json"),
		"tsconfig.json":   read("tsconfig.json"),
		"src/add.ts":      read("src/add.ts"),
		"src/add.test.ts": read("src/add.test.ts"),
	}, func(dir string) {
		stageNodeModules(t, fixture, dir)
	})
	// pure addition
	write(t, repo, "src/mul.ts", "export function mul(a: number, b: number) {\n  return a * b\n}\n")
	write(t, repo, "src/mul.test.ts", "import { describe, it, expect } from \"vitest\"\nimport { mul } from \"./mul.js\"\ndescribe(\"mul\", () => { it(\"multiplies\", () => expect(mul(2, 3)).toBe(6)) })\n")
	addRes := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if addRes.Verdict != bundle.Verified {
		t.Fatalf("ts pure addition must verify: %s ev=%+v unverified=%v", addRes.Verdict, addRes.Evidences, addRes.Unverified)
	}
}

func TestCaseCommand_PerRunnerFiltering(t *testing.T) {
	goCmd, ok := scheduler.CaseFilterFor("go test ./...", []string{"example.com/m::TestFlaky", "example.com/m::TestOther"})
	if !ok || !strings.Contains(goCmd, "-run '^(") || !strings.Contains(goCmd, "TestFlaky") || !strings.Contains(goCmd, "TestOther") {
		t.Fatalf("go filter=%q ok=%v", goCmd, ok)
	}
	pyCmd, ok := scheduler.CaseFilterFor("pytest -q", []string{"tests/test_a.py::test_x"})
	if !ok || !strings.Contains(pyCmd, "tests/test_a.py::test_x") {
		t.Fatalf("pytest filter=%q ok=%v", pyCmd, ok)
	}
	vCmd, ok := scheduler.CaseFilterFor("vitest run", []string{"src/a.test.ts::adds"})
	if !ok || !strings.Contains(vCmd, "related") && !strings.Contains(vCmd, "src/a.test.ts") || !strings.Contains(vCmd, "--reporter=json") {
		t.Fatalf("vitest filter=%q ok=%v", vCmd, ok)
	}
	jCmd, ok := scheduler.CaseFilterFor("jest", []string{"src/a.test.js::adds"})
	if !ok || !strings.Contains(jCmd, "src/a.test.js") || !strings.Contains(jCmd, "--json") {
		t.Fatalf("jest filter=%q ok=%v", jCmd, ok)
	}
	// unsupported runner: no filter → factory must return nil (fail-safe keeps regression)
	if _, ok := scheduler.CaseFilterFor("mocha", []string{"a::b"}); ok {
		t.Fatal("mocha must not claim case filtering")
	}
	if scheduler.RerunSupported("mocha") {
		t.Fatal("unsupported runner must not claim rerun support (keeps regression)")
	}
}

func TestScheduler_BudgetExhaustionRecordsUnverifiedClaims(t *testing.T) {
	// Setup shares the run's TOTAL budget: a deadline that has already passed
	// when the run starts expires during commit resolution. That must be
	// reported as UNVERIFIED with the setup stage and the reason — not as a
	// tool error (an agent cannot act on "signal: killed") and not as a silent
	// pass. The parent deadline is pre-expired so the stage under test is
	// deterministic instead of racing a 1ms budget against a git exec.
	repo := scenarioRepo(t, goFiles())
	parent, cancelParent := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelParent()
	res, err := scheduler.Run(parent, scheduler.Config{
		RepoRoot: repo, BaseRef: "HEAD", DisableCache: true,
		BudgetMs: 1, // total budget far below setup cost
	})
	if err != nil {
		t.Fatalf("exhaustion during setup must be a verdict, not an error: %v", err)
	}
	if res.Verdict != bundle.Unverified {
		t.Fatalf("exhausted budget must be UNVERIFIED, got %s", res.Verdict)
	}
	// Setup never persisted a bundle, so the reason lives in Result.Unverified.
	if len(res.Unverified) == 0 {
		t.Fatalf("trimmed work must be recorded in unverified claims: %+v", res)
	}
	joined := strings.Join(res.Unverified, "\n")
	t.Logf("setup-exhaustion claims: %v", res.Unverified)
	for _, want := range []string{"budget exhausted", "setup", "resolve base", "context deadline exceeded", "1ms"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("claim must name %q precisely, got: %v", want, res.Unverified)
		}
	}
	if len(res.Evidences) != 0 {
		t.Fatalf("no probe may produce evidence under an expired budget: %+v", res.Evidences)
	}
	// Setup never completed, so nothing was persisted: no fabricated bundle id
	// may claim a content address (that would collide with a clean-diff run).
	if res.Bundle.BundleID != "" {
		t.Fatalf("an unobserved diff must not be persisted: %s", res.Bundle.BundleID)
	}
}

func TestScheduler_RemainingBudgetCapsProbeAndTrimsTheRest(t *testing.T) {
	// Controlled slow commands: typecheck finishes quickly (inside the total
	// budget), the test command idles for 30s while its own per-probe budget is
	// 60s. Only the TOTAL budget can stop it, and only by giving it what is
	// left after typecheck — a per-probe reading would let the run take 30s.
	const slowSeconds = 30
	override := `{"language":["shell"],"commands":` +
		`{"typecheck":{"cmd":"sh -c 'sleep 0.5; exit 0'","source":".vouch/profile.json"},` +
		`"test":{"cmd":"sh -c 'sleep 30'","source":".vouch/profile.json"}},` +
		`"confidence":"high","gaps":[]}`
	repo := scenarioRepo(t, map[string]string{
		".vouch/profile.json": override,
		"README.md":           "budget fixture\n",
	})
	const totalMs = 5000
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := scheduler.Run(ctx, scheduler.Config{
		RepoRoot: repo, BaseRef: "HEAD", DisableCache: true,
		BudgetMs:      totalMs,
		ProbeBudgetMs: map[string]int{"typecheck": 60_000, "test": 60_000},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("a spent budget is a verdict, not an error: %v", err)
	}
	if res.Verdict != bundle.Unverified {
		t.Fatalf("a killed probe must be UNVERIFIED, got %s (unverified=%v)", res.Verdict, res.Unverified)
	}
	byProbe := map[string]bundle.Evidence{}
	for _, ev := range res.Evidences {
		byProbe[ev.Probe] = ev
	}
	tc, ok := byProbe["typecheck"]
	if !ok || tc.Verdict != bundle.EvidencePass {
		t.Fatalf("typecheck must complete inside the total budget: %+v", res.Evidences)
	}
	claimText := strings.Join(res.Bundle.UnverifiedClaims, "\n")
	t.Logf("remaining-budget run took %v, claims: %v", elapsed, res.Bundle.UnverifiedClaims)
	if ev, ok := byProbe["test"]; ok {
		if ev.Verdict != bundle.EvidenceInconclusive {
			t.Fatalf("a probe killed by the remaining budget is inconclusive, got %s: %+v", ev.Verdict, ev)
		}
		if !strings.Contains(claimText, "killed by budget") {
			t.Fatalf("the claim must name the budget kill, got: %v", res.Bundle.UnverifiedClaims)
		}
	} else if !strings.Contains(claimText, "test: skipped (budget exhausted)") {
		// On a very slow machine setup may eat the whole budget before the test
		// probe starts; that is the trim path, and it must also be attributed.
		t.Fatalf("no test evidence and no budget-trim claim: %v", res.Bundle.UnverifiedClaims)
	}
	// The decisive assertion: with a 60s per-probe budget and a 30s command the
	// run can only finish quickly if the remaining TOTAL time capped the probe.
	maxElapsed := time.Duration(totalMs)*time.Millisecond + 20*time.Second
	if elapsed >= maxElapsed {
		t.Fatalf("slow command must be capped by the remaining budget, took %v (limit %v; slow command sleeps %ds)", elapsed, maxElapsed, slowSeconds)
	}
	if elapsed < 500*time.Millisecond {
		t.Fatalf("typecheck (0.5s) must be given time to finish, took %v", elapsed)
	}
}

func TestScenarioPython_FlakyAbsorbed(t *testing.T) {
	if _, ok := pythonTestCommand(); !ok {
		t.Skip("pytest unavailable (neither pytest nor python3 -m pytest)")
	}
	t.Setenv("VOUCH_FLAKY_TOKEN", fmt.Sprintf("%s-%d", t.Name(), os.Getpid()))
	repo := scenarioRepo(t, map[string]string{
		"pyproject.toml":    "[tool.pytest.ini_options]\ntestpaths=[\"tests\"]\n",
		"src/__init__.py":   "",
		"src/add.py":        "def add(a, b):\n    return a + b\n",
		"tests/__init__.py": "",
		"tests/test_add.py": "from src.add import add\n\n\ndef test_add():\n    assert add(1, 2) == 3\n",
		"tests/test_flaky.py": `import os
import pathlib


def test_flaky():
    # base always passes; candidate fails its first run in this worktree
    wd = pathlib.Path.cwd()
    if wd.name == "base":
        return
    marker = pathlib.Path(os.environ.get("TMPDIR", "/tmp")) / (
        "vouch-pyflaky-" + os.environ.get("VOUCH_FLAKY_TOKEN", "x") + "-" + wd.name
    )
    if not marker.exists():
        marker.write_text("1")
        raise AssertionError("simulated flake (first run in this worktree)")
`,
	})
	res := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if res.Verdict != bundle.Unverified {
		t.Fatalf("python single-side instability must be UNVERIFIED: %s", res.Verdict)
	}
	for _, ev := range res.Evidences {
		if len(ev.Delta.FlakyAbsorbed) != 0 {
			t.Fatalf("python instability must not be absorbed: %+v", ev)
		}
	}
}

func TestScenarioTypeScript_FlakyAndRemovedTest(t *testing.T) {
	root := repoRootOf(t)
	fixture := filepath.Join(root, "testdata/fixtures/ts-vitest")
	if _, err := os.Stat(filepath.Join(fixture, "node_modules")); err != nil {
		t.Skip("ts fixture node_modules not installed (CI installs it)")
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(fixture, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	t.Setenv("VOUCH_FLAKY_TOKEN", fmt.Sprintf("%s-%d", t.Name(), os.Getpid()))
	repo := scenarioRepoWithSetup(t, map[string]string{
		"package.json":    read("package.json"),
		"tsconfig.json":   read("tsconfig.json"),
		"src/add.ts":      read("src/add.ts"),
		"src/add.test.ts": read("src/add.test.ts"),
		"src/flaky.test.ts": `import { describe, it, expect } from "vitest"
import { existsSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { basename, join } from "node:path"

describe("flaky", () => {
  it("flakes once in the candidate", () => {
    const cwdBase = basename(process.cwd())
    if (cwdBase === "base") {
      expect(true).toBe(true)
      return
    }
    const marker = join(tmpdir(), "vouch-tsflaky-" + (process.env.VOUCH_FLAKY_TOKEN || "x") + "-" + cwdBase)
    if (!existsSync(marker)) {
      writeFileSync(marker, "1")
      expect("flake").toBe("pass")
    }
  })
})
`,
	}, func(dir string) {
		stageNodeModules(t, fixture, dir)
	})
	flakyRes := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if flakyRes.Verdict != bundle.Unverified {
		t.Fatalf("ts single-side instability must be UNVERIFIED: %s", flakyRes.Verdict)
	}
	for _, ev := range flakyRes.Evidences {
		if len(ev.Delta.FlakyAbsorbed) != 0 {
			t.Fatalf("ts instability must not be absorbed: %+v", ev)
		}
	}

	// removed failing test: delete the flaky file from a state where it fails
	// (base worktree) → candidate has no failure, base has one → VERIFIED + warning
	repo2 := scenarioRepoWithSetup(t, map[string]string{
		"package.json":    read("package.json"),
		"tsconfig.json":   read("tsconfig.json"),
		"src/add.ts":      read("src/add.ts"),
		"src/add.test.ts": read("src/add.test.ts"),
		"src/known_bad.test.ts": `import { describe, it, expect } from "vitest"
describe("known bad", () => { it("always fails", () => expect(1).toBe(2)) })
`,
	}, func(dir string) {
		stageNodeModules(t, fixture, dir)
	})
	if err := os.Remove(filepath.Join(repo2, "src", "known_bad.test.ts")); err != nil {
		t.Fatal(err)
	}
	removed := run(t, repo2, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if removed.Verdict != bundle.Verified {
		t.Fatalf("ts removing a base-failing test is not a regression: %s ev=%+v", removed.Verdict, removed.Evidences)
	}
	if joined := strings.Join(removed.Warnings, "\n"); !strings.Contains(joined, "removed_tests") {
		t.Fatalf("removed_tests warning missing: %v delta=%+v", removed.Warnings, removed.Evidences[0].Delta)
	}
}

func TestScheduler_PersistsBundleForReproduce(t *testing.T) {
	repo := scenarioRepo(t, goFiles())
	res := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	path := filepath.Join(repo, ".vouch", "bundles", res.Bundle.BundleID, "bundle.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("bundle must be persisted for reproduce: %v", err)
	}
	loaded, err := bundle.NewStore(repo).Load(res.Bundle.BundleID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("persisted bundle invalid: %v", err)
	}
	if len(loaded.Evidence) == 0 {
		t.Fatal("persisted bundle lost evidence")
	}
	// Reproduce command must reference the persisted id.
	if !strings.Contains(loaded.Evidence[0].Reproduce, loaded.BundleID) {
		t.Fatalf("reproduce must name the persisted bundle: %q", loaded.Evidence[0].Reproduce)
	}
}

func TestScheduler_IdenticalRunsShareBundleIdentity(t *testing.T) {
	repo := scenarioRepo(t, goFiles())
	first := run(t, repo, scheduler.Config{BaseRef: "HEAD"}) // writes .vouch/cache
	second := run(t, repo, scheduler.Config{BaseRef: "HEAD"})
	if first.Bundle.BundleID != second.Bundle.BundleID {
		t.Fatalf("identical runs must share bundle id: %s vs %s", first.Bundle.BundleID, second.Bundle.BundleID)
	}
	if first.Bundle.Subject.DiffSHA256 != second.Bundle.Subject.DiffSHA256 {
		t.Fatalf("identical runs must share diff sha: %s vs %s", first.Bundle.Subject.DiffSHA256, second.Bundle.Subject.DiffSHA256)
	}
	// vouch state must not appear in the candidate snapshot
	for _, ev := range second.Evidences {
		if strings.Contains(strings.Join(ev.Delta.NewPassing, ","), ".vouch") {
			t.Fatalf("vouch state leaked into evidence: %+v", ev.Delta)
		}
	}
}

// pythonTestCommand reports how pytest can be invoked on this machine.
func pythonTestCommand() (string, bool) {
	if _, err := exec.LookPath("pytest"); err == nil {
		return "pytest", true
	}
	if _, err := exec.LookPath("python3"); err != nil {
		return "", false
	}
	cmd := exec.Command("python3", "-m", "pytest", "--version")
	if err := cmd.Run(); err != nil {
		return "", false
	}
	return "python3 -m pytest", true
}

func TestScenarioGo_BranchRangeBroken(t *testing.T) {
	// Two committed refs: base = first commit, candidate = second commit. The
	// worktree is clean, so only an explicit --to can select the candidate.
	repo := scenarioRepo(t, goFiles())
	base := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
	write(t, repo, "calc.go", "package fixture\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "break add")

	res := run(t, repo, scheduler.Config{BaseRef: base, CandidateRef: "HEAD", DisableCache: true})
	if res.Verdict != bundle.Broken {
		t.Fatalf("clean worktree + explicit candidate ref must see the range: %s ev=%+v", res.Verdict, res.Evidences)
	}
	if len(res.Evidences[0].Delta.Regressions) == 0 {
		t.Fatalf("regression must be attributed to the range: %+v", res.Evidences[0].Delta)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

func TestScheduler_DependencyGapsAreRecorded(t *testing.T) {
	repo := scenarioRepo(t, map[string]string{
		"pyproject.toml":    "[tool.pytest.ini_options]\ntestpaths=[\"tests\"]\n",
		"src/__init__.py":   "",
		"tests/__init__.py": "",
		"tests/test_x.py":   "def test_x():\n    assert True\n",
	})
	// Force the "no runner" branch regardless of this machine's setup: expose
	// only the tools the pipeline itself needs, and hide python/pytest.
	binDir := t.TempDir()
	for _, tool := range []string{"git", "go", "sh"} {
		if p, err := exec.LookPath(tool); err == nil {
			if err := os.Symlink(p, filepath.Join(binDir, tool)); err != nil && !os.IsExist(err) {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("PATH", binDir)
	cfg := scheduler.Config{RepoRoot: repo, BaseRef: "HEAD", DisableCache: true,
		ProbeBudgetMs: map[string]int{"test": 3000}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := scheduler.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	joined := strings.Join(res.Profile.Gaps, "\n")
	if !strings.Contains(joined, "pytest") && !strings.Contains(joined, "python") {
		t.Fatalf("missing runner must be reported as a gap: %v", res.Profile.Gaps)
	}
	if res.Verdict == bundle.Verified {
		t.Fatalf("without a runner the verdict cannot be VERIFIED: %+v", res.Evidences)
	}
	if len(res.Bundle.UnverifiedClaims) == 0 {
		t.Fatalf("unverified claims must explain the missing runner: %+v", res.Bundle)
	}
}

func TestRerun_ReproducesFlakyAndRegression(t *testing.T) {
	// Regression: verify BROKEN → rerun must also be BROKEN.
	repo := scenarioRepo(t, goFiles())
	write(t, repo, "calc.go", "package fixture\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n")
	res := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if res.Verdict != bundle.Broken {
		t.Fatalf("setup: %s", res.Verdict)
	}
	rr, err := scheduler.Rerun(context.Background(), scheduler.RerunConfig{
		RepoRoot: repo, BaseRef: res.Bundle.Subject.BaseRef,
		CandidateRef: deref(t, res.Bundle.Subject.CandidateRef),
		Probe:        "test", Command: "go test -json ./...",
	})
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if rr.Verdict != bundle.Broken {
		t.Fatalf("rerun must reproduce BROKEN, got %s", rr.Verdict)
	}

	// Flaky: verify absorbs; rerun must absorb the same way (not BROKEN).
	t.Setenv("VOUCH_FLAKY_TOKEN", fmt.Sprintf("rerun-%d", os.Getpid()))
	files := map[string]string{
		"go.mod":  "module example.com/fixture\n\ngo 1.24\n",
		"calc.go": "package fixture\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"flaky_test.go": `package fixture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFlaky(t *testing.T) {
	wd, _ := os.Getwd()
	if filepath.Base(wd) == "base" {
		return
	}
	marker := filepath.Join(os.TempDir(), "vouch-rerunflaky-"+os.Getenv("VOUCH_FLAKY_TOKEN")+"-"+filepath.Base(filepath.Dir(wd)))
	if _, err := os.Stat(marker); err != nil {
		_ = os.WriteFile(marker, []byte("1"), 0o644)
		t.Fatal("simulated flake (first run in this worktree)")
	}
}
`,
		"calc_test.go": "package fixture\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatalf(\"bad\")\n\t}\n}\n",
	}
	repo2 := scenarioRepo(t, files)
	flaky := run(t, repo2, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if flaky.Verdict != bundle.Unverified {
		t.Fatalf("setup flaky: %s", flaky.Verdict)
	}
	rr2, err := scheduler.Rerun(context.Background(), scheduler.RerunConfig{
		RepoRoot: repo2, BaseRef: flaky.Bundle.Subject.BaseRef,
		CandidateRef: deref(t, flaky.Bundle.Subject.CandidateRef),
		Probe:        "test", Command: "go test -json ./...",
	})
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if rr2.Verdict != bundle.Unverified {
		t.Fatalf("rerun must preserve single-side uncertainty, got %s delta=%+v", rr2.Verdict, rr2.Delta)
	}
	if len(rr2.Delta.FlakyAbsorbed) != 0 {
		t.Fatalf("single-side instability cannot be absorbed on rerun: %+v", rr2.Delta)
	}
}

func deref(t *testing.T, s *string) string {
	t.Helper()
	if s == nil {
		t.Fatal("bundle has no candidate ref")
	}
	return *s
}

func TestScheduler_SelectorFullRunKeepsItsOwnReason(t *testing.T) {
	// A repo where the selector itself cannot map tests (Python file without a
	// conventional test name): the bundle must keep the selector's own
	// mechanism/reason instead of claiming "sides cannot share targets".
	repo := scenarioRepo(t, map[string]string{
		"pyproject.toml":      "[tool.pytest.ini_options]\ntestpaths=[\"tests\"]\n",
		"src/__init__.py":     "",
		"src/orphan.py":       "def f():\n    return 1\n",
		"tests/__init__.py":   "",
		"tests/test_other.py": "def test_other():\n    assert True\n",
	})
	write(t, repo, "src/nomap.py", "def g():\n    return 2\n")
	res := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if res.Profile.TestSelection == nil {
		t.Fatal("test_selection must be recorded")
	}
	if strings.Contains(res.Profile.TestSelection.Mechanism, "sides cannot share") {
		t.Fatalf("selector-chosen FullRun must not be reported as a side mismatch: %+v", res.Profile.TestSelection)
	}
	if res.Verdict == bundle.Verified && res.Profile.TestSelection.Supported {
		t.Fatalf("FullRun must not claim narrowed support: %+v", res.Profile.TestSelection)
	}
}

func TestScheduler_NodeModulesNotCapturedInSnapshot(t *testing.T) {
	// A JS repo whose .gitignore does NOT ignore node_modules: the dependency
	// tree must still stay out of the candidate snapshot and the bundle identity.
	repo := scenarioRepo(t, map[string]string{
		"package.json": `{"name":"x","scripts":{"test":"vitest run"}}`,
	})
	if err := os.MkdirAll(filepath.Join(repo, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "node_modules", "pkg", "index.js"), []byte("module.exports=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true, InstallDeps: false})
	second := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true, InstallDeps: false})
	if first.Bundle.BundleID != second.Bundle.BundleID {
		t.Fatalf("node_modules must not change bundle identity: %s vs %s", first.Bundle.BundleID, second.Bundle.BundleID)
	}
	if first.Bundle.Subject.DiffSHA256 != second.Bundle.Subject.DiffSHA256 {
		t.Fatalf("node_modules must not change diff identity")
	}
	for _, ev := range first.Evidences {
		if strings.Contains(ev.Method, "node_modules") {
			t.Fatalf("node_modules leaked into evidence: %q", ev.Method)
		}
	}
}

func TestScenarioGo_MakeDrivenRepoStillRunsFullCommand(t *testing.T) {
	// A Makefile-driven Go repo: the recorded test command is `make test`, which
	// cannot be narrowed by appending package paths.
	repo := scenarioRepo(t, map[string]string{
		"go.mod":       "module example.com/fixture\n\ngo 1.24\n",
		"calc.go":      "package fixture\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"calc_test.go": "package fixture\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatalf(\"bad\")\n\t}\n}\n",
		"Makefile":     "test:\n\tgo test ./...\n",
	})
	clean := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if clean.Verdict != bundle.Verified {
		t.Fatalf("make-driven clean repo must verify: %s gaps=%v unverified=%v", clean.Verdict, clean.Profile.Gaps, clean.Unverified)
	}
	for _, ev := range clean.Evidences {
		if strings.Contains(ev.Method, "make test ./") {
			t.Fatalf("make target must never be narrowed by appending paths: %q", ev.Method)
		}
	}
	// regression through the same entry point
	write(t, repo, "calc.go", "package fixture\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n")
	broken := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if broken.Verdict != bundle.Broken {
		t.Fatalf("make-driven regression must be BROKEN: %s ev=%+v", broken.Verdict, broken.Evidences)
	}
}

func TestScenarioGo_UntestedPackageFallsBackToFullSuite(t *testing.T) {
	// A change in a package without tests: the narrowing selects nothing, so
	// verify must fall back to the project's full test command (and say so)
	// rather than reporting "pass" from an empty run.
	repo := scenarioRepo(t, map[string]string{
		"go.mod":       "module example.com/fixture\n\ngo 1.24\n",
		"util/u.go":    "package util\n\nfunc U() int { return 1 }\n",
		"main_test.go": "package fixture\n\nimport \"testing\"\n\nfunc TestRoot(t *testing.T) {}\n",
		"root.go":      "package fixture\n",
	})
	write(t, repo, "util/u.go", "package util\n\nfunc U() int { return 2 }\n")
	res := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if len(res.Evidences) == 0 {
		t.Fatal("full-suite fallback must produce evidence")
	}
	if !strings.Contains(res.Evidences[0].Method, "go test ./...") {
		t.Fatalf("fallback must run the project's full command: %q", res.Evidences[0].Method)
	}
	// Both sides must come from the full run. A shadowed base variable (the
	// fallback's own := declaration) silently kept the narrowed base result and
	// paired it with a full candidate, which either fakes regressions or turns
	// the fallback into a no-op.
	ev := res.Evidences[0]
	if ev.Baseline.Pass+ev.Baseline.Fail < 1 || ev.Candidate.Pass+ev.Candidate.Fail < 1 {
		t.Fatalf("fallback must measure the full suite on BOTH sides: base=%+v candidate=%+v",
			ev.Baseline, ev.Candidate)
	}
	if ev.Baseline.DurationMs < 0 || ev.Candidate.DurationMs < 0 {
		t.Fatalf("durations must come from a real run: %+v", ev)
	}
	joined := strings.Join(res.Profile.Gaps, "\n")
	if !strings.Contains(joined, "no cases") && !strings.Contains(joined, "full") {
		t.Fatalf("fallback must be recorded in gaps: %v", res.Profile.Gaps)
	}
}

func TestScenarioGo_RepoWithoutTestsIsUnverified(t *testing.T) {
	// No tests anywhere: zero measured cases on both sides is not a pass.
	repo := scenarioRepo(t, map[string]string{
		"go.mod":    "module example.com/fixture\n\ngo 1.24\n",
		"util/u.go": "package util\n\nfunc U() int { return 1 }\n",
	})
	write(t, repo, "util/u.go", "package util\n\nfunc U() int { return 2 }\n")
	res := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if res.Verdict == bundle.Verified {
		t.Fatalf("a repo without tests cannot be VERIFIED: ev=%+v", res.Evidences)
	}
	if len(res.Bundle.UnverifiedClaims) == 0 {
		t.Fatalf("must explain why it could not verify: %+v", res.Bundle)
	}
}

func TestScenarioTypeScript_NarrowedEmptyFallsBackToFullSuite(t *testing.T) {
	root := repoRootOf(t)
	fixture := filepath.Join(root, "testdata/fixtures/ts-vitest")
	if _, err := os.Stat(filepath.Join(fixture, "node_modules")); err != nil {
		t.Skip("ts fixture node_modules not installed (CI installs it)")
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(fixture, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	repo := scenarioRepoWithSetup(t, map[string]string{
		"package.json":    read("package.json"),
		"tsconfig.json":   read("tsconfig.json"),
		"src/add.ts":      read("src/add.ts"),
		"src/add.test.ts": read("src/add.test.ts"),
	}, func(dir string) {
		stageNodeModules(t, fixture, dir)
	})
	// a module no test imports: `vitest related` matches nothing and exits 1
	write(t, repo, "src/unused.ts", "export function unused() {\n  return 1\n}\n")
	res := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if res.Verdict != bundle.Verified {
		t.Fatalf("full-suite fallback must verify, got %s ev=%+v gaps=%v", res.Verdict, res.Evidences, res.Profile.Gaps)
	}
	method := res.Evidences[0].Method
	if strings.Contains(method, "related") {
		t.Fatalf("empty narrowing must be replaced by the full suite: %q", method)
	}
	gaps := strings.Join(res.Profile.Gaps, "\n")
	if !strings.Contains(gaps, "no cases") && !strings.Contains(gaps, "full run") {
		t.Fatalf("fallback must be recorded: %v", res.Profile.Gaps)
	}
}

func TestScheduler_SetupTimeoutPreservesStoredEvidence(t *testing.T) {
	repo := scenarioRepo(t, goFiles())
	good := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if good.Verdict != bundle.Verified || len(good.Bundle.Evidence) == 0 || good.Bundle.Subject.CandidateRef == nil {
		t.Fatalf("expected persisted clean verification: %+v", good)
	}
	path := filepath.Join(repo, ".vouch", "bundles", good.Bundle.BundleID, "bundle.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	failed, err := scheduler.Run(ctx, scheduler.Config{RepoRoot: repo, BaseRef: good.Bundle.Subject.BaseRef})
	if err != nil || failed.Verdict != bundle.Unverified {
		t.Fatalf("setup result: %+v, %v", failed, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("setup timeout overwrote the existing clean-diff bundle")
	}
	loaded, err := bundle.NewStore(repo).Load(good.Bundle.BundleID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Subject.CandidateRef == nil || *loaded.Subject.CandidateRef != *good.Bundle.Subject.CandidateRef || len(loaded.Evidence) != len(good.Bundle.Evidence) {
		t.Fatalf("saved evidence identity was lost: %+v", loaded)
	}
	if failed.Bundle.BundleID != "" || failed.Bundle.Subject.DiffSHA256 != "" || failed.Bundle.Subject.CandidateRef != nil {
		t.Fatalf("unobserved subject must not claim a content address: %+v", failed.Bundle)
	}
}

func TestScheduler_PersistsFailureDetail(t *testing.T) {
	repo := scenarioRepo(t, goFiles())
	write(t, repo, "calc.go", "package fixture\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n")
	res := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if res.Verdict != bundle.Broken {
		t.Fatalf("setup: %s", res.Verdict)
	}
	ev := res.Evidences[0]
	if len(ev.Failures) == 0 {
		t.Fatalf("regression must carry a failure message: %+v", ev.Delta)
	}
	if ev.Failures[0].Message == "" || !strings.Contains(ev.Failures[0].ID, "TestAdd") {
		t.Fatalf("failure detail wrong: %+v", ev.Failures)
	}
	if ev.Logs == "" {
		t.Fatal("failure detail must be persisted as a blob")
	}
	blob := filepath.Join(repo, ".vouch", "bundles", res.Bundle.BundleID, filepath.FromSlash(ev.Logs))
	data, err := os.ReadFile(blob)
	if err != nil {
		t.Fatalf("blob missing: %v", err)
	}
	if !strings.Contains(string(data), "TestAdd") {
		t.Fatalf("blob must contain the failure: %s", data)
	}
}

func TestScenarioTypeScript_NoTestsCollectedIsNotVerified(t *testing.T) {
	root := repoRootOf(t)
	fixture := filepath.Join(root, "testdata/fixtures/ts-vitest")
	if _, err := os.Stat(filepath.Join(fixture, "node_modules")); err != nil {
		t.Skip("ts fixture node_modules not installed (CI installs it)")
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(fixture, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// vitest config excludes all tests → "No test files found" on both sides
	repo := scenarioRepoWithSetup(t, map[string]string{
		"package.json":       read("package.json"),
		"tsconfig.json":      read("tsconfig.json"),
		"src/add.ts":         read("src/add.ts"),
		"vitest.config.json": `{"test":{"include":["src/__none__/*.test.ts"]}}`,
	}, func(dir string) {
		stageNodeModules(t, fixture, dir)
	})
	res := run(t, repo, scheduler.Config{BaseRef: "HEAD", DisableCache: true})
	if res.Verdict == bundle.Verified {
		t.Fatalf("a run that collected no tests cannot be VERIFIED: ev=%+v", res.Evidences)
	}
}
