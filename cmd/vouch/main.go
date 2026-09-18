// Command vouch — single static binary for the VOUCH differential certifier.
//
// Implemented: verify (also the default command and `show`/`rerun`/`gc`),
// init (agent wiring), hook (completion-hook adapter), mcp (agent protocol
// server). Remaining stubs are `export` and the `probe` subcommands; they return
// ExitError with code 2 (UNVERIFIED) so they stay testable through
// newRootCmd().Execute() without killing the test process.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/render"
	"github.com/junqingyongyuanbusi/vouch/internal/scheduler"
	"github.com/junqingyongyuanbusi/vouch/internal/verdict"
	"github.com/junqingyongyuanbusi/vouch/internal/version"
	"github.com/junqingyongyuanbusi/vouch/internal/worktree"
)

// verifyOptions holds the flags shared by the root command and `verify`.
type verifyOptions struct {
	from        string
	to          string
	path        string
	intent      string
	output      string
	jsonOut     bool
	ci          bool
	noLLM       bool
	explainGaps bool
	installDeps bool
}

// ExitError carries a process exit code for main() to use.
// Cobra stubs return this instead of calling os.Exit directly.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit %d", e.Code)
}

func main() {
	cmd := newRootCmd()
	if err := cmd.Execute(); err != nil {
		var ee *ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.Code)
		}
		fmt.Fprintln(os.Stderr, err)
		// Unknown errors (flag parse, I/O) are UNVERIFIED (2), not BROKEN (1),
		// so CI can distinguish regression from tool crash (P0/P1 audit).
		os.Exit(2)
	}
}

func newRootCmd() *cobra.Command {
	rootOpts := verifyOptions{}
	root := &cobra.Command{
		Use:   "vouch",
		Short: "VOUCH — Validation Of Untrusted Code Hypotheses",
		Long: "vouch — husky for agent hooks, with differential evidence.\n\n" +
			"Available: vouch [verify] · vouch init <agent> · vouch show · vouch version\n\n" +
			"Agents drive vouch through the completion hook and the agent protocol server\n" +
			"that `vouch init` writes into the repository (see docs/agent-loop.md).",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// `vouch` with no subcommand is verify (the plan's minimal surface).
			return runVerify(cmd, rootOpts)
		},
	}
	addVerifyFlags(root, &rootOpts)

	root.AddCommand(newVersionCmd())
	root.AddCommand(newHookCmd())
	root.AddCommand(newShowCmd())
	root.AddCommand(newVerifyCmd())
	root.AddCommand(newMcpCmd())
	root.AddCommand(newRerunCmd())
	root.AddCommand(newGCCmd())
	root.AddCommand(newInitCmd())
	root.AddCommand(newExportCmd())
	root.AddCommand(newProbeCmd())

	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print vouch version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("vouch %s (%s)\n", version.Version, version.Commit)
		},
	}
}

func newVerifyCmd() *cobra.Command {
	opts := verifyOptions{}
	c := &cobra.Command{
		Use:   "verify",
		Short: "Verify a change and print the three-state verdict",
		Long: "Runs detector → selector → base/candidate worktrees → built-in probes\n" +
			"→ differential compare, writes .vouch/bundles/<id>/, and prints the verdict.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(cmd, opts)
		},
	}
	addVerifyFlags(c, &opts)
	return c
}

func addVerifyFlags(c *cobra.Command, opts *verifyOptions) {
	c.Flags().StringVar(&opts.from, "from", "", "base ref (default HEAD)")
	c.Flags().StringVar(&opts.to, "to", "", "candidate ref (default: working tree snapshot, else HEAD)")
	c.Flags().StringVar(&opts.path, "path", "", "repository path (default .)")
	c.Flags().StringVar(&opts.intent, "intent", "", "optional intent text for the bundle subject")
	c.Flags().StringVar(&opts.output, "output", "", "write JSON bundle to this file")
	c.Flags().BoolVar(&opts.jsonOut, "json", false, "machine-readable JSON output")
	c.Flags().BoolVar(&opts.ci, "ci", false, "accepted for compatibility: the exit code is always the verdict (0/1/2)")
	c.Flags().BoolVar(&opts.noLLM, "no-llm", true, "accepted for compatibility: P1 is fully deterministic (no LLM in the gate)")
	c.Flags().BoolVar(&opts.installDeps, "install-deps", false, "opt in to installing missing JS dependencies (network)")
	c.Flags().BoolVar(&opts.explainGaps, "explain-gaps", false, "print detector provenance and unverified claims")
}

