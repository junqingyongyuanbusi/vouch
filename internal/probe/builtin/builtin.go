// Package builtin — P1.4 — built-in probes (build / test / typecheck).
//
// Built-ins are in-process (see probe.Runner): no Path, no exec-self double
// spawn. They share the same RunResult shape as external probes, so kernel,
// differ and bundle layers never branch on probe origin.
package builtin

import (
	"context"
	"strings"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/probe"
	"github.com/junqingyongyuanbusi/vouch/internal/sandbox"
)

// Input is the typed input for an in-process builtin probe.
type Input struct {
	Workdir      string
	Role         string // "base" | "candidate"
	Command      string // already selector-applied by the scheduler
	BudgetMs     int
	NeedsNetwork bool
}

// Runner is an in-process builtin probe.
type Runner interface {
	Name() string
	Kind() probe.Kind
	Run(ctx context.Context, in Input) (probe.RunResult, error)
}

// All returns the P1 built-in probe set.
func All() []Runner { return []Runner{Test{}, Build{}, Typecheck{}} }

// runShell executes the command under the sandbox policy and returns combined
// stdout+stderr (parsers tolerate log noise and extract the report).
func runShell(ctx context.Context, in Input) (out string, exit int, timedOut bool, err error) {
	timeout := 0 * time.Second
	if in.BudgetMs > 0 {
		timeout = time.Duration(in.BudgetMs) * time.Millisecond
	}
	res, err := sandbox.Run(ctx, in.Workdir, in.Command, sandbox.Policy{
		Timeout:      timeout,
		NeedsNetwork: in.NeedsNetwork,
	})
	if err != nil {
		return "", 0, false, err
	}
	out = res.Stdout
	if res.Stderr != "" {
		if out != "" {
			out += "\n"
		}
		out += res.Stderr
	}
	return out, res.ExitCode, res.TimedOut, nil
}

func dataOf(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return map[string]interface{}{}
	}
	return m
}

// missingRunner reports whether the command never started because the runner
// is absent (exit 126/127 or a "command not found" message). That is "cannot
// measure", not a failing test/build: reading it as a failure would both hide
// the prerequisite and let a missing runner count as an existing baseline
// failure (which diff semantics would then absorb as "no regression").
func missingRunner(out string, exit int) bool {
	// Only the launcher failing counts: shell exit 126/127, a shell "not found"
	// line, or the command's own binary missing from PATH. A program reporting
	// "No such file or directory" for one of ITS inputs is a real failure.
	if exit == 126 || exit == 127 {
		return true
	}
	first := ""
	for _, line := range strings.Split(out, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			first = strings.ToLower(t)
			break
		}
	}
	if first != "" {
		// Shell launcher wording only: "<sh>: <cmd>: not found". A program's own
		// "No such file or directory" for an input must stay a real failure.
		if strings.Contains(first, "command not found") {
			return true
		}
		if strings.Contains(first, ": not found") {
			return true
		}
	}
	return false
}
