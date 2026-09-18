package probe

// Protocol-level readers for the data shapes defined in docs/probe-protocol.md §6.
// They live here (not in builtin) so any consumer — differ, third-party probes,
// future TUI — can interpret results without importing an implementation.

// TestFacts is the per-case test outcome carried in RunResult.Data.
type TestFacts struct {
	Passed     []string  `json:"passed"`
	Failed     []string  `json:"failed"`
	Skipped    []string  `json:"skipped"`
	DurationMs int       `json:"duration_ms"`
	Failures   []Failure `json:"failures,omitempty"`
}

// Failure is a short structured failure message.
type Failure struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// Diagnostic is one structured typecheck finding.
type Diagnostic struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// TestFactsOf extracts per-case facts from a probe result (empty when absent).
func TestFactsOf(r RunResult) TestFacts {
	f := TestFacts{Passed: []string{}, Failed: []string{}, Skipped: []string{}}
	if r.Data == nil {
		return f
	}
	f.Passed = StringList(r.Data["passed"])
	f.Failed = StringList(r.Data["failed"])
	f.Skipped = StringList(r.Data["skipped"])
	switch v := r.Data["duration_ms"].(type) {
	case float64:
		f.DurationMs = int(v)
	case int:
		f.DurationMs = v
	}
	if raw, ok := r.Data["failures"]; ok {
		if bs, err := marshalJSON(raw); err == nil {
			_ = unmarshalJSON(bs, &f.Failures)
		}
	}
	return f
}

// DiagnosticsOf extracts structured diagnostics (empty when absent).
func DiagnosticsOf(r RunResult) []Diagnostic {
	out := []Diagnostic{}
	if r.Data == nil {
		return out
	}
	raw, ok := r.Data["diagnostics"]
	if !ok {
		return out
	}
	if bs, err := marshalJSON(raw); err == nil {
		_ = unmarshalJSON(bs, &out)
	}
	return out
}

// StringList coerces a JSON-decoded list into []string.
func StringList(v interface{}) []string {
	out := []string{}
	switch xs := v.(type) {
	case []string:
		out = append(out, xs...)
	case []interface{}:
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}
