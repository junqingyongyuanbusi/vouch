package bundle_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
)

func TestValidate_RejectsEmptyClaimStatus(t *testing.T) {
	base := bundle.Evidence{
		ID:             "ev_a1b2c3",
		Claim:          "c",
		Kind:           bundle.KindDeterministic,
		Probe:          "test",
		Method:         "m",
		Baseline:       bundle.BaselineResult{Pass: 1, Status: bundle.BaselineAllGreen},
		Candidate:      bundle.CandidateResult{Pass: 1},
		Delta:          bundle.Delta{Regressions: []string{}, NewPassing: []string{}, RemovedTests: []string{}, SkipChanges: []string{}, FlakyAbsorbed: []string{}},
		Verdict:        bundle.EvidencePass,
		Reproduce:      "vouch rerun a3f9e2 --probe test",
		StartedAt:      time.Now().UTC(),
		EnvFingerprint: bundle.EnvFingerprint("a"),
	}
	cases := []struct {
		name string
		mut  func(*bundle.Evidence)
	}{
		{"empty claim", func(e *bundle.Evidence) { e.Claim = "" }},
		{"empty method", func(e *bundle.Evidence) { e.Method = "" }},
		{"zero started_at", func(e *bundle.Evidence) { e.StartedAt = time.Time{} }},
		{"bad baseline status", func(e *bundle.Evidence) { e.Baseline.Status = "green-ish" }},
		{"empty env fingerprint", func(e *bundle.Evidence) { e.EnvFingerprint = "" }},
		{"bad env fingerprint", func(e *bundle.Evidence) { e.EnvFingerprint = "not-sha" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := base
			tc.mut(&e)
			if err := e.Validate(); err == nil {
				t.Fatalf("expected Validate error for %s", tc.name)
			}
		})
	}
}

func TestMarshal_NoNull(t *testing.T) {
	// Nil slices/maps must never emit null — schema requires array/object.
	d := bundle.Delta{}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "null") {
		t.Fatalf("Delta nil slices emitted null: %s", data)
	}
	if strings.Contains(string(data), `"Alias"`) {
		t.Fatalf("Delta wrapper leaked Alias: %s", data)
	}
	var back bundle.Delta
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Delta round-trip: %v", err)
	}
	if back.Regressions == nil || back.NewPassing == nil {
		t.Fatalf("Delta round-trip lost fields: %+v", back)
	}
	// Empty profile with nil Commands/Language/Gaps must marshal as {} / [] not null.
	b := bundle.NewProofBundle(
		"a3f9e2",
		bundle.Subject{RepoRoot: ".", BaseRef: "abc123", DiffSHA256: bundle.DiffSHA256([]byte("x"))},
		bundle.ProjectProfile{Language: nil, Commands: nil, Confidence: bundle.ConfidenceLow},
		nil, nil, bundle.Unverified, nil,
	)
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, `"commands":null`) {
		t.Fatalf("Commands nil emitted null: %s", s)
	}
	if strings.Contains(s, `"evidence":null`) {
		t.Fatalf("Evidence nil emitted null: %s", s)
	}
	if strings.Contains(s, `"inferences":null`) {
		t.Fatalf("Inferences nil emitted null: %s", s)
	}
	if strings.Contains(s, `"regressions":null`) {
		t.Fatalf("Delta regressions null leaked via evidence: %s", s)
	}
}

func TestBundleVerdictConsistency(t *testing.T) {
	base := bundle.Subject{RepoRoot: ".", BaseRef: "abc123", DiffSHA256: bundle.DiffSHA256([]byte("x"))}
	profile := bundle.ProjectProfile{Language: []string{"go"}, Commands: map[string]bundle.Command{}, Confidence: bundle.ConfidenceHigh, Gaps: []string{}}
	evFail := bundle.Evidence{
		ID: "ev_a1b2c3", Claim: "c", Kind: bundle.KindDeterministic, Probe: "test", Method: "m",
		Baseline: bundle.BaselineResult{Pass: 1, Status: bundle.BaselineAllGreen}, Candidate: bundle.CandidateResult{Pass: 1},
		Delta:   bundle.Delta{Regressions: []string{}, NewPassing: []string{}, RemovedTests: []string{}, SkipChanges: []string{}, FlakyAbsorbed: []string{}},
		Verdict: bundle.EvidenceFail, Reproduce: "vouch rerun a3f9e2 --probe test", StartedAt: time.Now().UTC(), EnvFingerprint: bundle.EnvFingerprint("a"),
	}
	// fail evidence but bundle claims VERIFIED → must be rejected
	b := bundle.NewProofBundle("a3f9e2", base, profile, []bundle.Evidence{evFail}, nil, bundle.Verified, &bundle.BaselineSummary{Status: bundle.BaselineAllGreen})
	if err := b.Validate(); err == nil {
		t.Fatal("expected Validate to reject fail+VERIFIED")
	}
	// pass evidence but bundle claims BROKEN → must be rejected
	evPass := evFail
	evPass.ID = "ev_b2c3d4"
	evPass.Verdict = bundle.EvidencePass
	b2 := bundle.NewProofBundle("b3c4d5", base, profile, []bundle.Evidence{evPass}, nil, bundle.Broken, nil)
	if err := b2.Validate(); err == nil {
		t.Fatal("expected Validate to reject pass+BROKEN")
	}
}

