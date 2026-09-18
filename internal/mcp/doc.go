// Package mcp — Model Context Protocol server (stdio) exposing VOUCH to agents.
//
// Layering (PLAN-V2 §9 P1.5, "Agent 回路"): this package is the agent surface
// only. Every verification runs behind the Backend interface, so the MCP layer
// never imports the scheduler and can never invent a verdict: it serializes
// what the kernel produced. A bug here can make a tool call fail, but it cannot
// turn a regression into VERIFIED.
//
// The wire protocol is served by the official Go SDK
// (github.com/modelcontextprotocol/go-sdk, PLAN-V2 §10), so protocol revisions
// and supported version lists are the SDK's business rather than ours. The SDK
// release is pinned in docs/adr/001-mcp-official-go-sdk.md.
//
// Tools exposed: vouch_verify, vouch_show, vouch_gaps.
package mcp
