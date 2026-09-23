package differ_test

import (
	"context"
	"testing"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/differ"
	"github.com/junqingyongyuanbusi/vouch/internal/probe"
)

func TestArbitrate_SingleSideInstabilityCannotVerify(t *testing.T) {
	base := probe.TestFacts{Passed: []string{"case"}}
	cand := probe.TestFacts{Failed: []string{"case"}}
	arb, err := differ.Arbitrate(context.Background(), base, cand, func(_ context.Context, role string, ids []string) (map[string]differ.CaseVerdict, error) {
		// Base remains green; candidate's observed failure disappears on retry.
		return map[string]differ.CaseVerdict{"case": differ.CasePassed}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(arb.Absorbed) != 0 || len(arb.Unresolved) != 1 {
		t.Fatalf("single-side instability must stay unresolved: %+v", arb)
	}
	ev, err := differ.EvidenceWithArbitration("test", "vouch rerun abc123 --probe test", bundle.EnvFingerprint("env"), time.Now().UTC(), result([]string{"case"}, nil, probe.VerdictPass), result(nil, []string{"case"}, probe.VerdictFail), arb)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Verdict != bundle.EvidenceInconclusive {
		t.Fatalf("want inconclusive, got %s", ev.Verdict)
	}
}

func TestArbitrate_UnresolvedCaseCannotHideConfirmedRegression(t *testing.T) {
	base := probe.TestFacts{Passed: []string{"A", "B"}}
	cand := probe.TestFacts{Failed: []string{"A", "B"}}
	arb, err := differ.Arbitrate(context.Background(), base, cand, func(_ context.Context, role string, ids []string) (map[string]differ.CaseVerdict, error) {
		v := differ.CasePassed
		if role == "candidate" && ids[0] == "A" {
			v = differ.CaseFailed
		}
		return map[string]differ.CaseVerdict{ids[0]: v}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := differ.EvidenceWithArbitration("test", "vouch rerun abc123 --probe test", bundle.EnvFingerprint("env"), time.Now().UTC(), result(base.Passed, nil, probe.VerdictPass), result(nil, cand.Failed, probe.VerdictFail), arb)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Verdict != bundle.EvidenceFail || len(ev.Delta.Regressions) != 1 || ev.Delta.Regressions[0] != "A" {
		t.Fatalf("A must remain a confirmed regression without treating B as confirmed: %+v", ev)
	}
}