func runVerify(cmd *cobra.Command, opts verifyOptions) error {
	repoRoot := opts.path
	if repoRoot == "" {
		repoRoot = "."
	}
	baseRef := opts.from
	if baseRef == "" {
		baseRef = "HEAD"
	}
	res, err := scheduler.Run(cmd.Context(), scheduler.Config{
		RepoRoot:     repoRoot,
		BaseRef:      baseRef,
		CandidateRef: opts.to,
		Intent:       opts.intent,
		InstallDeps:  opts.installDeps,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "vouch verify: %v\n", err)
		// Tool failure is "cannot verify", never "broken".
		return &ExitError{Code: verdict.ExitCode(bundle.Unverified), Err: err}
	}

	if opts.jsonOut {
		data, err := render.JSON(res)
		if err != nil {
			return &ExitError{Code: verdict.ExitCode(bundle.Unverified), Err: err}
		}
		if opts.output != "" {
			if err := os.WriteFile(opts.output, data, 0o644); err != nil {
				return &ExitError{Code: verdict.ExitCode(bundle.Unverified), Err: err}
			}
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), string(data))
		}
	} else {
		fmt.Fprint(cmd.OutOrStdout(), render.Human(res))
	}
	if opts.explainGaps {
		fmt.Fprint(cmd.OutOrStdout(), render.Gaps(res))
	}
	if opts.output != "" && !opts.jsonOut {
		data, _ := render.JSON(res)
		if err := os.WriteFile(opts.output, data, 0o644); err != nil {
			return &ExitError{Code: verdict.ExitCode(bundle.Unverified), Err: err}
		}
	}
	return &ExitError{Code: verdict.ExitCode(res.Verdict)}
}

func newShowCmd() *cobra.Command {
	var jsonOut bool
	var mdOut bool
	var showPath string
	c := &cobra.Command{
		Use:   "show [bundle-id]",
		Short: "Show a stored evidence bundle (latest when no id is given)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := showPath
			if root == "" {
				root = "."
			}
			store := bundle.NewStore(root)
			id := ""
			if len(args) == 1 {
				id = args[0]
			} else {
				latest, ok, err := store.Latest()
				if err != nil {
					return &ExitError{Code: 2, Err: err}
				}
				if !ok {
					fmt.Fprintln(os.Stderr, "vouch show: no bundles stored in .vouch/bundles")
					return &ExitError{Code: 2}
				}
				id = latest.BundleID
			}
			b, err := store.Load(id)
			if err != nil {
				fmt.Fprintf(os.Stderr, "vouch show: %v\n", err)
				return &ExitError{Code: 2, Err: err}
			}
			if jsonOut {
				data, _ := json.MarshalIndent(b, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(data))
				return nil
			}
			if mdOut {
				fmt.Fprint(cmd.OutOrStdout(), render.Markdown(b))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s  bundle=%s  base=%s\n", b.Verdict, b.BundleID, b.Subject.BaseRef)
			if b.BaselineSummary != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "  base: %s (failing=%d)\n", b.BaselineSummary.Status, b.BaselineSummary.Failing)
			}
			for _, ev := range b.Evidence {
				fmt.Fprintf(cmd.OutOrStdout(), "  %-9s %s  %s\n", ev.Probe, ev.Verdict, ev.Reproduce)
				for _, f := range ev.Failures {
					fmt.Fprintf(cmd.OutOrStdout(), "      · %s: %s\n", f.ID, f.Message)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	c.Flags().BoolVar(&mdOut, "md", false, "PR-comment markdown output")
	c.Flags().StringVar(&showPath, "path", "", "repository path (default .)")
	return c
}

func newRerunCmd() *cobra.Command {
	var repoPath, probeName string
	c := &cobra.Command{
		Use:    "rerun <bundle-id>",
		Hidden: true,
		Short:  "Re-execute a stored bundle's probe on the same base/candidate commits",
		Long: "Recreates the base and candidate worktrees from the bundle's subject refs and\n" +
			"runs the recorded probe command, so a stored verdict can be reproduced.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := repoPath
			if root == "" {
				root = "."
			}
			store := bundle.NewStore(root)
			b, err := store.Load(args[0])
			if err != nil {
				fmt.Fprintf(os.Stderr, "vouch rerun: %v\n", err)
				return &ExitError{Code: verdict.ExitCode(bundle.Unverified), Err: err}
			}
			ev, err := pickEvidence(b, probeName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "vouch rerun: %v\n", err)
				return &ExitError{Code: verdict.ExitCode(bundle.Unverified), Err: err}
			}
			if b.Subject.CandidateRef == nil {
				err := fmt.Errorf("bundle %s has no candidate ref to reproduce", b.BundleID)
				fmt.Fprintf(os.Stderr, "vouch rerun: %v\n", err)
				return &ExitError{Code: verdict.ExitCode(bundle.Unverified), Err: err}
			}
			command := commandFromMethod(ev.Method, b.Profile.Commands[ev.Probe].Cmd)
			rc := scheduler.RerunConfig{
				RepoRoot:     root,
				BaseRef:      b.Subject.BaseRef,
				CandidateRef: *b.Subject.CandidateRef,
				Probe:        ev.Probe,
				Command:      command,
			}
			res, err := scheduler.Rerun(cmd.Context(), rc)
			if err != nil {
				fmt.Fprintf(os.Stderr, "vouch rerun: %v\n", err)
				return &ExitError{Code: verdict.ExitCode(bundle.Unverified), Err: err}
			}
			fmt.Fprint(cmd.OutOrStdout(), render.Rerun(res, b.BundleID))
			return &ExitError{Code: verdict.ExitCode(res.Verdict)}
		},
	}
	c.Flags().StringVar(&repoPath, "path", "", "repository path (default .)")
	c.Flags().StringVar(&probeName, "probe", "", "probe to re-run (default: first evidence)")
	return c
}

