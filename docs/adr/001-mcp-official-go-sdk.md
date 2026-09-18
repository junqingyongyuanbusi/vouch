# ADR 001 — MCP transport uses the official Go SDK

* Status: accepted
* Date: 2026-09-15
* Context: P1.5 (agent loop), PLAN-V2 §10 lists `MCP | 官方 Go SDK | 跟随协议演进`

## Decision

`vouch mcp` serves the Model Context Protocol through
`github.com/modelcontextprotocol/go-sdk`, pinned to **v1.4.0**.

Our own code keeps only the *product* surface: a `Backend` interface
(`Verify` / `Show` / `Gaps`), the request/result structs, and the tool
descriptions. The wire protocol, handshake, capability negotiation, schema
generation and input validation belong to the SDK.

## Why this version

| SDK | `go` directive |
|---|---|
| v1.4.0 | go 1.24.0 |
| v1.6.1+ | go 1.25.0 |

`go.mod` targets the Go 1.24 toolchain and CI installs Go 1.24, which is also
what `go install github.com/junqingyongyuanbusi/vouch/cmd/vouch@latest` will use for anyone
building from source. v1.4.0 is the newest release that does not force a
toolchain bump, so it is the pin. Upgrading is a deliberate step:

* bump the `go` directive and CI together,
* re-read the SDK changelog for protocol revisions (`initialize` versions,
  new content types, `structuredContent` behavior),
* re-run `go test ./internal/mcp ./cmd/vouch`.

## Consequences

* Protocol revisions are tracked upstream instead of by hand. The previous
  hand-written JSON-RPC layer had to enumerate protocol versions and implement
  `id:null` error semantics, message size limits and notification handling
  itself; every MCP revision would have been manual work.
* Tool input schemas are generated from Go structs (`jsonschema` tags), so the
  schema an agent sees cannot drift from the fields we actually read.
* Input validation happens before our handler, so a mistyped argument cannot
  start a verification.
* We gain dependencies (`go-sdk`, `jsonschema-go`, `golang-jwt`, `segcore`,
  `x/sys`). They are build-time only: the shipped binary stays a single static
  executable with no runtime dependencies, no daemon and no account.
* The MCP layer still cannot invent a verdict: it only serializes what the
  kernel produced, and the worst case of an SDK bug is a failed tool call.

## Alternatives considered

* **Hand-written JSON-RPC (previous implementation).** ~600 lines plus tests,
  fully deterministic, zero dependencies. Rejected because PLAN-V2 explicitly
  chose upstream tracking, and because each protocol revision would require
  hand-auditing version lists and error semantics. Its only real advantage —
  full control over the wire format — is not something this project needs.
* **Official SDK at v1.6+.** Rejected for now: requires Go ≥ 1.25, which would
  force a toolchain download on every contributor and CI runner.
