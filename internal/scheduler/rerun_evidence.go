package scheduler

import (
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/differ"
	"github.com/junqingyongyuanbusi/vouch/internal/probe"
)

// differEvidence is a thin wrapper so rerun.go does not import differ twice.
func differEvidence(id, probeName string, base, cand probe.RunResult, arb differ.TestArbitration) (bundle.Evidence, error) {
	return differ.EvidenceWithArbitration(probeName, "vouch rerun "+id+" --probe "+probeName,
		bundle.EnvFingerprint("rerun"), time.Now().UTC(), base, cand, arb)
}
