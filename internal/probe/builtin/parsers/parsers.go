// Package parsers — P1.4 — test-output parsers for the built-in test probe.
//
// Contract: parsers only report facts (pass/fail/skip per case + duration);
// they never edit commands, never guess, and return an error (mapped to
// `inconclusive` by the caller) when the output shape is not understood.
//
// IDs are normalized so the same case gets the same id on base and candidate
// worktrees: paths are made relative to the run workdir.
package parsers

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// TestResult is the probe-level test outcome.
type TestResult struct {
	Passed     []string  `json:"passed"`
	Failed     []string  `json:"failed"`
	Skipped    []string  `json:"skipped"`
	DurationMs int       `json:"duration_ms"`
	Failures   []Failure `json:"failures,omitempty"`
}

// Failure carries a short, structured message for the agent repair loop.
type Failure struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// Parse dispatches on the test command (runner detection), then parses output.
// workdir is used to normalize absolute paths so ids are worktree-independent.
func Parse(command string, out []byte, workdir string) (TestResult, error) {
	switch {
	case strings.Contains(command, "vitest"):
		return ParseVitest(out, workdir)
	case strings.Contains(command, "jest"):
		return ParseJest(out, workdir)
	case strings.Contains(command, "go test"):
		return ParseGoTest(out)
	case strings.Contains(command, "pytest") || strings.Contains(command, "tox"):
		return ParsePytest(out)
	default:
		return TestResult{}, fmt.Errorf("no test parser for command %q", command)
	}
}

// relID normalizes an absolute path to be worktree-relative so base and
// candidate produce identical case ids (differential comparison depends on it).
// It also resolves symlinks because macOS reports /private/var/... while the
// workdir was created as /var/...
func relID(workdir, abs string) string {
	abs = strings.TrimSpace(abs)
	if workdir == "" {
		return abs
	}
	if rel, ok := trimPathPrefix(abs, workdir); ok {
		return rel
	}
	if wd, err := filepath.EvalSymlinks(workdir); err == nil {
		if a, err2 := filepath.EvalSymlinks(abs); err2 == nil {
			if rel, ok := trimPathPrefix(a, wd); ok {
				return rel
			}
		}
	}
	return abs
}

// trimPathPrefix strips prefix from p when it is a path-boundary prefix.
func trimPathPrefix(p, prefix string) (string, bool) {
	p = filepath.Clean(p)
	prefix = filepath.Clean(prefix)
	if p == prefix {
		return "", true
	}
	if strings.HasPrefix(p, prefix+string(filepath.Separator)) {
		return filepath.ToSlash(strings.TrimPrefix(p, prefix+string(filepath.Separator))), true
	}
	return "", false
}

func shortMessage(msgs []string) string {
	if len(msgs) == 0 {
		return ""
	}
	m := strings.TrimSpace(msgs[0])
	if i := strings.IndexByte(m, '\n'); i >= 0 {
		m = m[:i]
	}
	if len(m) > 300 {
		m = m[:300] + "…"
	}
	return m
}

// ---------------- vitest ----------------

type vitestReport struct {
	NumPassedTests      int  `json:"numPassedTests"`
	NumFailedTests      int  `json:"numFailedTests"`
	NumPendingTests     int  `json:"numPendingTests"`
	NumFailedTestSuites int  `json:"numFailedTestSuites"`
	Success             bool `json:"success"`
	TestResults         []struct {
		Name             string `json:"name"`
		Status           string `json:"status"`
		Message          string `json:"message"`
		AssertionResults []struct {
			FullName        string   `json:"fullName"`
			Status          string   `json:"status"`
			Duration        *float64 `json:"duration"`
			FailureMessages []string `json:"failureMessages"`
		} `json:"assertionResults"`
	} `json:"testResults"`
}

