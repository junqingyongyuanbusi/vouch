// Package differ — P1.5 — differential comparison of probe runs.
//
// Ownership: built-in probes only report facts (passed/failed/skipped lists,
// diagnostics, exit codes). The verdict-relevant interpretation — delta
// (regressions / new_passing / removed_tests / skip_changes / flaky_absorbed)
// and the evidence verdict — lives here, so there is exactly one implementation
// of the differential semantics that the gate depends on.
package differ

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/probe"
)

// Evidence assembles a validated bundle.Evidence from the two probe runs.
//
// Differential semantics (PLAN-V2 §7.4): the evidence verdict reflects the
// *delta*, not the raw candidate result —
//   - either side inconclusive              → inconclusive
//   - a regression relative to base         → fail
//   - anything else (incl. base-fail kept)  → pass, with base failures recorded
//     via Baseline.Status / baseline_summary
func Evidence(probeName, reproduce, envFingerprint string, startedAt time.Time, base, cand probe.RunResult) (bundle.Evidence, error) {
	return EvidenceWithArbitration(probeName, reproduce, envFingerprint, startedAt, base, cand, TestArbitration{})
}

// EvidenceWithArbitration is Evidence plus the flaky-arbitration outcome:
// absorbed cases move into flaky_absorbed and out of regressions; unresolved
// cases force the evidence to inconclusive instead of a guessed BROKEN.
func EvidenceWithArbitration(probeName, reproduce, envFingerprint string, startedAt time.Time, base, cand probe.RunResult, arb TestArbitration) (bundle.Evidence, error) {
	ev := bundle.Evidence{
		ID:             evidenceID(probeName, base, cand),
		Claim:          claimFor(probeName),
		Kind:           bundle.KindDeterministic,
		Probe:          probeName,
		Method:         methodFor(probeName),
		Reproduce:      reproduce,
		StartedAt:      startedAt,
		EnvFingerprint: envFingerprint,
		Delta:          emptyDelta(),
	}
	// A side that reports fail without any attributable case is not comparable:
	// we must not read it as "no regression".
	unattributable := false
	// Dispatch on the *data shape* the probe reported (probe-protocol.md §6),
	// never on the probe name: third-party probes with the same shape get the
	// same semantics, and unrecognized shapes can never default to pass.
	switch {
	case hasTestShape(base) && hasTestShape(cand):
		b := probe.TestFactsOf(base)
		c := probe.TestFactsOf(cand)
		ev.Baseline = bundle.BaselineResult{
			Pass:       len(b.Passed),
			Fail:       len(b.Failed),
			Skip:       len(b.Skipped),
			DurationMs: max0(b.DurationMs),
			Status:     baselineStatus(base),
		}
		ev.Candidate = bundle.CandidateResult{
			Pass:       len(c.Passed),
			Fail:       len(c.Failed),
			Skip:       len(c.Skipped),
			DurationMs: max0(c.DurationMs),
		}
		ev.Delta = testDelta(b, c)
		if emptyCaseDomain(b) && emptyCaseDomain(c) {
			// Zero cases on both sides means the same statement as zero
			// evidence: nothing was measured. VERIFIED with pass=0 would be a
			// lie ("no tests ran" != "no regressions").
			unattributable = true
		}
		if base.Verdict == probe.VerdictFail && len(b.Failed) == 0 {
			unattributable = true
		}
		if cand.Verdict == probe.VerdictFail && len(c.Failed) == 0 {
			unattributable = true
		}
		ev.Failures = failuresFor(c.Failures, ev.Delta.Regressions)
		if len(arb.Absorbed) > 0 {
			absorbed := map[string]bool{}
			for _, id := range arb.Absorbed {
				absorbed[id] = true
			}
			var keptRegressions []string
			var movedToFlaky []string
			for _, id := range ev.Delta.Regressions {
				if absorbed[id] {
					movedToFlaky = append(movedToFlaky, id)
					continue
				}
				keptRegressions = append(keptRegressions, id)
			}
			if keptRegressions == nil {
				keptRegressions = []string{}
			}
			ev.Delta.Regressions = keptRegressions
			// Absorbed failures must not survive in the failure messages either:
			// a VERIFIED bundle that still explains "this case failed" would
			// contradict itself.
			var keptFailures []bundle.FailureDetail
			for _, f := range failuresFor(c.Failures, ev.Delta.Regressions) {
				if !absorbed[f.ID] {
					keptFailures = append(keptFailures, f)
				}
			}
			ev.Failures = keptFailures
			// An absorbed case must not also be reported as a gain: the same id
			// cannot be both "newly passing" and "flaky".
			var keptNewPassing []string
			for _, id := range ev.Delta.NewPassing {
				if !absorbed[id] {
					keptNewPassing = append(keptNewPassing, id)
				}
			}
			if keptNewPassing == nil {
				keptNewPassing = []string{}
			}
			ev.Delta.NewPassing = keptNewPassing
			ev.Delta.FlakyAbsorbed = append(append([]string{}, ev.Delta.FlakyAbsorbed...), movedToFlaky...)
			sort.Strings(ev.Delta.FlakyAbsorbed)
		}
	case hasDiagShape(base) || hasDiagShape(cand):
		b := probe.DiagnosticsOf(base)
		c := probe.DiagnosticsOf(cand)
		ev.Baseline = bundle.BaselineResult{Fail: len(b), Status: baselineStatus(base)}
		ev.Candidate = bundle.CandidateResult{Fail: len(c)}
		ev.Delta.Regressions = newDiagnostics(b, c)
		if base.Verdict == probe.VerdictPass && cand.Verdict == probe.VerdictFail && len(ev.Delta.Regressions) == 0 {
			// Failure not attributable to a parsed diagnostic: do not pass silently.
			ev.Delta.Regressions = []string{probeName + ":unattributed"}
		}
	default:
		// Shape unknown (e.g. build probe, third-party probe with summary only):
		// fall back to the probe-level verdicts, which remain a deterministic
		// signal. A green base and a failing candidate is never "pass".
		ev.Baseline = bundle.BaselineResult{
			DurationMs: max0(intField(base, "duration_ms")),
			Status:     baselineStatus(base),
		}
		ev.Candidate = bundle.CandidateResult{DurationMs: max0(intField(cand, "duration_ms"))}
		if base.Verdict == probe.VerdictPass && cand.Verdict == probe.VerdictFail {
			ev.Delta.Regressions = []string{probeName}
		}
	}

	if len(arb.Unresolved) > 0 {
		unresolved := make(map[string]bool, len(arb.Unresolved))
		for _, id := range arb.Unresolved {
			unresolved[id] = true
		}
		confirmed := make([]string, 0, len(ev.Delta.Regressions))
		for _, id := range ev.Delta.Regressions {
			if !unresolved[id] {
				confirmed = append(confirmed, id)
			}
		}
		ev.Delta.Regressions = confirmed
		ev.Method += "; unresolved cases: " + strings.Join(arb.Unresolved, ", ")
	}
	switch {
	case len(arb.Unresolved) > 0 && len(ev.Delta.Regressions) > 0:
		ev.Verdict = bundle.EvidenceFail
	case len(arb.Unresolved) > 0:
		ev.Verdict = bundle.EvidenceInconclusive
	case unattributable:
		// Suite/scaffold failure with no case-level attribution (e.g. a JS test
		// file that failed to load): inconclusive, never a silent pass.
		ev.Verdict = bundle.EvidenceInconclusive
	case base.Verdict == probe.VerdictInconclusive || cand.Verdict == probe.VerdictInconclusive:
		ev.Verdict = bundle.EvidenceInconclusive
	case len(ev.Delta.Regressions) > 0:
		ev.Verdict = bundle.EvidenceFail
	default:
		ev.Verdict = bundle.EvidencePass
	}
	if err := ev.Validate(); err != nil {
		return bundle.Evidence{}, fmt.Errorf("built evidence invalid: %w", err)
	}
	return ev, nil
}

