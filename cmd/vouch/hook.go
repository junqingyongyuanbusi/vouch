package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/gitexclude"
	"github.com/junqingyongyuanbusi/vouch/internal/render"
	"github.com/junqingyongyuanbusi/vouch/internal/scheduler"
	"github.com/spf13/cobra"
)

// agentHookPayload is the common part of the Stop-hook input an agent sends on
// stdin. Field names differ per agent; unknown fields are ignored.
type agentHookPayload struct {
	// Claude Code: true when the agent is already continuing because of a Stop
	// hook — re-blocking then would loop forever.
	StopHookActive bool `json:"stop_hook_active"`
	// Cursor (deferred adapter): how many automatic follow-ups this hook already
	// triggered.
	LoopCount int    `json:"loop_count"`
	Status    string `json:"status"`
	Cwd       string `json:"cwd"`
	SessionID string `json:"session_id"`
}

// hookOptions are shared by the per-agent hook adapters.
type hookOptions struct {
	path string
	from string
	to   string
}

// newHookCmd implements `vouch hook <agent>`: the program an agent's completion
// hook calls. The blocking contract differs per agent, so each adapter is a
// small, testable Go function instead of a shell one-liner (JSON escaping in sh
// is exactly the kind of thing that silently breaks a hook).
//
// PLAN-V2 §7.5: P1.5 ships ONE adapter (Claude Code) done properly; Cursor is
// deliberately deferred (see docs/agent-loop.md).
func newHookCmd() *cobra.Command {
	opts := hookOptions{}
	c := &cobra.Command{
		Use:    "hook <agent>",
		Hidden: true,
		Short:  "Agent completion hook entry point (P1: claude-code)",
		Long: "Reads the agent's hook payload on stdin, runs verify, and answers in the\n" +
			"protocol that agent expects. Never fails the agent on a vouch error.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			payload := readHookPayload(cmd.InOrStdin())
			switch args[0] {
			case "claude-code":
				return claudeCodeHook(cmd, opts, payload)
			default:
				fmt.Fprintf(cmd.ErrOrStderr(), "vouch hook: unsupported agent %q (P1 supports: claude-code)\n", args[0])
				// Unknown agent: never block, never fail the agent loop.
				return &ExitError{Code: 0}
			}
		},
	}
	c.Flags().StringVar(&opts.path, "path", "", "repository path (default .)")
	c.Flags().StringVar(&opts.from, "from", "", "base ref (default HEAD)")
	c.Flags().StringVar(&opts.to, "to", "", "candidate ref (default: working tree)")
	return c
}

// claudeCodeHook implements the Claude Code Stop-hook contract:
//
//	exit 0 → allow the agent to stop (success, or "could not verify")
//	exit 2 → block the stop and feed stderr back to the model
//
// Only BROKEN (exit 1 from verify) may block: a real regression must not be
// silently declared done. UNVERIFIED must NOT block (crying wolf on an honest
// "cannot measure" would train users to disable the hook), and a vouch failure
// must never block either.
func claudeCodeHook(cmd *cobra.Command, opts hookOptions, payload agentHookPayload) error {
	if payload.StopHookActive {
		// The agent is already continuing because of this hook; blocking again
		// would loop forever. Claude Code sets this exactly for that reason.
		return &ExitError{Code: 0}
	}
	res, runErr := hookVerify(cmd, opts)
	if runErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "vouch hook: %v\n", runErr)
		return &ExitError{Code: 0} // fail open: a broken verifier must not block
	}
	switch res.Verdict {
	case bundle.Broken:
		fmt.Fprint(cmd.ErrOrStderr(), render.AgentFeedback(res))
		return &ExitError{Code: 2}
	case bundle.Unverified:
		// Printed so the reason lands in the transcript; stdout does not block.
		fmt.Fprint(cmd.OutOrStdout(), render.AgentFeedback(res))
		return &ExitError{Code: 0}
	default:
		fmt.Fprint(cmd.OutOrStdout(), render.AgentFeedback(res))
		return &ExitError{Code: 0}
	}
}

