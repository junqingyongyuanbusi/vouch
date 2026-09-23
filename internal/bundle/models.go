// Package bundle — evidence and bundle types per docs/bundle-schema.json v1.
//
// Data model discipline (PLAN-V2 §5):
//   - Evidence.Kind == "deterministic" only; Inferences are a separate array.
//   - Every Evidence has a required `reproduce` command — no reproduce, no storage.
//   - Delta captures removed_tests/skip_changes explicitly to prevent masking regressions.
//   - Baseline.status is required to avoid "base was half-broken but shown as green" trust loss.
//   - ProofBundle.Verdict is aggregated from evidence only; empty evidence → UNVERIFIED.
//
// JSON invariants (P0/P1 audit):
//   - Nil slices marshal as null in Go, but schema requires array. Delta and bundle
//     custom MarshalJSON convert nil → [] so exported bundles always validate.
//   - VouchVersion is populated by NewProofBundle / MarshalJSON from
//     internal/version.Version; single ldflag on internal/version.Version only
//     (bundle.VouchVersion() reads it, no dual ldflags).
package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/version"
)

// SchemaVersion is the current bundle format version (bumped with breaking changes).
const SchemaVersion = 1

// VouchVersion returns the current version string, always reading
// internal/version.Version so a single ldflag is sufficient:
//
//	-X github.com/junqingyongyuanbusi/vouch/internal/version.Version={{.Version}}
func VouchVersion() string { return version.Version }

