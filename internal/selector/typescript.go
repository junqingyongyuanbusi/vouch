package selector

import "strings"

// selectTypeScript uses the runner's own reverse-dependency resolution:
// vitest `related` and jest `--findRelatedTests` both accept changed files and
// resolve the affected test set natively — we do not reimplement module graphs.
func selectTypeScript(files []string, testCmd string) Selection {
	switch {
	case strings.Contains(testCmd, "vitest"):
		return Selection{Targets: testFilesOrSources(files), Mechanism: "vitest related"}
	case strings.Contains(testCmd, "jest"):
		return Selection{Targets: testFilesOrSources(files), Mechanism: "jest --findRelatedTests"}
	default:
		return Selection{
			FullRun:   true,
			Mechanism: "full fallback",
			Reason:    "test command is neither vitest nor jest: " + testCmd,
		}
	}
}

// testFilesOrSources keeps changed test files and passes source files through;
// the runner resolves the reverse dependency set itself.
func testFilesOrSources(files []string) []string {
	// Pass-through today; the runner resolves the reverse dependency set itself.
	return append(make([]string, 0, len(files)), files...)
}
