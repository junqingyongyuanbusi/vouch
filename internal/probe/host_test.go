package probe_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/probe"
)

// TestHostMissingBinary asserts the core crash-isolation contract.
func TestHostMissingBinary(t *testing.T) {
	h := probe.Host{Path: "/nonexistent/probe-xyz", Timeout: time.Second}
	inv := h.Invoke(context.Background(), probe.RunParams{Workdir: t.TempDir(), Role: "candidate", BudgetMs: 1000})
	if inv.Result.Verdict != probe.VerdictInconclusive {
		t.Fatalf("missing binary → got %q want inconclusive", inv.Result.Verdict)
	}
	if inv.Result.Reason == "" {
		t.Fatal("inconclusive should carry reason")
	}
	if inv.Caps.Kind != probe.KindDeterministic {
		t.Fatalf("missing binary caps should default to deterministic, got %q", inv.Caps.Kind)
	}
}

func TestHostZeroTimeoutIsInconclusive(t *testing.T) {
	h := probe.Host{Path: "/bin/echo", Timeout: 0}
	inv := h.Invoke(context.Background(), probe.RunParams{Workdir: t.TempDir(), Role: "candidate"})
	if inv.Result.Verdict != probe.VerdictInconclusive {
		t.Fatalf("zero timeout → got %q want inconclusive", inv.Result.Verdict)
	}
}

func TestHostCrashIsInconclusive(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "crash.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := probe.Host{Path: script, Timeout: 2 * time.Second}
	inv := h.Invoke(context.Background(), probe.RunParams{Workdir: dir, Role: "candidate", BudgetMs: 1000})
	if inv.Result.Verdict != probe.VerdictInconclusive {
		t.Fatalf("crash → got %q want inconclusive", inv.Result.Verdict)
	}
}

func TestHostTimeoutIsInconclusive(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "sleep.sh")
	content := "#!/bin/sh\nread line; echo '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"name\":\"sleep\",\"version\":\"0.1.0\",\"capabilities\":{\"differential\":true,\"kind\":\"deterministic\"}}}'; read line; sleep 5; echo '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"verdict\":\"pass\",\"summary\":\"late\"}}'\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	h := probe.Host{Path: script, Timeout: 500 * time.Millisecond}
	inv := h.Invoke(context.Background(), probe.RunParams{Workdir: dir, Role: "candidate", BudgetMs: 200})
	if inv.Result.Verdict != probe.VerdictInconclusive {
		t.Fatalf("timeout → got %q want inconclusive", inv.Result.Verdict)
	}
}

func TestHostHonorsProgress(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "progress.sh")
	content := "#!/bin/sh\n" +
		"read line; echo '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"name\":\"p\",\"version\":\"0.1.0\",\"capabilities\":{\"differential\":false,\"kind\":\"inferred\"}}}';\n" +
		"read line; echo '{\"jsonrpc\":\"2.0\",\"method\":\"progress\",\"params\":{\"message\":\"running...\"}}';\n" +
		"echo '{\"jsonrpc\":\"2.0\",\"method\":\"progress\",\"params\":{\"message\":\"still running\"}}';\n" +
		"echo '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"verdict\":\"pass\",\"summary\":\"ok\"}}';\n" +
		"read line; echo '{\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{}}'\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	h := probe.Host{Path: script, Timeout: 2 * time.Second}
	inv := h.Invoke(context.Background(), probe.RunParams{Workdir: dir, Role: "candidate", BudgetMs: 1000})
	if inv.Result.Verdict != probe.VerdictPass {
		t.Fatalf("progress probe → got %q want pass", inv.Result.Verdict)
	}
	if inv.Caps.Kind != probe.KindInferred {
		t.Fatalf("expected inferred caps, got %q", inv.Caps.Kind)
	}
}

