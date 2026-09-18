package probe

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
)

// Runner is the execution path for one probe. The kernel (scheduler) treats
// external and built-in probes identically; only the transport differs:
//
//   - HostRunner wraps an external executable speaking Probe Protocol v1 over stdio.
//   - builtin runners implement the same shape in-process (no exec, no Path).
//
// This closes the gap where probe-protocol.md promised built-ins "via
// in-process host" while Host itself could only spawn external files.
type Runner interface {
	// Name is the probe name, e.g. "test".
	Name() string
	// Capabilities declares the static defaults (builtins know theirs up front).
	Capabilities() Capabilities
	// Run executes the probe once and returns the result *plus the capabilities
	// the probe declared at initialize time*. The kernel must route on the
	// returned value: inferred probes never enter the gate, and a runner may not
	// silently upgrade them.
	Run(ctx context.Context, params RunParams) (Invocation, error)
}

// HostRunner adapts an external Host to Runner.
type HostRunner struct {
	Host  Host
	Name_ string
}

func (h HostRunner) Name() string { return h.Name_ }

// Capabilities for an external probe is unknown until it answers `initialize`:
// returning a deterministic default here would be a lie the gate could act on.
// Callers MUST route on the Invocation returned by Run.
func (h HostRunner) Capabilities() Capabilities {
	return Capabilities{}
}

// GateEligible reports whether a probe's declared capabilities allow its
// results to participate in the verdict gate. Only deterministic probes may;
// inferred probes are attachments, never evidence.
func GateEligible(caps Capabilities) bool {
	return caps.Kind == KindDeterministic
}

func (h HostRunner) Run(ctx context.Context, params RunParams) (Invocation, error) {
	return h.Host.Invoke(ctx, params), nil
}

// ProfileFromParams re-hydrates the typed ProjectProfile that the scheduler
// passes through RunParams.Profile (map form on the wire).
func ProfileFromParams(params RunParams) (bundle.ProjectProfile, error) {
	if params.Profile == nil {
		return bundle.ProjectProfile{}, nil
	}
	data, err := json.Marshal(params.Profile)
	if err != nil {
		return bundle.ProjectProfile{}, err
	}
	var p bundle.ProjectProfile
	if err := json.Unmarshal(data, &p); err != nil {
		return bundle.ProjectProfile{}, err
	}
	return p, nil
}

// Inconclusive builds a well-formed inconclusive result (probes never guess).
func Inconclusive(reason string, role string) RunResult {
	return RunResult{
		Verdict: VerdictInconclusive,
		Summary: "inconclusive",
		Reason:  reason,
		Data:    map[string]interface{}{"role": role},
	}
}

// EnsureHostRunnerKeepsType is a compile-time guard for the adapter contract.
var _ Runner = HostRunner{}

// String is a small helper for probe ids in evidence.
func String(p, role string) string { return fmt.Sprintf("%s/%s", p, role) }