// hookVerify runs the pipeline for a hook. The error is returned separately
// because "vouch could not run" and "the change is broken" must never be
// confused (they lead to opposite hook actions).
func hookVerify(cmd *cobra.Command, opts hookOptions) (scheduler.Result, error) {
	repoRoot := opts.path
	if repoRoot == "" {
		repoRoot = "."
	}
	return scheduler.Run(cmd.Context(), scheduler.Config{
		RepoRoot:     repoRoot,
		BaseRef:      orDefault(opts.from, "HEAD"),
		CandidateRef: opts.to,
		Intent:       "agent completion hook",
	})
}

// readHookPayload parses the agent's stdin JSON. A hook must tolerate an empty
// or unparsable payload (the agent, not vouch, owns that format).
func readHookPayload(r io.Reader) agentHookPayload {
	var p agentHookPayload
	if r == nil {
		return p
	}
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil || len(data) == 0 {
		return p
	}
	_ = json.Unmarshal(data, &p)
	return p
}

// initClaudeCode registers vouch as an MCP server and installs the Stop hook.
// Both halves are needed for the loop: the MCP server is how the agent asks
// vouch to verify while it works, the Stop hook is what runs when it claims to
// be done. Running it twice is a no-op.
func initClaudeCode(root string) (initReport, error) {
	var rep initReport
	// Resolve the launcher before writing anything: a half-written config that
	// points at a missing binary is worse than a failed init.
	resolved, err := resolveLauncher()
	if err != nil {
		return rep, err
	}
	rep.Warn = resolved.warn
	repoDir := filepath.Join(root, ".claude")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return rep, err
	}
	_ = gitexclude.Add(root, ".claude/")

	settingsPath := filepath.Join(repoDir, "settings.json")
	settings := map[string]any{}
	if data, err := os.ReadFile(settingsPath); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &settings); err != nil {
			return rep, fmt.Errorf("%s is not valid JSON: %w", settingsPath, err)
		}
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	stop, _ := hooks["Stop"].([]any)
	// Exit-code contract translation: verify uses 1 = BROKEN (must block) and
	// 2 = UNVERIFIED (must not block), while Claude Code's Stop hook treats 2 as
	// "block and feed stderr back to the model". `vouch hook claude-code`
	// performs that mapping in Go.
	//
	// The hook and the MCP registration must use the SAME launcher: on a machine
	// without `vouch` on PATH, a hardcoded "vouch ..." hook would die with
	// "command not found" (exit 127 → silently non-blocking) while the MCP server
	// worked, silently disconnecting half of the loop.
	hookEntry := map[string]any{
		"matcher": "",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": resolved.command("hook", "claude-code"),
				// Verify can legitimately take minutes on a cold cache; the
				// default 600s is kept explicit so a slow repo is not cut off.
				"timeout": 900,
			},
		},
	}
	if stopHookWired(stop) {
		rep.Hook = "already present"
	} else {
		replaced := len(dropLegacyHooks(stop)) != len(stop)
		hooks["Stop"] = append(dropLegacyHooks(stop), hookEntry)
		settings["hooks"] = hooks
		if err := writeJSONFile(settingsPath, settings); err != nil {
			return rep, err
		}
		if replaced {
			rep.Hook = "written (replaced the pre-P1.5 shell hook)"
		} else {
			rep.Hook = "written"
		}
	}

	// MCP registration: project-scoped .mcp.json is the file Claude Code reads
	// and the one meant to be committed with the repo.
	mcpPath := filepath.Join(root, ".mcp.json")
	mcpCfg := map[string]any{}
	if data, err := os.ReadFile(mcpPath); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &mcpCfg); err != nil {
			return rep, fmt.Errorf("%s is not valid JSON: %w", mcpPath, err)
		}
	}
	servers, _ := mcpCfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if existing, ok := servers["vouch"].(map[string]any); ok {
		if existing["command"] == resolved.Name {
			rep.MCP = "already present"
		} else {
			rep.MCP = fmt.Sprintf("left as-is (%v %v)", existing["command"], existing["args"])
		}
	} else {
		servers["vouch"] = map[string]any{
			"command": resolved.Name,
			"args":    resolved.args("mcp"),
		}
		mcpCfg["mcpServers"] = servers
		if err := writeJSONFile(mcpPath, mcpCfg); err != nil {
			return rep, err
		}
		rep.MCP = "written (" + resolved.how + ")"
	}
	return rep, nil
}

