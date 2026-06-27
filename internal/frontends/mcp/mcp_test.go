package mcp

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/murtaugh/internal/tools"
)

type fakeTool struct {
	name   string
	schema *jsonschema.Schema
	result any
}

func (f *fakeTool) Name() string                    { return f.name }
func (f *fakeTool) Description() string             { return "fake tool" }
func (f *fakeTool) InputSchema() *jsonschema.Schema { return f.schema }
func (f *fakeTool) Invoke(_ context.Context, _ map[string]any) (any, error) {
	return f.result, nil
}

func newConnectedClient(t *testing.T, f *Frontend) *mcpsdk.ClientSession {
	t.Helper()
	ctx := context.Background()

	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	server := f.Server()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server.Connect: %v", err)
	}

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "v0.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestServer_ListsRegisteredTool(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "ping", result: "pong"})

	session := newConnectedClient(t, New(reg))

	res, err := session.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "ping" {
		t.Fatalf("ListTools = %+v, want one ping tool", res.Tools)
	}
}

func TestServer_CallTool_StringResult_PassesThrough(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "ping", result: "pong"})

	session := newConnectedClient(t, New(reg))

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "ping"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned error result: %+v", res)
	}
	text, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("CallTool content = %T, want *TextContent", res.Content[0])
	}
	if text.Text != "pong" {
		t.Fatalf("CallTool text = %q, want %q", text.Text, "pong")
	}
}

func TestServer_CallTool_StructResult_JSONMarshalled(t *testing.T) {
	type out struct {
		OK   bool   `json:"ok"`
		Name string `json:"name"`
	}
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name: "jobs.run",
		schema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"name": {Type: "string"}},
			Required:   []string{"name"},
		},
		result: out{OK: true, Name: "demo"},
	})
	session := newConnectedClient(t, New(reg))

	// A dotted registry name ("jobs.run") is published dot-free ("jobs_run"),
	// and the call dispatches on the published id.
	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "jobs_run",
		Arguments: map[string]any{"name": "demo"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned error result: %+v", res)
	}
	text := res.Content[0].(*mcpsdk.TextContent).Text
	var got out
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("result text not valid JSON: %v; text=%q", err, text)
	}
	if got != (out{OK: true, Name: "demo"}) {
		t.Fatalf("decoded = %+v, want {OK:true Name:demo}", got)
	}
}

