package bundle_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
)

func mustString(s string) *string { return &s }

func validEvidence(id, probe string, verdict bundle.EvidenceVerdict) bundle.Evidence {
	return bundle.Evidence{
		ID:             id,
		Claim:          "change does not regress tests",
		Kind:           bundle.KindDeterministic,
		Probe:          probe,
		Method:         "pnpm vitest run (base/candidate, 2x arbitration)",
		Baseline:       bundle.BaselineResult{Pass: 140, Fail: 2, Skip: 5, DurationMs: 12400, Status: bundle.BaselineExistingFailures},
		Candidate:      bundle.CandidateResult{Pass: 143, Fail: 2, Skip: 5, DurationMs: 12900},
		Delta:          bundle.Delta{Regressions: []string{}, NewPassing: []string{"auth.spec.ts::refresh-token"}, RemovedTests: []string{}, SkipChanges: []string{}, FlakyAbsorbed: []string{"net.spec.ts::timeout"}},
		Verdict:        verdict,
		Reproduce:      "vouch rerun a3f9e2 --probe " + probe,
		StartedAt:      time.Date(2026, 9, 15, 8, 30, 12, 0, time.UTC),
		EnvFingerprint: "sha256:e1a22222e1a22222e1a22222e1a22222e1a22222e1a22222e1a22222e1a22222",
	}
}

