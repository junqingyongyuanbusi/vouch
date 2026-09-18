// Host for Probe Protocol v1 — process spawning with crash/timeout isolation.
//
// Contract (PLAN-V2 §6.2):
//   - Probe reports facts; differ/verdict decide. Host never panics.
//   - Crash / timeout / malformed JSON → Inconclusive, never fail the whole verify.
//   - Shutdown is best-effort (SIGTERM → SIGKILL). Budget includes startup.
//   - Inferred probes must be distinguishable: Initialize capabilities are returned
//     so the kernel can route deterministic → evidence, inferred → inferences.
package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// Host runs a single probe invocation (one role: base or candidate).
type Host struct {
	// Path is the probe executable.
	Path string
	// Timeout for the whole run (including initialize). Kernel passes budget_ms + grace.
	// Zero is treated as missing budget and yields inconclusive immediately (prevents
	// WithTimeout(0) from instantly expiring).
	Timeout time.Duration
}

// Invocation is the full result of one probe run, including the probe's declared
// capabilities so the caller can enforce deterministic/inferred isolation.
type Invocation struct {
	Result RunResult
	Caps   Capabilities
}

// Invoke runs initialize → run → shutdown against Path and returns the probe result
// plus its capabilities. Any transport or probe failure is mapped to Inconclusive
// with a reason. Capabilities default to deterministic when initialize fails, so
// the caller can still route the result safely (defense in depth).
func (h Host) Invoke(ctx context.Context, params RunParams) Invocation {
	if h.Timeout <= 0 {
		return Invocation{
			Result: inconclusive("no budget: host Timeout must be >0", params),
			Caps:   Capabilities{Differential: true, Kind: KindDeterministic},
		}
	}
	ctx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, h.Path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Invocation{Result: inconclusive("stdin pipe: "+err.Error(), params), Caps: Capabilities{Kind: KindDeterministic}}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Invocation{Result: inconclusive("stdout pipe: "+err.Error(), params), Caps: Capabilities{Kind: KindDeterministic}}
	}
	if err := cmd.Start(); err != nil {
		return Invocation{Result: inconclusive("start: "+err.Error(), params), Caps: Capabilities{Kind: KindDeterministic}}
	}
	// Unified cleanup: close stdin and bounded reap (shutdown may be ignored).
	defer func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	enc := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

	write := func(id *int, method string, p interface{}) error {
		ch := make(chan error, 1)
		go func() { ch <- enc.Encode(RPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: p}) }()
		select {
		case err := <-ch:
			return err
		case <-ctx.Done():
			// Close stdin to unblock a stalled Encode (pipe full, probe not reading).
			_ = stdin.Close()
			go func() { <-ch }()
			return ctx.Err()
		}
	}

	read := func() (RPCResponse, error) {
		ch := make(chan struct {
			resp RPCResponse
			err  error
			ok   bool
		}, 1)
		go func() {
			ok := scanner.Scan()
			var resp RPCResponse
			var err error
			if !ok {
				if e := scanner.Err(); e != nil {
					err = e
				} else {
					err = fmt.Errorf("probe closed stdout")
				}
			} else {
				err = json.Unmarshal(scanner.Bytes(), &resp)
				if err != nil {
					err = fmt.Errorf("malformed JSON: %w", err)
				}
			}
			ch <- struct {
				resp RPCResponse
				err  error
				ok   bool
			}{resp, err, ok}
		}()
		select {
		case r := <-ch:
			return r.resp, r.err
		case <-ctx.Done():
			// Drain to avoid leaking the scanner goroutine on timeout.
			go func() { <-ch }()
			return RPCResponse{}, ctx.Err()
		}
	}

	// 1. initialize
	if err := write(intPtr(1), "initialize", map[string]interface{}{"protocol_version": ProtocolVersion}); err != nil {
		_ = cmd.Process.Kill()
		return Invocation{Result: inconclusive("write initialize: "+err.Error(), params), Caps: Capabilities{Kind: KindDeterministic}}
	}
	resp, err := read()
	if err != nil {
		_ = cmd.Process.Kill()
		return Invocation{Result: inconclusive("read initialize: "+err.Error(), params), Caps: Capabilities{Kind: KindDeterministic}}
	}
	if resp.Error != nil {
		_ = cmd.Process.Kill()
		return Invocation{Result: inconclusive("initialize error: "+resp.Error.Message, params), Caps: Capabilities{Kind: KindDeterministic}}
	}
	var initRes InitializeResult
	if len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, &initRes); err != nil {
			_ = cmd.Process.Kill()
			return Invocation{Result: inconclusive("bad initialize result: "+err.Error(), params), Caps: Capabilities{Kind: KindDeterministic}}
		}
	}
	// Default kind if probe omitted it (defense in depth — treat as deterministic).
	if initRes.Capabilities.Kind == "" {
		initRes.Capabilities.Kind = KindDeterministic
	}

	// 2. run
	if err := write(intPtr(2), "run", params); err != nil {
		_ = cmd.Process.Kill()
		return Invocation{Result: inconclusive("write run: "+err.Error(), params), Caps: initRes.Capabilities}
	}
	for {
		resp, err = read()
		if err != nil {
			_ = cmd.Process.Kill()
			if ctx.Err() != nil {
				return Invocation{Result: inconclusive("timeout: "+ctx.Err().Error(), params), Caps: initRes.Capabilities}
			}
			return Invocation{Result: inconclusive("read run: "+err.Error(), params), Caps: initRes.Capabilities}
		}
		if resp.ID == nil {
			continue // progress
		}
		if resp.Error != nil {
			_ = cmd.Process.Kill()
			return Invocation{Result: inconclusive("run error: "+resp.Error.Message, params), Caps: initRes.Capabilities}
		}
		var out RunResult
		if err := json.Unmarshal(resp.Result, &out); err != nil {
			_ = cmd.Process.Kill()
			return Invocation{Result: inconclusive("bad run result: "+err.Error(), params), Caps: initRes.Capabilities}
		}
		if out.Verdict != VerdictPass && out.Verdict != VerdictFail && out.Verdict != VerdictInconclusive {
			out.Verdict = VerdictInconclusive
			if out.Reason == "" {
				out.Reason = "unknown verdict normalized to inconclusive"
			}
		}
		// 3. shutdown best-effort (don't block on it).
		_ = write(intPtr(3), "shutdown", map[string]interface{}{})
		return Invocation{Result: out, Caps: initRes.Capabilities}
	}
}

func inconclusive(reason string, params RunParams) RunResult {
	return RunResult{
		Verdict: VerdictInconclusive,
		Summary: "inconclusive",
		Reason:  reason,
		Data:    map[string]interface{}{"role": params.Role},
	}
}

func intPtr(i int) *int { return &i }