func TestBundleUnknownVerdict(t *testing.T) {
	// Unknown probe verdict must be treated as inconclusive (keep in sync with verdict.Aggregate default).
	base := bundle.Subject{RepoRoot: ".", BaseRef: "abc123", DiffSHA256: bundle.DiffSHA256([]byte("x"))}
	profile := bundle.ProjectProfile{Language: []string{"go"}, Commands: map[string]bundle.Command{}, Confidence: bundle.ConfidenceHigh, Gaps: []string{}}
	ev := bundle.Evidence{
		ID: "ev_a1b2c3", Claim: "c", Kind: bundle.KindDeterministic, Probe: "test", Method: "m",
		Baseline: bundle.BaselineResult{Pass: 1, Status: bundle.BaselineAllGreen}, Candidate: bundle.CandidateResult{Pass: 1},
		Delta:   bundle.Delta{Regressions: []string{}, NewPassing: []string{}, RemovedTests: []string{}, SkipChanges: []string{}, FlakyAbsorbed: []string{}},
		Verdict: bundle.EvidenceVerdict("unknown"), Reproduce: "vouch rerun a3f9e2 --probe test", StartedAt: time.Now().UTC(), EnvFingerprint: bundle.EnvFingerprint("a"),
	}
	// Direct Evidence.Validate must reject unknown (hard fail at probe boundary is caught by Host normalization in production).
	if err := ev.Validate(); err == nil {
		t.Fatal("expected Evidence.Validate to reject unknown verdict")
	}
	// For bundle consistency, unknown counts as inconclusive → VERIFIED must be rejected, UNVERIFIED allowed.
	bBad := bundle.NewProofBundle("a3f9e2", base, profile, []bundle.Evidence{func() bundle.Evidence { e := ev; e.Verdict = bundle.EvidenceVerdict("unknown"); return e }()}, nil, bundle.Verified, &bundle.BaselineSummary{Status: bundle.BaselineAllGreen})
	if err := bBad.Validate(); err == nil {
		t.Fatal("expected Validate to reject unknown+VERIFIED (unknown evidence is invalid)")
	}
	// Host normalizes unknown to inconclusive — bundle with inconclusive evidence and UNVERIFIED should pass
	ev2 := bundle.Evidence{
		ID: "ev_b2c3d4", Claim: "c", Kind: bundle.KindDeterministic, Probe: "test", Method: "m",
		Baseline: bundle.BaselineResult{Pass: 1, Status: bundle.BaselineAllGreen}, Candidate: bundle.CandidateResult{Pass: 1},
		Delta:   bundle.Delta{Regressions: []string{}, NewPassing: []string{}, RemovedTests: []string{}, SkipChanges: []string{}, FlakyAbsorbed: []string{}},
		Verdict: bundle.EvidenceInconclusive, Reproduce: "vouch rerun a3f9e2 --probe test", StartedAt: time.Now().UTC(), EnvFingerprint: bundle.EnvFingerprint("a"),
	}
	bOk := bundle.NewProofBundle("c4d5e6", base, profile, []bundle.Evidence{ev2}, nil, bundle.Unverified, nil)
	if err := bOk.Validate(); err != nil {
		t.Fatalf("unexpected Validate for inconclusive+UNVERIFIED: %v", err)
	}
}

