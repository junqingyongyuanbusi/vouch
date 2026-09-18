package bundle_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
)

func TestStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := bundle.NewStore(dir)
	base := bundle.Subject{RepoRoot: ".", BaseRef: "abc123", DiffSHA256: bundle.DiffSHA256([]byte("diff1"))}
	profile := bundle.ProjectProfile{Language: []string{"go"}, Commands: map[string]bundle.Command{"test": {Cmd: "go test ./...", Source: "go.mod#default"}}, Confidence: bundle.ConfidenceHigh, Gaps: []string{}}
	ev := bundle.Evidence{
		ID: "ev_a1b2c3", Claim: "c", Kind: bundle.KindDeterministic, Probe: "test", Method: "go test",
		Baseline: bundle.BaselineResult{Pass: 2, Status: bundle.BaselineAllGreen}, Candidate: bundle.CandidateResult{Pass: 2},
		Delta:   bundle.Delta{Regressions: []string{}, NewPassing: []string{}, RemovedTests: []string{}, SkipChanges: []string{}, FlakyAbsorbed: []string{}},
		Verdict: bundle.EvidencePass, Reproduce: "vouch rerun a3f9e2 --probe test", StartedAt: time.Now().UTC(), EnvFingerprint: bundle.EnvFingerprint("a"),
	}
	b := bundle.NewProofBundle("a3f9e2", base, profile, []bundle.Evidence{ev}, nil, bundle.Verified, &bundle.BaselineSummary{Status: bundle.BaselineAllGreen})
	if err := b.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	// Save with blobs
	logs := map[string][]byte{"blobs/sha256-abc/test.log": []byte("hello")}
	id, err := store.Save(b, logs)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if id != "a3f9e2" {
		t.Fatalf("want a3f9e2 got %q", id)
	}
	// bundle.json is single source of truth
	if _, err := os.Stat(filepath.Join(dir, ".vouch", "bundles", "a3f9e2", "bundle.json")); err != nil {
		t.Fatalf("bundle.json missing: %v", err)
	}
	// evidence projection exists but Load ignores it
	if _, err := os.Stat(filepath.Join(dir, ".vouch", "bundles", "a3f9e2", "evidence", "ev_a1b2c3.json")); err != nil {
		t.Fatalf("evidence projection missing: %v", err)
	}
	// blobs must be at blobs/sha256-*/test.log, not double prefix blobs/blobs
	if _, err := os.Stat(filepath.Join(dir, ".vouch", "bundles", "a3f9e2", "blobs", "sha256-abc", "test.log")); err != nil {
		t.Fatalf("blob not at expected path (TrimPrefix failed): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".vouch", "bundles", "a3f9e2", "blobs", "blobs")); err == nil {
		t.Fatalf("double prefix blobs/blobs should not exist")
	}
	loaded, err := store.Load("a3f9e2")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.BundleID != b.BundleID || loaded.Verdict != b.Verdict {
		t.Fatalf("round-trip mismatch")
	}
	if len(loaded.Evidence) != 1 || loaded.Evidence[0].ID != ev.ID {
		t.Fatalf("evidence not preserved")
	}
	// List
	metas, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(metas) != 1 || metas[0].BundleID != "a3f9e2" {
		t.Fatalf("list mismatch: %+v", metas)
	}
}

func TestBundleIDFor_Stable(t *testing.T) {
	id1 := bundle.BundleIDFor([]byte("diff"), "abc123")
	id2 := bundle.BundleIDFor([]byte("diff"), "abc123")
	if id1 != id2 {
		t.Fatal("not stable")
	}
	if len(id1) != 8 {
		t.Fatalf("want 8 hex, got %q", id1)
	}
	id3 := bundle.BundleIDFor([]byte("diff2"), "abc123")
	if id1 == id3 {
		t.Fatal("different diff should give different id")
	}
	id4 := bundle.BundleIDFor([]byte("diff"), "def456")
	if id1 == id4 {
		t.Fatal("different base should give different id")
	}
	// Must match reBundleID ^[a-f0-9]{6,12}$
	for _, id := range []string{id1, id3, id4} {
		if len(id) < 6 || len(id) > 12 {
			t.Fatalf("id length out of range: %q", id)
		}
		for _, c := range id {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Fatalf("id not hex: %q", id)
			}
		}
	}
}

