package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/junqingyongyuanbusi/vouch/internal/mcp"
)

// fakeBackend records what was asked and returns canned data, so the tests
// assert the tool contract without running a verification.
type fakeBackend struct {
	verifyReq mcp.VerifyRequest
	showReq   mcp.ShowRequest
	gapsReq   mcp.GapsRequest
	verifyErr error
}

func (f *fakeBackend) Verify(_ context.Context, req mcp.VerifyRequest) (mcp.VerifyResult, error) {
	f.verifyReq = req
	if f.verifyErr != nil {
		return mcp.VerifyResult{}, f.verifyErr
	}
	return mcp.VerifyResult{
		Verdict: "BROKEN", ExitCode: 1, BundleID: "a3f9e2",
		Regressions: []mcp.Regression{{Probe: "test", ID: "t::x", Message: "boom"}},
		Probes:      []mcp.ProbeResult{{Probe: "test", Verdict: "fail", Regressions: []string{"t::x"}}},
		Reproduce:   "vouch rerun a3f9e2 --probe test",
		Feedback:    "vouch: BROKEN (exit 1) — regressions\n",
		NextAction:  "fix them",
	}, nil
}

func (f *fakeBackend) Show(_ context.Context, req mcp.ShowRequest) (mcp.ShowResult, error) {
	f.showReq = req
	return mcp.ShowResult{BundleID: "a3f9e2", Verdict: "VERIFIED"}, nil
}

func (f *fakeBackend) Gaps(_ context.Context, req mcp.GapsRequest) (mcp.GapsResult, error) {
	f.gapsReq = req
	return mcp.GapsResult{Path: req.Path, Confidence: "low", Summary: "detection confidence: low\n"}, nil
}

// connect starts the server over the SDK's in-memory transport and returns a
// client session: a real MCP handshake, not a re-implementation of one.
func connect(t *testing.T, be mcp.Backend) *sdkmcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	server := mcp.New(be, "0.1.0-test")
	ss, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Close() })
	return cs
}

func structured[T any](t *testing.T, res *sdkmcp.CallToolResult) T {
	t.Helper()
	var out T
	data, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("structured content is not marshalable: %v", err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("structured content does not decode: %v (%s)", err, data)
	}
	return out
}

