package builtin

import (
	"context"
	"regexp"
	"strings"

	"github.com/junqingyongyuanbusi/vouch/internal/probe"
)

// Typecheck is the built-in typecheck probe. TypeScript diagnostics are parsed
// into a structured list; ecosystems without a supported diagnostics format
// report inconclusive (never a silent pass).
type Typecheck struct{}

func (Typecheck) Name() string     { return "typecheck" }
func (Typecheck) Kind() probe.Kind { return probe.KindDeterministic }

// tscRe matches `path(line,col): error TS1234: message`.
var tscRe = regexp.MustCompile(`^(.+?)\((\d+),(\d+)\):\s+(error|warning)\s+(TS\d+):\s+(.*)$`)

// Diagnostic is one structured typecheck finding.
type Diagnostic struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (Typecheck) Run(ctx context.Context, in Input) (probe.RunResult, error) {
	if in.Command == "" {
		return probe.Inconclusive("no typecheck command in profile", in.Role), nil
	}
	out, exit, timedOut, err := runShell(ctx, in)
	if err != nil {
		return probe.Inconclusive("run: "+err.Error(), in.Role), nil
	}
	if timedOut {
		return probe.Inconclusive("command killed by budget", in.Role), nil
	}

	diags := ParseTSC(out)
	if len(diags) == 0 && missingRunner(out, exit) {
		return probe.Inconclusive("runner not available ("+in.Command+")", in.Role), nil
	}
	isTS := strings.Contains(in.Command, "tsc") || strings.Contains(in.Command, "typescript")
	if len(diags) == 0 {
		if exit == 0 {
			data := map[string]interface{}{}
			data["diagnostics"] = []Diagnostic{}
			data["exit_code"] = 0
			return probe.RunResult{Verdict: probe.VerdictPass, Summary: "no diagnostics", Data: data}, nil
		}
		if !isTS {
			// Non-TypeScript typecheck output (e.g. cargo check): no parser yet.
			return probe.Inconclusive("no diagnostics parser for command "+in.Command, in.Role), nil
		}
		return probe.Inconclusive("tsc failed without parseable diagnostics", in.Role), nil
	}
	data := map[string]interface{}{}
	data["diagnostics"] = diags
	data["exit_code"] = exit
	return probe.RunResult{
		Verdict: probe.VerdictFail,
		Summary: itoa(len(diags)) + " diagnostics",
		Data:    data,
	}, nil
}

// ParseTSC extracts `file(line,col): error TSxxxx: msg` diagnostics.
func ParseTSC(out string) []Diagnostic {
	var diags []Diagnostic
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		m := tscRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		diags = append(diags, Diagnostic{
			File:    m[1],
			Line:    atoi(m[2]),
			Column:  atoi(m[3]),
			Code:    m[5],
			Message: m[6],
		})
	}
	return diags
}

// DiagnosticsOf extracts diagnostics from a RunResult (protocol-layer reader).
func DiagnosticsOf(r probe.RunResult) []Diagnostic {
	out := []Diagnostic{}
	for _, d := range probe.DiagnosticsOf(r) {
		out = append(out, Diagnostic{File: d.File, Line: d.Line, Column: d.Column, Code: d.Code, Message: d.Message})
	}
	return out
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