func claimFor(probe string) string {
	switch probe {
	case "test":
		return "change does not introduce test regressions"
	case "build":
		return "change does not break the build"
	case "typecheck":
		return "change does not add type diagnostics"
	default:
		return "probe " + probe + " reports no regression"
	}
}

func methodFor(probe string) string {
	return probe + " (base/candidate, same selection)"
}

func baselineStatus(r probe.RunResult) bundle.BaselineStatus {
	switch r.Verdict {
	case probe.VerdictPass:
		// A passing base run can still contain failing cases (existing failures):
		// that distinction is carried by the counts, not by this status.
		return bundle.BaselineAllGreen
	case probe.VerdictFail:
		return bundle.BaselineExistingFailures
	default:
		return bundle.BaselineExistingBroken
	}
}

func evidenceID(probe string, base, cand probe.RunResult) string {
	seed := probe + "|" + string(base.Verdict) + "|" + string(cand.Verdict)
	h := bundle.EnvFingerprint(seed)
	hexPart := strings.TrimPrefix(h, "sha256:")
	return "ev_" + hexPart[:6]
}

// hasTestShape reports whether a result carries per-case test data.
func hasTestShape(r probe.RunResult) bool {
	if r.Data == nil {
		return false
	}
	_, ok := r.Data["passed"]
	return ok
}