// initReport says what each half of init did, so the CLI can print it and the
// tests can assert idempotency.
type initReport struct {
	Hook string
	MCP  string
	Warn string
}

// stopHookWired reports whether the CURRENT vouch hook command is configured.
//
// A legacy hook (pre-P1.5 wrote `vouch verify --json` with shell exit-code
// mapping) does NOT count: it lacks the `stop_hook_active` loop guard and the
// Go-side contract, so init must replace it instead of reporting success.
func stopHookWired(stop []any) bool {
	for _, existing := range stop {
		if cmdText := hookCommandOf(existing); strings.Contains(cmdText, "vouch hook") {
			return true
		}
	}
	return false
}

// dropLegacyHooks removes previous-generation vouch hooks so an upgrade leaves
// exactly one vouch entry.
func dropLegacyHooks(stop []any) []any {
	kept := make([]any, 0, len(stop))
	for _, existing := range stop {
		cmdText := hookCommandOf(existing)
		if strings.Contains(cmdText, "vouch verify") && !strings.Contains(cmdText, "vouch hook") {
			continue
		}
		kept = append(kept, existing)
	}
	return kept
}

// hookCommandOf extracts the command string of a Stop-hook entry.
func hookCommandOf(entry any) string {
	m, ok := entry.(map[string]any)
	if !ok {
		return ""
	}
	hs, ok := m["hooks"].([]any)
	if !ok {
		return ""
	}
	var out []string
	for _, h := range hs {
		if hm, ok := h.(map[string]any); ok {
			if cmdText, _ := hm["command"].(string); cmdText != "" {
				out = append(out, cmdText)
			}
		}
	}
	return strings.Join(out, " ")
}

// launcherSpec is how this machine starts vouch: the binary on PATH when it is
// there, otherwise the npm wrapper (so a project without a Go toolchain still
// gets a working loop).
type launcherSpec struct {
	Name string
	Args []string
	how  string
	// warn is non-empty when the generated configs depend on something that is
	// not provably present (the npm package). init reports it instead of
	// printing an unqualified success.
	warn string
}

// args returns the launcher arguments followed by extra.
func (l launcherSpec) args(extra ...string) []string {
	out := make([]string, 0, len(l.Args)+len(extra))
	out = append(out, l.Args...)
	return append(out, extra...)
}

// command renders a shell command line, used by hook configs that spawn a shell.
func (l launcherSpec) command(extra ...string) string {
	return strings.Join(append([]string{l.Name}, l.args(extra...)...), " ")
}

// resolveLauncher picks the launcher once so every generated config agrees.
//
// It fails rather than guessing: a config that names a binary which does not
// exist produces a hook that dies with "command not found" (exit 127, which
// agents treat as non-blocking) — a silently disabled loop.
func resolveLauncher() (launcherSpec, error) {
	if _, err := exec.LookPath("vouch"); err == nil {
		return launcherSpec{Name: "vouch", how: "PATH"}, nil
	}
	if _, err := exec.LookPath("npx"); err == nil {
		return launcherSpec{
			Name: "npx", Args: []string{"--yes", "vouch"}, how: "npx",
			warn: "`vouch` is not on PATH, so both configs launch `npx --yes vouch …`. " +
				"That requires the vouch npm package to be published and reachable; until then, " +
				"install the binary (go install github.com/junqingyongyuanbusi/vouch/cmd/vouch@latest) and re-run init.",
		}, nil
	}
	return launcherSpec{}, fmt.Errorf("neither `vouch` nor `npx` is on PATH; install vouch " +
		"(go install github.com/junqingyongyuanbusi/vouch/cmd/vouch@latest) before wiring an agent")
}

// writeJSONFile writes indented JSON atomically (tmp + rename) so a crash never
// leaves an agent config file half-written.
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
