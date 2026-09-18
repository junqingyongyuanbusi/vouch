# Probe Protocol v1

> **Status:** Stable (P0) — versioned, semver. Breaking changes bump `protocol_version` and `schema_version`.
> **Audience:** Anyone can write a probe without reading kernel code. Builtins and third-party probes speak the same protocol.

A probe is an executable (any language) that communicates with the `vouch` kernel over `stdin`/`stdout` using JSON-lines (one JSON object per line, UTF-8, `\n` delimited, no streaming JSON). `stderr` is reserved for human logs and is never parsed.

---

## 1. Lifecycle

```
kernel → probe  {"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocol_version":1}}
probe  → kernel {"jsonrpc":"2.0","id":1,"result":{"name":"test","version":"0.1.0","capabilities":{"differential":true,"kind":"deterministic","needs_network":false}}}
kernel → probe  {"jsonrpc":"2.0","id":2,"method":"run","params":{"workdir":"/tmp/vouch-wt-candidate-a3f9","role":"candidate","profile":{...ProjectProfile},"budget_ms":120000,"diff_files":["src/auth.ts"]}}
probe  → kernel {"jsonrpc":"2.0","method":"progress","params":{"message":"running 142 tests..."}}   // 0..N, optional
probe  → kernel {"jsonrpc":"2.0","id":2,"result":{"verdict":"pass","summary":"143 passed","artifacts":{"logs":"<base64 or path>"},"data":{...probe-specific}}}
kernel → probe  {"jsonrpc":"2.0","id":3,"method":"shutdown"}
probe  → kernel {"jsonrpc":"2.0","id":3,"result":{}}
probe exits 0
```

* `initialize` must be first. Kernel aborts if probe's `protocol_version` mismatches.
* `run` is called once per invocation (base or candidate). Kernel invokes the probe **twice** in parallel (one per worktree) when `differential: true`; otherwise once on `candidate` only.
* `progress` is fire-and-forget (no `id`), may be sent any time between `run` and `result`.
* `shutdown` is best-effort; probe should exit within 2s even if not received (kernel SIGTERM → SIGKILL).
* Any crash, non-zero exit, timeout, or malformed JSON → kernel records `inconclusive` for that role, never fails the whole `verify`.

## 2. Message Framing

* Transport: JSON-RPC 2.0 over `stdio` with `Content-Length` **not** used — plain lines. This matches LSP's "simple" mode and keeps probes trivial (read line, parse JSON, write line + flush).
* Each message is a single line, max 16 MiB. Pretty printing is forbidden on the wire.
* Unknown `method` → respond `{"error":{"code":-32601,"message":"method not found"}}`.
* Kernel sets `budget_ms` — probe should respect it and return partial `inconclusive` if exceeded (kernel kills after `budget_ms + 500ms` grace).

## 3. Capabilities (declared in `initialize` result)

```json
{
  "name": "test",
  "version": "0.1.0",
  "capabilities": {
    "differential": true,          // if false, kernel does not run on base
    "kind": "deterministic",       // "deterministic" | "inferred" — determines gate participation
    "needs_network": false,        // see §4
    "needs_network_allow": []      // when needs_network=true, list of allowlisted hosts (see §4)
  }
}
```

* `kind` is authoritative: the same binary cannot flip kinds between runs. `deterministic` evidence enters `verdict` aggregation; `inferred` goes to `bundle.inferences` only, even if the probe reports `fail`.
* `differential: false` is for `inferred` probes (e.g., `scope`) that only need the diff, not base/candidate comparison.

## 4. Sandbox & Network

P1 sandbox is **best-effort**:

* Default: egress **denied**, `loopback (127.0.0.1/::1)` **allowlisted**. Rationale (PLAN-V2 §7.2): local integration tests commonly start `localhost` servers; blocking loopback would make most projects `inconclusive`.
* `needs_network: false` (default) → kernel enforces the deny+loopback rule.
* `needs_network: true` → probe must declare `needs_network_allow: ["registry.npmjs.org","api.example.com"]`. Kernel allowlists only those hosts (exact match, no wildcards in v1) **plus** loopback. DNS is resolved in-kernel for enforcement; any non-allowlisted egress → `inconclusive` with `reason: "network_blocked"`.
* File writes are confined to `workdir` (the worktree). Probes must not write outside it; kernel may enforce with `sandbox-exec` (darwin, deprecated, best-effort) or netns (linux).

On darwin `sandbox-exec` is documented as deprecated by Apple; `vouch` documents "best-effort network isolation" and never claims stronger guarantees than the platform provides.

