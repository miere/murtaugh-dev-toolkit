package native

import (
	"context"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/llm"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

type fakeApprover struct {
	allow  bool
	note   string
	called int
}

func (f *fakeApprover) Approve(_ context.Context, _, _ string) (bool, string) {
	f.called++
	return f.allow, f.note
}

// gatedTool optionally classifies its call as needing approval and counts runs.
type gatedTool struct {
	requires bool
	runs     int
}

func (g *gatedTool) Name() string                         { return "gated" }
func (g *gatedTool) Description() string                  { return "x" }
func (g *gatedTool) InputSchema() *jsonschema.Schema      { return nil }
func (g *gatedTool) RequiresApproval(map[string]any) bool { return g.requires }
func (g *gatedTool) Invoke(context.Context, map[string]any) (any, error) {
	g.runs++
	return "ran", nil
}

func runInvoke(l *Loop) string {
	return l.invokeTool(context.Background(), llm.ToolCall{ID: "1", Name: "gated"}, func(agent.Event) {})
}

func TestInvokeTool_GateDenied(t *testing.T) {
	tool := &gatedTool{requires: true}
	appr := &fakeApprover{allow: false, note: "Denied by the user."}
	out := runInvoke(NewLoop(nil, "m", []tools.Tool{tool}, 1).WithApprover(appr))

	if appr.called != 1 {
		t.Fatalf("approver called %d times, want 1", appr.called)
	}
	if tool.runs != 0 {
		t.Fatalf("tool ran %d times despite denial, want 0", tool.runs)
	}
	if out != "Denied by the user." {
		t.Fatalf("result = %q, want the denial note", out)
	}
}

func TestInvokeTool_GateApproved(t *testing.T) {
	tool := &gatedTool{requires: true}
	appr := &fakeApprover{allow: true}
	out := runInvoke(NewLoop(nil, "m", []tools.Tool{tool}, 1).WithApprover(appr))

	if appr.called != 1 {
		t.Fatalf("approver called %d times, want 1", appr.called)
	}
	if tool.runs != 1 || out != "ran" {
		t.Fatalf("tool runs=%d out=%q, want 1/\"ran\"", tool.runs, out)
	}
}

func TestInvokeTool_NotClassified_NotGated(t *testing.T) {
	tool := &gatedTool{requires: false}
	appr := &fakeApprover{allow: false}
	out := runInvoke(NewLoop(nil, "m", []tools.Tool{tool}, 1).WithApprover(appr))

	if appr.called != 0 {
		t.Fatalf("approver should not be consulted for a non-side-effecting call, called %d", appr.called)
	}
	if tool.runs != 1 || out != "ran" {
		t.Fatalf("tool runs=%d out=%q, want 1/\"ran\"", tool.runs, out)
	}
}

func TestInvokeTool_NoApprover_NotGated(t *testing.T) {
	// requires approval, but no approver wired (CLI/delegated): the call runs.
	tool := &gatedTool{requires: true}
	out := runInvoke(NewLoop(nil, "m", []tools.Tool{tool}, 1))

	if tool.runs != 1 || out != "ran" {
		t.Fatalf("tool runs=%d out=%q, want 1/\"ran\"", tool.runs, out)
	}
}