func TestEvidenceValidate(t *testing.T) {
	tests := []struct {
		name    string
		ev      bundle.Evidence
		wantErr bool
	}{
		{name: "valid pass", ev: validEvidence("ev_8f2a91", "test", bundle.EvidencePass)},
		{name: "valid fail", ev: validEvidence("ev_8f2a92", "build", bundle.EvidenceFail)},
		{name: "valid inconclusive", ev: validEvidence("ev_8f2a93", "typecheck", bundle.EvidenceInconclusive)},
		{name: "missing reproduce", ev: func() bundle.Evidence {
			e := validEvidence("ev_8f2a94", "test", bundle.EvidencePass)
			e.Reproduce = ""
			return e
		}(), wantErr: true},
		{name: "reproduce wrong prefix", ev: func() bundle.Evidence {
			e := validEvidence("ev_8f2a95", "test", bundle.EvidencePass)
			e.Reproduce = "rerun a3f9e2"
			return e
		}(), wantErr: true},
		{name: "wrong kind", ev: func() bundle.Evidence {
			e := validEvidence("ev_8f2a96", "test", bundle.EvidencePass)
			e.Kind = bundle.KindInferred
			return e
		}(), wantErr: true},
		{name: "empty id", ev: func() bundle.Evidence {
			e := validEvidence("ev_8f2a97", "test", bundle.EvidencePass)
			e.ID = ""
			return e
		}(), wantErr: true},
		{name: "invalid verdict", ev: func() bundle.Evidence {
			e := validEvidence("ev_8f2a98", "test", bundle.EvidencePass)
			e.Verdict = "unknown"
			return e
		}(), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.ev.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestProofBundleValidate(t *testing.T) {
	validBundle := func(verdict bundle.GlobalVerdict, evs []bundle.Evidence) bundle.ProofBundle {
		b := bundle.ProofBundle{
			BundleID:      "a3f9e2",
			CreatedAt:     time.Now().UTC(),
			Subject:       bundle.Subject{RepoRoot: ".", BaseRef: "abc123", DiffSHA256: bundle.DiffSHA256([]byte("diff"))},
			Profile:       bundle.ProjectProfile{Language: []string{"typescript"}, PackageManager: mustString("pnpm"), Commands: map[string]bundle.Command{"test": {Cmd: "pnpm vitest run", Source: "package.json#test"}}, Confidence: bundle.ConfidenceHigh},
			Evidence:      evs,
			Inferences:    []bundle.Inference{{ID: "inf_01", Kind: bundle.KindInferred, Probe: "scope", Msg: "2 suspicious files"}},
			Verdict:       verdict,
			SchemaVersion: bundle.SchemaVersion,
		}
		if verdict == bundle.Verified {
			b.BaselineSummary = &bundle.BaselineSummary{Status: bundle.BaselineExistingFailures, Failing: 2}
		}
		return b
	}

	t.Run("empty evidence VERIFIED forbidden", func(t *testing.T) {
		b := validBundle(bundle.Verified, nil)
		if err := b.Validate(); err == nil {
			t.Fatal("expected error for VERIFIED with empty evidence")
		}
	})
	t.Run("empty evidence UNVERIFIED allowed", func(t *testing.T) {
		b := validBundle(bundle.Unverified, nil)
		if err := b.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("VERIFIED without baseline_summary forbidden", func(t *testing.T) {
		b := validBundle(bundle.Verified, []bundle.Evidence{validEvidence("ev_8f2a91", "test", bundle.EvidencePass)})
		b.BaselineSummary = nil
		if err := b.Validate(); err == nil {
			t.Fatal("expected error for VERIFIED without baseline_summary")
		}
	})
	t.Run("evidence inferred kind in bundle forbidden", func(t *testing.T) {
		ev := validEvidence("ev_8f2a91", "test", bundle.EvidencePass)
		ev.Kind = bundle.KindInferred
		b := validBundle(bundle.Broken, []bundle.Evidence{ev})
		if err := b.Validate(); err == nil {
			t.Fatal("expected error for inferred evidence in bundle")
		}
	})
	t.Run("valid round-trip", func(t *testing.T) {
		b := validBundle(bundle.Verified, []bundle.Evidence{validEvidence("ev_8f2a91", "test", bundle.EvidencePass)})
		if err := b.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out bundle.ProofBundle
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out.BundleID != b.BundleID || out.Verdict != b.Verdict {
			t.Fatalf("round-trip mismatch: got %+v want %+v", out, b)
		}
		// Inferences must stay separated
		if len(out.Inferences) != 1 || out.Inferences[0].Kind != bundle.KindInferred {
			t.Fatalf("inferences not preserved")
		}
		// Ensure deterministic/inferred isolation: evidence count untouched
		if len(out.Evidence) != 1 || out.Evidence[0].Kind != bundle.KindDeterministic {
			t.Fatalf("evidence isolation broken")
		}
	})
	t.Run("delta removed_tests preserved", func(t *testing.T) {
		ev := validEvidence("ev_8f2a91", "test", bundle.EvidencePass)
		ev.Delta.RemovedTests = []string{"old.spec.ts::flaky"}
		ev.Delta.SkipChanges = []string{"slow.spec.ts::heavy"}
		b := validBundle(bundle.Verified, []bundle.Evidence{ev})
		data, _ := json.Marshal(b)
		var out bundle.ProofBundle
		_ = json.Unmarshal(data, &out)
		if len(out.Evidence[0].Delta.RemovedTests) != 1 || out.Evidence[0].Delta.RemovedTests[0] != "old.spec.ts::flaky" {
			t.Fatalf("removed_tests not preserved: %+v", out.Evidence[0].Delta)
		}
	})
	t.Run("schema_version must be 1", func(t *testing.T) {
		b := validBundle(bundle.Unverified, nil)
		b.SchemaVersion = 99
		if err := b.Validate(); err == nil {
			t.Fatal("expected schema_version error")
		}
	})
}

func TestDiffSHA256AndFingerprint(t *testing.T) {
	h1 := bundle.DiffSHA256([]byte("hello"))
	h2 := bundle.DiffSHA256([]byte("hello"))
	if h1 != h2 {
		t.Fatal("deterministic hash failed")
	}
	if len(h1) != len("sha256:")+64 {
		t.Fatalf("unexpected hash length: %q", h1)
	}
	f1 := bundle.EnvFingerprint("a", "b")
	f2 := bundle.EnvFingerprint("a", "b")
	f3 := bundle.EnvFingerprint("a", "c")
	if f1 != f2 || f1 == f3 {
		t.Fatal("fingerprint not deterministic or not sensitive")
	}
}
