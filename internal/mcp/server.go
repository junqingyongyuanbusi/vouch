// Server construction and tool registration. The package contract is in doc.go.
package mcp

import (
	"context"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/junqingyongyuanbusi/vouch/internal/version"
)

// ServerName is reported to clients during initialize.
const ServerName = "vouch"

// instructions tell the agent how to read the tri-state verdict. They are part
// of the contract, not decoration: an agent that treats UNVERIFIED as success
// defeats the whole point of the tool.
const instructions = "vouch verifies a change differentially: it runs the same probes on the " +
	"base and candidate worktrees and reports the delta. Always read the exit_code " +
	"semantics: 0=VERIFIED (no regressions), 1=BROKEN (regressions, fix them), " +
	"2=UNVERIFIED (could not measure — do not claim success). Never treat " +
	"UNVERIFIED as a pass, and never merge on it without human review."

// New builds the MCP server for a backend. Version is reported to clients.
func New(backend Backend, ver string) *sdkmcp.Server {
	if ver == "" {
		ver = version.Version
	}
	s := sdkmcp.NewServer(&sdkmcp.Implementation{Name: ServerName, Version: ver},
		&sdkmcp.ServerOptions{Instructions: instructions})

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name:  "vouch_verify",
		Title: "Verify a change with differential evidence",
		Description: "Run vouch on a repository: the same probes execute on the base and candidate " +
			"worktrees and only the delta is judged. Returns VERIFIED (no regressions), BROKEN " +
			"(regressions exist: read them and fix them), or UNVERIFIED (the change could not be " +
			"measured — report that honestly instead of claiming success). Deterministic evidence " +
			"only; no LLM is involved.",
	}, verifyHandler(backend))

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name:  "vouch_show",
		Title: "Read a stored evidence bundle",
		Description: "Load a bundle previously written under .vouch/bundles/ (latest when bundle_id is " +
			"omitted). Use it to re-read the evidence behind a verdict without re-running probes.",
	}, showHandler(backend))

	sdkmcp.AddTool(s, &sdkmcp.Tool{
		Name:  "vouch_gaps",
		Title: "Explain what vouch could not verify",
		Description: "Run detection only (no probes, no writes) and report which commands were found, " +
			"from which file, and what could not be determined. Call it before trusting a verdict: a " +
			"VERIFIED with gaps means the change was verified only along the detected commands.",
	}, gapsHandler(backend))

	return s
}

// ServeStdio runs the MCP server on stdin/stdout until the client disconnects.
func ServeStdio(ctx context.Context, backend Backend, ver string) error {
	return New(backend, ver).Run(ctx, &sdkmcp.StdioTransport{})
}

// verifyHandler adapts Backend.Verify to a tool. A backend error becomes a tool
// error (isError), never a protocol error, so the agent can read the reason.
func verifyHandler(b Backend) sdkmcp.ToolHandlerFor[VerifyRequest, VerifyResult] {
	return func(ctx context.Context, _ *sdkmcp.CallToolRequest, in VerifyRequest) (*sdkmcp.CallToolResult, VerifyResult, error) {
		res, err := b.Verify(ctx, in)
		if err != nil {
			return nil, VerifyResult{}, err
		}
		return textResult(verifyText(res)), res, nil
	}
}

// showHandler adapts Backend.Show to a tool.
func showHandler(b Backend) sdkmcp.ToolHandlerFor[ShowRequest, ShowResult] {
	return func(ctx context.Context, _ *sdkmcp.CallToolRequest, in ShowRequest) (*sdkmcp.CallToolResult, ShowResult, error) {
		res, err := b.Show(ctx, in)
		if err != nil {
			return nil, ShowResult{}, err
		}
		return textResult(showText(res)), res, nil
	}
}

// gapsHandler adapts Backend.Gaps to a tool.
func gapsHandler(b Backend) sdkmcp.ToolHandlerFor[GapsRequest, GapsResult] {
	return func(ctx context.Context, _ *sdkmcp.CallToolRequest, in GapsRequest) (*sdkmcp.CallToolResult, GapsResult, error) {
		res, err := b.Gaps(ctx, in)
		if err != nil {
			return nil, GapsResult{}, err
		}
		return textResult(res.Summary), res, nil
	}
}

// textResult sets the human-readable content explicitly, so clients that ignore
// structuredContent still get something an agent can act on.
func textResult(text string) *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: text}}}
}