func TestHostInferredIsolation(t *testing.T) {
	// P0/P1 invariant: inferred probes must never be routed to evidence.
	dir := t.TempDir()
	script := filepath.Join(dir, "inferred.sh")
	content := "#!/bin/sh\n" +
		"read line; echo '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"name\":\"scope\",\"version\":\"0.1.0\",\"capabilities\":{\"differential\":false,\"kind\":\"inferred\"}}}';\n" +
		"read line; echo '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"verdict\":\"fail\",\"summary\":\"scope fail — should not gate\"}}';\n" +
		"read line; echo '{\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{}}'\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	h := probe.Host{Path: script, Timeout: 2 * time.Second}
	inv := h.Invoke(context.Background(), probe.RunParams{Workdir: dir, Role: "candidate", BudgetMs: 1000})
	if inv.Caps.Kind != probe.KindInferred {
		t.Fatalf("want inferred, got %q", inv.Caps.Kind)
	}
	// Even though the probe says fail, the caller must check Caps.Kind before
	// feeding to verdict.Aggregate. This test documents the contract; host itself
	// never promotes inferred to deterministic.
	if inv.Result.Verdict != probe.VerdictFail {
		t.Fatalf("inferred fail preserved, got %q", inv.Result.Verdict)
	}
}

func TestHostIgnoresShutdown(t *testing.T) {
	// Probe sends result then ignores shutdown and sleeps — Invoke must still
	// return within Timeout+3s via bounded reap, not hang.
	dir := t.TempDir()
	script := filepath.Join(dir, "ignore.sh")
	content := "#!/bin/sh\n" +
		"read line; echo '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"name\":\"x\",\"version\":\"0.1.0\",\"capabilities\":{\"differential\":true,\"kind\":\"deterministic\"}}}';\n" +
		"read line; echo '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"verdict\":\"pass\",\"summary\":\"ok\"}}';\n" +
		"sleep 10\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	h := probe.Host{Path: script, Timeout: 2 * time.Second}
	start := time.Now()
	inv := h.Invoke(context.Background(), probe.RunParams{Workdir: dir, Role: "candidate", BudgetMs: 1000})
	elapsed := time.Since(start)
	if inv.Result.Verdict != probe.VerdictPass {
		t.Fatalf("want pass, got %q reason %q", inv.Result.Verdict, inv.Result.Reason)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Invoke hung on ignored shutdown: %v", elapsed)
	}
}

// TestGateEligibility_InferredNeverEntersGate locks the P0 rule at the seam the
// scheduler will use: routing must use Run's returned capabilities.
func TestGateEligibility_InferredNeverEntersGate(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "inferred.sh")
	content := "#!/bin/sh\n" +
		"read line; echo '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"name\":\"scope\",\"version\":\"0.1.0\",\"capabilities\":{\"differential\":false,\"kind\":\"inferred\"}}}';\n" +
		"read line; echo '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"verdict\":\"fail\",\"summary\":\"scope says fail\"}}';\n" +
		"read line; echo '{\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{}}'\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	r := probe.HostRunner{Host: probe.Host{Path: script, Timeout: 5 * time.Second}, Name_: "scope"}
	// Static Capabilities must not claim determinism for an unknown external probe.
	if got := r.Capabilities(); got.Kind != "" {
		t.Fatalf("HostRunner.Capabilities must be unknown, got %+v", got)
	}
	inv, err := r.Run(context.Background(), probe.RunParams{Workdir: dir, Role: "candidate", BudgetMs: 1000})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if inv.Result.Verdict != probe.VerdictFail {
		t.Fatalf("probe said fail: %+v", inv.Result)
	}
	if probe.GateEligible(inv.Caps) {
		t.Fatalf("inferred probe must not be gate eligible: %+v", inv.Caps)
	}
	// Deterministic built-in path remains eligible.
	if !probe.GateEligible(probe.Capabilities{Kind: probe.KindDeterministic, Differential: true}) {
		t.Fatal("deterministic probe must be gate eligible")
	}
}
