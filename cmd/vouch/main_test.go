package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/mcp"
	"github.com/junqingyongyuanbusi/vouch/internal/scheduler"
)

func TestVersionExit0(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version should not error, got %v", err)
	}
}

func TestVerifyInEmptyDirIsUnverified(t *testing.T) {
	// A directory with no detectable project cannot be verified: exit 2.
	dir := t.TempDir()
	cmd := newRootCmd()
	cmd.SetArgs([]string{"verify", "--path", dir})
	err := cmd.Execute()
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("want ExitError, got %T %v", err, err)
	}
	if ee.Code != 2 {
		t.Fatalf("empty project should be UNVERIFIED(2), got %d", ee.Code)
	}
}

func TestRootRunsVerify(t *testing.T) {
	// `vouch` with no subcommand is verify; on an empty project that is UNVERIFIED.
	dir := t.TempDir()
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--path", dir})
	err := cmd.Execute()
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 2 {
		t.Fatalf("root verify on empty project should exit 2, got %v", err)
	}
}

// buildVouchBinary compiles the CLI once per test run: the MCP transport reads
// the process's real stdio, so an in-process test cannot exercise it.
func buildVouchBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "vouch")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build vouch: %v\n%s", err, out)
	}
	return bin
}

func TestCLI_McpSpeaksMCPOverStdio(t *testing.T) {
	// End-to-end: the command written into .mcp.json by `vouch init` is what an
	// agent launches. Run exactly that command against a real repository and
	// drive it with the official SDK client.
	repo := t.TempDir()
	initCmd := newRootCmd()
	initCmd.SetArgs([]string{"init", "claude-code", "--path", repo})
	if err := initCmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	data, err := os.ReadFile(filepath.Join(repo, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	entry, ok := cfg.MCPServers["vouch"]
	if !ok {
		t.Fatalf("init must register the server: %s", data)
	}
	if len(entry.Args) == 0 || entry.Args[len(entry.Args)-1] != "mcp" {
		t.Fatalf("registration must launch the mcp subcommand: %+v", entry)
	}
	// The registered launcher is `vouch` (PATH) or `npx --yes vouch`; run the
	// same shape with the freshly built binary so the test is self-contained.
	args := []string{"mcp", "--path", repo}
	bin := buildVouchBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdkmcp.CommandTransport{Command: exec.Command(bin, args...)}, nil)
	if err != nil {
		t.Fatalf("connect to `vouch mcp`: %v", err)
	}
	defer func() { _ = session.Close() }()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(tools.Tools) != 3 {
		t.Fatalf("expected the three vouch tools, got %d", len(tools.Tools))
	}
	// vouch_gaps exercises the real backend (detector) without any fixture deps.
	res, err := session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: "vouch_gaps", Arguments: map[string]any{"path": repo},
	})
	if err != nil {
		t.Fatalf("vouch_gaps: %v", err)
	}
	if res.IsError {
		t.Fatalf("vouch_gaps must answer on an empty repo: %+v", res)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if !strings.Contains(text.String(), "detection confidence") {
		t.Fatalf("gaps must report the real detection result: %q", text.String())
	}
}

func TestShowNoBundlesExit2(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()
	cmd := newRootCmd()
	cmd.SetArgs([]string{"show"})
	err := cmd.Execute()
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 2 {
		t.Fatalf("show without bundles should exit 2, got %v", err)
	}
}