func textOf(res *sdkmcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestToolsList_AdvertisesTheThreeToolsWithSchemas(t *testing.T) {
	cs := connect(t, &fakeBackend{})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	got := map[string]*sdkmcp.Tool{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool
		if tool.Description == "" {
			t.Fatalf("tool %s needs a description", tool.Name)
		}
	}
	for _, want := range []string{"vouch_verify", "vouch_show", "vouch_gaps"} {
		tool, ok := got[want]
		if !ok {
			t.Fatalf("tools/list must include %s, got %v", want, got)
		}
		if tool.InputSchema == nil {
			t.Fatalf("tool %s must carry an input schema", want)
		}
	}
	// The schema is generated from our input structs: the agent must be able to
	// discover the arguments without reading our source.
	schema, _ := json.Marshal(got["vouch_verify"].InputSchema)
	for _, field := range []string{"path", "from", "to", "intent", "budget_ms", "install_deps"} {
		if !strings.Contains(string(schema), `"`+field+`"`) {
			t.Fatalf("vouch_verify schema must document %q: %s", field, schema)
		}
	}
}

func TestVerifyTool_ReturnsStructuredAndTextContent(t *testing.T) {
	be := &fakeBackend{}
	cs := connect(t, be)
	res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "vouch_verify",
		Arguments: map[string]any{"path": "/repo", "budget_ms": 1000},
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if res.IsError {
		t.Fatalf("a BROKEN verdict is a successful tool call: %+v", res)
	}
	out := structured[mcp.VerifyResult](t, res)
	if out.Verdict != "BROKEN" || out.ExitCode != 1 {
		t.Fatalf("verdict must be carried as data: %+v", out)
	}
	if len(out.Regressions) != 1 || out.Regressions[0].ID != "t::x" {
		t.Fatalf("regressions must be structured: %+v", out.Regressions)
	}
	if text := textOf(res); !strings.Contains(text, "BROKEN") || !strings.Contains(text, "bundle a3f9e2") {
		t.Fatalf("text content must stand on its own: %q", text)
	}
	if !strings.Contains(textOf(res), "next: fix them") {
		t.Fatalf("the agent needs the next action: %q", textOf(res))
	}
	if be.verifyReq.Path != "/repo" || be.verifyReq.BudgetMs != 1000 {
		t.Fatalf("arguments must reach the backend: %+v", be.verifyReq)
	}
}

func TestShowAndGapsTools_ReachTheBackend(t *testing.T) {
	be := &fakeBackend{}
	cs := connect(t, be)
	if _, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name: "vouch_show", Arguments: map[string]any{"bundle_id": "a3f9e2"},
	}); err != nil {
		t.Fatalf("vouch_show: %v", err)
	}
	if be.showReq.BundleID != "a3f9e2" {
		t.Fatalf("show arguments must reach the backend: %+v", be.showReq)
	}
	gaps, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name: "vouch_gaps", Arguments: map[string]any{"path": "/repo"},
	})
	if err != nil {
		t.Fatalf("vouch_gaps: %v", err)
	}
	if be.gapsReq.Path != "/repo" {
		t.Fatalf("gaps arguments must reach the backend: %+v", be.gapsReq)
	}
	if text := textOf(gaps); !strings.Contains(text, "detection confidence: low") {
		t.Fatalf("gaps text must summarise the finding: %q", text)
	}
}

func TestBackendFailureIsAToolError(t *testing.T) {
	cs := connect(t, &fakeBackend{verifyErr: errors.New("not a git repository")})
	res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "vouch_verify"})
	if err != nil {
		// A protocol-level error would be acceptable per spec, but the agent must
		// be able to read the reason either way.
		if !strings.Contains(err.Error(), "not a git repository") {
			t.Fatalf("the reason must survive: %v", err)
		}
		return
	}
	if !res.IsError || !strings.Contains(textOf(res), "not a git repository") {
		t.Fatalf("backend failure must be reported to the agent: %+v", res)
	}
}

func TestInvalidArgumentsAreRejectedBeforeTheHandler(t *testing.T) {
	be := &fakeBackend{}
	cs := connect(t, be)
	res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "vouch_verify",
		Arguments: map[string]any{"budget_ms": "not-a-number"},
	})
	if err == nil && !res.IsError {
		t.Fatalf("schema validation must reject a mistyped argument: %+v", res)
	}
	if be.verifyReq.BudgetMs != 0 || be.verifyReq.Path != "" {
		t.Fatalf("the handler must not run on invalid input: %+v", be.verifyReq)
	}
}

func TestUnknownToolIsRejected(t *testing.T) {
	cs := connect(t, &fakeBackend{})
	res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "vouch_nope"})
	if err == nil && !res.IsError {
		t.Fatalf("an unknown tool must not succeed: %+v", res)
	}
}

func TestInitialize_ReportsInstructionsAndVersion(t *testing.T) {
	cs := connect(t, &fakeBackend{})
	init := cs.InitializeResult()
	if init == nil {
		t.Fatal("initialize must produce a result")
	}
	if init.ServerInfo.Name != "vouch" || init.ServerInfo.Version != "0.1.0-test" {
		t.Fatalf("serverInfo must identify vouch: %+v", init.ServerInfo)
	}
	if !strings.Contains(init.Instructions, "UNVERIFIED") {
		t.Fatalf("instructions must state the tri-state contract: %q", init.Instructions)
	}
	if init.Capabilities.Tools == nil {
		t.Fatal("tools capability must be advertised")
	}
}