func pickEvidence(b bundle.ProofBundle, probe string) (bundle.Evidence, error) {
	if probe == "" {
		if len(b.Evidence) == 0 {
			return bundle.Evidence{}, fmt.Errorf("bundle %s has no evidence", b.BundleID)
		}
		return b.Evidence[0], nil
	}
	for _, ev := range b.Evidence {
		if ev.Probe == probe {
			return ev, nil
		}
	}
	return bundle.Evidence{}, fmt.Errorf("bundle %s has no evidence for probe %q", b.BundleID, probe)
}

// commandFromMethod recovers the executed command recorded at verify time.
// Format (scheduler): "<probe>: <cmd>" or "<probe>: base=<cmd> | candidate=<cmd>".
func commandFromMethod(method, fallback string) string {
	if i := strings.Index(method, "candidate="); i >= 0 {
		return strings.TrimSpace(method[i+len("candidate="):])
	}
	if i := strings.Index(method, ": "); i >= 0 {
		cmd := strings.TrimSpace(method[i+2:])
		if cmd != "" && !strings.Contains(cmd, "base=") {
			return cmd
		}
	}
	return fallback
}
func newGCCmd() *cobra.Command {
	var repoPath string
	var keep int
	c := &cobra.Command{
		Use:    "gc",
		Hidden: true,
		Short:  "Garbage-collect old bundles, worktrees and snapshot refs",
		RunE: func(cmd *cobra.Command, args []string) error {
			root := repoPath
			if root == "" {
				root = "."
			}
			removedBundles, removedRefs, err := gcRun(root, keep)
			if err != nil {
				fmt.Fprintf(os.Stderr, "vouch gc: %v\n", err)
				return &ExitError{Code: verdict.ExitCode(bundle.Unverified), Err: err}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "gc: removed %d bundle(s), %d snapshot ref(s)\n", removedBundles, removedRefs)
			return nil
		},
	}
	c.Flags().StringVar(&repoPath, "path", "", "repository path (default .)")
	c.Flags().IntVar(&keep, "keep", 50, "number of most recent bundles to keep")
	return c
}

