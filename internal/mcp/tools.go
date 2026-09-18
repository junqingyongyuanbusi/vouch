package mcp

import (
	"context"
	"fmt"
	"strings"
)

// Backend is the kernel surface this server exposes. The CLI wires it to the
// scheduler; tests wire it to fakes. Keeping it an interface is what stops the
// MCP layer from growing its own verification logic (and its own verdicts).
type Backend interface {
	Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error)
	Show(ctx context.Context, req ShowRequest) (ShowResult, error)
	Gaps(ctx context.Context, req GapsRequest) (GapsResult, error)
}

// VerifyRequest are the arguments of the vouch_verify tool.
type VerifyRequest struct {
	Path        string `json:"path,omitempty" jsonschema:"repository path (default: the server's --path, else \".\")"`
	From        string `json:"from,omitempty" jsonschema:"base ref (default HEAD)"`
	To          string `json:"to,omitempty" jsonschema:"candidate ref (default: the working tree)"`
	Intent      string `json:"intent,omitempty" jsonschema:"optional one-line intent of the change, stored in the bundle subject"`
	BudgetMs    int    `json:"budget_ms,omitempty" jsonschema:"total verification budget in milliseconds (default 600000); context-aware steps, process-based setup (git, dependency copy) and probes share the deadline and are canceled on expiry; pure filesystem cleanup may still run past it; not a strict return-time bound"`
	InstallDeps bool   `json:"install_deps,omitempty" jsonschema:"allow installing missing JS dependencies (network); default false"`
}

// Regression is one introduced failure, addressed by a stable case id.
type Regression struct {
	Probe   string `json:"probe"`
	ID      string `json:"id"`
	Message string `json:"message,omitempty"`
}

// ProbeResult summarises one probe's verdict.
type ProbeResult struct {
	Probe         string   `json:"probe"`
	Verdict       string   `json:"verdict"`
	Regressions   []string `json:"regressions,omitempty"`
	NewPassing    []string `json:"new_passing,omitempty"`
	RemovedTests  []string `json:"removed_tests,omitempty"`
	FlakyAbsorbed []string `json:"flaky_absorbed,omitempty"`
	Reproduce     string   `json:"reproduce,omitempty"`
}

// Baseline reports base-side state, so VERIFIED on a half-broken base is visible.
type Baseline struct {
	Status  string `json:"status"`
	Failing int    `json:"failing"`
}

// VerifyResult is the structured vouch_verify payload.
type VerifyResult struct {
	Verdict       string        `json:"verdict"`
	ExitCode      int           `json:"exit_code"`
	BundleID      string        `json:"bundle_id"`
	Path          string        `json:"path"`
	BaseRef       string        `json:"base_ref"`
	CandidateRef  string        `json:"candidate_ref,omitempty"`
	Probes        []ProbeResult `json:"probes"`
	Regressions   []Regression  `json:"regressions"`
	RemovedTests  []string      `json:"removed_tests,omitempty"`
	Baseline      *Baseline     `json:"baseline,omitempty"`
	Reproduce     string        `json:"reproduce,omitempty"`
	Unverified    []string      `json:"unverified_claims,omitempty"`
	EvidencePath  string        `json:"evidence_path,omitempty"`
	Summary       string        `json:"summary"`
	Feedback      string        `json:"feedback"`
	FixChecklist  []string      `json:"fix_checklist,omitempty"`
	NextAction    string        `json:"next_action"`
	SelectionKind string        `json:"selection,omitempty"`
}

// ShowRequest are the arguments of the vouch_show tool.
type ShowRequest struct {
	Path     string `json:"path,omitempty" jsonschema:"repository path (default: the server's --path, else \".\")"`
	BundleID string `json:"bundle_id,omitempty" jsonschema:"bundle id (6-12 hex); omit for the most recent bundle"`
}

// ShowResult is the structured vouch_show payload.
type ShowResult struct {
	BundleID     string        `json:"bundle_id"`
	Verdict      string        `json:"verdict"`
	CreatedAt    string        `json:"created_at,omitempty"`
	BaseRef      string        `json:"base_ref,omitempty"`
	CandidateRef string        `json:"candidate_ref,omitempty"`
	Probes       []ProbeResult `json:"probes"`
	Regressions  []Regression  `json:"regressions"`
	Reproduce    string        `json:"reproduce,omitempty"`
	Markdown     string        `json:"markdown,omitempty"`
}

// GapsRequest are the arguments of the vouch_gaps tool.
type GapsRequest struct {
	Path string `json:"path,omitempty" jsonschema:"repository path (default: the server's --path, else \".\")"`
}

// DetectedCommand is one command the detector found, with its provenance.
type DetectedCommand struct {
	Kind    string `json:"kind"`
	Command string `json:"command"`
	Source  string `json:"source"`
}

// GapsResult is the structured vouch_gaps payload.
type GapsResult struct {
	Path           string            `json:"path"`
	Confidence     string            `json:"confidence"`
	PackageManager string            `json:"package_manager,omitempty"`
	Languages      []string          `json:"languages,omitempty"`
	Commands       []DetectedCommand `json:"commands"`
	Gaps           []string          `json:"gaps"`
	Summary        string            `json:"summary"`
}

// verifyText is the textual form of a verify result: the compact agent
// feedback plus the machine verdict line. Clients that ignore
// structuredContent still get something actionable.
func verifyText(res VerifyResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (exit %d) · bundle %s\n", res.Verdict, res.ExitCode, res.BundleID)
	if strings.TrimSpace(res.Feedback) != "" {
		sb.WriteString(res.Feedback)
		if !strings.HasSuffix(res.Feedback, "\n") {
			sb.WriteString("\n")
		}
	}
	if res.NextAction != "" {
		fmt.Fprintf(&sb, "next: %s\n", res.NextAction)
	}
	return sb.String()
}

// showText is the textual form of a stored bundle.
func showText(res ShowResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s · bundle %s\n", res.Verdict, res.BundleID)
	for _, p := range res.Probes {
		fmt.Fprintf(&sb, "  %-9s %s", p.Probe, p.Verdict)
		if len(p.Regressions) > 0 {
			fmt.Fprintf(&sb, " · regressions %d", len(p.Regressions))
		}
		sb.WriteString("\n")
	}
	for _, r := range res.Regressions {
		fmt.Fprintf(&sb, "      · [%s] %s: %s\n", r.Probe, r.ID, r.Message)
	}
	if res.Reproduce != "" {
		fmt.Fprintf(&sb, "reproduce: %s\n", res.Reproduce)
	}
	return sb.String()
}
