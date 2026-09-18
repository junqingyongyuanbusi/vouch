package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/detector"
	"github.com/junqingyongyuanbusi/vouch/internal/mcp"
	"github.com/junqingyongyuanbusi/vouch/internal/render"
	"github.com/junqingyongyuanbusi/vouch/internal/scheduler"
	"github.com/junqingyongyuanbusi/vouch/internal/verdict"
	"github.com/junqingyongyuanbusi/vouch/internal/version"
)

// mcpBackend adapts the kernel (scheduler + store + detector) to the MCP tool
// surface. It is the only place where MCP and the verification path meet, so
// the protocol layer stays free of verification logic.
type mcpBackend struct {
	defaultPath string
}

func (b mcpBackend) resolve(p string) string {
	if p != "" {
		return p
	}
	if b.defaultPath != "" {
		return b.defaultPath
	}
	return "."
}

// Verify runs the full pipeline and returns the structured facts an agent needs
// to decide what to do next. The verdict is carried as data: an MCP tool call
// must not turn "BROKEN" into a transport error.
func (b mcpBackend) Verify(ctx context.Context, req mcp.VerifyRequest) (mcp.VerifyResult, error) {
	repo := b.resolve(req.Path)
	budget := req.BudgetMs
	if budget <= 0 {
		budget = scheduler.DefaultBudgetMs
	}
	// budget_ms is the scheduler's total budget; process-based setup (git
	// snapshot/add, dependency copy) and the probes are canceled and reaped
	// when it expires. Pure filesystem cleanup (removing worktree dirs) is not
	// deadline-bounded, so the call may still slightly outlive the deadline:
	// this is not a strict return bound.
	ctx, cancel := context.WithTimeout(ctx, verifyCallTimeout(budget))
	defer cancel()

	res, err := scheduler.Run(ctx, scheduler.Config{
		RepoRoot:     repo,
		BaseRef:      orDefault(req.From, "HEAD"),
		CandidateRef: req.To,
		Intent:       req.Intent,
		InstallDeps:  req.InstallDeps,
		// The scheduler owns the total budget and exhaustion attribution.
		BudgetMs: budget,
	})
	if err != nil {
		return mcp.VerifyResult{}, err
	}
	out := mcp.VerifyResult{
		Verdict:  string(res.Verdict),
		ExitCode: verdict.ExitCode(res.Verdict),
		BundleID: res.Bundle.BundleID,
		Path:     repo,
		BaseRef:  res.Bundle.Subject.BaseRef,
		// Only a persisted run may claim an evidence path; an unpersisted setup
		// failure has no bundle id and nothing on disk to point at.
		EvidencePath: evidencePath(res.Bundle.BundleID),
		Summary:      render.Human(res),
		Feedback:     render.AgentFeedback(res),
		Unverified:   unverifiedClaims(res),
		NextAction:   nextAction(res.Verdict),
	}
	if res.Bundle.Subject.CandidateRef != nil {
		out.CandidateRef = *res.Bundle.Subject.CandidateRef
	}
	if res.Bundle.BaselineSummary != nil {
		out.Baseline = &mcp.Baseline{
			Status:  string(res.Bundle.BaselineSummary.Status),
			Failing: res.Bundle.BaselineSummary.Failing,
		}
	}
	if res.Selection.Mechanism != "" {
		out.SelectionKind = res.Selection.Mechanism
	}
	for _, ev := range res.Evidences {
		out.Probes = append(out.Probes, mcp.ProbeResult{
			Probe:         ev.Probe,
			Verdict:       string(ev.Verdict),
			Regressions:   ev.Delta.Regressions,
			NewPassing:    ev.Delta.NewPassing,
			RemovedTests:  ev.Delta.RemovedTests,
			FlakyAbsorbed: ev.Delta.FlakyAbsorbed,
			Reproduce:     ev.Reproduce,
		})
		for _, f := range ev.Failures {
			out.Regressions = append(out.Regressions, mcp.Regression{
				Probe: ev.Probe, ID: f.ID, Message: f.Message,
			})
		}
		out.RemovedTests = append(out.RemovedTests, ev.Delta.RemovedTests...)
		if out.Reproduce == "" && ev.Verdict != bundle.EvidencePass {
			out.Reproduce = ev.Reproduce
		}
	}
	if out.Reproduce == "" {
		out.Reproduce = fmt.Sprintf("vouch rerun %s", res.Bundle.BundleID)
	}
	if res.Verdict == bundle.Broken {
		out.FixChecklist = []string{
			"Reproduce the regressions with the reproduce command.",
			"Fix the cause, not the assertion — unless the assertion itself is wrong.",
			"Call vouch_verify again; only exit_code 0 (VERIFIED) means done.",
		}
	}
	return out, nil
}