func TestServer_PublishesInputSchema(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name: "jobs.run",
		schema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"name": {Type: "string"}},
			Required:   []string{"name"},
		},
		result: "ok",
	})
	session := newConnectedClient(t, New(reg))

	res, err := session.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	raw, err := json.Marshal(res.Tools[0].InputSchema)
	if err != nil {
		t.Fatalf("marshal input schema: %v", err)
	}
	var got struct {
		Type     string   `json:"type"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v; raw=%s", err, string(raw))
	}
	if got.Type != "object" {
		t.Fatalf("schema type = %q, want object", got.Type)
	}
	if len(got.Required) != 1 || got.Required[0] != "name" {
		t.Fatalf("required = %v, want [name]", got.Required)
	}
}

func TestServer_PublishesDotFreeName(t *testing.T) {
	// Murtaugh's tools are dot-namespaced (jobs.define, journal.query, …) but
	// providers such as Gemini reject the dot in a function name. The MCP
	// frontend must publish a dot-free id.
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "jobs.define", result: "ok"})

	session := newConnectedClient(t, New(reg))

	res, err := session.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "jobs_define" {
		t.Fatalf("ListTools = %+v, want one tool published as jobs_define", res.Tools)
	}
	if !validMCPName.MatchString(res.Tools[0].Name) {
		t.Fatalf("published name %q is not a valid MCP/LLM identifier", res.Tools[0].Name)
	}
}

// validMCPName mirrors the strictest provider constraint (Gemini's
// function-name regex) so the test fails if a published name regresses to
// containing a dot, space, or other disallowed character.
var validMCPName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func TestMCPToolName(t *testing.T) {
	cases := map[string]string{
		"jobs.define":        "jobs_define",
		"journal.query":      "journal_query",
		"setup.mcp-register": "setup_mcp-register", // hyphen already valid, kept
		"slack.send-msg":     "slack_send-msg",
		"ping":               "ping", // already valid, unchanged
		"a.b.c":              "a_b_c",
	}
	for in, want := range cases {
		if got := mcpToolName(in); got != want {
			t.Errorf("mcpToolName(%q) = %q, want %q", in, got, want)
		}
		if !validMCPName.MatchString(mcpToolName(in)) {
			t.Errorf("mcpToolName(%q) = %q is not a valid MCP/LLM identifier", in, mcpToolName(in))
		}
	}
}

func TestServer_PanicsOnNameCollision(t *testing.T) {
	// Two registry keys that sanitise to the same published name must fail
	// loudly rather than silently shadow each other.
	// "a.b" and "a_b" are distinct registry keys that both sanitise to "a_b".
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "a.b", result: "x"})
	reg.Register(&fakeTool{name: "a_b", result: "y"})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Server() did not panic on a published-name collision")
		}
	}()
	_ = New(reg).Server()
}

// gatedTool opts into approval and records whether Invoke ran.
type gatedTool struct {
	name     string
	requires bool
	summary  string
	invoked  *bool
}

func (g *gatedTool) Name() string                          { return g.name }
func (g *gatedTool) Description() string                   { return "gated tool" }
func (g *gatedTool) InputSchema() *jsonschema.Schema       { return nil }
func (g *gatedTool) RequiresApproval(map[string]any) bool  { return g.requires }
func (g *gatedTool) ApprovalSummary(map[string]any) string { return g.summary }
func (g *gatedTool) Invoke(context.Context, map[string]any) (any, error) {
	if g.invoked != nil {
		*g.invoked = true
	}
	return "ran", nil
}

// fakeApprover answers Approve with a fixed verdict and records the summary it saw.
type fakeApprover struct {
	allow      bool
	note       string
	gotTool    string
	gotSummary string
}

func (a *fakeApprover) Approve(_ context.Context, toolName, summary string) (bool, string) {
	a.gotTool, a.gotSummary = toolName, summary
	return a.allow, a.note
}

func callTool(t *testing.T, session *mcpsdk.ClientSession, name string) *mcpsdk.CallToolResult {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name})
	if err != nil {
		t.Fatalf("CallTool(%q): %v", name, err)
	}
	return res
}

func textOf(t *testing.T, res *mcpsdk.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatalf("result has no content: %+v", res)
	}
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("first content is not text: %T", res.Content[0])
	}
	return tc.Text
}

func TestAggregator_GateDeniesAndSkipsInvoke(t *testing.T) {
	invoked := false
	tool := &gatedTool{name: "jobs.run", requires: true, summary: "run nightly job", invoked: &invoked}
	approver := &fakeApprover{allow: false, note: "denied by human"}

	session := newConnectedClient(t, NewFromTools([]tools.Tool{tool}, approver))
	res := callTool(t, session, "jobs_run")

	if invoked {
		t.Fatal("Invoke ran despite the call being denied")
	}
	if got := textOf(t, res); got != "denied by human" {
		t.Fatalf("denial result = %q, want the approver note", got)
	}
	if approver.gotTool != "jobs.run" || approver.gotSummary != "run nightly job" {
		t.Fatalf("approver saw tool=%q summary=%q, want the unsanitised name and the ApprovalSummary", approver.gotTool, approver.gotSummary)
	}
}

func TestAggregator_GateAllowsAndInvokes(t *testing.T) {
	invoked := false
	tool := &gatedTool{name: "jobs.run", requires: true, summary: "run", invoked: &invoked}
	approver := &fakeApprover{allow: true}

	session := newConnectedClient(t, NewFromTools([]tools.Tool{tool}, approver))
	res := callTool(t, session, "jobs_run")

	if !invoked {
		t.Fatal("Invoke did not run after approval")
	}
	if got := textOf(t, res); got != "ran" {
		t.Fatalf("result = %q, want the tool output", got)
	}
}

func TestAggregator_NoApproverNeverGates(t *testing.T) {
	invoked := false
	tool := &gatedTool{name: "jobs.run", requires: true, invoked: &invoked}

	session := newConnectedClient(t, NewFromTools([]tools.Tool{tool}, nil))
	_ = callTool(t, session, "jobs_run")

	if !invoked {
		t.Fatal("Invoke did not run when no approver is configured")
	}
}
