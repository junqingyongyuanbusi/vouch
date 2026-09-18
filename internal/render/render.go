// Package render — human and machine output for a verify result.
package render

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/scheduler"
	"github.com/junqingyongyuanbusi/vouch/internal/verdict"
)

// claimsOf is the single source of truth for "what could not be measured".
// A normal run fills Bundle.UnverifiedClaims and Result.Unverified with the
// same values; a setup failure is never persisted, so only Result.Unverified
// carries the reason. Reading through one helper keeps every renderer honest.
func claimsOf(res scheduler.Result) []string {
	if len(res.Bundle.UnverifiedClaims) > 0 {
		return res.Bundle.UnverifiedClaims
	}
	return res.Unverified
}

// Human renders the terminal summary. The base baseline line is mandatory for
// VERIFIED (PLAN-V2 §5): a half-broken base must never look all green.
func Human(res scheduler.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", verdictBanner(res.Verdict))
	if res.Bundle.BaselineSummary != nil {
		bs := res.Bundle.BaselineSummary
		line := fmt.Sprintf("  base: %s", bs.Status)
		if bs.Failing > 0 {
			line += fmt.Sprintf("（%d 失败，基线非全绿）", bs.Failing)
		}
		b.WriteString(line + "\n")
	}
	for _, ev := range res.Evidences {
		fmt.Fprintf(&b, "  %-9s %s", ev.Probe, ev.Verdict)
		if len(ev.Delta.Regressions) > 0 {
			fmt.Fprintf(&b, " · 回归 %d", len(ev.Delta.Regressions))
		}
		if len(ev.Delta.NewPassing) > 0 {
			fmt.Fprintf(&b, " · 新增通过 %d", len(ev.Delta.NewPassing))
		}
		if len(ev.Delta.RemovedTests) > 0 {
			fmt.Fprintf(&b, " · removed_tests %d", len(ev.Delta.RemovedTests))
		}
		if len(ev.Delta.FlakyAbsorbed) > 0 {
			fmt.Fprintf(&b, " · flaky 已吸收 %d", len(ev.Delta.FlakyAbsorbed))
		}
		b.WriteString("\n")
	}
	for _, ev := range res.Evidences {
		for _, f := range firstN(ev.Failures, 5) {
			fmt.Fprintf(&b, "      · %s: %s\n", f.ID, f.Message)
		}
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "  ⚠ %s\n", w)
	}
	for _, u := range claimsOf(res) {
		fmt.Fprintf(&b, "  ○ %s\n", u)
	}
	// An unpersisted result (setup never completed) has no bundle id: printing
	// an evidence path would point at a directory that does not exist.
	if res.Bundle.BundleID != "" {
		fmt.Fprintf(&b, "  证据包：.vouch/bundles/%s\n", res.Bundle.BundleID)
	}
	if ev := firstNonPassing(res.Evidences); ev != nil {
		fmt.Fprintf(&b, "  复现：%s\n", ev.Reproduce)
	} else if len(res.Evidences) > 0 {
		fmt.Fprintf(&b, "  复现：%s\n", res.Evidences[0].Reproduce)
	}
	return b.String()
}

func verdictBanner(v bundle.GlobalVerdict) string {
	switch v {
	case bundle.Verified:
		return "✓ VERIFIED"
	case bundle.Broken:
		return "✗ BROKEN"
	default:
		return "○ UNVERIFIED"
	}
}

// JSON renders the machine view. A setup failure is never persisted, so the
// bundle stays zero-valued: verdict and reasons come from the result,
// `persisted:false` marks the absence of a stored bundle, and no bundle id or
// diff sha is fabricated for a diff that was never observed.
func JSON(res scheduler.Result) ([]byte, error) {
	if res.Bundle.BundleID == "" {
		return json.MarshalIndent(struct {
			Verdict          string   `json:"verdict"`
			ExitCode         int      `json:"exit_code"`
			Persisted        bool     `json:"persisted"`
			UnverifiedClaims []string `json:"unverified_claims,omitempty"`
			Warnings         []string `json:"warnings,omitempty"`
		}{
			Verdict:          string(res.Verdict),
			ExitCode:         verdict.ExitCode(res.Verdict),
			UnverifiedClaims: claimsOf(res),
			Warnings:         res.Warnings,
		}, "", "  ")
	}
	return json.MarshalIndent(res.Bundle, "", "  ")
}

// Gaps renders the detector provenance for --explain-gaps.
func Gaps(res scheduler.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "profile confidence: %s\n", res.Profile.Confidence)
	if res.Profile.PackageManager != nil {
		fmt.Fprintf(&b, "package manager: %s\n", *res.Profile.PackageManager)
	}
	for _, k := range sortedKeys(res) {
		c := res.Profile.Commands[k]
		fmt.Fprintf(&b, "%s: %s  (%s)\n", k, c.Cmd, c.Source)
	}
	if res.Selection.Mechanism != "" {
		fmt.Fprintf(&b, "selection: %s fullRun=%v targets=%d\n", res.Selection.Mechanism, res.Selection.FullRun, len(res.Selection.Targets))
	}
	for _, g := range res.Profile.Gaps {
		fmt.Fprintf(&b, "gap: %s\n", g)
	}
	for _, u := range claimsOf(res) {
		fmt.Fprintf(&b, "unverified: %s\n", u)
	}
	return b.String()
}

