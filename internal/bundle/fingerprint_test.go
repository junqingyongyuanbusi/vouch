package bundle_test

import (
	"testing"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
)

func TestProfileFingerprint_Stability(t *testing.T) {
	profile := bundle.ProjectProfile{
		Language: []string{"go"},
		Commands: map[string]bundle.Command{
			"test":  {Cmd: "go test ./...", Source: "go.mod#default"},
			"build": {Cmd: "go build ./...", Source: "Makefile#build"},
		},
		Confidence: bundle.ConfidenceHigh,
	}
	f1 := bundle.ProfileFingerprint("go1.24.3", "", "", "lock123", profile)
	f2 := bundle.ProfileFingerprint("go1.24.3", "", "", "lock123", profile)
	if f1 != f2 {
		t.Fatal("same input must give same fingerprint")
	}
	// Reordered Commands (map iteration order) must still be equal — sorted internally.
	profile2 := bundle.ProjectProfile{
		Language: []string{"go"},
		Commands: map[string]bundle.Command{
			"build": {Cmd: "go build ./...", Source: "Makefile#build"},
			"test":  {Cmd: "go test ./...", Source: "go.mod#default"},
		},
		Confidence: bundle.ConfidenceHigh,
	}
	f3 := bundle.ProfileFingerprint("go1.24.3", "", "", "lock123", profile2)
	if f1 != f3 {
		t.Fatalf("reordered commands must be equal: %q vs %q", f1, f3)
	}
	// Different lockHash must differ.
	f4 := bundle.ProfileFingerprint("go1.24.3", "", "", "lock456", profile)
	if f1 == f4 {
		t.Fatal("different lockHash should give different fingerprint")
	}
	// Nil vs non-nil TestSelection must differ but be stable.
	pNil := profile
	pNil.TestSelection = nil
	pSel := profile
	pSel.TestSelection = &bundle.TestSelection{Supported: true, Mechanism: "vitest related"}
	fn1 := bundle.ProfileFingerprint("go1.24.3", "", "", "lock123", pNil)
	fn2 := bundle.ProfileFingerprint("go1.24.3", "", "", "lock123", pNil)
	if fn1 != fn2 {
		t.Fatal("nil TestSelection must be stable")
	}
	fs := bundle.ProfileFingerprint("go1.24.3", "", "", "lock123", pSel)
	if fn1 == fs {
		t.Fatal("nil vs non-nil TestSelection should differ")
	}
	// Separator is \x00 — verified via EnvFingerprint primitive (no bare concatenation).
}

func TestProfileFingerprint_LanguageDiffers(t *testing.T) {
	base := bundle.ProjectProfile{
		Language:   []string{"go"},
		Commands:   map[string]bundle.Command{"test": {Cmd: "make test", Source: "Makefile#test"}},
		Confidence: bundle.ConfidenceHigh,
	}
	other := base
	other.Language = []string{"typescript"}
	f1 := bundle.ProfileFingerprint("go1.24.3", "", "", "lock123", base)
	f2 := bundle.ProfileFingerprint("go1.24.3", "", "", "lock123", other)
	if f1 == f2 {
		t.Fatal("different Language should give different fingerprint")
	}
	pm := "pnpm"
	withPM := base
	withPM.PackageManager = &pm
	f3 := bundle.ProfileFingerprint("go1.24.3", "", "", "lock123", withPM)
	if f1 == f3 {
		t.Fatal("different PackageManager should differ")
	}
}
