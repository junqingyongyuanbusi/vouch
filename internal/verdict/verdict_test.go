package verdict_test

import (
	"testing"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/verdict"
)

func ev(probe string, v bundle.EvidenceVerdict) bundle.Evidence {
	idMap := map[string]string{"test": "ev_a1b2c3", "build": "ev_b2c3d4", "typecheck": "ev_c3d4e5"}
	id := idMap[probe]
	if id == "" {
		id = "ev_ffffff"
	}
	return bundle.Evidence{
		ID:             id,
		Claim:          "probe " + probe + " does not regress",
		Kind:           bundle.KindDeterministic,
		Probe:          probe,
		Method:         "test",
		Baseline:       bundle.BaselineResult{Pass: 10, Fail: 0, Status: bundle.BaselineAllGreen},
		Candidate:      bundle.CandidateResult{Pass: 10, Fail: 0},
		Delta:          bundle.Delta{Regressions: []string{}, NewPassing: []string{}, RemovedTests: []string{}, SkipChanges: []string{}, FlakyAbsorbed: []string{}},
		Verdict:        v,
		Reproduce:      "vouch rerun a3f9e2 --probe " + probe,
		StartedAt:      time.Now().UTC(),
		EnvFingerprint: bundle.EnvFingerprint("test", probe),
	}
}

func evWithDelta(v bundle.EvidenceVerdict, d bundle.Delta) bundle.Evidence {
	e := ev("test", v)
	e.Delta = d
	return e
}

func TestAggregate_EmptySet(t *testing.T) {
	// V1 spec fix: empty deterministic set must be UNVERIFIED, not VERIFIED (all-quantifier on empty set).
	r := verdict.Aggregate(nil)
	if r.Verdict != bundle.Unverified {
		t.Fatalf("empty nil → got %v want UNVERIFIED", r.Verdict)
	}
	r = verdict.Aggregate([]bundle.Evidence{})
	if r.Verdict != bundle.Unverified {
		t.Fatalf("empty slice → got %v want UNVERIFIED", r.Verdict)
	}
	if len(r.UnverifiedReason) == 0 {
		t.Fatal("empty set should carry unverified reason")
	}
}

func TestAggregate_Singletons(t *testing.T) {
	tests := []struct {
		name string
		evs  []bundle.Evidence
		want bundle.GlobalVerdict
	}{
		{"single pass → VERIFIED", []bundle.Evidence{ev("test", bundle.EvidencePass)}, bundle.Verified},
		{"single fail → BROKEN", []bundle.Evidence{ev("test", bundle.EvidenceFail)}, bundle.Broken},
		{"single inconclusive → UNVERIFIED", []bundle.Evidence{ev("test", bundle.EvidenceInconclusive)}, bundle.Unverified},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := verdict.Aggregate(tc.evs)
			if r.Verdict != tc.want {
				t.Fatalf("got %v want %v", r.Verdict, tc.want)
			}
		})
	}
}

func TestAggregate_V2SpecRemovedAndSkip(t *testing.T) {
	// Removed_tests / skip_changes alone must stay VERIFIED but surface warning (v2 fix).
	evRemoved := evWithDelta(bundle.EvidencePass, bundle.Delta{RemovedTests: []string{"old.spec::flaky"}})
	r := verdict.Aggregate([]bundle.Evidence{evRemoved})
	if r.Verdict != bundle.Verified {
		t.Fatalf("removed_tests with pass → got %v want VERIFIED", r.Verdict)
	}
	if len(r.Warnings) == 0 {
		t.Fatal("expected warning for removed_tests")
	}

	evSkip := evWithDelta(bundle.EvidencePass, bundle.Delta{SkipChanges: []string{"slow.spec::heavy"}})
	r = verdict.Aggregate([]bundle.Evidence{evSkip})
	if r.Verdict != bundle.Verified {
		t.Fatalf("skip_changes with pass → got %v want VERIFIED", r.Verdict)
	}

	// Unknown verdict string is treated as inconclusive → UNVERIFIED.
	evUnknown := ev("test", bundle.EvidenceVerdict("unknown"))
	r = verdict.Aggregate([]bundle.Evidence{evUnknown})
	if r.Verdict != bundle.Unverified {
		t.Fatalf("unknown verdict → got %v want UNVERIFIED", r.Verdict)
	}
}