func TestBundleValidate_RejectsBadProfile(t *testing.T) {
	base := bundle.Subject{RepoRoot: ".", BaseRef: "abc123", DiffSHA256: bundle.DiffSHA256([]byte("x"))}
	goodProfile := bundle.ProjectProfile{Language: []string{"go"}, Commands: map[string]bundle.Command{}, Confidence: bundle.ConfidenceHigh, Gaps: []string{}}
	// bad confidence
	bad := bundle.NewProofBundle("a3f9e2", base, bundle.ProjectProfile{Language: []string{"go"}, Commands: map[string]bundle.Command{}, Confidence: ""}, nil, nil, bundle.Unverified, nil)
	if err := bad.Validate(); err == nil {
		t.Fatal("expected Validate to reject empty confidence")
	}
	// nil commands via literal is now normalized to {} in Marshal/NewProofBundle and treated as valid in Validate
	// (keeps literal and New* paths consistent, schema requires object not null but Go nil is normalized)
	b2 := bundle.ProofBundle{
		BundleID: "b3c4d5", CreatedAt: time.Now().UTC(), Subject: base,
		Profile:  bundle.ProjectProfile{Language: []string{"go"}, Commands: nil, Confidence: bundle.ConfidenceHigh, Gaps: []string{}},
		Evidence: []bundle.Evidence{}, Inferences: []bundle.Inference{}, Verdict: bundle.Unverified, SchemaVersion: bundle.SchemaVersion,
	}
	good := bundle.NewProofBundle("c4d5e6", base, goodProfile, nil, nil, bundle.Unverified, nil)
	if err := good.Validate(); err != nil {
		t.Fatalf("unexpected Validate for goodProfile: %v", err)
	}
	if err := b2.Validate(); err != nil {
		t.Fatalf("unexpected Validate for nil Commands (should be normalized): %v", err)
	}
}

func TestBundleValidate_RejectsBadSummary(t *testing.T) {
	base := bundle.Subject{RepoRoot: ".", BaseRef: "abc123", DiffSHA256: bundle.DiffSHA256([]byte("x"))}
	profile := bundle.ProjectProfile{Language: []string{"go"}, Commands: map[string]bundle.Command{}, Confidence: bundle.ConfidenceHigh, Gaps: []string{}}
	ev := bundle.Evidence{
		ID: "ev_a1b2c3", Claim: "c", Kind: bundle.KindDeterministic, Probe: "test", Method: "m",
		Baseline: bundle.BaselineResult{Pass: 1, Status: bundle.BaselineAllGreen}, Candidate: bundle.CandidateResult{Pass: 1},
		Delta:   bundle.Delta{Regressions: []string{}, NewPassing: []string{}, RemovedTests: []string{}, SkipChanges: []string{}, FlakyAbsorbed: []string{}},
		Verdict: bundle.EvidencePass, Reproduce: "vouch rerun a3f9e2 --probe test", StartedAt: time.Now().UTC(), EnvFingerprint: bundle.EnvFingerprint("a"),
	}
	b := bundle.NewProofBundle("a3f9e2", base, profile, []bundle.Evidence{ev}, nil, bundle.Verified, &bundle.BaselineSummary{Status: "bad"})
	if err := b.Validate(); err == nil {
		t.Fatal("expected Validate to reject bad BaselineSummary.Status")
	}
}

func TestBundleValidate_RejectsBadInference(t *testing.T) {
	base := bundle.Subject{RepoRoot: ".", BaseRef: "abc123", DiffSHA256: bundle.DiffSHA256([]byte("x"))}
	profile := bundle.ProjectProfile{Language: []string{"go"}, Commands: map[string]bundle.Command{}, Confidence: bundle.ConfidenceHigh, Gaps: []string{}}
	infBad := bundle.Inference{ID: "bad", Kind: bundle.KindInferred, Probe: "", Msg: ""}
	b := bundle.NewProofBundle("a3f9e2", base, profile, nil, []bundle.Inference{infBad}, bundle.Unverified, nil)
	if err := b.Validate(); err == nil {
		t.Fatal("expected Validate to reject bad Inference (id/probe/msg)")
	}
}

