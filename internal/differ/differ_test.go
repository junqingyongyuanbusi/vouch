package differ_test

import (
	"testing"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/differ"
	"github.com/junqingyongyuanbusi/vouch/internal/probe"
)

func TestEvidence_TestDeltaAndValidate(t *testing.T) {
	env := bundle.EnvFingerprint("test", "test")
	mk := func(passed, failed []string, verdict probe.Verdict) probe.RunResult {
		data := map[string]interface{}{
			"passed": passed, "failed": failed, "skipped": []string{}, "duration_ms": 10,
		}
		return probe.RunResult{Verdict: verdict, Data: data}
	}
	// 1) base pass → candidate same pass: pass, no delta
	base := mk([]string{"a::t1"}, nil, probe.VerdictPass)
	cand := mk([]string{"a::t1"}, nil, probe.VerdictPass)
	ev, err := differ.Evidence("test", "vouch rerun x123456 --probe test", env, time.Now().UTC(), base, cand)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if ev.Verdict != bundle.EvidencePass || len(ev.Delta.Regressions) != 0 {
		t.Fatalf("ev=%+v", ev)
	}

	// 2) regression: t1 passes in base, fails in candidate
	candFail := mk(nil, []string{"a::t1"}, probe.VerdictFail)
	ev2, err := differ.Evidence("test", "vouch rerun x123456 --probe test", env, time.Now().UTC(), base, candFail)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if ev2.Verdict != bundle.EvidenceFail || len(ev2.Delta.Regressions) != 1 {
		t.Fatalf("ev2=%+v", ev2)
	}
	// 3) pre-existing failure kept: no regression → pass, base status records it
	baseFail := mk(nil, []string{"a::t1"}, probe.VerdictFail)
	ev3, err := differ.Evidence("test", "vouch rerun x123456 --probe test", env, time.Now().UTC(), baseFail, candFail)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if ev3.Verdict != bundle.EvidencePass || len(ev3.Delta.Regressions) != 0 {
		t.Fatalf("pre-existing failure must not be a regression: %+v", ev3)
	}
	if ev3.Baseline.Fail != 1 {
		t.Fatalf("baseline counts must record the existing failure: %+v", ev3.Baseline)
	}
	// 4) inconclusive side → inconclusive evidence
	inc := probe.RunResult{Verdict: probe.VerdictInconclusive, Reason: "boom"}
	ev4, err := differ.Evidence("test", "vouch rerun x123456 --probe test", env, time.Now().UTC(), base, inc)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if ev4.Verdict != bundle.EvidenceInconclusive {
		t.Fatalf("ev4=%+v", ev4)
	}
}

func TestEvidence_BuildAndTypecheck(t *testing.T) {
	env := bundle.EnvFingerprint("test", "test")
	// build regression
	ev, err := differ.Evidence("build", "vouch rerun x123456 --probe build", env, time.Now().UTC(),
		probe.RunResult{Verdict: probe.VerdictPass},
		probe.RunResult{Verdict: probe.VerdictFail, Data: map[string]interface{}{"exit_code": 2}})
	if err != nil || ev.Verdict != bundle.EvidenceFail || ev.Delta.Regressions[0] != "build" {
		t.Fatalf("build regression: %v %+v", err, ev)
	}
	// typecheck: new diagnostic only on candidate
	baseTC := probe.RunResult{Verdict: probe.VerdictPass, Data: map[string]interface{}{"diagnostics": []probe.Diagnostic{}}}
	candTC := probe.RunResult{Verdict: probe.VerdictFail, Data: map[string]interface{}{
		"diagnostics": []probe.Diagnostic{{File: "src/a.ts", Line: 3, Column: 7, Code: "TS2345", Message: "x"}},
	}}
	ev2, err := differ.Evidence("typecheck", "vouch rerun x123456 --probe typecheck", env, time.Now().UTC(), baseTC, candTC)
	if err != nil || ev2.Verdict != bundle.EvidenceFail || len(ev2.Delta.Regressions) != 1 {
		t.Fatalf("typecheck regression: %v %+v", err, ev2)
	}
	if err := ev2.Validate(); err != nil {
		t.Fatalf("evidence must be storable: %v", err)
	}
}

