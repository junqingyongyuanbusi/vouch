package render_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/render"
	"github.com/junqingyongyuanbusi/vouch/internal/scheduler"
)

func sample(verdict bundle.GlobalVerdict, evs ...bundle.Evidence) scheduler.Result {
	res := scheduler.Result{
		Verdict:   verdict,
		Evidences: evs,
		Bundle: bundle.ProofBundle{
			BundleID:      "a3f9e2",
			Verdict:       verdict,
			Subject:       bundle.Subject{BaseRef: "HEAD"},
			CreatedAt:     time.Now().UTC(),
			SchemaVersion: 1,
			VouchVersion:  "0.1.0",
		},
	}
	res.Bundle.Evidence = evs // Markdown renders a stored bundle, not the live result
	res.Bundle.UnverifiedClaims = []string{"build: not measured"}
	res.Bundle.BaselineSummary = &bundle.BaselineSummary{Status: bundle.BaselineExistingFailures, Failing: 2}
	res.Warnings = []string{"removed_tests: 1 test disappeared"}
	return res
}

func evidence(verdict bundle.EvidenceVerdict, regressions []string, failures []bundle.FailureDetail) bundle.Evidence {
	return bundle.Evidence{
		ID: "ev_a1b2c3", Probe: "test", Verdict: verdict,
		Delta:     bundle.Delta{Regressions: regressions},
		Failures:  failures,
		Reproduce: "vouch rerun a3f9e2 --probe test",
	}
}

func TestAgentFeedback_BrokenNamesRegressionsAndReproduce(t *testing.T) {
	res := sample(bundle.Broken, evidence(bundle.EvidenceFail,
		[]string{"src/add.test.ts::injected-regression"},
		[]bundle.FailureDetail{{ID: "src/add.test.ts::injected-regression", Message: "expected 4 but got 5"}}))
	out := render.AgentFeedback(res)
	for _, want := range []string{
		"BROKEN (exit 1)",
		"src/add.test.ts::injected-regression",
		"expected 4 but got 5",
		"reproduce: vouch rerun a3f9e2 --probe test",
		"base: existing_failures (2 already failing before this change)",
		"not measured: build: not measured",
		"evidence: .vouch/bundles/a3f9e2",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("agent feedback must contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "test: pass") {
		t.Fatalf("passing probes must not be restated (token cost):\n%s", out)
	}
}

func TestAgentFeedback_UnverifiedSaysDoNotClaimSuccess(t *testing.T) {
	res := sample(bundle.Unverified, evidence(bundle.EvidenceInconclusive, nil, nil))
	out := render.AgentFeedback(res)
	if !strings.Contains(out, "UNVERIFIED (exit 2)") || !strings.Contains(out, "Do NOT claim") {
		t.Fatalf("UNVERIFIED must forbid claiming success:\n%s", out)
	}
}

func TestAgentFeedback_VerifiedIsShort(t *testing.T) {
	res := sample(bundle.Verified, evidence(bundle.EvidencePass, nil, nil))
	out := render.AgentFeedback(res)
	if !strings.Contains(out, "VERIFIED (exit 0)") {
		t.Fatalf("verified feedback must state the verdict:\n%s", out)
	}
	if len(strings.Split(strings.TrimSpace(out), "\n")) > 5 {
		t.Fatalf("verified feedback must stay short:\n%s", out)
	}
}

func TestHumanAndMarkdownUseFailingProbeForReproduce(t *testing.T) {
	passing := bundle.Evidence{Probe: "typecheck", Verdict: bundle.EvidencePass, Reproduce: "vouch rerun a3f9e2 --probe typecheck"}
	failing := evidence(bundle.EvidenceFail, []string{"t::x"}, nil)
	res := sample(bundle.Broken, passing, failing)
	if out := render.Human(res); !strings.Contains(out, "vouch rerun a3f9e2 --probe test") {
		t.Fatalf("human output must point at the failing probe:\n%s", out)
	}
	if md := render.Markdown(res.Bundle); !strings.Contains(md, "vouch rerun a3f9e2 --probe test") {
		t.Fatalf("markdown must point at the failing probe:\n%s", md)
	}
}

func TestGapsListsProvenanceAndClaims(t *testing.T) {
	res := sample(bundle.Unverified)
	res.Profile = bundle.ProjectProfile{
		Confidence: bundle.ConfidenceLow,
		Commands: map[string]bundle.Command{
			"test": {Cmd: "go test ./...", Source: "go.mod#default"},
		},
		Gaps: []string{"no typecheck command found"},
	}
	out := render.Gaps(res)
	for _, want := range []string{"profile confidence: low", "test: go test ./...  (go.mod#default)", "gap: no typecheck command found", "unverified: build: not measured"} {
		if !strings.Contains(out, want) {
			t.Fatalf("gaps output must contain %q:\n%s", want, out)
		}
	}
}

func TestUnpersistedSetupFailure_StaysAttributableWithoutFakePaths(t *testing.T) {
	// Setup never completed: Bundle is zero-valued by design. Every surface
	// must still carry UNVERIFIED plus the stage/reason, and none may print an
	// evidence or reproduce path for something that was never stored.
	res := scheduler.Result{
		Verdict:    bundle.Unverified,
		Profile:    bundle.ProjectProfile{Confidence: bundle.ConfidenceLow},
		Unverified: []string{`budget exhausted during setup (resolve base "HEAD"): context deadline exceeded (configured total budget 1ms)`},
	}
	human := render.Human(res)
	for _, want := range []string{"UNVERIFIED", "budget exhausted", "resolve base", "1ms"} {
		if !strings.Contains(human, want) {
			t.Fatalf("Human must keep the reason, got:\n%s", human)
		}
	}
	if strings.Contains(human, "证据包") || strings.Contains(human, "复现") {
		t.Fatalf("no evidence path may be printed for an unpersisted result:\n%s", human)
	}
	fb := render.AgentFeedback(res)
	for _, want := range []string{"UNVERIFIED", "not measured: budget exhausted"} {
		if !strings.Contains(fb, want) {
			t.Fatalf("AgentFeedback must keep the reason, got:\n%s", fb)
		}
	}
	if strings.Contains(fb, "evidence:") || strings.Contains(fb, "reproduce:") {
		t.Fatalf("no fake evidence/reproduce may be printed:\n%s", fb)
	}
	data, err := render.JSON(res)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["verdict"] != "UNVERIFIED" || m["persisted"] != false {
		t.Fatalf("machine view must carry verdict and persisted:false, got %v", m)
	}
	if _, ok := m["bundle_id"]; ok {
		t.Fatalf("no bundle id may be fabricated: %v", m)
	}
	if claims, _ := m["unverified_claims"].([]any); len(claims) != 1 {
		t.Fatalf("claims must survive into the machine view: %v", m)
	}
}

func TestRerun_PrintsCleanupWarnings(t *testing.T) {
	out := render.Rerun(scheduler.RerunResult{
		Probe: "test", Verdict: bundle.Verified,
		Warnings: []string{"worktree cleanup failed: exit status 1"},
	}, "a3f9e2")
	if !strings.Contains(out, "⚠ worktree cleanup failed: exit status 1") {
		t.Fatalf("rerun output must surface warnings, got:\n%s", out)
	}
}
