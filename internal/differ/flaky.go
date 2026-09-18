package differ

import (
	"context"
	"sort"

	"github.com/junqingyongyuanbusi/vouch/internal/probe"
)

// CaseVerdict is the re-run outcome for one case.
type CaseVerdict string

const (
	CasePassed CaseVerdict = "pass"
	CaseFailed CaseVerdict = "fail"
)

// ReRunFunc re-executes the given case ids on one side and reports their
// verdicts. It is supplied by the scheduler (it owns the worktrees); differ
// stays pure apart from this injected effect.
type ReRunFunc func(ctx context.Context, role string, ids []string) (map[string]CaseVerdict, error)

// TestArbitration is the outcome of flaky arbitration over unstable cases.
type TestArbitration struct {
	// Absorbed are cases whose disagreement was instability, not a regression:
	// they are removed from regressions and surfaced as flaky_absorbed.
	Absorbed []string
	// Unresolved are cases that stayed contradictory after the rerun budget.
	// They make the evidence inconclusive rather than guessing BROKEN.
	Unresolved []string
}

// MaxFlakyReruns is the arbitration budget per side (PLAN-V2 §7.4: ≤2).
const MaxFlakyReruns = 2

// Arbitrate resolves cases where base and candidate disagree.
//
// Rules (deterministic, bounded):
//   - candidate retries pass, base stays green      → unresolved (inconclusive)
//   - base fails on any rerun (both sides unstable)  → flaky (absorbed)
//   - candidate fails every rerun, base stays green  → regression (kept)
//   - rerun error / no verdict                       → unresolved (inconclusive)
//
// Only disagreeing cases are re-run: stable cases cost nothing.
func Arbitrate(ctx context.Context, base, cand probe.TestFacts, rerun ReRunFunc) (TestArbitration, error) {
	baseStatus := caseMap(base)
	candStatus := caseMap(cand)
	var unstable []string
	for id, cv := range candStatus {
		bv, ok := baseStatus[id]
		if !ok {
			continue // new test: not a disagreement
		}
		if bv != cv {
			unstable = append(unstable, id)
		}
	}
	sort.Strings(unstable)
	arb := TestArbitration{}
	if len(unstable) == 0 {
		return arb, nil
	}
	if rerun == nil {
		// No rerun capability: we cannot call these flaky, so keep them as
		// regressions (fail-safe direction: never silently absorb a real break).
		return arb, nil
	}

	for _, id := range unstable {
		absorbed, unresolved, err := arbitrateCase(ctx, id, rerun)
		if err != nil || unresolved {
			arb.Unresolved = append(arb.Unresolved, id)
			continue
		}
		if absorbed {
			arb.Absorbed = append(arb.Absorbed, id)
		}
	}
	sort.Strings(arb.Absorbed)
	sort.Strings(arb.Unresolved)
	return arb, nil
}

func arbitrateCase(ctx context.Context, id string, rerun ReRunFunc) (absorbed bool, unresolved bool, err error) {
	candidateUnstable := false
	for i := 0; i < MaxFlakyReruns; i++ {
		got, err := rerun(ctx, "candidate", []string{id})
		if err != nil {
			return false, true, err
		}
		v, ok := got[id]
		if !ok {
			// The rerun executed but reported nothing for this case (filter did
			// not match, case skipped, output unparsed). That is "we could not
			// re-measure", not "the sides contradict each other": keep the
			// regression (fail-safe) instead of downgrading it to UNVERIFIED.
			return false, false, nil
		}
		if v == CasePassed {
			candidateUnstable = true
			break
		}
	}
	// Check base before attributing instability to an existing baseline issue.
	got, err := rerun(ctx, "base", []string{id})
	if err != nil {
		return false, true, err
	}
	if v, ok := got[id]; ok && v == CaseFailed {
		return true, false, nil // both sides unstable → no signal
	}
	return false, candidateUnstable, nil // a candidate retry passing alone does not establish baseline flakiness
}

func caseMap(r probe.TestFacts) map[string]CaseVerdict {
	m := make(map[string]CaseVerdict, len(r.Passed)+len(r.Failed))
	for _, id := range r.Passed {
		m[id] = CasePassed
	}
	for _, id := range r.Failed {
		m[id] = CaseFailed
	}
	return m
}