func TestMainFallbackExit2(t *testing.T) {
	// Unknown cobra errors (e.g. bad flag) must not be treated as BROKEN (1);
	// main() maps them to UNVERIFIED (2) so CI can distinguish crash from regression.
	cmd := newRootCmd()
	cmd.SetArgs([]string{"verify", "--unknown-flag"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("bad flag should error")
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		t.Fatalf("bad flag should not be ExitError, got code %d", ee.Code)
	}
	// main() would map this to 2 (UNVERIFIED), not 1 (BROKEN)
}

func TestCLI_VerifyRealRepoThreeStates(t *testing.T) {
	repo := t.TempDir()
	repo, _ = filepath.EvalSymlinks(repo)
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/cli\n\ngo 1.24\n")
	write("x.go", "package cli\n\nfunc X() int { return 1 }\n")
	write("x_test.go", "package cli\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) { if X() != 1 { t.Fatal(\"bad\") } }\n")
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}, {"add", "-A"}, {"commit", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// 1) clean → VERIFIED (exit 0)
	cmd := newRootCmd()
	cmd.SetArgs([]string{"verify", "--path", repo})
	if err := cmd.Execute(); err != nil {
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != 0 {
			t.Fatalf("clean verify must exit 0, got %v", err)
		}
	}
	// bundle persisted
	metas, err := bundle.NewStore(repo).List()
	if err != nil || len(metas) == 0 {
		t.Fatalf("bundle must be persisted: %v %v", err, metas)
	}
	bundleID := metas[0].BundleID

	// 2) injected regression → BROKEN (exit 1)
	write("x.go", "package cli\n\nfunc X() int { return 2 }\n")
	cmd2 := newRootCmd()
	cmd2.SetArgs([]string{"verify", "--path", repo})
	err = cmd2.Execute()
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Fatalf("regression must exit 1 (BROKEN), got %v", err)
	}

	// 3) show reads the persisted bundle back
	old, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()
	show := newRootCmd()
	show.SetArgs([]string{"show", bundleID})
	if err := show.Execute(); err != nil {
		t.Fatalf("show must load the stored bundle: %v", err)
	}
}