func TestAggregate_PriorityAndCombination(t *testing.T) {
	t.Run("fail wins over inconclusive", func(t *testing.T) {
		r := verdict.Aggregate([]bundle.Evidence{
			ev("test", bundle.EvidenceFail),
			ev("build", bundle.EvidenceInconclusive),
		})
		if r.Verdict != bundle.Broken {
			t.Fatalf("got %v want BROKEN", r.Verdict)
		}
		if len(r.FailedProbes) == 0 {
			t.Fatal("BROKEN should list failed probes")
		}
	})
	t.Run("inconclusive without fail → UNVERIFIED", func(t *testing.T) {
		r := verdict.Aggregate([]bundle.Evidence{
			ev("test", bundle.EvidencePass),
			ev("build", bundle.EvidenceInconclusive),
		})
		if r.Verdict != bundle.Unverified {
			t.Fatalf("got %v want UNVERIFIED", r.Verdict)
		}
	})
	t.Run("all pass → VERIFIED", func(t *testing.T) {
		r := verdict.Aggregate([]bundle.Evidence{
			ev("test", bundle.EvidencePass),
			ev("build", bundle.EvidencePass),
			ev("typecheck", bundle.EvidencePass),
		})
		if r.Verdict != bundle.Verified {
			t.Fatalf("got %v want VERIFIED", r.Verdict)
		}
		if len(r.Warnings) != 0 {
			t.Fatalf("unexpected warnings: %v", r.Warnings)
		}
	})
	t.Run("pass + removed + fail → BROKEN with warning preserved", func(t *testing.T) {
		r := verdict.Aggregate([]bundle.Evidence{
			evWithDelta(bundle.EvidenceFail, bundle.Delta{RemovedTests: []string{"x"}}),
			ev("build", bundle.EvidencePass),
		})
		if r.Verdict != bundle.Broken {
			t.Fatalf("got %v want BROKEN", r.Verdict)
		}
		if len(r.Warnings) == 0 {
			t.Fatal("warnings should be preserved even on BROKEN")
		}
	})
	t.Run("pass + inconclusive + removed → UNVERIFIED with warning", func(t *testing.T) {
		r := verdict.Aggregate([]bundle.Evidence{
			evWithDelta(bundle.EvidencePass, bundle.Delta{RemovedTests: []string{"x"}}),
			ev("build", bundle.EvidenceInconclusive),
		})
		if r.Verdict != bundle.Unverified {
			t.Fatalf("got %v want UNVERIFIED", r.Verdict)
		}
		if len(r.Warnings) == 0 {
			t.Fatal("removed warning should survive UNVERIFIED")
		}
	})
}

func TestExitCode(t *testing.T) {
	if verdict.ExitCode(bundle.Verified) != 0 {
		t.Fatal("VERIFIED exit should be 0")
	}
	if verdict.ExitCode(bundle.Broken) != 1 {
		t.Fatal("BROKEN exit should be 1")
	}
	if verdict.ExitCode(bundle.Unverified) != 2 {
		t.Fatal("UNVERIFIED exit should be 2")
	}
}

func TestAggregate_FlakyAndRegressionsDoNotAffectVerdict(t *testing.T) {
	// Defensive invariant: Differ must set fail when regressions non-empty.
	// Aggregate demotes illegal pass+regressions to UNVERIFIED to avoid hiding a regression.
	e := evWithDelta(bundle.EvidencePass, bundle.Delta{Regressions: []string{"should-have-been-fail"}})
	r := verdict.Aggregate([]bundle.Evidence{e})
	if r.Verdict != bundle.Unverified {
		t.Fatalf("pass+regressions must be demoted to UNVERIFIED, got %v", r.Verdict)
	}
	if len(r.Warnings) == 0 {
		t.Fatal("expected invariant violation warning")
	}
}

func TestAggregate_FixturesAreStorable(t *testing.T) {
	// P0 invariant: test fixtures must pass Evidence.Validate so Aggregate green
	// never masks a bundle-store failure (single source: bundle.Validate).
	for _, probe := range []string{"test", "build", "typecheck"} {
		e := ev(probe, bundle.EvidencePass)
		if err := e.Validate(); err != nil {
			t.Fatalf("fixture %q not storable: %v", probe, err)
		}
	}
}