## 5. `run` params

```json
{
  "workdir": "/tmp/vouch-wt-candidate-a3f9",
  "role": "candidate", // "base" | "candidate"
  "profile": {          // ProjectProfile per bundle-schema.json
    "language": ["typescript"],
    "package_manager": "pnpm",
    "commands": {
      "build": {"cmd":"pnpm build","source":".github/workflows/ci.yml#L23"},
      "test": {"cmd":"pnpm vitest run","source":".github/workflows/ci.yml#L31"}
    },
    "test_selection":{"supported":true,"mechanism":"vitest related"},
    "confidence":"high","gaps":[]
  },
  "budget_ms": 120000,
  "diff_files": ["src/auth.ts","src/auth.test.ts"],
  "bundle_id": "a3f9e2",          // for artifact naming
  "command": "go test ./...",      // optional override: selector-applied command
  "env_fingerprint": "sha256:e1a22222e1a22222e1a22222e1a22222e1a22222e1a22222e1a22222e1a22222" // 64hex, must match ^sha256:[a-f0-9]{64}$
}
```

Probe may ignore `diff_files` (e.g., `build` always builds all). `budget_ms` includes probe startup.

## 6. `run` result

```json
{
  "verdict": "pass", // "pass" | "fail" | "inconclusive"
  "summary": "143 passed, 2 failed",
  "reason": "optional human reason when inconclusive",
  "data": {
    // probe-specific structured data, must be JSON-serializable
    // test probe example:
    "passed": ["auth.spec.ts::refresh-token"],
    "failed": [{"id":"net.spec.ts::timeout","message":"exceeded 2000ms"}],
    "skipped": [],
    "removed": [],
    "duration_ms": 12900
  },
  "artifacts": {
    "logs": "/absolute/path/to/logs.txt", // or inline base64 with prefix "base64:"
    "extra": {}
  }
}
```

* `verdict` is probe-local pass/fail, **not** the global `VERIFIED/BROKEN`. `differ` computes `delta` and `verdict` aggregation computes the global verdict.
* `data` should be as structured as possible; `differ` relies on `passed/failed/skipped` lists for test probes and `diagnostics[]` for typecheck. Unstructured probes still work but `delta` will be `summary`-only.
* `logs` should be truncated to last 200 KiB if huge; kernel stores them under `.vouch/bundles/<id>/blobs/`.

## 7. Discovery

* **Builtins** (`build`, `test`, `typecheck`) are compiled into `vouch` and implement
  `probe.Runner` **in-process** (no `Path`, no exec-self double spawn). External probes
  are wrapped by `probe.HostRunner`, which adapts the same stdio protocol to the same
  `Runner` interface — the kernel never branches on probe origin.
* `run.command` (optional) carries the selector-applied command so both base and
  candidate run the exact same target set; when absent the probe uses the profile
  command for its name.
* **Third-party**: any executable file named `vouch-probe-*` or `probe-*` under `~/.vouch/probes/` or `<repo>/.vouch/probes/` is discovered and `initialize` is probed. If `initialize` fails or times out (3s), the file is ignored.
* `vouch probe list` shows discovered probes and their capabilities.

## 8. Example: minimal shell probe

```bash
#!/usr/bin/env bash
set -euo pipefail
read line # initialize
echo '{"jsonrpc":"2.0","id":1,"result":{"name":"hello","version":"0.1.0","capabilities":{"differential":false,"kind":"inferred"}}}'
read line # run
echo '{"jsonrpc":"2.0","id":2,"result":{"verdict":"pass","summary":"hello","data":{"msg":"world"}}}'
read line # shutdown
echo '{"jsonrpc":"2.0","id":3,"result":{}}'
```

## 9. Versioning

* `protocol_version` is integer, current `1`. Probes must reject unknown major versions.
* `schema_version` in bundles is independent but bumped together when wire format changes.
* Changes are documented in `docs/adr/` and `CHANGELOG.md`.

## 10. Invariants & Defensive Host

* **Differ contract:** If a probe reports `delta.regressions` non-empty, its `verdict` must be `fail`. The kernel's `internal/verdict.Aggregate` is defensive: a `pass` with regressions is demoted to `inconclusive` (UNVERIFIED) with a warning, so a buggy probe can never silently hide a regression as VERIFIED.
* **Host is pure isolation:** crashes, timeouts, malformed JSON, missing binary → `inconclusive` with `reason`, never `fail` the whole `verify`. Verified by `internal/probe/host_test.go`.