func TestCLI_GCRemovesOldBundlesAndRefs(t *testing.T) {
	repo := t.TempDir()
	repo, _ = filepath.EvalSymlinks(repo)
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/gc\n\ngo 1.24\n")
	write("x.go", "package gc\n\nfunc X() int { return 1 }\n")
	write("x_test.go", "package gc\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) { if X() != 1 { t.Fatal(\"bad\") } }\n")
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}, {"add", "-A"}, {"commit", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// two dirty-tree runs → two bundles and two snapshot refs
	for i := 0; i < 2; i++ {
		write("x.go", fmt.Sprintf("package gc\n\nfunc X() int { return %d }\n", 1))
		if err := os.WriteFile(filepath.Join(repo, "note.txt"), []byte(fmt.Sprintf("n%d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		c := newRootCmd()
		c.SetArgs([]string{"verify", "--path", repo})
		if err := c.Execute(); err != nil {
			var ee *ExitError
			if !errors.As(err, &ee) || ee.Code != 0 {
				t.Fatalf("verify %d: %v", i, err)
			}
		}
	}
	// run gc from a DIFFERENT cwd with --path: must still clean the target repo
	old, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()
	gcCmd := newRootCmd()
	gcCmd.SetArgs([]string{"gc", "--path", repo, "--keep", "1"})
	if err := gcCmd.Execute(); err != nil {
		t.Fatalf("gc: %v", err)
	}
	metas, err := bundle.NewStore(repo).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("gc --keep 1 must leave exactly 1 bundle, got %d", len(metas))
	}
	// The surviving bundle's snapshot ref must still resolve (protected from gc).
	store := bundle.NewStore(repo)
	b, err := store.Load(metas[0].BundleID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Subject.CandidateRef == nil {
		t.Fatal("surviving bundle lost its candidate ref")
	}
	chk := exec.Command("git", "rev-parse", "--verify", *b.Subject.CandidateRef+"^{commit}")
	chk.Dir = repo
	if out, err := chk.CombinedOutput(); err != nil {
		t.Fatalf("kept bundle's snapshot ref must survive gc: %v\n%s", err, out)
	}
}

func TestCLI_InitClaudeCodeWritesHookIdempotently(t *testing.T) {
	repo := t.TempDir()
	for i := 0; i < 2; i++ {
		c := newRootCmd()
		c.SetArgs([]string{"init", "claude-code", "--path", repo})
		if err := c.Execute(); err != nil {
			t.Fatalf("init run %d: %v", i, err)
		}
	}

	settings := readJSON(t, filepath.Join(repo, ".claude", "settings.json"))
	hooks := settings["hooks"].(map[string]any)
	stop := hooks["Stop"].([]any)
	if len(stop) != 1 {
		t.Fatalf("the Stop hook must be written exactly once: %v", stop)
	}
	entry := stop[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	hookCmd := entry["command"].(string)
	if !strings.Contains(hookCmd, "hook claude-code") {
		t.Fatalf("the hook must call the Go adapter (exit-code mapping lives there): %q", hookCmd)
	}
	if strings.Contains(hookCmd, "verify --json") && strings.Contains(hookCmd, "exit 2") {
		t.Fatalf("shell-level exit mapping must be gone: %q", hookCmd)
	}
	if entry["timeout"] == nil {
		t.Fatalf("the hook needs an explicit timeout for slow verifications: %v", entry)
	}

	mcpCfg := readJSON(t, filepath.Join(repo, ".mcp.json"))
	servers := mcpCfg["mcpServers"].(map[string]any)
	vouch, ok := servers["vouch"].(map[string]any)
	if !ok {
		t.Fatalf("the vouch MCP server must be registered: %v", servers)
	}
	args := vouch["args"].([]any)
	if args[len(args)-1] != "mcp" {
		t.Fatalf("the registration must launch the mcp subcommand: %v", args)
	}

	// Both halves must start vouch the same way — a mismatch means one of them
	// is "command not found" on a machine without vouch on PATH.
	if got, want := strings.Fields(hookCmd)[0], vouch["command"].(string); got != want {
		t.Fatalf("hook and MCP registration must use the same launcher: hook=%q mcp=%q", got, want)
	}

	// Unknown agent is rejected, not silently accepted.
	c := newRootCmd()
	c.SetArgs([]string{"init", "unknown-agent", "--path", repo})
	err := c.Execute()
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 2 {
		t.Fatalf("unknown agent must fail with UNVERIFIED, got %v", err)
	}
}

func TestCLI_InitPreservesExistingAgentConfig(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := map[string]any{
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{"matcher": "", "hooks": []any{
					map[string]any{"type": "command", "command": "./scripts/format.sh"},
				}},
			},
		},
	}
	writeJSON(t, filepath.Join(repo, ".claude", "settings.json"), existing)
	writeJSON(t, filepath.Join(repo, ".mcp.json"), map[string]any{
		"mcpServers": map[string]any{"other": map[string]any{"command": "other-mcp"}},
	})

	c := newRootCmd()
	c.SetArgs([]string{"init", "claude-code", "--path", repo})
	if err := c.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	settings := readJSON(t, filepath.Join(repo, ".claude", "settings.json"))
	stop := settings["hooks"].(map[string]any)["Stop"].([]any)
	if len(stop) != 2 {
		t.Fatalf("the user's hook must survive alongside ours: %v", stop)
	}
	servers := readJSON(t, filepath.Join(repo, ".mcp.json"))["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Fatalf("other MCP servers must survive: %v", servers)
	}
	if _, ok := servers["vouch"]; !ok {
		t.Fatalf("vouch must be registered next to them: %v", servers)
	}
}

func TestCLI_InitLauncherFollowsPATH(t *testing.T) {
	// Without vouch on PATH the npm wrapper is the only way to start it; both
	// generated configs must then use npx, or the Stop hook silently dies.
	newRepoWithPATH := func(t *testing.T, bins ...string) string {
		t.Helper()
		binDir := t.TempDir()
		for _, name := range bins {
			script := "#!/bin/sh\nexit 0\n"
			if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("PATH", binDir)
		return t.TempDir()
	}

	t.Run("vouch on PATH", func(t *testing.T) {
		repo := newRepoWithPATH(t, "vouch", "npx")
		if _, err := initClaudeCode(repo); err != nil {
			t.Fatal(err)
		}
		hook := firstHookCommand(t, repo)
		if !strings.HasPrefix(hook, "vouch hook claude-code") {
			t.Fatalf("PATH launcher expected, got %q", hook)
		}
	})

	t.Run("only npx available", func(t *testing.T) {
		repo := newRepoWithPATH(t, "npx")
		rep, err := initClaudeCode(repo)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Warn == "" {
			t.Fatal("a launcher that depends on the published npm package must be flagged, not reported as a clean success")
		}
		hook := firstHookCommand(t, repo)
		if !strings.HasPrefix(hook, "npx --yes vouch hook claude-code") {
			t.Fatalf("npx launcher expected, got %q", hook)
		}
		servers := readJSON(t, filepath.Join(repo, ".mcp.json"))["mcpServers"].(map[string]any)
		if got := servers["vouch"].(map[string]any)["command"]; got != "npx" {
			t.Fatalf("MCP registration must use the same launcher, got %v", got)
		}
	})

	t.Run("neither vouch nor npx available", func(t *testing.T) {
		repo := newRepoWithPATH(t) // empty PATH
		if _, err := initClaudeCode(repo); err == nil {
			t.Fatal("init must fail instead of writing configs that cannot start")
		}
		if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.json")); err == nil {
			t.Fatal("nothing may be written when the launcher cannot be resolved")
		}
		if _, err := os.Stat(filepath.Join(repo, ".mcp.json")); err == nil {
			t.Fatal("nothing may be written when the launcher cannot be resolved")
		}
	})
}

func TestCLI_InitWarnsWhenOnlyNpxIsAvailable(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "npx"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	repo := t.TempDir()
	c := newRootCmd()
	c.SetArgs([]string{"init", "claude-code", "--path", repo})
	var out strings.Builder
	c.SetOut(&out)
	if err := c.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(out.String(), "npm package") {
		t.Fatalf("the user must be told the configs need the published package:\n%s", out.String())
	}
}

func firstHookCommand(t *testing.T, repo string) string {
	t.Helper()
	settings := readJSON(t, filepath.Join(repo, ".claude", "settings.json"))
	stop := settings["hooks"].(map[string]any)["Stop"].([]any)
	return stop[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"].(string)
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s is not valid JSON: %v\n%s", path, err, data)
	}
	return out
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCLI_HiddenCommandsStillWork(t *testing.T) {
	dir := t.TempDir()
	gc := newRootCmd()
	gc.SetArgs([]string{"gc", "--path", dir})
	if err := gc.Execute(); err != nil {
		t.Fatalf("hidden gc should still run: %v", err)
	}
	// help must not list them (minimal first screen)
	root := newRootCmd()
	var out strings.Builder
	root.SetOut(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	help := out.String()
	for _, hidden := range []string{"rerun", "gc ", "mcp", "probe", "export"} {
		if strings.Contains(help, hidden) {
			t.Fatalf("hidden command %q leaked into help:\n%s", hidden, help)
		}
	}
	// first screen must show the three public commands
	for _, shown := range []string{"init", "show", "verify"} {
		if !strings.Contains(help, shown) {
			t.Fatalf("public command %q missing from help:\n%s", shown, help)
		}
	}
}

func TestCLI_InitThenVerifyIdentityStable(t *testing.T) {
	repo := t.TempDir()
	repo, _ = filepath.EvalSymlinks(repo)
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/init\n\ngo 1.24\n")
	write("x.go", "package in\n\nfunc X() int { return 1 }\n")
	write("x_test.go", "package in\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) { if X() != 1 { t.Fatal(\"bad\") } }\n")
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}, {"add", "-A"}, {"commit", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	initCmd := newRootCmd()
	initCmd.SetArgs([]string{"init", "claude-code", "--path", repo})
	if err := initCmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	ids := map[string]bool{}
	outFile := filepath.Join(t.TempDir(), "out.json") // never inside the audited repo
	for i := 0; i < 2; i++ {
		c := newRootCmd()
		c.SetArgs([]string{"verify", "--path", repo, "--json", "--output", outFile})
		if err := c.Execute(); err != nil {
			var ee *ExitError
			if !errors.As(err, &ee) || ee.Code != 0 {
				t.Fatalf("verify %d: %v", i, err)
			}
		}
		data, err := os.ReadFile(outFile)
		if err != nil {
			t.Fatal(err)
		}
		var b bundle.ProofBundle
		if err := json.Unmarshal(data, &b); err != nil {
			t.Fatal(err)
		}
		ids[b.BundleID] = true
	}
	if len(ids) != 1 {
		t.Fatalf("the hook file must not change bundle identity: %v", ids)
	}
	// .claude/ must be locally excluded so git status stays clean
	ex, err := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
	if err != nil || !strings.Contains(string(ex), ".claude/") {
		t.Fatalf(".claude/ must be added to .git/info/exclude: %v %s", err, ex)
	}
}

func TestCLI_ShowMarkdownAndFailures(t *testing.T) {
	repo := t.TempDir()
	repo, _ = filepath.EvalSymlinks(repo)
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/md\n\ngo 1.24\n")
	write("x.go", "package md\n\nfunc X() int { return 1 }\n")
	write("x_test.go", "package md\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) { if X() != 1 { t.Fatalf(\"want 1 got %d\", X()) } }\n")
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}, {"add", "-A"}, {"commit", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write("x.go", "package md\n\nfunc X() int { return 2 }\n")
	verify := newRootCmd()
	verify.SetArgs([]string{"verify", "--path", repo})
	if err := verify.Execute(); err != nil {
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != 1 {
			t.Fatalf("want BROKEN, got %v", err)
		}
	}
	metas, _ := bundle.NewStore(repo).List()
	if len(metas) != 1 {
		t.Fatalf("want 1 bundle, got %d", len(metas))
	}
	show := newRootCmd()
	var out strings.Builder
	show.SetOut(&out)
	show.SetArgs([]string{"show", metas[0].BundleID, "--path", repo, "--md"})
	if err := show.Execute(); err != nil {
		t.Fatalf("show --md: %v", err)
	}
	md := out.String()
	if !strings.Contains(md, "## vouch: BROKEN") || !strings.Contains(md, "TestX") {
		t.Fatalf("markdown must carry the verdict and failure detail:\n%s", md)
	}
	// plain show must also surface the message
	show2 := newRootCmd()
	var out2 strings.Builder
	show2.SetOut(&out2)
	show2.SetArgs([]string{"show", metas[0].BundleID, "--path", repo})
	if err := show2.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2.String(), "want 1 got 2") {
		t.Fatalf("show must print the failure message:\n%s", out2.String())
	}
}

func TestCLI_HookClaudeCodeContract(t *testing.T) {
	// A real regression in a real repo: the Stop hook must block (exit 2) and
	// put the evidence on stderr, which is what Claude Code feeds back.
	repo := t.TempDir()
	repo, _ = filepath.EvalSymlinks(repo)
	runCLI := func(t *testing.T, args ...string) {
		t.Helper()
		c := newRootCmd()
		c.SetArgs(append(args, "--path", repo))
		if err := c.Execute(); err != nil {
			t.Fatalf("cli %v: %v", args, err)
		}
	}
	runCLI(t, "init", "claude-code")
	if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.json")); err != nil {
		t.Fatalf("init must actually run before the hook test: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/hook\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "package hook\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(repo, "x_test.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "-c", "user.email=v@v", "-c", "user.name=v", "commit", "-m", "base")

	t.Run("verified exits 0", func(t *testing.T) {
		var out, errOut strings.Builder
		c := newRootCmd()
		c.SetArgs([]string{"hook", "claude-code", "--path", repo})
		c.SetIn(strings.NewReader(`{"session_id":"s","stop_hook_active":false}`))
		c.SetOut(&out)
		c.SetErr(&errOut)
		err := c.Execute()
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != 0 {
			t.Fatalf("VERIFIED must not block, got %v", err)
		}
		if !strings.Contains(out.String(), "VERIFIED") {
			t.Fatalf("the verdict must be reported to the agent: %q", out.String())
		}
	})

	t.Run("loop guard exits 0 without verifying", func(t *testing.T) {
		broken := strings.Replace(body, "func TestX(t *testing.T) {}", "func TestX(t *testing.T) { t.Fatal(\"boom\") }", 1)
		if err := os.WriteFile(filepath.Join(repo, "x_test.go"), []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		var errOut strings.Builder
		c := newRootCmd()
		c.SetArgs([]string{"hook", "claude-code", "--path", repo})
		c.SetIn(strings.NewReader(`{"stop_hook_active":true}`))
		c.SetErr(&errOut)
		err := c.Execute()
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != 0 {
			t.Fatalf("stop_hook_active must short-circuit the hook, got %v", err)
		}
	})

	t.Run("broken exits 2 with evidence on stderr", func(t *testing.T) {
		var out, errOut strings.Builder
		c := newRootCmd()
		c.SetArgs([]string{"hook", "claude-code", "--path", repo})
		c.SetIn(strings.NewReader(`{"stop_hook_active":false}`))
		c.SetOut(&out)
		c.SetErr(&errOut)
		err := c.Execute()
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != 2 {
			t.Fatalf("BROKEN must block with exit 2, got %v", err)
		}
		if !strings.Contains(errOut.String(), "BROKEN") || !strings.Contains(errOut.String(), "TestX") {
			t.Fatalf("the blocking message must name the regression: %q", errOut.String())
		}
	})

	t.Run("unsupported agent never blocks", func(t *testing.T) {
		c := newRootCmd()
		c.SetArgs([]string{"hook", "some-other-agent", "--path", repo})
		c.SetIn(strings.NewReader("{}"))
		c.SetErr(&strings.Builder{})
		err := c.Execute()
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != 0 {
			t.Fatalf("an unknown agent must fail open, got %v", err)
		}
	})
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestCLI_InitUpgradesLegacyShellHook(t *testing.T) {
	// Repos wired before P1.5 have `vouch verify --json` with shell exit-code
	// tricks. That hook has no loop guard, so init must replace it — reporting
	// "already present" would silently leave the old contract in place.
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{"matcher": "", "hooks": []any{
					map[string]any{"type": "command", "command": `sh -c 'out=$(vouch verify --json 2>&1); c=$?; if [ "$c" -eq 1 ]; then exit 2; fi; exit 0'`},
				}},
				map[string]any{"matcher": "", "hooks": []any{
					map[string]any{"type": "command", "command": "./scripts/format.sh"},
				}},
			},
		},
	}
	writeJSON(t, filepath.Join(repo, ".claude", "settings.json"), legacy)

	c := newRootCmd()
	c.SetArgs([]string{"init", "claude-code", "--path", repo})
	var out strings.Builder
	c.SetOut(&out)
	if err := c.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(out.String(), "replaced") {
		t.Fatalf("an upgrade must be reported, got %q", out.String())
	}
	settings := readJSON(t, filepath.Join(repo, ".claude", "settings.json"))
	stop := settings["hooks"].(map[string]any)["Stop"].([]any)
	var vouchHooks, legacyHooks, otherHooks int
	for _, e := range stop {
		cmdText := hookCommandOf(e)
		switch {
		case strings.Contains(cmdText, "vouch hook"):
			vouchHooks++
		case strings.Contains(cmdText, "vouch verify"):
			legacyHooks++
		default:
			otherHooks++
		}
	}
	if vouchHooks != 1 || legacyHooks != 0 || otherHooks != 1 {
		t.Fatalf("legacy hook must be replaced by exactly one new hook, kept user hook intact: %+v", stop)
	}

	// Running init again must now be a no-op.
	c = newRootCmd()
	c.SetArgs([]string{"init", "claude-code", "--path", repo})
	out.Reset()
	c.SetOut(&out)
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already present") {
		t.Fatalf("second run must be idempotent, got %q", out.String())
	}
}

func TestVerifyCallTimeoutIsTotalBudgetPlusBoundedSlack(t *testing.T) {
	// budget_ms is the run's TOTAL wall-clock budget and the scheduler owns it;
	// this guard only absorbs non-context-aware setup/cleanup, so it must be
	// budget+slack. A shorter guard would truncate a healthy run and blame the
	// repository for a tool-level limit; a per-probe multiple would let a
	// wedged call hang for several times the budget the caller asked for.
	if got, want := verifyCallTimeout(60_000), 60*time.Second+callGuardSlack; got != want {
		t.Fatalf("verifyCallTimeout(60s) = %v, want %v", got, want)
	}
	if got, want := verifyCallTimeout(0), time.Duration(scheduler.DefaultBudgetMs)*time.Millisecond+callGuardSlack; got != want {
		t.Fatalf("verifyCallTimeout(0) must default to %dms + slack, got %v", scheduler.DefaultBudgetMs, got)
	}
	if verifyCallTimeout(60_000) <= 60*time.Second {
		t.Fatal("the guard must exceed the kernel's total budget")
	}
	if verifyCallTimeout(60_000) >= 3*60*time.Second {
		t.Fatal("the guard must not be a per-probe multiple of the budget")
	}
	// A huge budget must be clamped before the ms→ns conversion overflows into
	// a negative Duration (which would expire the call instantly).
	if got, want := verifyCallTimeout(scheduler.MaxBudgetMs+1), time.Duration(scheduler.MaxBudgetMs)*time.Millisecond+callGuardSlack; got != want {
		t.Fatalf("huge budget must clamp to MaxBudgetMs+slack, got %v want %v", got, want)
	}
}

func TestMcpBackend_VerifyRunsEveryProbeWithinBudget(t *testing.T) {
	// A real repository with test/build/typecheck commands and an explicit
	// budget_ms: all three probes must produce evidence and nothing may be
	// skipped because of the tool-level guard.
	repo := t.TempDir()
	repo, _ = filepath.EvalSymlinks(repo)
	writeFile := func(rel, content string) {
		t.Helper()
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("go.mod", "module example.com/mcpbudget\n\ngo 1.24\n")
	writeFile("add.go", "package mcpbudget\n\nfunc Add(a, b int) int { return a + b }\n")
	writeFile("add_test.go", "package mcpbudget\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 3 { t.Fatal(\"bad\") } }\n")
	writeFile("Makefile", "test:\n\tgo test ./...\n\nbuild:\n\tgo build ./...\n\ntypecheck:\n\tgo vet ./...\n")
	for _, args := range [][]string{{"init"}, {"add", "-A"}, {"-c", "user.email=v@v", "-c", "user.name=v", "commit", "-m", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := mcpBackend{}.Verify(ctx, mcp.VerifyRequest{Path: repo, BudgetMs: 60_000})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Verdict != "VERIFIED" {
		t.Fatalf("a clean repo with three runnable probes must verify, got %+v (unverified=%v)", res, res.Unverified)
	}
	probes := map[string]bool{}
	for _, p := range res.Probes {
		probes[p.Probe] = true
	}
	for _, want := range []string{"test", "build", "typecheck"} {
		if !probes[want] {
			t.Fatalf("probe %q must have run inside the call budget, got %v", want, res.Probes)
		}
	}
	for _, claim := range res.Unverified {
		if strings.Contains(claim, "budget exhausted") {
			t.Fatalf("the tool-level guard must not truncate a healthy run: %v", res.Unverified)
		}
	}
}