// firstNonPassing prefers a failing/inconclusive probe for the reproduce hint:
// pointing at a passing probe would send the reader to the wrong evidence.
func firstNonPassing(evs []bundle.Evidence) *bundle.Evidence {
	for i := range evs {
		if evs[i].Verdict != bundle.EvidencePass {
			return &evs[i]
		}
	}
	return nil
}

func sortedKeys(res scheduler.Result) []string {
	keys := make([]string, 0, len(res.Profile.Commands))
	for k := range res.Profile.Commands {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Rerun renders the reproduction outcome for one probe.
func Rerun(res scheduler.RerunResult, bundleID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "rerun %s · probe %s → %s\n", bundleID, res.Probe, res.Verdict)
	fmt.Fprintf(&b, "  base:      %s\n", res.Base.Summary)
	fmt.Fprintf(&b, "  candidate: %s\n", res.Candidate.Summary)
	if len(res.Delta.Regressions) > 0 {
		fmt.Fprintf(&b, "  回归: %v\n", res.Delta.Regressions)
	}
	if len(res.Delta.FlakyAbsorbed) > 0 {
		fmt.Fprintf(&b, "  flaky 已吸收: %v\n", res.Delta.FlakyAbsorbed)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "  ⚠ %s\n", w)
	}
	return b.String()
}

func firstN(fs []bundle.FailureDetail, n int) []bundle.FailureDetail {
	if len(fs) <= n {
		return fs
	}
	return fs[:n]
}

// Markdown renders the PR-comment form of a stored bundle (P2 export minimum).
func Markdown(b bundle.ProofBundle) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## vouch: %s\n\n", b.Verdict)
	fmt.Fprintf(&sb, "- bundle: `%s` · base: `%s`", b.BundleID, b.Subject.BaseRef)
	if b.Subject.CandidateRef != nil {
		fmt.Fprintf(&sb, " · candidate: `%s`", *b.Subject.CandidateRef)
	}
	sb.WriteString("\n")
	if b.BaselineSummary != nil {
		fmt.Fprintf(&sb, "- base baseline: %s (failing: %d)\n", b.BaselineSummary.Status, b.BaselineSummary.Failing)
	}
	sb.WriteString("\n| probe | verdict | regressions | new passing | flaky absorbed |\n|---|---|---|---|---|\n")
	for _, ev := range b.Evidence {
		fmt.Fprintf(&sb, "| %s | %s | %d | %d | %d |\n", ev.Probe, ev.Verdict,
			len(ev.Delta.Regressions), len(ev.Delta.NewPassing), len(ev.Delta.FlakyAbsorbed))
	}
	for _, ev := range b.Evidence {
		if len(ev.Failures) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "\n**%s failures**\n\n", ev.Probe)
		for _, f := range ev.Failures {
			fmt.Fprintf(&sb, "- `%s`: %s\n", f.ID, f.Message)
		}
	}
	if ev := firstNonPassing(b.Evidence); ev != nil {
		fmt.Fprintf(&sb, "\n复现：`%s`\n", ev.Reproduce)
	} else if len(b.Evidence) > 0 {
		fmt.Fprintf(&sb, "\n复现：`%s`\n", b.Evidence[0].Reproduce)
	}
	return sb.String()
}

// AgentFeedback renders the compact form an agent needs to fix a failure and
// nothing else: verdict, regressions with stable case ids, the reproduce
// command, and the honest list of what could not be measured.
//
// This is the hook/MCP feedback channel. It is deliberately terse — an agent
// pays tokens for every line — and it never suppresses a regression: if a
// regression exists, it is printed even when other probes also failed.
func AgentFeedback(res scheduler.Result) string {
	var b strings.Builder
	switch res.Verdict {
	case bundle.Verified:
		fmt.Fprintf(&b, "vouch: VERIFIED (exit 0) — no regressions in the measured commands.\n")
	case bundle.Broken:
		fmt.Fprintf(&b, "vouch: BROKEN (exit 1) — this change introduced regressions. Fix them before claiming done.\n")
	default:
		fmt.Fprintf(&b, "vouch: UNVERIFIED (exit 2) — nothing could be measured. Do NOT claim the change works.\n")
	}
	if res.Bundle.BaselineSummary != nil {
		bs := res.Bundle.BaselineSummary
		fmt.Fprintf(&b, "base: %s", bs.Status)
		if bs.Failing > 0 {
			fmt.Fprintf(&b, " (%d already failing before this change)", bs.Failing)
		}
		b.WriteString("\n")
	}
	for _, ev := range res.Evidences {
		if ev.Verdict == bundle.EvidencePass {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", ev.Probe, ev.Verdict)
		for _, id := range ev.Delta.Regressions {
			fmt.Fprintf(&b, "  - regression: %s\n", id)
		}
		for _, f := range firstN(ev.Failures, 10) {
			fmt.Fprintf(&b, "  - %s: %s\n", f.ID, f.Message)
		}
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	for _, u := range claimsOf(res) {
		fmt.Fprintf(&b, "not measured: %s\n", u)
	}
	if ev := firstNonPassing(res.Evidences); ev != nil {
		fmt.Fprintf(&b, "reproduce: %s\n", ev.Reproduce)
	}
	if res.Bundle.BundleID != "" {
		fmt.Fprintf(&b, "evidence: .vouch/bundles/%s\n", res.Bundle.BundleID)
	}
	return b.String()
}
