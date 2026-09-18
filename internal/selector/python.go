package selector

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// selectPython maps changed source files to test files by convention:
//
//	src/foo.py        → tests/test_foo.py | tests/foo_test.py | src/test_foo.py
//	tests/test_foo.py → tests/test_foo.py (already a test)
//
// When any changed file has no mapping we fall back to a full run — honest
// over clever (PLAN-V2 §7.3).
func selectPython(repoRoot string, files []string) Selection {
	targets := make([]string, 0, len(files))
	seen := map[string]bool{}
	for _, f := range files {
		candidates := pythonTestCandidates(f)
		matched := ""
		for _, c := range candidates {
			if existsUnder(repoRoot, c) {
				matched = c
				break
			}
		}
		if matched == "" {
			return Selection{
				FullRun:   true,
				Mechanism: "full fallback",
				Reason:    "no conventional test file for " + f,
			}
		}
		if !seen[matched] {
			seen[matched] = true
			targets = append(targets, matched)
		}
	}
	sort.Strings(targets)
	return Selection{Targets: targets, Mechanism: "pytest path map"}
}

// pythonTestCandidates returns conventional test file paths for a source file.
func pythonTestCandidates(f string) []string {
	dir := filepath.Dir(f)
	base := filepath.Base(f)
	if !strings.HasSuffix(base, ".py") {
		return nil
	}
	stem := strings.TrimSuffix(base, ".py")
	// Already a test file.
	if strings.HasPrefix(stem, "test_") || strings.HasSuffix(stem, "_test") {
		return []string{f}
	}
	return []string{
		filepath.ToSlash(filepath.Join("tests", "test_"+stem+".py")),
		filepath.ToSlash(filepath.Join("tests", stem+"_test.py")),
		filepath.ToSlash(filepath.Join(dir, "test_"+stem+".py")),
		filepath.ToSlash(filepath.Join(dir, stem+"_test.py")),
		filepath.ToSlash(filepath.Join(dir, "tests", "test_"+stem+".py")),
	}
}

func existsUnder(repoRoot, rel string) bool {
	_, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(rel)))
	return err == nil
}
