// Package selector — P1.3 — affected-test selection per PLAN-V2 §7.3.
//
// Ecosystem-native mechanisms only (no self-built dependency graphs where the
// runner already solves it):
//   - TypeScript: vitest `related` / jest `--findRelatedTests`
//   - Go:         `go list -json ./...` import graph, reverse deps of changed files
//   - Python:     path mapping (src/foo.py → tests/test_foo.py); no mapping → full run
//
// Honesty rule: when we cannot map a changed file to a narrower target, we
// return FullRun=true instead of guessing.
package selector

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
)

// Selection describes the affected-test subset for one verify run.
//
// Contract for the scheduler: compute the Selection ONCE (on the candidate
// view) and apply the same target set to both base and candidate worktrees.
// Selecting per worktree would let a newly added test file change only one
// side and fabricate removed_tests/new_passing deltas.
type Selection struct {
	// FullRun means no reliable narrowing was possible; run the whole suite.
	FullRun bool
	// Targets are runner-specific targets appended by Apply: files for
	// vitest/jest/pytest, package patterns (./pkg) for go.
	Targets []string
	// Mechanism is the human-auditable selection mechanism name.
	Mechanism string
	// Reason explains a FullRun fallback ("" when narrowed).
	Reason string
}

// Select computes the affected-test selection for diffFiles under profile.
func Select(repoRoot string, diffFiles []string, profile bundle.ProjectProfile) Selection {
	files := normalizeFiles(repoRoot, diffFiles)
	if len(files) == 0 {
		return Selection{FullRun: true, Mechanism: "empty diff", Reason: "no changed files"}
	}
	testCmd := profile.Commands["test"].Cmd
	switch {
	case isTypeScript(profile):
		return selectTypeScript(files, testCmd)
	case isGo(profile):
		return selectGo(repoRoot, files)
	case isPython(profile):
		return selectPython(repoRoot, files)
	default:
		return Selection{FullRun: true, Mechanism: "unsupported ecosystem", Reason: "no selector for this profile"}
	}
}

// Apply builds the command to run for a Selection.
// FullRun (or empty mechanism) returns the detector's original command.
func Apply(profile bundle.ProjectProfile, sel Selection) string {
	cmd, _ := ApplyNarrowed(profile, sel)
	return cmd
}

// ApplyNarrowed returns the narrowed command plus whether narrowing actually
// happened. A recorded command that does not invoke the ecosystem runner (e.g.
// a Makefile target `make test`) cannot be narrowed by appending paths — that
// would produce an unrunnable command — so callers must fall back to a full run.
func ApplyNarrowed(profile bundle.ProjectProfile, sel Selection) (string, bool) {
	original := profile.Commands["test"].Cmd
	if sel.FullRun || original == "" || len(sel.Targets) == 0 {
		return original, false
	}
	targets := shellJoin(sel.Targets)
	switch sel.Mechanism {
	case "vitest related":
		// Reuse the detected invocation so pnpm/yarn repos don't fall back to
		// `npx` (which may try to fetch from the network under default sandbox).
		if strings.Contains(original, "vitest run") {
			return strings.Replace(original, "vitest run", "vitest related "+targets+" --run", 1), true
		}
		if strings.Contains(original, "vitest") {
			return original + " related " + targets + " --run", true
		}
		return "npx vitest related " + targets + " --run", true
	case "jest --findRelatedTests":
		return original + " --findRelatedTests " + targets, true
	case "go list import graph":
		if !strings.Contains(original, "go test") {
			// e.g. `make test`: appending package paths would be an invalid
			// make invocation. Caller must run the project command in full.
			return original, false
		}
		if strings.Contains(original, "./...") {
			return strings.Replace(original, "./...", targets, 1), true
		}
		return original + " " + targets, true
	case "pytest path map":
		if !strings.Contains(original, "pytest") {
			return original, false
		}
		// Keep the detected/overridden invocation (e.g. `python3 -m pytest`,
		// `uv run pytest`, profile override) and append the selected cases.
		return original + " " + targets, true
	default:
		return original, false
	}
}

// NewProfileWithSelection records the selection mechanism into the profile
// (bundle.test_selection) so bundles carry why they ran what they ran.
func NewProfileWithSelection(profile bundle.ProjectProfile, sel Selection) bundle.ProjectProfile {
	p := profile
	supported := !sel.FullRun && len(sel.Targets) > 0
	p.TestSelection = &bundle.TestSelection{
		Supported: supported,
		Mechanism: sel.Mechanism,
	}
	return p
}

func normalizeFiles(repoRoot string, diffFiles []string) []string {
	out := make([]string, 0, len(diffFiles))
	seen := map[string]bool{}
	for _, f := range diffFiles {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		f = filepath.ToSlash(f)
		if filepath.IsAbs(f) {
			if rel, err := filepath.Rel(repoRoot, f); err == nil {
				f = filepath.ToSlash(rel)
			}
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func isTypeScript(p bundle.ProjectProfile) bool {
	for _, l := range p.Language {
		if l == "typescript" || l == "javascript" {
			return true
		}
	}
	return false
}

func isGo(p bundle.ProjectProfile) bool {
	for _, l := range p.Language {
		if l == "go" {
			return true
		}
	}
	return false
}

func isPython(p bundle.ProjectProfile) bool {
	for _, l := range p.Language {
		if l == "python" {
			return true
		}
	}
	return false
}

func shellJoin(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, it := range items {
		if strings.ContainsAny(it, " \t\"'") {
			quoted = append(quoted, "'"+strings.ReplaceAll(it, "'", `'\''`)+"'")
		} else {
			quoted = append(quoted, it)
		}
	}
	return strings.Join(quoted, " ")
}

// RestrictToExisting prunes targets that do not exist under root. This is used
// for the BASE side of a differential run: the selection is computed once on
// the candidate, and tests that only exist on the candidate side cannot be run
// on base. If nothing remains, the caller falls back to a full run.
func RestrictToExisting(root string, sel Selection, profile bundle.ProjectProfile) Selection {
	if sel.FullRun || len(sel.Targets) == 0 {
		return sel
	}
	out := sel
	out.Targets = nil
	for _, t := range sel.Targets {
		if existsUnder(root, t) {
			out.Targets = append(out.Targets, t)
		}
	}
	if len(out.Targets) == 0 {
		out.FullRun = true
		out.Targets = nil
		out.Reason = "no selected target exists on this side; falling back to full run"
	}
	return out
}
