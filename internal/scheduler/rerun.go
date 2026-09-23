package scheduler

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/junqingyongyuanbusi/vouch/internal/differ"
	"github.com/junqingyongyuanbusi/vouch/internal/probe"
)

// caseRerunFactory builds the ReRunFunc that flaky arbitration needs. It maps
// a role to the matching worktree and re-executes only the requested cases
// using the ecosystem's own filtering flags.
// Returns nil when the runner offers no case-level filter: differ.Arbitrate's
// nil branch then keeps the regression (fail-safe) instead of downgrading a
// real failure to inconclusive.
func caseRerunFactory(baseDir, candDir, testCommand string, budgetMs int) differ.ReRunFunc {
	if _, ok := caseCommand(testCommand, []string{"probe::case"}); !ok {
		return nil
	}
	return func(ctx context.Context, role string, ids []string) (map[string]differ.CaseVerdict, error) {
		workdir := baseDir
		if role == "candidate" {
			workdir = candDir
		}
		cmd, filtered := caseCommand(testCommand, ids)
		if !filtered {
			// No case-level filter available for this runner: report nothing so
			// the arbitration stays unresolved instead of guessing.
			return map[string]differ.CaseVerdict{}, nil
		}
		res := runBuiltin(ctx, "test", workdir, role, cmd, budgetMs)
		return caseVerdicts(res, ids), nil
	}
}

// caseCommand narrows a test command to the given case ids.
// Supported: go test, pytest, vitest, jest. Returns filtered=false when the
// runner cannot filter per case (then arbitration must not guess).
func caseCommand(testCmd string, ids []string) (string, bool) {
	switch {
	case strings.Contains(testCmd, "go test"):
		pkgs := map[string]bool{}
		var names []string
		for _, id := range ids {
			i := strings.LastIndex(id, "::")
			if i <= 0 {
				continue
			}
			pkg, name := id[:i], id[i+2:]
			if strings.HasPrefix(name, "(") { // synthetic package-level failure
				continue
			}
			pkgs[pkg] = true
			names = append(names, regexp.QuoteMeta(name))
		}
		if len(names) == 0 {
			return "", false
		}
		var pkgList []string
		for p := range pkgs {
			pkgList = append(pkgList, p)
		}
		sort.Strings(pkgList)
		sort.Strings(names)
		// Derive from the recorded command so runner flags survive the rerun:
		// dropping -race/-count/... would let a race-only failure look flaky.
		base := strings.TrimSpace(testCmd)
		if !strings.Contains(base, "-json") {
			base += " -json"
		}
		if i := strings.Index(base, "./..."); i >= 0 {
			base = base[:i] + strings.Join(pkgList, " ") + base[i+len("./..."):]
		} else if i := strings.Index(base, "./"); i >= 0 {
			// Replace a narrowed package pattern with the requested packages.
			fields := strings.Fields(base)
			out := make([]string, 0, len(fields)+1)
			replaced := false
			for _, f := range fields {
				if strings.HasPrefix(f, "./") && !strings.HasPrefix(f, "-") {
					if !replaced {
						out = append(out, strings.Join(pkgList, " "))
						replaced = true
					}
					continue
				}
				out = append(out, f)
			}
			base = strings.Join(out, " ")
		} else {
			base += " " + strings.Join(pkgList, " ")
		}
		return base + " -run '^(" + strings.Join(names, "|") + ")$'", true

	case strings.Contains(testCmd, "pytest"):
		var args []string
		for _, id := range ids {
			if strings.Contains(id, "::") {
				args = append(args, shellQuote(id))
			}
		}
		if len(args) == 0 {
			return "", false
		}
		base := "pytest -v"
		if strings.Contains(testCmd, "python3 -m pytest") {
			base = "python3 -m pytest -v"
		}
		return base + " " + strings.Join(args, " "), true

	case strings.Contains(testCmd, "vitest"):
		files := map[string]bool{}
		var titles []string
		for _, id := range ids {
			i := strings.LastIndex(id, "::")
			if i <= 0 {
				continue
			}
			files[id[:i]] = true
			titles = append(titles, regexp.QuoteMeta(strings.TrimSpace(id[i+2:])))
		}
		if len(files) == 0 {
			return "", false
		}
		var fileList []string
		for f := range files {
			fileList = append(fileList, f)
		}
		sort.Strings(fileList)
		cmd := "vitest run " + strings.Join(fileList, " ")
		if len(titles) > 0 {
			cmd += " -t " + shellQuote(strings.Join(titles, "|"))
		}
		return cmd + " --reporter=json", true

	case strings.Contains(testCmd, "jest"):
		files := map[string]bool{}
		var titles []string
		for _, id := range ids {
			i := strings.LastIndex(id, "::")
			if i <= 0 {
				continue
			}
			files[id[:i]] = true
			titles = append(titles, regexp.QuoteMeta(strings.TrimSpace(id[i+2:])))
		}
		if len(files) == 0 {
			return "", false
		}
		var fileList []string
		for f := range files {
			fileList = append(fileList, f)
		}
		sort.Strings(fileList)
		cmd := "jest " + strings.Join(fileList, " ")
		if len(titles) > 0 {
			cmd += " -t " + shellQuote(strings.Join(titles, "|"))
		}
		return cmd + " --json", true

	default:
		return "", false
	}
}

// caseVerdicts extracts the requested ids' outcomes from a probe result.
func caseVerdicts(res probe.RunResult, ids []string) map[string]differ.CaseVerdict {
	out := map[string]differ.CaseVerdict{}
	if res.Data == nil {
		return out
	}
	failed := stringSet(res.Data["failed"])
	passed := stringSet(res.Data["passed"])
	for _, id := range ids {
		switch {
		case failed[id]:
			out[id] = differ.CaseFailed
		case passed[id]:
			out[id] = differ.CasePassed
		}
	}
	return out
}

func stringSet(v interface{}) map[string]bool {
	out := map[string]bool{}
	switch xs := v.(type) {
	case []string:
		for _, x := range xs {
			out[x] = true
		}
	case []interface{}:
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

func shellQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'|()[]{}*?$&;<>\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CaseFilterFor is exported for scheduler tests and future CLI diagnostics: it
// reports the narrowed command and whether case-level filtering is available.
func CaseFilterFor(testCmd string, ids []string) (string, bool) { return caseCommand(testCmd, ids) }

// RerunSupported reports whether a runner can be re-run per case.
func RerunSupported(testCmd string) bool { return caseRerunFactory("", "", testCmd, 0) != nil }
