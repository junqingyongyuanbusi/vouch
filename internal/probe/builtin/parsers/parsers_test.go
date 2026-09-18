package parsers_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junqingyongyuanbusi/vouch/internal/probe/builtin/parsers"
)

func sample(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("sample %s: %v", name, err)
	}
	return b
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func anyContains(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestParseVitest_PassAndFail(t *testing.T) {
	pass := sample(t, "vitest-pass.json")
	res, err := parsers.ParseVitest(pass, "/work/ts-vitest")
	if err != nil {
		t.Fatalf("parse pass: %v", err)
	}
	if len(res.Passed) != 2 || len(res.Failed) != 0 {
		t.Fatalf("pass: passed=%v failed=%v", res.Passed, res.Failed)
	}
	if !anyContains(res.Passed, "add adds") {
		t.Fatalf("pass ids=%v", res.Passed)
	}
	// IDs must be worktree-relative (base/candidate produce the same id).
	for _, id := range res.Passed {
		if strings.HasPrefix(id, "/") {
			t.Fatalf("id not relative: %q", id)
		}
	}

	fail := sample(t, "vitest-fail.json")
	resF, err := parsers.ParseVitest(fail, "/work/ts-vitest")
	if err != nil {
		t.Fatalf("parse fail: %v", err)
	}
	if len(resF.Failed) != 2 {
		t.Fatalf("fail: failed=%v", resF.Failed)
	}
	if len(resF.Failures) == 0 || resF.Failures[0].Message == "" {
		t.Fatalf("fail must carry structured messages: %+v", resF.Failures)
	}
}

func TestParseJest_Pass(t *testing.T) {
	jest := []byte(`{"numPassedTests":1,"testResults":[{"name":"/repo/src/a.test.ts","assertionResults":[{"fullName":"a works","status":"passed","duration":5,"failureMessages":[]}]}]}`)
	res, err := parsers.ParseJest(jest, "/repo")
	if err != nil {
		t.Fatalf("jest: %v", err)
	}
	if len(res.Passed) != 1 || !anyContains(res.Passed, "src/a.test.ts::a works") {
		t.Fatalf("jest ids=%v", res.Passed)
	}
	// dispatch by command
	res2, err := parsers.Parse("jest --json", jest, "/repo")
	if err != nil || len(res2.Passed) != 1 {
		t.Fatalf("dispatch jest: %v %v", err, res2)
	}
}

func TestParseGoTest_PassAndFail(t *testing.T) {
	pass := sample(t, "gotest-pass.json")
	res, err := parsers.ParseGoTest(pass)
	if err != nil {
		t.Fatalf("go pass: %v", err)
	}
	if len(res.Passed) != 2 || len(res.Failed) != 0 {
		t.Fatalf("go pass: passed=%v failed=%v", res.Passed, res.Failed)
	}
	if !anyContains(res.Passed, "::TestAdd") {
		t.Fatalf("go ids=%v", res.Passed)
	}

	fail := sample(t, "gotest-fail.json")
	resF, err := parsers.ParseGoTest(fail)
	if err != nil {
		t.Fatalf("go fail: %v", err)
	}
	if !anyContains(resF.Failed, "::TestBad") {
		t.Fatalf("go fail ids=%v", resF.Failed)
	}
	// Package-level failure without a test must not be lost.
	pkgFail := []byte(`{"Action":"fail","Package":"example.com/x"}`)
	resP, err := parsers.ParseGoTest(pkgFail)
	if err != nil || len(resP.Failed) != 1 || resP.Failed[0] != "example.com/x::(package)" {
		t.Fatalf("package-level fail must synthesize id: %v %v", err, resP)
	}
}

func TestParsePytest_PassAndFail(t *testing.T) {
	pass := sample(t, "pytest-pass.txt")
	res, err := parsers.ParsePytest(pass)
	if err != nil {
		t.Fatalf("pytest pass: %v", err)
	}
	if len(res.Passed) != 2 || len(res.Failed) != 0 {
		t.Fatalf("pytest pass: passed=%v failed=%v", res.Passed, res.Failed)
	}
	if res.DurationMs <= 0 {
		t.Fatalf("pytest duration not parsed: %d", res.DurationMs)
	}
	if !has(res.Passed, "tests/test_add.py::test_add") {
		t.Fatalf("pytest ids=%v", res.Passed)
	}

	fail := sample(t, "pytest-fail.txt")
	resF, err := parsers.ParsePytest(fail)
	if err != nil {
		t.Fatalf("pytest fail: %v", err)
	}
	if len(resF.Failed) != 1 || !has(resF.Failed, "tests/test_add.py::test_add") {
		t.Fatalf("pytest fail ids=%v", resF.Failed)
	}
}

func TestParse_UnsupportedShapeIsErrorNotGuess(t *testing.T) {
	if _, err := parsers.Parse("mocha", []byte("all good"), "/repo"); err == nil {
		t.Fatal("unsupported runner must error (caller maps to inconclusive)")
	}
	if _, err := parsers.ParsePytest([]byte("some random log\n")); err == nil {
		t.Fatal("unrecognized pytest output must error, not guess")
	}
	if _, err := parsers.ParseGoTest([]byte("plain text\n")); err == nil {
		t.Fatal("go test without -json must error")
	}
}

func TestParseVitest_SuiteFailureWithoutCasesIsNotLost(t *testing.T) {
	// A test file that fails to load: no assertionResults, suite status failed.
	out := []byte(`{"success":false,"numFailedTests":0,"numFailedTestSuites":1,"testResults":[{"name":"/work/ts-vitest/src/broken.test.ts","status":"failed","message":"Failed to load url ./missing.js","assertionResults":[]}]}`)
	res, err := parsers.ParseVitest(out, "/work/ts-vitest")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Failed) != 1 || !strings.Contains(res.Failed[0], "src/broken.test.ts::(suite)") {
		t.Fatalf("suite failure must be synthesized into failed[]: %+v", res)
	}
	if len(res.Failures) == 0 || res.Failures[0].Message == "" {
		t.Fatalf("suite failure must carry a message: %+v", res.Failures)
	}
	if strings.HasPrefix(res.Failed[0], "/") {
		t.Fatalf("suite id must be worktree-relative: %q", res.Failed[0])
	}
}

