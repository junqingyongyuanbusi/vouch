// Package verdict — pure-function aggregation of deterministic evidence.
//
// Verdict is a pure function (PLAN-V2 §7.4): same inputs → same outputs, no I/O,
// exhaustively unit-tested. Inferred results are never consulted here; the caller
// must pass only deterministic evidence. Empty set → UNVERIFIED.
package verdict

import (
	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
)

// Result is the aggregation outcome plus human-actionable reasons.
type Result struct {
	Verdict          bundle.GlobalVerdict
	Warnings         []string // non-blocking: removed_tests/skip_changes on VERIFIED
	UnverifiedReason []string // populated when UNVERIFIED
	FailedProbes     []string // probes whose verdict == fail (for BROKEN)
}

// Aggregate implements §7.4 (with v2 spec fixes):
//
//	any deterministic evidence verdict == fail              → BROKEN
//	any verdict == inconclusive (and no fail)               → UNVERIFIED
//	empty deterministic set                                 → UNVERIFIED
//	all pass (even with removed_tests/skip_changes)         → VERIFIED + warnings
//
// Invariant (Differ contract): regressions non-empty implies verdict == fail.
// The host is defensive: if a probe violates the invariant (pass + regressions),
// that evidence is demoted to UNVERIFIED with a warning instead of silently
// becoming VERIFIED and hiding a regression.
//
// The removed_tests/skip_changes case stays VERIFIED (no regression on diff
// semantics) but must surface a warning line for human review — masking a
// regression by deleting a failing test is not a regression, but it is suspicious.
func Aggregate(evidences []bundle.Evidence) Result {
	if len(evidences) == 0 {
		return Result{
			Verdict:          bundle.Unverified,
			UnverifiedReason: []string{"no deterministic evidence: no probe produced evidence"},
		}
	}

	var failed []string
	var inconclusive []string
	var warnings []string

	for _, ev := range evidences {
		// Defensive invariant: Differ must set fail when regressions non-empty.
		// A pass with regressions is illegal — treat as inconclusive so we never
		// silently VERIFIED a real regression.
		if ev.Verdict == bundle.EvidencePass && len(ev.Delta.Regressions) > 0 {
			inconclusive = append(inconclusive, ev.Probe)
			warnings = append(warnings, "probe "+ev.Probe+": invariant violation — pass with regressions "+joinList(ev.Delta.Regressions)+" (demoted to inconclusive)")
		} else {
			switch ev.Verdict {
			case bundle.EvidenceFail:
				failed = append(failed, ev.Probe)
			case bundle.EvidenceInconclusive:
				inconclusive = append(inconclusive, ev.Probe)
			case bundle.EvidencePass:
				// pass — check masking below
			default:
				inconclusive = append(inconclusive, ev.Probe)
			}
		}

		if len(ev.Delta.RemovedTests) > 0 {
			warnings = append(warnings, "probe "+ev.Probe+": removed_tests "+joinList(ev.Delta.RemovedTests)+" (deletion of base-failing tests is not a regression but needs review)")
		}
		if len(ev.Delta.SkipChanges) > 0 {
			warnings = append(warnings, "probe "+ev.Probe+": skip_changes "+joinList(ev.Delta.SkipChanges))
		}
		if len(ev.Delta.Regressions) > 0 && ev.Verdict != bundle.EvidencePass {
			// Surface regressions as warning as well for human triage.
			warnings = append(warnings, "probe "+ev.Probe+": regressions "+joinList(ev.Delta.Regressions))
		}
	}

	if len(failed) > 0 {
		return Result{
			Verdict:      bundle.Broken,
			FailedProbes: failed,
			Warnings:     warnings,
		}
	}
	if len(inconclusive) > 0 {
		reasons := make([]string, 0, len(inconclusive))
		for _, p := range inconclusive {
			reasons = append(reasons, "probe "+p+": inconclusive")
		}
		return Result{
			Verdict:          bundle.Unverified,
			UnverifiedReason: reasons,
			Warnings:         warnings,
		}
	}

	return Result{
		Verdict:  bundle.Verified,
		Warnings: warnings,
	}
}

func joinList(ss []string) string {
	if len(ss) == 1 {
		return ss[0]
	}
	out := "["
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	out += "]"
	return out
}

// ExitCode maps the three-state verdict to process exit codes for --ci.
// 0=VERIFIED, 1=BROKEN, 2=UNVERIFIED (PLAN-V2 §3, principle three).
func ExitCode(v bundle.GlobalVerdict) int {
	switch v {
	case bundle.Verified:
		return 0
	case bundle.Broken:
		return 1
	default:
		return 2
	}
}