// Show reads a stored bundle without re-running anything.
func (b mcpBackend) Show(_ context.Context, req mcp.ShowRequest) (mcp.ShowResult, error) {
	root := b.resolve(req.Path)
	store := bundle.NewStore(root)
	id := req.BundleID
	if id == "" {
		latest, ok, err := store.Latest()
		if err != nil {
			return mcp.ShowResult{}, err
		}
		if !ok {
			return mcp.ShowResult{}, fmt.Errorf("no bundles stored in %s/.vouch/bundles", root)
		}
		id = latest.BundleID
	}
	bun, err := store.Load(id)
	if err != nil {
		return mcp.ShowResult{}, err
	}
	out := mcp.ShowResult{
		BundleID:  bun.BundleID,
		Verdict:   string(bun.Verdict),
		CreatedAt: bun.CreatedAt.Format(time.RFC3339),
		BaseRef:   bun.Subject.BaseRef,
		Markdown:  render.Markdown(bun),
	}
	if bun.Subject.CandidateRef != nil {
		out.CandidateRef = *bun.Subject.CandidateRef
	}
	for _, ev := range bun.Evidence {
		out.Probes = append(out.Probes, mcp.ProbeResult{
			Probe:         ev.Probe,
			Verdict:       string(ev.Verdict),
			Regressions:   ev.Delta.Regressions,
			NewPassing:    ev.Delta.NewPassing,
			RemovedTests:  ev.Delta.RemovedTests,
			FlakyAbsorbed: ev.Delta.FlakyAbsorbed,
			Reproduce:     ev.Reproduce,
		})
		for _, f := range ev.Failures {
			out.Regressions = append(out.Regressions, mcp.Regression{Probe: ev.Probe, ID: f.ID, Message: f.Message})
		}
		if out.Reproduce == "" && ev.Verdict != bundle.EvidencePass {
			out.Reproduce = ev.Reproduce
		}
	}
	return out, nil
}

// Gaps runs detection only — no probes, no writes — so an agent can ask what is
// (not) covered before trusting a verdict.
func (b mcpBackend) Gaps(_ context.Context, req mcp.GapsRequest) (mcp.GapsResult, error) {
	root := b.resolve(req.Path)
	profile, err := detector.Detect(root)
	if err != nil {
		return mcp.GapsResult{}, err
	}
	out := mcp.GapsResult{
		Path:       root,
		Confidence: string(profile.Confidence),
		Languages:  profile.Language,
		Gaps:       profile.Gaps,
	}
	if profile.PackageManager != nil {
		out.PackageManager = *profile.PackageManager
	}
	kinds := make([]string, 0, len(profile.Commands))
	for k := range profile.Commands {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		c := profile.Commands[k]
		out.Commands = append(out.Commands, mcp.DetectedCommand{Kind: k, Command: c.Cmd, Source: c.Source})
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "detection confidence: %s\n", profile.Confidence)
	for _, c := range out.Commands {
		fmt.Fprintf(&sb, "%s: %s  (%s)\n", c.Kind, c.Command, c.Source)
	}
	for _, g := range profile.Gaps {
		fmt.Fprintf(&sb, "gap: %s\n", g)
	}
	if len(out.Commands) == 0 {
		sb.WriteString("no commands detected: a verify on this repository can only return UNVERIFIED\n")
	}
	out.Summary = sb.String()
	return out, nil
}

// evidencePath reports a bundle-relative path only when a bundle exists.
func evidencePath(bundleID string) string {
	if bundleID == "" {
		return ""
	}
	return filepath.Join(".vouch", "bundles", bundleID)
}

// unverifiedClaims keeps Result.Unverified as the fallback so a setup failure
// that was never persisted still carries its reason to the agent.
func unverifiedClaims(res scheduler.Result) []string {
	if len(res.Bundle.UnverifiedClaims) > 0 {
		return res.Bundle.UnverifiedClaims
	}
	return res.Unverified
}

// callGuardSlack delays the outer context deadline beyond the kernel budget.
// It does not bound non-cancellable setup or cleanup.
const callGuardSlack = 60 * time.Second

// verifyCallTimeout computes the outer context deadline, not a guaranteed
// return time. The scheduler owns the total budget; duration conversion is
// clamped to avoid overflow.
func verifyCallTimeout(totalBudgetMs int) time.Duration {
	if totalBudgetMs <= 0 {
		totalBudgetMs = scheduler.DefaultBudgetMs
	}
	if totalBudgetMs > scheduler.MaxBudgetMs {
		totalBudgetMs = scheduler.MaxBudgetMs
	}
	return time.Duration(totalBudgetMs)*time.Millisecond + callGuardSlack
}

func nextAction(v bundle.GlobalVerdict) string {
	switch v {
	case bundle.Verified:
		return "No regressions in the measured commands. Merge, or keep working."
	case bundle.Broken:
		return "Regressions exist: fix them and call vouch_verify again."
	default:
		return "Unverified: nothing was measured. Report this honestly; do not claim success."
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// newMcpCmd serves the MCP protocol on stdio. Agents launch it through their
// MCP client config (see `vouch init claude-code`); humans rarely run it by hand.
func newMcpCmd() *cobra.Command {
	var repoPath string
	c := &cobra.Command{
		Use:    "mcp",
		Hidden: true,
		Short:  "Run as an MCP server (stdio) exposing vouch_verify / vouch_show / vouch_gaps",
		Long: "Serves the Model Context Protocol on stdin/stdout so an agent can ask\n" +
			"whether a change is verifiable without leaving its own loop.\n" +
			"Diagnostics go to stderr; stdout carries protocol messages only.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// The SDK's stdio transport talks to the process's real stdin/stdout:
			// stdout must carry protocol messages only, so every diagnostic goes
			// to stderr.
			if err := mcp.ServeStdio(cmd.Context(), mcpBackend{defaultPath: repoPath}, version.Version); err != nil {
				fmt.Fprintf(os.Stderr, "vouch mcp: %v\n", err)
				return &ExitError{Code: verdict.ExitCode(bundle.Unverified), Err: err}
			}
			return nil
		},
	}
	c.Flags().StringVar(&repoPath, "path", "", "default repository path for tool calls (default .)")
	return c
}