func TestStore_BundleIDConsistency(t *testing.T) {
	// Save id must equal BundleIDFor(diff, baseRef) — single source, rerun/show/cache share it.
	diff := []byte("diff-bundle-id-test")
	baseRef := "abc123"
	want := bundle.BundleIDFor(diff, baseRef)
	if len(want) != 8 {
		t.Fatalf("want 8 hex, got %q", want)
	}
	dir := t.TempDir()
	store := bundle.NewStore(dir)
	base := bundle.Subject{RepoRoot: ".", BaseRef: baseRef, DiffSHA256: bundle.DiffSHA256(diff)}
	profile := bundle.ProjectProfile{Language: []string{"go"}, Commands: map[string]bundle.Command{"test": {Cmd: "go test ./...", Source: "go.mod#default"}}, Confidence: bundle.ConfidenceHigh, Gaps: []string{}}
	ev := bundle.Evidence{
		ID: "ev_a1b2c3", Claim: "c", Kind: bundle.KindDeterministic, Probe: "test", Method: "go test",
		Baseline: bundle.BaselineResult{Pass: 1, Status: bundle.BaselineAllGreen}, Candidate: bundle.CandidateResult{Pass: 1},
		Delta:   bundle.Delta{Regressions: []string{}, NewPassing: []string{}, RemovedTests: []string{}, SkipChanges: []string{}, FlakyAbsorbed: []string{}},
		Verdict: bundle.EvidencePass, Reproduce: "vouch rerun " + want + " --probe test", StartedAt: time.Now().UTC(), EnvFingerprint: bundle.EnvFingerprint("a"),
	}
	b := bundle.NewProofBundle(want, base, profile, []bundle.Evidence{ev}, nil, bundle.Verified, &bundle.BaselineSummary{Status: bundle.BaselineAllGreen})
	if err := b.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	got, err := store.Save(b, nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if got != want {
		t.Fatalf("Save id %q != BundleIDFor %q", got, want)
	}
}

func TestStore_ConcurrentSavesShareBundleID(t *testing.T) {
	// Two verifies of the same diff compute the same bundle id; they must not
	// clobber each other's temp file or produce a corrupt bundle.json.
	dir := t.TempDir()
	store := bundle.NewStore(dir)
	base := bundle.Subject{RepoRoot: ".", BaseRef: "abc123", DiffSHA256: bundle.DiffSHA256([]byte("same"))}
	profile := bundle.ProjectProfile{Language: []string{"go"}, Commands: map[string]bundle.Command{"test": {Cmd: "go test ./...", Source: "go.mod#default"}}, Confidence: bundle.ConfidenceHigh, Gaps: []string{}}
	mk := func() bundle.ProofBundle {
		ev := bundle.Evidence{
			ID: "ev_a1b2c3", Claim: "c", Kind: bundle.KindDeterministic, Probe: "test", Method: "go test",
			Baseline: bundle.BaselineResult{Pass: 1, Status: bundle.BaselineAllGreen}, Candidate: bundle.CandidateResult{Pass: 1},
			Delta:   bundle.Delta{Regressions: []string{}, NewPassing: []string{}, RemovedTests: []string{}, SkipChanges: []string{}, FlakyAbsorbed: []string{}},
			Verdict: bundle.EvidencePass, Reproduce: "vouch rerun a3f9e2 --probe test", StartedAt: time.Now().UTC(), EnvFingerprint: bundle.EnvFingerprint("a"),
		}
		return bundle.NewProofBundle("a3f9e2", base, profile, []bundle.Evidence{ev}, nil, bundle.Verified, &bundle.BaselineSummary{Status: bundle.BaselineAllGreen})
	}
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { _, err := store.Save(mk(), nil); done <- err }()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent save: %v", err)
		}
	}
	loaded, err := store.Load("a3f9e2")
	if err != nil {
		t.Fatalf("bundle must remain loadable after concurrent saves: %v", err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("persisted bundle invalid: %v", err)
	}
}
