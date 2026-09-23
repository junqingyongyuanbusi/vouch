package builtin

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/junqingyongyuanbusi/vouch/internal/probe"
	"github.com/junqingyongyuanbusi/vouch/internal/probe/builtin/parsers"
)

// Test is the built-in test probe: runs the selected test command once and
// reports per-case facts (passed/failed/skipped + duration). It never judges a
// delta — that is the differ's job.
type Test struct{}

func (Test) Name() string     { return "test" }
func (Test) Kind() probe.Kind { return probe.KindDeterministic }

func (Test) Run(ctx context.Context, in Input) (probe.RunResult, error) {
	if in.Command == "" {
		return probe.Inconclusive("no test command in profile", in.Role), nil
	}
	cmd := ensureMachineReadable(in.Command)
	out, exit, timedOut, err := runShell(ctx, Input{
		Workdir: in.Workdir, Role: in.Role, Command: cmd, BudgetMs: in.BudgetMs, NeedsNetwork: in.NeedsNetwork,
	})
	if err != nil {
		return probe.Inconclusive("run: "+err.Error(), in.Role), nil
	}
	if timedOut {
		return probe.Inconclusive("command killed by budget", in.Role), nil
	}

	res, perr := parsers.Parse(cmd, []byte(out), in.Workdir)
	if perr != nil {
		if missingRunner(out, exit) {
			return probe.Inconclusive("runner not available ("+cmd+")", in.Role), nil
		}
		if noTestsCollected(out, exit) {
			return probe.Inconclusive("runner collected no tests ("+cmd+")", in.Role), nil
		}
		// No case-level parser for this command (e.g. `make test`): report the
		// command-level fact only. No test-shape data is emitted, so differ
		// treats it with verdict-level semantics (base pass + cand fail =
		// regression) instead of inventing case lists.
		verdict := probe.VerdictPass
		summary := "command passed (no case-level parser: " + perr.Error() + ")"
		if exit != 0 {
			verdict = probe.VerdictFail
			summary = "command failed (no case-level parser: " + perr.Error() + ")"
		}
		return probe.RunResult{
			Verdict: verdict,
			Summary: summary,
			Reason:  perr.Error(),
			Data:    map[string]interface{}{"exit_code": exit, "role": in.Role},
		}, nil
	}
	verdict := probe.VerdictPass
	if len(res.Failed) > 0 {
		verdict = probe.VerdictFail
	} else if exit != 0 {
		// Command failed but no case-level failure parsed: report fail with the
		// exit code rather than silently passing.
		verdict = probe.VerdictFail
	}
	data := map[string]interface{}{}
	data["passed"] = res.Passed
	data["failed"] = res.Failed
	data["skipped"] = res.Skipped
	data["duration_ms"] = res.DurationMs
	data["exit_code"] = exit
	if len(res.Failures) > 0 {
		data["failures"] = res.Failures
	}
	return probe.RunResult{
		Verdict: verdict,
		Summary: fmt.Sprintf("%d passed, %d failed, %d skipped", len(res.Passed), len(res.Failed), len(res.Skipped)),
		Data:    data,
	}, nil
}

// TestResultOf extracts the parsed per-case lists from a RunResult. It is a
// thin adapter over probe.TestFactsOf (the protocol-layer reader).
func TestResultOf(r probe.RunResult) parsers.TestResult {
	f := probe.TestFactsOf(r)
	return parsers.TestResult{
		Passed:     f.Passed,
		Failed:     f.Failed,
		Skipped:    f.Skipped,
		DurationMs: f.DurationMs,
	}
}

// ensureMachineReadable appends the machine-readable flag a parser depends on
// when the detected command does not already ask for it. This is the bridge
// between "how the project runs tests" (detector) and "how we read the result"
// (parsers); without it, e.g. `go test ./...` yields non-JSON output that the
// parser must (correctly) refuse as inconclusive.
func ensureMachineReadable(cmd string) string {
	switch {
	case strings.Contains(cmd, "vitest"):
		if !strings.Contains(cmd, "--reporter=json") {
			return cmd + " --reporter=json"
		}
	case strings.Contains(cmd, "jest"):
		if !strings.Contains(cmd, "--json") {
			return cmd + " --json"
		}
	case strings.Contains(cmd, "go test"):
		if !strings.Contains(cmd, "-json") {
			return cmd + " -json"
		}
	case strings.Contains(cmd, "pytest"):
		cmd = resolvePytestInvocation(cmd)
		if !strings.Contains(cmd, "-v") && !strings.Contains(cmd, "--verbose") {
			// -q suppresses the per-test lines the regex fallback needs.
			cmd = strings.ReplaceAll(cmd, " -q", "")
			cmd = cmd + " -v"
		}
		return cmd
	}
	return cmd
}

// noTestsCollected reports the "runner worked, nothing was measured" signal:
// pytest exits 5 ("no tests ran"), vitest/jest print "No test files found" /
// "No tests found". Reading that as pass would let an empty run look green.
func noTestsCollected(out string, exit int) bool {
	low := strings.ToLower(out)
	// A collection error means tests exist but could not even be imported —
	// that is a real failure (usually exactly the regression being verified),
	// not "nothing to measure".
	if strings.Contains(low, "error during collection") || strings.Contains(low, "error collecting") {
		return false
	}
	for _, marker := range []string{"no test files found", "no tests found"} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	// pytest: exit 5 is the documented "no tests collected" code.
	return exit == 5 && strings.Contains(low, "no tests")
}

// resolvePytestInvocation rewrites a bare `pytest ...` into `python3 -m pytest ...`
// when the console script is absent (pip --user / venv installs commonly lack
// the PATH entry). Without this a project with a working pytest would be
// reported UNVERIFIED, which is exactly the failure the prerequisites note
// cannot cover by itself.
func resolvePytestInvocation(cmd string) string {
	if !strings.HasPrefix(strings.TrimSpace(cmd), "pytest") {
		return cmd
	}
	if _, err := exec.LookPath("pytest"); err == nil {
		return cmd
	}
	if _, err := exec.LookPath("python3"); err != nil {
		return cmd
	}
	if err := exec.Command("python3", "-m", "pytest", "--version").Run(); err != nil {
		return cmd
	}
	return "python3 -m " + cmd
}