func TestValidate_RejectsNegativeCounts(t *testing.T) {
	base := bundle.Subject{RepoRoot: ".", BaseRef: "abc123", DiffSHA256: bundle.DiffSHA256([]byte("x"))}
	profile := bundle.ProjectProfile{Language: []string{"go"}, Commands: map[string]bundle.Command{}, Confidence: bundle.ConfidenceHigh, Gaps: []string{}}
	cases := []struct {
		name string
		mut  func(*bundle.Evidence)
	}{
		{"baseline pass -1", func(e *bundle.Evidence) { e.Baseline.Pass = -1 }},
		{"candidate fail -1", func(e *bundle.Evidence) { e.Candidate.Fail = -1 }},
		{"duration -1", func(e *bundle.Evidence) { e.Baseline.DurationMs = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := bundle.Evidence{
				ID: "ev_a1b2c3", Claim: "c", Kind: bundle.KindDeterministic, Probe: "test", Method: "m",
				Baseline: bundle.BaselineResult{Pass: 1, Status: bundle.BaselineAllGreen}, Candidate: bundle.CandidateResult{Pass: 1},
				Delta:   bundle.Delta{Regressions: []string{}, NewPassing: []string{}, RemovedTests: []string{}, SkipChanges: []string{}, FlakyAbsorbed: []string{}},
				Verdict: bundle.EvidencePass, Reproduce: "vouch rerun a3f9e2 --probe test", StartedAt: time.Now().UTC(), EnvFingerprint: bundle.EnvFingerprint("a"),
			}
			tc.mut(&ev)
			b := bundle.NewProofBundle("a3f9e2", base, profile, []bundle.Evidence{ev}, nil, bundle.Broken, nil)
			// Broken is expected for fail cases, but negative counts should be caught by Evidence.Validate before consistency
			if err := b.Validate(); err == nil {
				t.Fatalf("expected Validate to reject %s", tc.name)
			}
		})
	}
	// baseline_summary failing -1
	ev := bundle.Evidence{
		ID: "ev_a1b2c3", Claim: "c", Kind: bundle.KindDeterministic, Probe: "test", Method: "m",
		Baseline: bundle.BaselineResult{Pass: 1, Status: bundle.BaselineAllGreen}, Candidate: bundle.CandidateResult{Pass: 1},
		Delta:   bundle.Delta{Regressions: []string{}, NewPassing: []string{}, RemovedTests: []string{}, SkipChanges: []string{}, FlakyAbsorbed: []string{}},
		Verdict: bundle.EvidencePass, Reproduce: "vouch rerun a3f9e2 --probe test", StartedAt: time.Now().UTC(), EnvFingerprint: bundle.EnvFingerprint("a"),
	}
	b2 := bundle.NewProofBundle("b3c4d5", base, profile, []bundle.Evidence{ev}, nil, bundle.Verified, &bundle.BaselineSummary{Status: bundle.BaselineAllGreen, Failing: -1})
	if err := b2.Validate(); err == nil {
		t.Fatal("expected Validate to reject negative failing")
	}
}

func TestBundleValidate_BaselineSummaryCrossCheck(t *testing.T) {
	base := bundle.Subject{RepoRoot: ".", BaseRef: "abc123", DiffSHA256: bundle.DiffSHA256([]byte("x"))}
	profile := bundle.ProjectProfile{Language: []string{"go"}, Commands: map[string]bundle.Command{}, Confidence: bundle.ConfidenceHigh, Gaps: []string{}}
	ev := bundle.Evidence{
		ID: "ev_a1b2c3", Claim: "c", Kind: bundle.KindDeterministic, Probe: "test", Method: "m",
		Baseline: bundle.BaselineResult{Pass: 1, Status: bundle.BaselineExistingBroken}, Candidate: bundle.CandidateResult{Pass: 1},
		Delta:   bundle.Delta{Regressions: []string{}, NewPassing: []string{}, RemovedTests: []string{}, SkipChanges: []string{}, FlakyAbsorbed: []string{}},
		Verdict: bundle.EvidencePass, Reproduce: "vouch rerun a3f9e2 --probe test", StartedAt: time.Now().UTC(), EnvFingerprint: bundle.EnvFingerprint("a"),
	}
	// Summary claims all_green but evidence shows existing_broken -> must be rejected
	b := bundle.NewProofBundle("a3f9e2", base, profile, []bundle.Evidence{ev}, nil, bundle.Verified, &bundle.BaselineSummary{Status: bundle.BaselineAllGreen})
	if err := b.Validate(); err == nil {
		t.Fatal("expected Validate to reject all_green summary with non-green evidence baseline")
	}
	// Correct summary should pass
	b2 := bundle.NewProofBundle("b3c4d5", base, profile, []bundle.Evidence{ev}, nil, bundle.Verified, &bundle.BaselineSummary{Status: bundle.BaselineExistingBroken})
	if err := b2.Validate(); err != nil {
		t.Fatalf("unexpected Validate for matching summary: %v", err)
	}
}