var (
	reBundleID       = regexp.MustCompile(`^[a-f0-9]{6,12}$`)
	reEvidenceID     = regexp.MustCompile(`^ev_[a-f0-9]{6}$`)
	reDiffSHA256     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	reEnvFingerprint = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// Kind distinguishes deterministic evidence from inferred opinions.
type Kind string

const (
	KindDeterministic Kind = "deterministic"
	KindInferred      Kind = "inferred"
)

// EvidenceVerdict is probe-local pass/fail, not the global bundle verdict.
type EvidenceVerdict string

const (
	EvidencePass         EvidenceVerdict = "pass"
	EvidenceFail         EvidenceVerdict = "fail"
	EvidenceInconclusive EvidenceVerdict = "inconclusive"
)

// GlobalVerdict is the three-state bundle conclusion visible to humans and CI.
type GlobalVerdict string

const (
	Verified   GlobalVerdict = "VERIFIED"
	Broken     GlobalVerdict = "BROKEN"
	Unverified GlobalVerdict = "UNVERIFIED"
)

// BaselineStatus records the health of the base worktree itself.
type BaselineStatus string

const (
	BaselineAllGreen         BaselineStatus = "all_green"
	BaselineExistingFailures BaselineStatus = "existing_failures"
	BaselineExistingBroken   BaselineStatus = "existing_broken"
)

// Confidence reflects how trustworthy the Detector's profile is.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// Command is a single build/test/typecheck command with auditable source.
type Command struct {
	Cmd    string `json:"cmd"`
	Source string `json:"source"`
}

// TestSelection describes whether affected-test selection is supported for this repo.
type TestSelection struct {
	Supported bool   `json:"supported"`
	Mechanism string `json:"mechanism,omitempty"`
}

// ProjectProfile is Detector output — every command carries its provenance.
type ProjectProfile struct {
	Language       []string           `json:"language"`
	PackageManager *string            `json:"package_manager"`
	Commands       map[string]Command `json:"commands"`
	TestSelection  *TestSelection     `json:"test_selection,omitempty"`
	Confidence     Confidence         `json:"confidence"`
	Gaps           []string           `json:"gaps"`
}

// Subject identifies what was verified.
type Subject struct {
	RepoRoot     string  `json:"repo_root"`
	BaseRef      string  `json:"base_ref"`
	CandidateRef *string `json:"candidate_ref,omitempty"`
	DiffSHA256   string  `json:"diff_sha256"`
	Intent       *string `json:"intent,omitempty"`
}

// BaselineResult and CandidateResult hold probe-side totals.
type BaselineResult struct {
	Pass       int            `json:"pass"`
	Fail       int            `json:"fail"`
	Skip       int            `json:"skip"`
	DurationMs int            `json:"duration_ms"`
	Status     BaselineStatus `json:"status"`
}

type CandidateResult struct {
	Pass       int `json:"pass"`
	Fail       int `json:"fail"`
	Skip       int `json:"skip"`
	DurationMs int `json:"duration_ms"`
}

// Delta is the heart of differential verification.
// Custom MarshalJSON ensures nil slices never become `null` (schema requires array).
type Delta struct {
	Regressions   []string `json:"regressions"`
	NewPassing    []string `json:"new_passing"`
	RemovedTests  []string `json:"removed_tests"`
	SkipChanges   []string `json:"skip_changes"`
	FlakyAbsorbed []string `json:"flaky_absorbed"`
}

// MarshalJSON converts nil slices to empty arrays so the bundle never emits `null`.
func (d Delta) MarshalJSON() ([]byte, error) {
	type Alias Delta
	aux := struct {
		Alias
	}{
		Alias: Alias(d),
	}
	if aux.Regressions == nil {
		aux.Regressions = []string{}
	}
	if aux.NewPassing == nil {
		aux.NewPassing = []string{}
	}
	if aux.RemovedTests == nil {
		aux.RemovedTests = []string{}
	}
	if aux.SkipChanges == nil {
		aux.SkipChanges = []string{}
	}
	if aux.FlakyAbsorbed == nil {
		aux.FlakyAbsorbed = []string{}
	}
	return json.Marshal(aux.Alias)
}

// FailureDetail carries one failing case's message so consumers (show/--json,
// agent hooks) learn WHY a regression happened without re-running the probe.
type FailureDetail struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// Evidence is the minimal unit — one probe, two runs (base/candidate), one delta.
type Evidence struct {
	ID             string          `json:"id"`
	Claim          string          `json:"claim"`
	Kind           Kind            `json:"kind"`
	Probe          string          `json:"probe"`
	Method         string          `json:"method"`
	Baseline       BaselineResult  `json:"baseline"`
	Candidate      CandidateResult `json:"candidate"`
	Delta          Delta           `json:"delta"`
	Verdict        EvidenceVerdict `json:"verdict"`
	Failures       []FailureDetail `json:"failures,omitempty"`
	Reproduce      string          `json:"reproduce"`
	Logs           string          `json:"logs,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	EnvFingerprint string          `json:"env_fingerprint"`
}

// Inference is an LLM-produced opinion — physically separated from Evidence.
type Inference struct {
	ID    string   `json:"id"`
	Kind  Kind     `json:"kind"`
	Probe string   `json:"probe"`
	Msg   string   `json:"msg"`
	Refs  []string `json:"refs,omitempty"`
}

// BaselineSummary is echoed at the bundle level for human rendering.
type BaselineSummary struct {
	Status  BaselineStatus `json:"status"`
	Failing int            `json:"failing,omitempty"`
}

// ProofBundle is the content-addressed evidence package.
// Custom MarshalJSON ensures slice fields never emit `null`.
type ProofBundle struct {
	BundleID         string           `json:"bundle_id"`
	CreatedAt        time.Time        `json:"created_at"`
	Subject          Subject          `json:"subject"`
	Profile          ProjectProfile   `json:"profile"`
	Evidence         []Evidence       `json:"evidence"`
	Inferences       []Inference      `json:"inferences"`
	Verdict          GlobalVerdict    `json:"verdict"`
	BaselineSummary  *BaselineSummary `json:"baseline_summary,omitempty"`
	UnverifiedClaims []string         `json:"unverified_claims,omitempty"`
	VouchVersion     string           `json:"vouch_version"`
	SchemaVersion    int              `json:"schema_version"`
}

// MarshalJSON ensures nil slices encode as [] and populates vouch_version if empty.
func (b ProofBundle) MarshalJSON() ([]byte, error) {
	type Alias ProofBundle
	aux := struct {
		Alias
	}{
		Alias: Alias(b),
	}
	if aux.Evidence == nil {
		aux.Evidence = []Evidence{}
	}
	if aux.Inferences == nil {
		aux.Inferences = []Inference{}
	}
	// UnverifiedClaims keeps omitempty: nil omitted is valid per schema.
	if aux.Profile.Gaps == nil {
		aux.Profile.Gaps = []string{}
	}
	if aux.Profile.Language == nil {
		aux.Profile.Language = []string{}
	}
	if aux.Profile.Commands == nil {
		aux.Profile.Commands = map[string]Command{}
	}
	if aux.VouchVersion == "" {
		aux.VouchVersion = VouchVersion()
	}
	return json.Marshal(aux.Alias)
}

// NewProofBundle is the single construction path that enforces P0 invariants:
//   - Verdict is derived from evidence via verdict.Aggregate (caller passes it)
//   - VERIFIED requires BaselineSummary (v2)
//   - Nil slices are normalized before Validate
//
// Use this instead of struct literals in production paths (tests may still use literals).
func NewProofBundle(bundleID string, subject Subject, profile ProjectProfile, evidences []Evidence, inferences []Inference, verdict GlobalVerdict, baselineSummary *BaselineSummary) ProofBundle {
	if evidences == nil {
		evidences = []Evidence{}
	}
	if inferences == nil {
		inferences = []Inference{}
	}
	if profile.Gaps == nil {
		profile.Gaps = []string{}
	}
	if profile.Language == nil {
		profile.Language = []string{}
	}
	if profile.Commands == nil {
		profile.Commands = map[string]Command{}
	}
	vv := VouchVersion()
	return ProofBundle{
		BundleID:        bundleID,
		CreatedAt:       time.Now().UTC(),
		Subject:         subject,
		Profile:         profile,
		Evidence:        evidences,
		Inferences:      inferences,
		Verdict:         verdict,
		BaselineSummary: baselineSummary,
		VouchVersion:    vv,
		SchemaVersion:   SchemaVersion,
	}
}

// Validate checks P0 invariants before storage.
func (e Evidence) Validate() error {
	if !reEvidenceID.MatchString(e.ID) {
		return fmt.Errorf("evidence id must match %s, got %q", reEvidenceID.String(), e.ID)
	}
	if e.Kind != KindDeterministic {
		return fmt.Errorf("evidence kind must be %q, got %q", KindDeterministic, e.Kind)
	}
	if e.Probe == "" {
		return fmt.Errorf("evidence probe required")
	}
	if e.Reproduce == "" {
		return fmt.Errorf("evidence %s: reproduce is required", e.ID)
	}
	if len(e.Reproduce) < len("vouch rerun ") || e.Reproduce[:len("vouch rerun ")] != "vouch rerun " {
		return fmt.Errorf("evidence %s: reproduce must start with %q", e.ID, "vouch rerun ")
	}
	switch e.Verdict {
	case EvidencePass, EvidenceFail, EvidenceInconclusive:
	default:
		return fmt.Errorf("evidence %s: invalid verdict %q", e.ID, e.Verdict)
	}
	if e.EnvFingerprint == "" {
		return fmt.Errorf("evidence %s: env_fingerprint required", e.ID)
	}
	if !reEnvFingerprint.MatchString(e.EnvFingerprint) {
		return fmt.Errorf("evidence %s: env_fingerprint must match %s, got %q", e.ID, reEnvFingerprint.String(), e.EnvFingerprint)
	}
	if e.Claim == "" {
		return fmt.Errorf("evidence %s: claim required", e.ID)
	}
	if e.Method == "" {
		return fmt.Errorf("evidence %s: method required", e.ID)
	}
	if e.StartedAt.IsZero() {
		return fmt.Errorf("evidence %s: started_at required", e.ID)
	}
	switch e.Baseline.Status {
	case BaselineAllGreen, BaselineExistingFailures, BaselineExistingBroken:
	default:
		return fmt.Errorf("evidence %s: baseline.status must be one of all_green/existing_failures/existing_broken, got %q", e.ID, e.Baseline.Status)
	}
	if e.Baseline.Pass < 0 || e.Baseline.Fail < 0 || e.Baseline.Skip < 0 || e.Baseline.DurationMs < 0 {
		return fmt.Errorf("evidence %s: baseline counts/duration must be >=0", e.ID)
	}
	if e.Candidate.Pass < 0 || e.Candidate.Fail < 0 || e.Candidate.Skip < 0 || e.Candidate.DurationMs < 0 {
		return fmt.Errorf("evidence %s: candidate counts/duration must be >=0", e.ID)
	}
	// Differ invariant: regressions imply fail. We don't hard-fail here (defense in depth),
	return nil
}

// Validate checks global invariants. Empty deterministic set must not be VERIFIED.
func (b ProofBundle) Validate() error {
	if !reBundleID.MatchString(b.BundleID) {
		return fmt.Errorf("bundle_id must match %s, got %q", reBundleID.String(), b.BundleID)
	}
	if b.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %d, got %d", SchemaVersion, b.SchemaVersion)
	}
	if b.Subject.BaseRef == "" {
		return fmt.Errorf("subject.base_ref required")
	}
	if b.Subject.RepoRoot == "" {
		return fmt.Errorf("subject.repo_root required")
	}
	if !reDiffSHA256.MatchString(b.Subject.DiffSHA256) {
		return fmt.Errorf("subject.diff_sha256 must match %s, got %q", reDiffSHA256.String(), b.Subject.DiffSHA256)
	}
	switch b.Profile.Confidence {
	case ConfidenceHigh, ConfidenceMedium, ConfidenceLow:
	default:
		return fmt.Errorf("profile.confidence must be one of high/medium/low, got %q", b.Profile.Confidence)
	}
	// Language/Commands nil is normalized to []/{} by MarshalJSON/NewProofBundle;
	// Validate treats nil as empty (valid) to keep literal and New* paths consistent
	// (schema requires array/object, not null, but Go nil is normalized, not rejected).
	if b.Verdict == Verified && len(b.Evidence) == 0 {
		return fmt.Errorf("VERIFIED with empty evidence is forbidden (empty set must be UNVERIFIED)")
	}
	for _, ev := range b.Evidence {
		if err := ev.Validate(); err != nil {
			return err
		}
		if ev.Kind != KindDeterministic {
			return fmt.Errorf("bundle evidence must be deterministic, got %q", ev.Kind)
		}
	}
	for _, inf := range b.Inferences {
		if inf.Kind != KindInferred {
			return fmt.Errorf("inference kind must be %q, got %q", KindInferred, inf.Kind)
		}
		if len(inf.ID) < 4 || inf.ID[:4] != "inf_" {
			return fmt.Errorf("inference id must have prefix %q, got %q", "inf_", inf.ID)
		}
		if inf.Probe == "" {
			return fmt.Errorf("inference probe required")
		}
		if inf.Msg == "" {
			return fmt.Errorf("inference msg required")
		}
	}
	switch b.Verdict {
	case Verified, Broken, Unverified:
	default:
		return fmt.Errorf("invalid global verdict %q", b.Verdict)
	}
	if b.Verdict == Verified && b.BaselineSummary == nil {
		return fmt.Errorf("VERIFIED must include baseline_summary")
	}
	if b.BaselineSummary != nil {
		switch b.BaselineSummary.Status {
		case BaselineAllGreen, BaselineExistingFailures, BaselineExistingBroken:
		default:
			return fmt.Errorf("baseline_summary.status must be one of all_green/existing_failures/existing_broken, got %q", b.BaselineSummary.Status)
		}
		if b.BaselineSummary.Failing < 0 {
			return fmt.Errorf("baseline_summary.failing must be >=0")
		}
		// Cross-check: BaselineSummary must not claim all_green when any *measured*
		// evidence baseline is non-green. Inconclusive evidence is unmeasured: the
		// summary intentionally does not aggregate it (scheduler.baselineSummary),
		// so it must not be treated as a contradicting measurement here either.
		if b.BaselineSummary.Status == BaselineAllGreen {
			for _, ev := range b.Evidence {
				if ev.Verdict == EvidenceInconclusive {
					continue
				}
				if ev.Baseline.Status != BaselineAllGreen {
					return fmt.Errorf("baseline_summary all_green conflicts with evidence %s baseline %q", ev.ID, ev.Baseline.Status)
				}
			}
		}
	}
	// Consistency with evidence: verdict must match deterministic evidence.
	// Keep in sync with verdict.Aggregate: fail→BROKEN, inconclusive→UNVERIFIED, all pass→VERIFIED.
	// Unknown verdicts are normalized to inconclusive by probe.Host, so Bundle only sees
	// pass/fail/inconclusive and no default is needed; Evidence.Validate still rejects unknown
	// for manually constructed bundles (defense in depth).
	if len(b.Evidence) == 0 {
		if b.Verdict != Unverified {
			return fmt.Errorf("empty evidence must be UNVERIFIED, got %q", b.Verdict)
		}
	} else {
		hasFail, hasInconclusive := false, false
		for _, ev := range b.Evidence {
			switch ev.Verdict {
			case EvidenceFail:
				hasFail = true
			case EvidenceInconclusive:
				hasInconclusive = true
			case EvidencePass:
				// ok — pass contributes nothing unless regressions present (below)
			}
			// Defensive: pass with regressions is illegal; treat as inconclusive for consistency.
			if ev.Verdict == EvidencePass && len(ev.Delta.Regressions) > 0 {
				hasInconclusive = true
			}
		}
		switch {
		case hasFail && b.Verdict != Broken:
			return fmt.Errorf("evidence has fail but bundle verdict is %q, want BROKEN", b.Verdict)
		case !hasFail && hasInconclusive && b.Verdict != Unverified:
			return fmt.Errorf("evidence has inconclusive but bundle verdict is %q, want UNVERIFIED", b.Verdict)
		case !hasFail && !hasInconclusive && b.Verdict != Verified:
			return fmt.Errorf("all evidence pass but bundle verdict is %q, want VERIFIED", b.Verdict)
		}
	}
	return nil
}

// DiffSHA256 computes the content hash for diff/patch bytes.
func DiffSHA256(diff []byte) string {
	h := sha256.Sum256(diff)
	return "sha256:" + hex.EncodeToString(h[:])
}

// EnvFingerprint is a helper for cache keying — hash of relevant env + profile.
func EnvFingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
