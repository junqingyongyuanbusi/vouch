package differ_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/differ"
	"github.com/junqingyongyuanbusi/vouch/internal/probe"
)

func result(passed, failed []string, verdict probe.Verdict) probe.RunResult {
	return probe.RunResult{
		Verdict: verdict,
		Data: map[string]interface{}{
			"passed": passed, "failed": failed, "skipped": []string{}, "duration_ms": 10,
		},
	}
}

func TestArbitrate_FlakyCandidateIsAbsorbed(t *testing.T) {
	base := probe.TestFacts{Passed: []string{"t::a"}, Failed: []string{}, Skipped: []string{}}
	cand := probe.TestFacts{Passed: []string{}, Failed: []string{"t::a"}, Skipped: []string{}}
	calls := 0
	rerun := func(_ context.Context, role string, ids []string) (map[string]differ.CaseVerdict, error) {
		calls++
		if role == "base" {
			return map[string]differ.CaseVerdict{ids[0]: differ.CaseFailed}, nil
		}
		// Both sides show instability; candidate alone must not be absorbed.
		return map[string]differ.CaseVerdict{ids[0]: differ.CasePassed}, nil
	}
	arb, err := differ.Arbitrate(context.Background(), base, cand, rerun)
	if err != nil {
		t.Fatalf("arbitrate: %v", err)
	}
	if len(arb.Absorbed) != 1 || arb.Absorbed[0] != "t::a" {
		t.Fatalf("want absorbed, got %+v", arb)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}

	// Evidence keeps it out of regressions and records it in flaky_absorbed.
	ev, err := differ.EvidenceWithArbitration("test", "vouch rerun x123456 --probe test",
		bundle.EnvFingerprint("env"), time.Now().UTC(),
		result([]string{"t::a"}, nil, probe.VerdictPass),
		result(nil, []string{"t::a"}, probe.VerdictFail), arb)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if len(ev.Delta.Regressions) != 0 {
		t.Fatalf("flaky case must not be a regression: %+v", ev.Delta)
	}
	if len(ev.Delta.FlakyAbsorbed) != 1 || ev.Delta.FlakyAbsorbed[0] != "t::a" {
		t.Fatalf("flaky_absorbed=%v", ev.Delta.FlakyAbsorbed)
	}
	if ev.Verdict != bundle.EvidencePass {
		t.Fatalf("verdict=%s (flaky absorbed ⇒ no regression)", ev.Verdict)
	}
}

func TestArbitrate_RealRegressionSurvives(t *testing.T) {
	base := probe.TestFacts{Passed: []string{"t::a"}, Failed: []string{}, Skipped: []string{}}
	cand := probe.TestFacts{Passed: []string{}, Failed: []string{"t::a"}, Skipped: []string{}}
	calls := 0
	rerun := func(_ context.Context, role string, ids []string) (map[string]differ.CaseVerdict, error) {
		calls++
		switch role {
		case "candidate":
			return map[string]differ.CaseVerdict{ids[0]: differ.CaseFailed}, nil
		case "base":
			return map[string]differ.CaseVerdict{ids[0]: differ.CasePassed}, nil
		}
		return nil, nil
	}
	arb, err := differ.Arbitrate(context.Background(), base, cand, rerun)
	if err != nil {
		t.Fatalf("arbitrate: %v", err)
	}
	if len(arb.Absorbed) != 0 || len(arb.Unresolved) != 0 {
		t.Fatalf("consistent failure must stay a regression: %+v", arb)
	}
	if calls != differ.MaxFlakyReruns+1 {
		t.Fatalf("expected %d reruns, got %d", differ.MaxFlakyReruns+1, calls)
	}
	ev, err := differ.EvidenceWithArbitration("test", "vouch rerun x123456 --probe test",
		bundle.EnvFingerprint("env"), time.Now().UTC(),
		result([]string{"t::a"}, nil, probe.VerdictPass),
		result(nil, []string{"t::a"}, probe.VerdictFail), arb)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if ev.Verdict != bundle.EvidenceFail || len(ev.Delta.Regressions) != 1 {
		t.Fatalf("regression must survive: %+v", ev)
	}
}

func TestArbitrate_BothSidesUnstableIsAbsorbed(t *testing.T) {
	base := probe.TestFacts{Passed: []string{"t::a"}}
	cand := probe.TestFacts{Failed: []string{"t::a"}}
	rerun := func(_ context.Context, role string, ids []string) (map[string]differ.CaseVerdict, error) {
		if role == "candidate" {
			return map[string]differ.CaseVerdict{ids[0]: differ.CaseFailed}, nil
		}
		return map[string]differ.CaseVerdict{ids[0]: differ.CaseFailed}, nil // base is flaky too
	}
	arb, _ := differ.Arbitrate(context.Background(), base, cand, rerun)
	if len(arb.Absorbed) != 1 {
		t.Fatalf("both-sides-unstable must be absorbed: %+v", arb)
	}
}

