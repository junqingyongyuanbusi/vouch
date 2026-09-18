# Agent loop — how vouch plugs into an agent

VOUCH occupies one slot in an agent's workflow: **the moment the agent claims it
is done**. Two surfaces do that, and `vouch init <agent>` wires both in one
command.

```
agent edits code
      │
      ├─ (while working)  MCP tool call ─ vouch_verify ──────────────┐
      │                                                              │
      └─ (claims done)    Stop hook ─ vouch hook <agent> ─ vouch verify ─┐
                                                                   │     │
   VERIFIED (exit 0)  ──────────────────────────────────────────► merge/stop
   BROKEN   (exit 1)  ─ regressions + reproduce ────────────────► fix, then re-verify
   UNVERIFIED (exit 2)─ reason, non-blocking ────────────────────► report honestly
```

Neither surface may invent a verdict: the MCP layer and the hook both call the
same kernel and pass through its bundle. See `docs/bundle-schema.json`.

## 1. MCP server (`vouch mcp`)

`vouch init claude-code` registers it in `.mcp.json`:

```json
{ "mcpServers": { "vouch": { "command": "vouch", "args": ["mcp"] } } }
```

Tools:

| tool | purpose | cost |
|---|---|---|
| `vouch_verify` | run the full differential verification on the current change | whole pipeline |
| `vouch_show` | re-read a stored bundle (latest when no id) | reads disk only |
| `vouch_gaps` | what detection found / could not determine | detection only, no probes, no writes |

`vouch_verify` returns both text content (for the model) and
`structuredContent` (for the client):

```json
{
  "verdict": "BROKEN", "exit_code": 1, "bundle_id": "6f546f60",
  "regressions": [{ "probe": "test", "id": "src/add.test.ts::injected-regression", "message": "expected 4 but got 5" }],
  "removed_tests": [], "baseline": { "status": "all_green", "failing": 0 },
  "reproduce": "vouch rerun 6f546f60 --probe test",
  "unverified_claims": [], "next_action": "Regressions exist: fix them and call vouch_verify again."
}
```

Protocol notes: JSON-lines over stdio. The wire protocol is served by the
official Go SDK (`github.com/modelcontextprotocol/go-sdk`), so the supported
protocol versions are whatever that SDK accepts — currently `2025-11-25`,
`2025-06-18`, `2025-03-26` and `2024-11-05` — and the client's version is echoed
when supported. See `docs/adr/001-mcp-official-go-sdk.md` for the version pin.
Tool failures are reported as `isError` results, never as JSON-RPC errors, so
"the tool ran and said no" stays distinguishable from "the transport broke".
Tool input schemas are generated from the Go structs, so the arguments an agent
sees cannot drift from the fields we read; invalid arguments are rejected before
any verification starts.

`budget_ms` is the **total** wall-clock budget of one verify call (default
`600000`): detection, worktree setup and the sequential probes all draw from
it, and each probe command gets at most what is left when it starts. Probes
that no longer fit are listed in `unverified_claims` as
`skipped (budget exhausted)`, and a command killed by the deadline is reported
there with its reason (`killed by budget`) instead of a bare "inconclusive".
The transport context expires at `budget_ms` plus 60s, but this is **not a
strict return-time guarantee**. Context-aware operations — commit resolution,
worktree setup (git snapshot/add), dependency copying and the probes — are
canceled on expiry: their process groups are killed and reaped, so no helper
subprocess outlives the run. Pure filesystem cleanup (removing the worktree
directories) is not deadline-bounded and may run slightly past the deadline;
worktree cleanup failures are surfaced as warnings instead of being silent. No
background goroutine is used to return while setup keeps writing to disk.

## 2. Completion hook (`vouch hook <agent>`)

Exit codes carry the verdict, and each agent interprets them differently, so the
mapping lives in Go rather than in a shell one-liner:

| agent | blocking contract | vouch mapping |
|---|---|---|
| Claude Code (P1.5) | Stop hook: exit 2 = block + feed stderr to the model, other non-zero = non-blocking | BROKEN → 2 + evidence on stderr; VERIFIED/UNVERIFIED → 0 |

Rules that keep the loop honest:

* **UNVERIFIED never blocks.** If nothing could be measured, blocking would cry
  wolf and teach users to switch the hook off. The reason still reaches the
  transcript.
* **A vouch failure never blocks.** If the verifier itself breaks, the agent is
  not held hostage; the error is printed and the hook exits 0.
* **Loop guard.** Claude Code sets `stop_hook_active: true` when the agent is
  already continuing because of this hook; the adapter exits 0 immediately then,
  so the hook can never ping-pong forever.

## 3. Scope decisions

* **P1.5 ships one adapter** (Claude Code, `PLAN-V2 §7.5`). Cursor's `stop` hook
  is a different contract — it takes `{ status, loop_count }` on stdin and
  answers with `{"followup_message": "..."}` on stdout (auto-submitting the next
  user message, `loop_limit` default 5) — and is deliberately deferred until the
  Claude Code loop is proven in the field. `internal/mcp` and `vouch hook` are
  already structured so that adding it is one adapter function, not a redesign.
* No telemetry, at all. Hooks and MCP calls stay inside the machine.
* Deterministic evidence only: no LLM runs in the gate at any point.

## 4. Manual wiring

Without `vouch init`, the two pieces are:

```bash
# MCP (project scope)
echo '{"mcpServers":{"vouch":{"command":"vouch","args":["mcp"]}}}' > .mcp.json

# Stop hook (Claude Code, .claude/settings.json)
# {"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"vouch hook claude-code","timeout":900}]}]}}
```

If `vouch` is not on `PATH`, `vouch init` writes the npm launcher instead
(`npx --yes vouch mcp` / `npx --yes vouch hook claude-code`) so the loop works
with zero installation. Both files are generated from one resolved launcher —
a mismatch would silently disable half the loop.
