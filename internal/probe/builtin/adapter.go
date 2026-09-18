package builtin

import (
	"context"

	"github.com/junqingyongyuanbusi/vouch/internal/probe"
)

// AsProbeRunner adapts an in-process builtin to probe.Runner so the scheduler
// can treat built-in and external probes identically.
func AsProbeRunner(r Runner) probe.Runner { return adapter{r: r} }

type adapter struct{ r Runner }

func (a adapter) Name() string { return a.r.Name() }

func (a adapter) Capabilities() probe.Capabilities {
	return probe.Capabilities{Differential: true, Kind: a.r.Kind()}
}

func (a adapter) Run(ctx context.Context, params probe.RunParams) (probe.Invocation, error) {
	caps := a.Capabilities()
	profile, err := probe.ProfileFromParams(params)
	if err != nil {
		return probe.Invocation{Result: probe.Inconclusive("profile decode: "+err.Error(), params.Role), Caps: caps}, nil
	}
	cmd := params.Command
	if cmd == "" {
		cmd = profile.Commands[a.r.Name()].Cmd
	}
	res, err := a.r.Run(ctx, Input{
		Workdir:  params.Workdir,
		Role:     params.Role,
		Command:  cmd,
		BudgetMs: params.BudgetMs,
	})
	return probe.Invocation{Result: res, Caps: caps}, err
}
