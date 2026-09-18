package scheduler

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/junqingyongyuanbusi/vouch/internal/differ"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/probe"
	"github.com/junqingyongyuanbusi/vouch/internal/worktree"
)

// RerunConfig reproduces one probe from a stored bundle.
type RerunConfig struct {
	RepoRoot     string
	BaseRef      string // resolved commit recorded in the bundle
	CandidateRef string // snapshot ref or commit recorded in the bundle
	Probe        string
	Command      string // command recorded in Evidence.Method
}

// RerunResult is the reproduction outcome for one probe.
type RerunResult struct {
	Probe     string
	Base      probe.RunResult
	Candidate probe.RunResult
	Delta     bundle.Delta
	Verdict   bundle.GlobalVerdict
	BaseDir   string
	// Warnings carries non-fatal problems observed while reproducing (e.g.
	// worktree cleanup failures) so they are visible, not silent.
	Warnings []string
}

// Rerun recreates the recorded worktrees and executes the recorded command on
// both sides, so a stored verdict can be checked by hand.
func Rerun(ctx context.Context, cfg RerunConfig) (res RerunResult, err error) {
	if cfg.BaseRef == "" || cfg.CandidateRef == "" {
		return RerunResult{}, fmt.Errorf("bundle is missing base/candidate refs")
	}
	if err := ensureRefAvailable(cfg.RepoRoot, cfg.CandidateRef); err != nil {
		return RerunResult{}, err
	}
	pair, err := worktree.CreateContext(ctx, cfg.RepoRoot, cfg.BaseRef, cfg.CandidateRef)
	if err != nil {
		return RerunResult{}, err
	}
	// Surface cleanup failures the same way verify does (RerunResult.Warnings).
	defer func() {
		if cerr := pair.Cleanup(); cerr != nil {
			res.Warnings = append(res.Warnings, "worktree cleanup failed: "+cerr.Error())
		}
	}()

	budget := 120_000
	command := cfg.Command
	if command == "" {
		return RerunResult{}, fmt.Errorf("no recorded command for probe %q", cfg.Probe)
	}
	baseRes := runBuiltin(ctx, cfg.Probe, pair.Base, "base", command, budget)
	candRes := runBuiltin(ctx, cfg.Probe, pair.Candidate, "candidate", command, budget)

	// Reproduce the same arbitration verify performed: without it a flaky case
	// would print BROKEN here while verify recorded VERIFIED + flaky_absorbed,
	// i.e. the reproduce command would contradict its own verdict.
	arb := differ.TestArbitration{}
	if cfg.Probe == "test" {
		rerun := caseRerunFactory(pair.Base, pair.Candidate, command, budget)
		arb, _ = differ.Arbitrate(ctx, probe.TestFactsOf(baseRes), probe.TestFactsOf(candRes), rerun)
	}
	ev, err := evidenceFor(cfg.Probe, baseRes, candRes, arb)
	if err != nil {
		return RerunResult{}, err
	}
	return RerunResult{
		Probe:     cfg.Probe,
		Base:      baseRes,
		Candidate: candRes,
		Delta:     ev.Delta,
		Verdict:   rerunVerdict(ev),
		BaseDir:   pair.Base,
	}, nil
}

// evidenceFor builds evidence with a stable reproduce id (the caller's bundle
// id is shown by the renderer, not needed for the verdict).
func evidenceFor(probeName string, base, cand probe.RunResult, arb differ.TestArbitration) (bundle.Evidence, error) {
	return differEvidence("rerun", probeName, base, cand, arb)
}

// rerunVerdict maps a single evidence verdict to the three-state pipeline verdict.
func rerunVerdict(ev bundle.Evidence) bundle.GlobalVerdict {
	switch ev.Verdict {
	case bundle.EvidenceFail:
		return bundle.Broken
	case bundle.EvidenceInconclusive:
		return bundle.Unverified
	default:
		return bundle.Verified
	}
}

// ensureRefAvailable turns "candidate snapshot was pruned" into a clear message
// instead of a raw git error, so the agent loop knows to re-run verify.
func ensureRefAvailable(repoRoot, ref string) error {
	cmd := exec.Command("git", "rev-parse", "--verify", ref+"^{commit}")
	cmd.Dir = repoRoot
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("candidate snapshot %s is no longer available (garbage-collected); re-run `vouch verify` to produce a new bundle", ref)
	}
	_ = strings.TrimSpace
	return nil
}