// ParseVitest parses `vitest run --reporter=json` output.
func ParseVitest(out []byte, workdir string) (TestResult, error) {
	raw, err := extractJSONObject(out)
	if err != nil {
		return TestResult{}, fmt.Errorf("vitest: %w", err)
	}
	var rep vitestReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return TestResult{}, fmt.Errorf("vitest: %w", err)
	}
	res := TestResult{Passed: []string{}, Failed: []string{}, Skipped: []string{}}
	suiteReported := false
	for _, tr := range rep.TestResults {
		file := relID(workdir, tr.Name)
		suiteFailed := false
		for _, a := range tr.AssertionResults {
			id := file + "::" + strings.TrimSpace(a.FullName)
			switch a.Status {
			case "passed":
				res.Passed = append(res.Passed, id)
			case "failed":
				res.Failed = append(res.Failed, id)
				suiteFailed = true
				res.Failures = append(res.Failures, Failure{ID: id, Message: shortMessage(a.FailureMessages)})
			default:
				res.Skipped = append(res.Skipped, id)
			}
			if a.Duration != nil {
				res.DurationMs += int(*a.Duration)
			}
		}
		// A suite can fail without any assertion result (file failed to load,
		// import error). Never drop that: synthesize a case id like Go does.
		if !suiteFailed && (tr.Status == "failed" || (len(tr.AssertionResults) == 0 && tr.Status != "passed" && tr.Status != "")) {
			id := file + "::(suite)"
			res.Failed = append(res.Failed, id)
			res.Failures = append(res.Failures, Failure{ID: id, Message: shortMessage([]string{tr.Message})})
			suiteReported = true
		}
	}
	// Suite-level failure must never be masked by unrelated case-level failures.
	if rep.Success == false && rep.NumFailedTestSuites > 0 && !suiteReported {
		res.Failed = append(res.Failed, "(test-suites)::(suite)")
		res.Failures = append(res.Failures, Failure{ID: "(test-suites)::(suite)", Message: "test suites failed without case-level detail"})
	}
	return res, nil
}

// ---------------- jest ----------------

type jestReport struct {
	NumFailedTestSuites int `json:"numFailedTestSuites"`
	TestResults         []struct {
		Name             string `json:"name"`
		Status           string `json:"status"`
		Message          string `json:"message"`
		AssertionResults []struct {
			FullName        string   `json:"fullName"`
			Status          string   `json:"status"`
			Duration        *float64 `json:"duration"`
			FailureMessages []string `json:"failureMessages"`
		} `json:"assertionResults"`
	} `json:"testResults"`
}

// ParseJest parses `jest --json` output.
func ParseJest(out []byte, workdir string) (TestResult, error) {
	raw, err := extractJSONObject(out)
	if err != nil {
		return TestResult{}, fmt.Errorf("jest: %w", err)
	}
	var rep jestReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return TestResult{}, fmt.Errorf("jest: %w", err)
	}
	res := TestResult{Passed: []string{}, Failed: []string{}, Skipped: []string{}}
	suiteReported := false
	for _, tr := range rep.TestResults {
		file := relID(workdir, tr.Name)
		suiteFailed := false
		for _, a := range tr.AssertionResults {
			id := file + "::" + strings.TrimSpace(a.FullName)
			switch a.Status {
			case "passed":
				res.Passed = append(res.Passed, id)
			case "failed":
				res.Failed = append(res.Failed, id)
				suiteFailed = true
				res.Failures = append(res.Failures, Failure{ID: id, Message: shortMessage(a.FailureMessages)})
			default:
				res.Skipped = append(res.Skipped, id)
			}
			if a.Duration != nil {
				res.DurationMs += int(*a.Duration)
			}
		}
		// A suite can fail without any assertion result (file failed to load,
		// import error). Never drop that: synthesize a case id like Go does.
		if !suiteFailed && (tr.Status == "failed" || (len(tr.AssertionResults) == 0 && tr.Status != "passed" && tr.Status != "")) {
			id := file + "::(suite)"
			res.Failed = append(res.Failed, id)
			res.Failures = append(res.Failures, Failure{ID: id, Message: shortMessage([]string{tr.Message})})
			suiteReported = true
		}
	}
	// Suite-level failure must never be masked by unrelated case-level failures.
	if rep.NumFailedTestSuites > 0 && !suiteReported {
		res.Failed = append(res.Failed, "(test-suites)::(suite)")
		res.Failures = append(res.Failures, Failure{ID: "(test-suites)::(suite)", Message: "test suites failed without case-level detail"})
	}
	return res, nil
}

// ---------------- go test ----------------

type goTestEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
}