func TestParseJest_SuiteFailureWithoutCasesIsNotLost(t *testing.T) {
	out := []byte(`{"numFailedTestSuites":1,"testResults":[{"name":"/work/js/a.test.js","status":"failed","message":"Cannot find module './x'","assertionResults":[]}]}`)
	res, err := parsers.ParseJest(out, "/work/js")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Failed) != 1 || !strings.Contains(res.Failed[0], "a.test.js::(suite)") {
		t.Fatalf("suite failure must be synthesized: %+v", res)
	}
}

func TestParseGoTest_DurationNotDoubleCounted(t *testing.T) {
	// package-level elapsed 0.243s, two tests 0.01+0.02 → duration must be max(pkg, sum)
	out := []byte(`{"Action":"pass","Package":"p","Test":"T1","Elapsed":0.01}
{"Action":"pass","Package":"p","Test":"T2","Elapsed":0.02}
{"Action":"pass","Package":"p","Elapsed":0.243}`)
	res, err := parsers.ParseGoTest(out)
	if err != nil {
		t.Fatal(err)
	}
	if res.DurationMs != 243 {
		t.Fatalf("duration=%d want 243 (max, not sum)", res.DurationMs)
	}
}

func TestParseVitest_SuiteFailureNotMaskedByOtherCaseFailure(t *testing.T) {
	// One pre-existing failing case + one suite that fails to load: the suite
	// must still be reported (previously gated on len(Failed)==0).
	out := []byte(`{"success":false,"numFailedTests":1,"numFailedTestSuites":1,"testResults":[
		{"name":"/work/ts/old.test.ts","status":"passed","assertionResults":[{"fullName":"old fails","status":"failed","duration":1,"failureMessages":["boom"]}]},
		{"name":"/work/ts/broken.test.ts","status":"failed","message":"Failed to load url ./missing.js","assertionResults":[]}]}`)
	res, err := parsers.ParseVitest(out, "/work/ts")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range res.Failed {
		if strings.Contains(id, "broken.test.ts::(suite)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("suite failure masked by unrelated case failure: %v", res.Failed)
	}
}

func TestParseJest_SuiteFailureNotMaskedByOtherCaseFailure(t *testing.T) {
	out := []byte(`{"numFailedTestSuites":1,"testResults":[
		{"name":"/work/js/old.test.js","status":"passed","assertionResults":[{"fullName":"old fails","status":"failed","duration":1,"failureMessages":["boom"]}]},
		{"name":"/work/js/broken.test.js","status":"failed","message":"Cannot find module './x'","assertionResults":[]}]}`)
	res, err := parsers.ParseJest(out, "/work/js")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range res.Failed {
		if strings.Contains(id, "broken.test.js::(suite)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("jest suite failure masked: %v", res.Failed)
	}
}
