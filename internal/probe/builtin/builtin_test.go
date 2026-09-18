package builtin_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/probe"
	"github.com/junqingyongyuanbusi/vouch/internal/probe/builtin"
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

// newGoModule writes a tiny module; withFail controls whether one test fails.
func newGoModule(t *testing.T, withFail bool) string {
	t.Helper()
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
	write("go.mod", "module example.com/fixture\n\ngo 1.24\n")
	write("x.go", "package fixture\n\nfunc X() int { return 1 }\n")
	body := "package fixture\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) { if X() != 1 { t.Fatal(\"ok\") } }\n"
	if withFail {
		body += "\nfunc TestBad(t *testing.T) { if X() != 2 { t.Fatalf(\"boom: got %d want 2\", X()) } }\n"
	}
	write("x_test.go", body)
	return dir
}

func TestTest_RunsGoFixture(t *testing.T) {
	dir := newGoModule(t, false)
	res, err := builtin.Test{}.Run(context.Background(), builtin.Input{
		Workdir: dir, Role: "candidate", Command: "go test -json ./...", BudgetMs: 60000,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Verdict != probe.VerdictPass {
		t.Fatalf("verdict=%s reason=%s", res.Verdict, res.Reason)
	}
	tr := builtin.TestResultOf(res)
	if len(tr.Passed) != 1 || len(tr.Failed) != 0 {
		t.Fatalf("parsed=%+v", tr)
	}
}

func TestTest_FailureInjection(t *testing.T) {
	dir := newGoModule(t, true)
	res, err := builtin.Test{}.Run(context.Background(), builtin.Input{
		Workdir: dir, Role: "candidate", Command: "go test -json ./...", BudgetMs: 60000,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Verdict != probe.VerdictFail {
		t.Fatalf("verdict=%s", res.Verdict)
	}
	tr := builtin.TestResultOf(res)
	found := false
	for _, id := range tr.Failed {
		if strings.HasSuffix(id, "::TestBad") {
			found = true
		}
	}
	if !found {
		t.Fatalf("failed ids=%v", tr.Failed)
	}
}

func TestTest_UnknownRunnerReportsCommandLevelFact(t *testing.T) {
	dir := t.TempDir()
	// Unparsed runner that succeeds: the fact "the command passed" is reported,
	// but without case lists so differ uses verdict-level semantics.
	res, err := builtin.Test{}.Run(context.Background(), builtin.Input{
		Workdir: dir, Role: "candidate", Command: "echo not-a-known-runner", BudgetMs: 10000,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Verdict != probe.VerdictPass {
		t.Fatalf("passing unknown runner must report pass, got %+v", res)
	}
	if _, hasCases := res.Data["passed"]; hasCases {
		t.Fatalf("unknown runner must not invent case lists: %+v", res.Data)
	}
	// Unparsed runner that fails → fail (differ decides regression vs existing).
	resFail, _ := builtin.Test{}.Run(context.Background(), builtin.Input{
		Workdir: dir, Role: "candidate", Command: "sh -c 'exit 3'", BudgetMs: 10000,
	})
	if resFail.Verdict != probe.VerdictFail {
		t.Fatalf("failing unknown runner must report fail, got %+v", resFail)
	}
	// no command at all → inconclusive (nothing was measured)
	res2, _ := builtin.Test{}.Run(context.Background(), builtin.Input{Workdir: dir, Role: "base"})
	if res2.Verdict != probe.VerdictInconclusive {
		t.Fatalf("missing command must be inconclusive, got %+v", res2)
	}
}

func TestBuild_PassFailAndArtifactHash(t *testing.T) {
	dir := t.TempDir()
	// artifact hash from dist/
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dist", "a.js"), []byte("console.log(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pass, err := builtin.Build{}.Run(context.Background(), builtin.Input{Workdir: dir, Role: "candidate", Command: "true"})
	if err != nil || pass.Verdict != probe.VerdictPass {
		t.Fatalf("build pass: %v %+v", err, pass)
	}
	if _, ok := pass.Data["artifact_sha256"]; !ok {
		t.Fatalf("artifact hash missing: %+v", pass.Data)
	}
	fail, err := builtin.Build{}.Run(context.Background(), builtin.Input{Workdir: dir, Role: "candidate", Command: "exit 2"})
	if err != nil || fail.Verdict != probe.VerdictFail {
		t.Fatalf("build fail: %v %+v", err, fail)
	}
}

func TestTypecheck_TSDiagnosticsAndUnsupported(t *testing.T) {
	dir := t.TempDir()
	ts := `echo "src/a.ts(3,7): error TS2345: Argument of type 'string' is not assignable to parameter of type 'number'."; exit 1`
	res, err := builtin.Typecheck{}.Run(context.Background(), builtin.Input{Workdir: dir, Role: "candidate", Command: "tsc --noEmit; " + ts})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Verdict != probe.VerdictFail {
		t.Fatalf("verdict=%s", res.Verdict)
	}
	diags := builtin.DiagnosticsOf(res)
	if len(diags) != 1 || diags[0].Code != "TS2345" || diags[0].File != "src/a.ts" || diags[0].Line != 3 {
		t.Fatalf("diagnostics=%+v", diags)
	}
	// Non-TS failing typecheck → inconclusive (no parser), never a silent pass.
	res2, _ := builtin.Typecheck{}.Run(context.Background(), builtin.Input{Workdir: dir, Role: "candidate", Command: "cargo check"})
	if res2.Verdict != probe.VerdictPass {
		// cargo isn't installed → nonzero exit; must be inconclusive
		if res2.Verdict != probe.VerdictInconclusive {
			t.Fatalf("unsupported typecheck must be inconclusive, got %+v", res2)
		}
	}
	// Passing TS typecheck → pass with empty diagnostics.
	res3, _ := builtin.Typecheck{}.Run(context.Background(), builtin.Input{Workdir: dir, Role: "candidate", Command: "tsc --noEmit"})
	if res3.Verdict != probe.VerdictPass && res3.Verdict != probe.VerdictInconclusive {
		t.Fatalf("tsc unavailable should be inconclusive, got %+v", res3)
	}
}

func TestAsProbeRunnerAdapter(t *testing.T) {
	dir := newGoModule(t, false)
	profile := bundle.ProjectProfile{
		Language:   []string{"go"},
		Commands:   map[string]bundle.Command{"test": {Cmd: "go test -json ./...", Source: "go.mod#default"}},
		Confidence: bundle.ConfidenceHigh,
		Gaps:       []string{},
	}
	raw, err := probe.ProfileFromParams(probe.RunParams{Profile: toMap(t, profile)})
	if err != nil || raw.Commands["test"].Cmd == "" {
		t.Fatalf("profile round-trip: %v %+v", err, raw)
	}
	r := builtin.AsProbeRunner(builtin.Test{})
	if r.Name() != "test" || r.Capabilities().Kind != probe.KindDeterministic {
		t.Fatalf("capabilities=%+v", r.Capabilities())
	}
	inv, err := r.Run(context.Background(), probe.RunParams{
		Workdir: dir, Role: "candidate", BudgetMs: 60000, Profile: toMap(t, profile),
	})
	if err != nil || inv.Result.Verdict != probe.VerdictPass {
		t.Fatalf("adapter run: %v %+v", err, inv)
	}
	// The adapter must carry the probe's declared kind so the kernel can keep
	// inferred probes out of the gate.
	if inv.Caps.Kind != probe.KindDeterministic {
		t.Fatalf("caps kind=%q", inv.Caps.Kind)
	}
}

// TestHostRunner_KeepsDeclaredInferredKind guards the isolation contract on the
// external path: HostRunner must not hardcode deterministic.
func TestHostRunner_KeepsDeclaredInferredKind(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "inferred.sh")
	content := "#!/bin/sh\n" +
		"read line; echo '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"name\":\"scope\",\"version\":\"0.1.0\",\"capabilities\":{\"differential\":false,\"kind\":\"inferred\"}}}';\n" +
		"read line; echo '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"verdict\":\"fail\",\"summary\":\"scope\"}}';\n" +
		"read line; echo '{\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{}}'\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	r := probe.HostRunner{Host: probe.Host{Path: script, Timeout: 5 * time.Second}, Name_: "scope"}
	inv, err := r.Run(context.Background(), probe.RunParams{Workdir: dir, Role: "candidate", BudgetMs: 1000})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if inv.Caps.Kind != probe.KindInferred {
		t.Fatalf("HostRunner dropped declared kind: got %q want inferred", inv.Caps.Kind)
	}
}

func toMap(t *testing.T, p bundle.ProjectProfile) map[string]interface{} {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestTest_TypeScriptFixtureEndToEnd is the P1.4 acceptance test:
// real vitest fixture → detector-style command → probe (sandbox+parser) →
// injected failure located precisely in Data["failed"].
func TestTest_TypeScriptFixtureEndToEnd(t *testing.T) {
	root := repoRoot(t)
	fixture := filepath.Join(root, "testdata/fixtures/ts-vitest")
	if _, err := os.Stat(filepath.Join(fixture, "node_modules")); err != nil {
		t.Skip("fixture node_modules not installed; run npm install in testdata/fixtures/ts-vitest")
	}
	work := t.TempDir()
	// copy fixture sources, share node_modules via symlink (fast, no install)
	if err := os.MkdirAll(filepath.Join(work, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"package.json", "tsconfig.json", "src/add.ts"} {
		b, err := os.ReadFile(filepath.Join(fixture, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, rel), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(fixture, "node_modules"), filepath.Join(work, "node_modules")); err != nil {
		t.Fatal(err)
	}
	// inject exactly one failing assertion
	testBody := `import { describe, it, expect } from "vitest"
import { add } from "./add.js"
describe("add", () => {
  it("adds", () => expect(add(1, 2)).toBe(3))
  it("injected-regression", () => expect(add(1, 2)).toBe(999))
})
`
	if err := os.WriteFile(filepath.Join(work, "src", "add.test.ts"), []byte(testBody), 0o644); err != nil {
		t.Fatal(err)
	}

	// detector-style command: exactly what parsePackageJSON emits for this fixture
	res, err := builtin.Test{}.Run(context.Background(), builtin.Input{
		Workdir: work, Role: "candidate", Command: "vitest run", BudgetMs: 120000,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Verdict != probe.VerdictFail {
		t.Fatalf("verdict=%s reason=%s data=%+v", res.Verdict, res.Reason, res.Data)
	}
	tr := builtin.TestResultOf(res)
	found := false
	for _, id := range tr.Failed {
		if strings.Contains(id, "add.test.ts") && strings.Contains(id, "injected-regression") {
			found = true
		}
	}
	if !found {
		t.Fatalf("injected failure not precisely located: failed=%v passed=%v", tr.Failed, tr.Passed)
	}
	if !strings.HasPrefix(tr.Failed[0], "src/") {
		t.Fatalf("ids must be worktree-relative (base/candidate comparable), got %v", tr.Failed)
	}
}

// TestTest_PythonFixtureEndToEnd: py-pytest fixture, injected failure must be
// located precisely by the real probe (detector command shape "pytest -q").
func TestTest_PythonFixtureEndToEnd(t *testing.T) {
	detectorCmd := "pytest -q"
	if _, err := exec.LookPath("pytest"); err != nil {
		if _, err2 := exec.LookPath("python3"); err2 != nil {
			t.Skip("pytest/python3 unavailable")
		}
		detectorCmd = "python3 -m pytest -q"
	}
	root := repoRoot(t)
	fixture := filepath.Join(root, "testdata/fixtures/py-pytest")
	work := t.TempDir()
	for _, rel := range []string{"pyproject.toml", "src/add.py"} {
		b, err := os.ReadFile(filepath.Join(fixture, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, rel), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(work, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "from src.add import add\n\n\ndef test_ok():\n    assert add(1, 2) == 3\n\n\ndef test_injected():\n    assert add(1, 2) == 999\n"
	if err := os.WriteFile(filepath.Join(work, "tests", "test_add.py"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "src", "__init__.py"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// tests/__init__.py makes pytest insert the project root on sys.path,
	// which is what `from src.add import add` relies on.
	if err := os.WriteFile(filepath.Join(work, "tests", "__init__.py"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// detector emits a pytest command; probe must upgrade to -v for parsing
	res, err := builtin.Test{}.Run(context.Background(), builtin.Input{
		Workdir: work, Role: "candidate", Command: detectorCmd, BudgetMs: 120000,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Verdict != probe.VerdictFail {
		t.Fatalf("verdict=%s reason=%s data=%+v", res.Verdict, res.Reason, res.Data)
	}
	tr := builtin.TestResultOf(res)
	found := false
	for _, id := range tr.Failed {
		if strings.Contains(id, "test_add.py::test_injected") {
			found = true
		}
	}
	if !found {
		t.Fatalf("injected failure not located: failed=%v", tr.Failed)
	}
}

func TestBuild_ProgramLevelMissingFileIsFailureNotInconclusive(t *testing.T) {
	dir := t.TempDir()
	// The command ran fine; its input is missing → real failure.
	res, err := builtin.Build{}.Run(context.Background(), builtin.Input{
		Workdir: dir, Role: "candidate",
		Command: `sh -c 'echo "src/x.h: No such file or directory"; exit 1'`,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Verdict != probe.VerdictFail {
		t.Fatalf("program-level missing file must be fail, got %+v", res)
	}
	// A genuinely missing runner stays inconclusive.
	res2, _ := builtin.Build{}.Run(context.Background(), builtin.Input{
		Workdir: dir, Role: "candidate", Command: "definitely-not-a-real-runner-xyz",
	})
	if res2.Verdict != probe.VerdictInconclusive {
		t.Fatalf("missing runner must be inconclusive, got %+v", res2)
	}
}

func TestTest_NoTestsCollectedIsInconclusive(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		cmd  string
	}{
		{"pytest-exit5", `sh -c 'echo "no tests ran in 0.01s"; exit 5'`},
		{"vitest-no-files", `sh -c 'echo "No test files found, exiting with code 1"; exit 1'`},
		{"jest-none", `sh -c 'echo "No tests found, exiting with code 1"; exit 1'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := builtin.Test{}.Run(context.Background(), builtin.Input{
				Workdir: dir, Role: "candidate", Command: tc.cmd, BudgetMs: 10000,
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Verdict != probe.VerdictInconclusive {
				t.Fatalf("no tests collected must be inconclusive, got %+v", res)
			}
		})
	}
}

func TestTest_CollectionErrorIsFailureNotNoTests(t *testing.T) {
	dir := t.TempDir()
	res, err := builtin.Test{}.Run(context.Background(), builtin.Input{
		Workdir: dir, Role: "candidate",
		Command: `sh -c 'echo "collected 0 items / 1 error"; echo "ERROR collecting tests/test_x.py"; echo "Interrupted: 1 error during collection"; exit 2'`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != probe.VerdictFail {
		t.Fatalf("a collection error is a real failure, got %+v", res)
	}
}