// ParseGoTest parses `go test -json` line-delimited events.
//
// A package-level failure without a Test name (build error, init panic) is
// recorded as a synthetic case `<pkg>::(package)` so it is never silently lost.
func ParseGoTest(out []byte) (TestResult, error) {
	res := TestResult{Passed: []string{}, Failed: []string{}, Skipped: []string{}}
	seen := false
	pkgFailedNoTest := map[string]bool{}
	pkgFailedTest := map[string]bool{}
	// Per-test output collected from `-json` output events, used to explain a
	// failure without re-running the probe.
	testOutput := map[string][]string{}
	// Duration: package-level Elapsed covers the whole package, per-test events
	// also carry Elapsed — summing both would double count.
	testElapsedMs := 0
	pkgElapsedMs := 0
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev goTestEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		seen = true
		if ev.Test == "" {
			if ms := int(ev.Elapsed * 1000); ms > pkgElapsedMs {
				pkgElapsedMs = ms
			}
		} else {
			testElapsedMs += int(ev.Elapsed * 1000)
		}
		switch ev.Action {
		case "output":
			if ev.Test != "" && strings.TrimSpace(ev.Output) != "" {
				testOutput[ev.Package+"::"+ev.Test] = append(testOutput[ev.Package+"::"+ev.Test], strings.TrimSpace(ev.Output))
			}
		case "pass":
			if ev.Test != "" {
				res.Passed = append(res.Passed, ev.Package+"::"+ev.Test)
			}
		case "fail":
			if ev.Test != "" {
				id := ev.Package + "::" + ev.Test
				res.Failed = append(res.Failed, id)
				pkgFailedTest[ev.Package] = true
				if lines := testOutput[id]; len(lines) > 0 {
					res.Failures = append(res.Failures, Failure{ID: id, Message: lastMeaningful(lines)})
				}
			} else {
				pkgFailedNoTest[ev.Package] = true
			}
		case "skip":
			if ev.Test != "" {
				res.Skipped = append(res.Skipped, ev.Package+"::"+ev.Test)
			}
		}
	}
	if !seen {
		return TestResult{}, fmt.Errorf("go test: no JSON events found (use -json)")
	}
	res.DurationMs = pkgElapsedMs
	if testElapsedMs > res.DurationMs {
		res.DurationMs = testElapsedMs
	}
	for pkg := range pkgFailedNoTest {
		if !pkgFailedTest[pkg] {
			res.Failed = append(res.Failed, pkg+"::(package)")
			res.Failures = append(res.Failures, Failure{ID: pkg + "::(package)", Message: "package failed without a specific test (build/init error)"})
		}
	}
	return res, nil
}

// ---------------- pytest ----------------

var (
	pytestLineRe = regexp.MustCompile(`^(\S+?::\S+)\s+(PASSED|FAILED|SKIPPED|ERROR|XFAIL|XPASS)`)
	pytestDurRe  = regexp.MustCompile(`in ([0-9.]+)s`)
)

// ParsePytest parses `pytest -v` (or `-q`) text output. There is no native
// machine-readable reporter, so this is a documented regex fallback: when the
// shape is not recognized we return an error → inconclusive (never guess).
func ParsePytest(out []byte) (TestResult, error) {
	res := TestResult{Passed: []string{}, Failed: []string{}, Skipped: []string{}}
	seen := false
	text := string(out)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if m := pytestLineRe.FindStringSubmatch(line); m != nil {
			id, status := m[1], m[2]
			seen = true
			switch status {
			case "PASSED", "XPASS":
				res.Passed = append(res.Passed, id)
			case "FAILED", "ERROR":
				res.Failed = append(res.Failed, id)
			default: // SKIPPED, XFAIL
				res.Skipped = append(res.Skipped, id)
			}
		}
	}
	if !seen {
		return TestResult{}, fmt.Errorf("pytest: no per-test lines recognized (use -v)")
	}
	if m := pytestDurRe.FindStringSubmatch(text); m != nil {
		if secs, err := strconv.ParseFloat(m[1], 64); err == nil {
			res.DurationMs = int(secs * 1000)
		}
	}
	return res, nil
}

// lastMeaningful returns the last non-trivial output line of a failing test.
func lastMeaningful(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.HasPrefix(l, "=== RUN") || strings.HasPrefix(l, "--- FAIL") || strings.HasPrefix(l, "--- PASS") {
			continue
		}
		if len(l) > 300 {
			l = l[:300] + "…"
		}
		return l
	}
	return ""
}

// extractJSONObject tolerates leading/trailing log noise around a JSON report.
func extractJSONObject(out []byte) ([]byte, error) {
	i := strings.IndexByte(string(out), '{')
	j := strings.LastIndexByte(string(out), '}')
	if i < 0 || j <= i {
		return nil, fmt.Errorf("no JSON object in output")
	}
	return out[i : j+1], nil
}