// gcRun removes bundles beyond `keep` (oldest first) and any snapshot ref that
// is no longer referenced by a surviving bundle.
func gcRun(root string, keep int) (bundles, refs int, err error) {
	store := bundle.NewStore(root)
	metas, err := store.List()
	if err != nil {
		return 0, 0, err
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].SavedAt.After(metas[j].SavedAt) })
	kept := map[string]bool{}
	for i, m := range metas {
		if i < keep {
			kept[m.BundleID] = true
		}
	}
	for i, m := range metas {
		if i < keep {
			continue
		}
		dir := filepath.Join(root, ".vouch", "bundles", m.BundleID)
		if err := os.RemoveAll(dir); err == nil {
			bundles++
		}
	}
	// Snapshot refs: keep those referenced by a kept bundle.
	listRefs := exec.Command("git", "for-each-ref", "--format=%(refname)", worktree.SnapshotRefPrefix)
	listRefs.Dir = root
	out, listErr := listRefs.Output()
	if listErr != nil {
		return bundles, 0, nil // not a git repo: bundles already handled
	}
	referenced := map[string]bool{}
	for _, m := range metas {
		if !kept[m.BundleID] {
			continue
		}
		if b, err := store.Load(m.BundleID); err == nil && b.Subject.CandidateRef != nil {
			referenced[*b.Subject.CandidateRef] = true
		}
	}
	cmdDir := exec.Command("git", "rev-parse", "--show-toplevel")
	cmdDir.Dir = root
	if top, err := cmdDir.Output(); err == nil {
		repoRoot := strings.TrimSpace(string(top))
		for _, line := range strings.Split(string(out), "\n") {
			ref := strings.TrimSpace(line)
			if ref == "" || referenced[ref] {
				continue
			}
			if err := worktree.DeleteSnapshotRef(repoRoot, ref); err == nil {
				refs++
			}
		}
	}
	return bundles, refs, nil
}

func newInitCmd() *cobra.Command {
	var repoPath string
	c := &cobra.Command{
		Use:   "init <agent>",
		Short: "Wire vouch into an agent: MCP server + completion hook (P1: claude-code)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := repoPath
			if root == "" {
				root = "."
			}
			switch args[0] {
			case "claude-code":
				rep, err := initClaudeCode(root)
				if err != nil {
					fmt.Fprintf(os.Stderr, "vouch init: %v\n", err)
					return &ExitError{Code: verdict.ExitCode(bundle.Unverified), Err: err}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ .mcp.json MCP server: %s\n", rep.MCP)
				fmt.Fprintf(cmd.OutOrStdout(), "✓ .claude/settings.json Stop hook: %s\n", rep.Hook)
				fmt.Fprintln(cmd.OutOrStdout(), "  Claude Code can now call vouch_verify while it works, and vouch runs when it claims to be done.")
				fmt.Fprintln(cmd.OutOrStdout(), "  BROKEN blocks the agent (exit 2 + evidence on stderr); UNVERIFIED only reports the reason.")
				if rep.Warn != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "⚠ %s\n", rep.Warn)
				}
				return nil
			default:
				err := fmt.Errorf("unsupported agent %q (P1 supports: claude-code)", args[0])
				fmt.Fprintf(os.Stderr, "vouch init: %v\n", err)
				return &ExitError{Code: verdict.ExitCode(bundle.Unverified), Err: err}
			}
		},
	}
	c.Flags().StringVar(&repoPath, "path", "", "repository path (default .)")
	return c
}

func newExportCmd() *cobra.Command {
	c := &cobra.Command{
		Use:    "export",
		Hidden: true,
		Short:  "Export bundle to markdown for PRs (P2 — stub)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stderr, "vouch export: P2 — PR export not yet implemented.")
			return &ExitError{Code: verdict.ExitCode(bundle.Unverified)}
		},
	}
	c.Flags().String("bundle", "", "bundle id to export")
	return c
}

func newProbeCmd() *cobra.Command {
	probe := &cobra.Command{
		Use:    "probe",
		Hidden: true,
		Short:  "Probe utilities",
	}
	probe.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List discovered probes (P1 — stub)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stderr, "vouch probe list: P1 — discovery not yet implemented.")
			return &ExitError{Code: verdict.ExitCode(bundle.Unverified)}
		},
	})
	probe.AddCommand(&cobra.Command{
		Use:   "scaffold <name>",
		Short: "Scaffold a new probe (P3 — stub)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stderr, "vouch probe scaffold: P3 — scaffold not yet implemented.")
			return &ExitError{Code: verdict.ExitCode(bundle.Unverified)}
		},
	})
	return probe
}