func TestArbitrate_UnresolvedIsInconclusive(t *testing.T) {
	base := probe.TestFacts{Passed: []string{"t::a"}}
	cand := probe.TestFacts{Failed: []string{"t::a"}}
	rerun := func(_ context.Context, role string, ids []string) (map[string]differ.CaseVerdict, error) {
		return nil, errors.New("probe crashed")
	}
	arb, err := differ.Arbitrate(context.Background(), base, cand, rerun)
	if err != nil {
		t.Fatalf("arbitrate must not error out: %v", err)
	}
	if len(arb.Unresolved) != 1 {
		t.Fatalf("rerun failure must be unresolved: %+v", arb)
	}
	ev, err := differ.EvidenceWithArbitration("test", "vouch rerun x123456 --probe test",
		bundle.EnvFingerprint("env"), time.Now().UTC(),
		result([]string{"t::a"}, nil, probe.VerdictPass),
		result(nil, []string{"t::a"}, probe.VerdictFail), arb)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if ev.Verdict != bundle.EvidenceInconclusive {
		t.Fatalf("unresolved must force inconclusive, got %s", ev.Verdict)
	}
}

func TestArbitrate_NoRerunCapabilityKeepsRegression(t *testing.T) {
	base := probe.TestFacts{Passed: []string{"t::a"}}
	cand := probe.TestFacts{Failed: []string{"t::a"}}
	arb, err := differ.Arbitrate(context.Background(), base, cand, nil)
	if err != nil {
		t.Fatalf("arbitrate: %v", err)
	}
	if len(arb.Absorbed) != 0 {
		t.Fatalf("without rerun we must not absorb: %+v", arb)
	}
	ev, _ := differ.EvidenceWithArbitration("test", "vouch rerun x123456 --probe test",
		bundle.EnvFingerprint("env"), time.Now().UTC(),
		result([]string{"t::a"}, nil, probe.VerdictPass),
		result(nil, []string{"t::a"}, probe.VerdictFail), arb)
	if ev.Verdict != bundle.EvidenceFail {
		t.Fatalf("fail-safe direction: %s", ev.Verdict)
	}
}

func TestArbitrate_StableCasesAreNotRerun(t *testing.T) {
	base := probe.TestFacts{Passed: []string{"t::a", "t::b"}}
	cand := probe.TestFacts{Passed: []string{"t::a", "t::b"}}
	called := false
	rerun := func(context.Context, string, []string) (map[string]differ.CaseVerdict, error) {
		called = true
		return nil, nil
	}
	arb, _ := differ.Arbitrate(context.Background(), base, cand, rerun)
	if called {
		t.Fatal("stable runs must not trigger reruns")
	}
	if len(arb.Absorbed) != 0 || len(arb.Unresolved) != 0 {
		t.Fatalf("arb=%+v", arb)
	}
}

func TestArbitrate_RerunWithoutVerdictKeepsRegression(t *testing.T) {
	// The rerun executed but reported nothing for the case (filter mismatch,
	// skipped case, unparsed output). That must NOT downgrade a real regression
	// to UNVERIFIED: failures stay failures unless we actually re-measured them.
	base := probe.TestFacts{Passed: []string{"t::a"}}
	cand := probe.TestFacts{Failed: []string{"t::a"}}
	rerun := func(_ context.Context, role string, ids []string) (map[string]differ.CaseVerdict, error) {
		return map[string]differ.CaseVerdict{}, nil // executed, no verdict
	}
	arb, err := differ.Arbitrate(context.Background(), base, cand, rerun)
	if err != nil {
		t.Fatalf("arbitrate: %v", err)
	}
	if len(arb.Unresolved) != 0 || len(arb.Absorbed) != 0 {
		t.Fatalf("missing verdict must be neither unresolved nor absorbed: %+v", arb)
	}
	ev, err := differ.EvidenceWithArbitration("test", "vouch rerun x123456 --probe test",
		bundle.EnvFingerprint("env"), time.Now().UTC(),
		result([]string{"t::a"}, nil, probe.VerdictPass),
		result(nil, []string{"t::a"}, probe.VerdictFail), arb)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if ev.Verdict != bundle.EvidenceFail {
		t.Fatalf("regression must survive an unmeasurable rerun: %s", ev.Verdict)
	}
}

func TestArbitrate_AbsorbedFailureIsNotReportedAsFailure(t *testing.T) {
	env := bundle.EnvFingerprint("env")
	base := probe.RunResult{Verdict: probe.VerdictPass, Data: map[string]interface{}{
		"passed": []string{"t::a"}, "failed": []string{}, "skipped": []string{},
	}}
	cand := probe.RunResult{Verdict: probe.VerdictFail, Data: map[string]interface{}{
		"passed": []string{}, "failed": []string{"t::a"}, "skipped": []string{},
		"failures": []map[string]string{{"id": "t::a", "message": "boom"}},
	}}
	rerun := func(_ context.Context, role string, _ []string) (map[string]differ.CaseVerdict, error) {
		if role == "base" {
			return map[string]differ.CaseVerdict{"t::a": differ.CaseFailed}, nil
		}
		return map[string]differ.CaseVerdict{"t::a": differ.CasePassed}, nil
	}
	arb, err := differ.Arbitrate(context.Background(),
		probe.TestFactsOf(base), probe.TestFactsOf(cand), rerun)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := differ.EvidenceWithArbitration("test", "vouch rerun x123456 --probe test", env, time.Now().UTC(), base, cand, arb)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Delta.FlakyAbsorbed) == 0 {
		t.Fatalf("case should be absorbed: %+v", ev.Delta)
	}
	if len(ev.Failures) != 0 {
		t.Fatalf("absorbed case must not stay in failures: %+v", ev.Failures)
	}
}