func TestEvidence_UnknownProbeShapeNeverDefaultsToPass(t *testing.T) {
	env := bundle.EnvFingerprint("env")
	// Third-party probe name (not built-in), green base → failing candidate:
	// must be a regression, not a silent pass.
	ev, err := differ.Evidence("thirdparty-lint", "vouch rerun x123456 --probe thirdparty-lint", env, time.Now().UTC(),
		probe.RunResult{Verdict: probe.VerdictPass, Data: map[string]interface{}{"summary": "ok"}},
		probe.RunResult{Verdict: probe.VerdictFail, Data: map[string]interface{}{"summary": "bad"}})
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if ev.Verdict != bundle.EvidenceFail || len(ev.Delta.Regressions) == 0 {
		t.Fatalf("unknown-shape regression must not default to pass: %+v", ev)
	}
	// Both sides failing with nothing to attribute it to: the probe never
	// produced comparable output (a missing package manager, an install that
	// never ran), so the two sides are equally blind. "No difference between
	// two blind runs" is not evidence of no regression -- found by the
	// regression-injection canary, where `unjs/defu` came back VERIFIED with an
	// injected failing test because neither side could start its runner.
	evBlind, err := differ.Evidence("thirdparty-lint", "vouch rerun x123456 --probe thirdparty-lint", env, time.Now().UTC(),
		probe.RunResult{Verdict: probe.VerdictFail, Data: map[string]interface{}{"summary": "command failed"}},
		probe.RunResult{Verdict: probe.VerdictFail, Data: map[string]interface{}{"summary": "command failed"}})
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if evBlind.Verdict == bundle.EvidencePass {
		t.Fatalf("both sides failed with no attribution; pass would clear an unmeasured change: %+v", evBlind)
	}
	if evBlind.Verdict != bundle.EvidenceInconclusive {
		t.Fatalf("want inconclusive for a double-blind run, got %s: %+v", evBlind.Verdict, evBlind)
	}
	// Both sides inconclusive → inconclusive.
	ev2, _ := differ.Evidence("thirdparty-lint", "vouch rerun x123456 --probe thirdparty-lint", env, time.Now().UTC(),
		probe.RunResult{Verdict: probe.VerdictInconclusive},
		probe.RunResult{Verdict: probe.VerdictInconclusive})
	if ev2.Verdict != bundle.EvidenceInconclusive {
		t.Fatalf("want inconclusive, got %s", ev2.Verdict)
	}
}

func TestEvidence_UnattributableFailureIsInconclusive(t *testing.T) {
	env := bundle.EnvFingerprint("env")
	// Candidate side reported fail but produced no case-level failure
	// (e.g. a vitest suite that failed to load) → inconclusive, never pass.
	base := probe.RunResult{Verdict: probe.VerdictPass, Data: map[string]interface{}{
		"passed": []string{"a.test.ts::t"}, "failed": []string{}, "skipped": []string{},
	}}
	cand := probe.RunResult{Verdict: probe.VerdictFail, Data: map[string]interface{}{
		"passed": []string{}, "failed": []string{}, "skipped": []string{}, "exit_code": 1,
	}}
	ev, err := differ.Evidence("test", "vouch rerun x123456 --probe test", env, time.Now().UTC(), base, cand)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if ev.Verdict != bundle.EvidenceInconclusive {
		t.Fatalf("unattributable failure must be inconclusive, got %s (%+v)", ev.Verdict, ev.Delta)
	}
}

func TestEvidence_OneSidedInconclusiveHasNoRemovedTests(t *testing.T) {
	env := bundle.EnvFingerprint("env")
	base := result([]string{"t::a", "t::b"}, nil, probe.VerdictPass)
	// candidate crashed: inconclusive with no data shape
	cand := probe.RunResult{Verdict: probe.VerdictInconclusive, Reason: "probe crashed"}
	ev, err := differ.Evidence("test", "vouch rerun x123456 --probe test", env, time.Now().UTC(), base, cand)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if ev.Verdict != bundle.EvidenceInconclusive {
		t.Fatalf("verdict=%s", ev.Verdict)
	}
	if len(ev.Delta.RemovedTests) != 0 {
		t.Fatalf("a crashed probe must not look like deleted tests: %+v", ev.Delta)
	}
}
