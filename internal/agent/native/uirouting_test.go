package native

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/llm"
	"github.com/miere/murtaugh/internal/tools"
)

// TestRun_ToolActivityIsTaskEventsOnly is the regression for the Slack UX bug
// where tool execution was mixed into the answer and the "Done thinking" status
// landed below it. Tool activity must surface ONLY as EventTask (which the
// gateway renders in the thinking surface, above the answer) — never as
// EventStatus or EventText, both of which the gateway streams into the answer
// message itself. The answer text must carry only the final reply.
func TestRun_ToolActivityIsTaskEventsOnly(t *testing.T) {
	prov := &fakeProvider{turns: []scriptedTurn{
		{toolCalls: []llm.ToolCall{{ID: "c1", Name: "alpha", Arguments: []byte(`{}`)}}, stopReason: "tool_use"},
		{text: "the final answer", stopReason: "end_turn"},
	}}
	alpha := &fakeTool{name: "alpha", result: "result"}
	loop := NewLoop(prov, "m", []tools.Tool{alpha}, 5)
	conv := NewConversation()
	conv.AppendUser("hi")

	var (
		evs            []agent.Event
		text           strings.Builder
		sawToolTask    bool
		sawStatusEvent bool
	)
	if _, err := loop.Run(context.Background(), conv, "sys", func(e agent.Event) { evs = append(evs, e) }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, e := range evs {
		switch e.Type {
		case agent.EventText:
			text.WriteString(e.Text)
		case agent.EventStatus:
			sawStatusEvent = true
		case agent.EventTask:
			if e.Task != nil && e.Task.Status == agent.TaskStatusInProgress && e.Task.Title == "alpha" {
				sawToolTask = true
			}
		}
	}

	if sawStatusEvent {
		t.Error("tool activity must not emit EventStatus (it would pollute the answer message)")
	}
	if !sawToolTask {
		t.Error("expected an in-progress EventTask for the tool")
	}
	if got := text.String(); got != "the final answer" {
		t.Errorf("answer text = %q, want only the final reply (no tool names)", got)
	}
}
