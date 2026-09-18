# Contributing to VOUCH

Thanks for helping make `vouch` trustworthy. This doc is P0 — read it before your first PR.

## Principles (non-negotiable)

1. **Deterministic / inferred isolation** — `internal/bundle` keeps two arrays `evidence` / `inferences`. Inferred results never enter `internal/verdict` aggregation. Adding a probe must declare `kind` in `initialize`.
2. **Differential is atomic** — every probe runs on `base` and `candidate` worktrees, `internal/differ` produces `delta`. Single-side results are not evidence.
3. **Three states only** — `VERIFIED / BROKEN / UNVERIFIED`. Empty deterministic set → `UNVERIFIED`. No scores.
4. **Zero telemetry** — do not add pings, counts, or opt-in telemetry. Quality is measured by the canary set (`scripts/canary`), adoption by `npm` slope.
5. **No guessing** — if `Detector` cannot infer how to build/test, return `UNVERIFIED + gaps`, never hallucinate a command.

## Repo layout

```
cmd/vouch/            cobra entry (verify/mcp/rerun/gc/init/export)
internal/detector/    cascade: .github/workflows → Makefile → pkg scripts → heuristic
internal/selector/    affected-test selection (vitest related / go list / pytest)
internal/worktree/    git worktree pair (stash-create snapshot strategy)
internal/sandbox/     darwin/linux isolation (loopback allowlisted)
internal/probe/       JSON-lines over stdio host + discovery (~/.vouch/probes/)
internal/probe/builtin/ build/test/typecheck
internal/differ/      delta + flaky arbitration
internal/verdict/     pure function aggregation (exhaustively tested)
internal/bundle/      Evidence / ProofBundle / Profile types + content-addressed store
internal/cache/       BaselineCache = same store, keyed by (base_ref, env)
internal/mcp/         MCP server (verify tool, structured failures)
internal/tui/         bubbletea minimal triage
internal/render/      human / --json / markdown
docs/probe-protocol.md + docs/bundle-schema.json + docs/adr/
testdata/fixtures/    micro real projects for e2e
```

Dependency direction is strictly downward: `cmd → internal/{detector…scheduler} → internal/{worktree…probe} → internal/{bundle,verdict}`. `probe` host must not import builtin implementations (dependency inversion via protocol).

## Good first issues

Label `good first issue` is reserved for probe-type issues: protocol is single-file, language-agnostic, isolated. Start with `vouch probe scaffold`.

## Workflow

```bash
make tidy   # go mod tidy
make fmt    # gofmt -w
make vet    # go vet ./...
make lint   # golangci-lint run (required in CI)
make test   # go test ./...
make build  # → bin/vouch
```

PRs:
- Small, one concern per PR.
- Include `docs/adr/NNN-*.md` for any ADR-level decision.
- Update `docs/bundle-schema.json` and `schema_version` if bundle format changes.

## Writing a probe

Read `docs/probe-protocol.md` first — you should be able to write a probe without reading kernel code. Life-cycle: `initialize → run(workdir, role, profile, budget_ms, diff_files) → progress* → result → shutdown`. `needs_network` only allows loopback; external access must be allow-listed. Crash/timeout → `inconclusive`, never `fail` the whole verify.

## Writing an agent adapter

`vouch` is agent-neutral: the kernel knows nothing about any agent, and every
adapter is a thin translation of one agent's contract into the same two surfaces
(read `docs/agent-loop.md` first).

**Rule: one adapter to perfection before cloning.** P1.5 ships Claude Code only.
Until it has been used in the field, a second adapter would spread the same bugs
across two integrations instead of fixing them once.

An adapter has exactly two halves, both written by `vouch init <agent>`:

1. **MCP registration** — a server entry that launches `vouch mcp`
   (`.mcp.json` for Claude Code). The SDK serves the protocol; do not hand-roll
   JSON-RPC.
2. **Completion hook** — a config entry that runs `vouch hook <agent>`, plus a
   `claudeCodeHook`-style function in `cmd/vouch/hook.go` that translates the
   verdict into the agent's blocking contract.

Checklist for a new adapter PR:

* [ ] `resolveLauncher()` is reused for **both** halves. Hardcoding `vouch …`
      breaks every machine where the binary is not on `PATH` (the hook then dies
      with exit 127, which agents treat as non-blocking, silently disabling
      verification).
* [ ] The loop guard is implemented from the agent's own signal — Claude Code
      sends `stop_hook_active`, Cursor sends `loop_count` + `loop_limit`. Without
      it a BROKEN verdict can ping-pong forever.
* [ ] Exit-code mapping lives in Go, in one function with a comment naming the
      agent's contract. Never in a shell one-liner: escaping is a bug source and
      cannot be unit-tested.
* [ ] **BROKEN blocks; UNVERIFIED does not.** Blocking on "could not measure"
      teaches users to switch the hook off.
* [ ] A vouch failure fails **open** (the agent is not held hostage) and is
      printed to stderr.
* [ ] `init` stays idempotent, preserves the user's existing hooks and other MCP
      servers, and upgrades a previous-generation vouch hook instead of reporting
      "already present".
* [ ] Tests in `cmd/vouch/main_test.go`: both halves written, launchers agree,
      idempotency, legacy upgrade, and the hook exit codes (VERIFIED / BROKEN /
      loop guard / unsupported agent).
* [ ] `docs/agent-loop.md` gains a row in the exit-code table and the agent's
      scope note.

## Neutrality

`vouch` is agent-neutral: the kernel never learns which agent is running, and an
adapter may not add verification logic — it translates a contract and nothing
else. See "Writing an agent adapter" above.

## License

Apache-2.0. By contributing you agree your contributions are licensed under the same.
