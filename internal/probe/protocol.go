// Package probe — JSON-lines over stdio host for Probe Protocol v1.
//
// The host spawns a probe executable, speaks initialize → run → shutdown,
// and isolates crashes/timeouts as inconclusive (never failing the whole verify).
// Built-in probes and third-party probes share this host (dependency inversion).
//
// Single source of truth: Kind/Verdict are aliases of bundle types so
// execution and evidence layers can never drift (P0/P1 audit).
package probe

import (
	"encoding/json"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
)

// ProtocolVersion is the current probe protocol version (see docs/probe-protocol.md).
const ProtocolVersion = 1

// Kind and verdicts are zero-cost aliases of bundle types.
type Kind = bundle.Kind

const (
	KindDeterministic = bundle.KindDeterministic
	KindInferred      = bundle.KindInferred
)

type Verdict = bundle.EvidenceVerdict

const (
	VerdictPass         = bundle.EvidencePass
	VerdictFail         = bundle.EvidenceFail
	VerdictInconclusive = bundle.EvidenceInconclusive
)

// Compile-time assertion: aliases never drift.
var (
	_ bundle.Kind            = KindDeterministic
	_ bundle.EvidenceVerdict = VerdictPass
)

// Capabilities declared by a probe in initialize.
type Capabilities struct {
	Differential      bool     `json:"differential"`
	Kind              Kind     `json:"kind"`
	NeedsNetwork      bool     `json:"needs_network,omitempty"`
	NeedsNetworkAllow []string `json:"needs_network_allow,omitempty"`
}

// InitializeResult is the probe's answer to initialize.
type InitializeResult struct {
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Capabilities Capabilities `json:"capabilities"`
}

// RunParams is the kernel → probe run payload.
type RunParams struct {
	Workdir        string                 `json:"workdir"`
	Role           string                 `json:"role"` // "base" | "candidate"
	Profile        map[string]interface{} `json:"profile"`
	BudgetMs       int                    `json:"budget_ms"`
	DiffFiles      []string               `json:"diff_files,omitempty"`
	BundleID       string                 `json:"bundle_id,omitempty"`
	EnvFingerprint string                 `json:"env_fingerprint,omitempty"`
	// Command overrides the profile's command for this invocation. The scheduler
	// sets the selector-applied command here so both base and candidate run the
	// exact same target set (differential validity).
	Command string `json:"command,omitempty"`
}

// RunResult is the probe → kernel result.
type RunResult struct {
	Verdict   Verdict                `json:"verdict"`
	Summary   string                 `json:"summary"`
	Reason    string                 `json:"reason,omitempty"`
	Data      map[string]interface{} `json:"data,omitempty"`
	Artifacts map[string]interface{} `json:"artifacts,omitempty"`
}

// Alias ensures single source; no conversion function needed.

// RPCRequest is a JSON-RPC 2.0 request over stdio (one line).
type RPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *int        `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// RPCResponse is a JSON-RPC 2.0 response (one line).
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