// hasDiagShape reports whether a result carries structured diagnostics.
func hasDiagShape(r probe.RunResult) bool {
	if r.Data == nil {
		return false
	}
	_, ok := r.Data["diagnostics"]
	return ok
}

func emptyDelta() bundle.Delta {
	return bundle.Delta{
		Regressions:   []string{},
		NewPassing:    []string{},
		RemovedTests:  []string{},
		SkipChanges:   []string{},
		FlakyAbsorbed: []string{},
	}
}

// testDelta compares per-case lists. Only the delta is a signal:
// a failure present on both sides is an existing failure, not a regression.
func testDelta(base, cand probe.TestFacts) bundle.Delta {
	d := emptyDelta()
	inBaseFailed := set(base.Failed)
	inBasePassed := set(base.Passed)
	inBaseAll := set(append(append(append([]string{}, base.Passed...), base.Failed...), base.Skipped...))
	inCandAll := set(append(append(append([]string{}, cand.Passed...), cand.Failed...), cand.Skipped...))
	inBaseSkipped := set(base.Skipped)
	inCandSkipped := set(cand.Skipped)

	for _, id := range cand.Failed {
		if !inBaseFailed[id] {
			d.Regressions = append(d.Regressions, id)
		}
	}
	for _, id := range cand.Passed {
		if !inBasePassed[id] {
			d.NewPassing = append(d.NewPassing, id)
		}
	}
	for id := range inBaseAll {
		if !inCandAll[id] {
			d.RemovedTests = append(d.RemovedTests, id)
		}
	}
	for id := range inCandSkipped {
		if !inBaseSkipped[id] {
			d.SkipChanges = append(d.SkipChanges, "+skip:"+id)
		}
	}
	for id := range inBaseSkipped {
		if !inCandSkipped[id] {
			d.SkipChanges = append(d.SkipChanges, "-skip:"+id)
		}
	}
	sort.Strings(d.Regressions)
	sort.Strings(d.NewPassing)
	sort.Strings(d.RemovedTests)
	sort.Strings(d.SkipChanges)
	return d
}

// failuresFor keeps the failure messages of the cases we actually report as
// regressions (other failures are pre-existing and would only add noise).
func failuresFor(failures []probe.Failure, regressions []string) []bundle.FailureDetail {
	if len(failures) == 0 || len(regressions) == 0 {
		return nil
	}
	want := make(map[string]bool, len(regressions))
	for _, id := range regressions {
		want[id] = true
	}
	var out []bundle.FailureDetail
	for _, f := range failures {
		if want[f.ID] {
			out = append(out, bundle.FailureDetail{ID: f.ID, Message: f.Message})
		}
	}
	// No attribution match: better to explain nothing than to attach an
	// unrelated pre-existing failure to this regression.
	return out
}

// emptyCaseDomain reports whether a test result carries no cases at all.
func emptyCaseDomain(f probe.TestFacts) bool {
	return len(f.Passed) == 0 && len(f.Failed) == 0 && len(f.Skipped) == 0
}

func set(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func intField(r probe.RunResult, key string) int {
	if r.Data == nil {
		return 0
	}
	switch v := r.Data[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func newDiagnostics(base, cand []probe.Diagnostic) []string {
	inBase := map[string]bool{}
	for _, d := range base {
		inBase[diagID(d)] = true
	}
	var out []string
	for _, d := range cand {
		if !inBase[diagID(d)] {
			out = append(out, diagID(d))
		}
	}
	sort.Strings(out)
	return out
}

func diagID(d probe.Diagnostic) string {
	return fmt.Sprintf("%s:%d:%d:%s", d.File, d.Line, d.Column, d.Code)
}
