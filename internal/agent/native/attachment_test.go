package native

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/llm"
	"github.com/miere/murtaugh/internal/tools"
)

// A tool that returns an *agent.AttachmentEvent makes the loop emit an
// EventAttachment to the user and feed only an acknowledgement — never the bytes
// — back into the conversation array.
func TestRun_ToolAttachmentEmitsEventAndAcks(t *testing.T) {
	prov := &fakeProvider{turns: []scriptedTurn{
		{
			text: "here you go",
			toolCalls: []llm.ToolCall{
				{ID: "a1", Name: "attach", Arguments: rawArgs(t, map[string]any{"path": "/tmp/report.pdf"})},
			},
		},
		{text: "done", stopReason: "end_turn"},
	}}
	att := &fakeTool{name: "attach", result: &agent.AttachmentEvent{Filename: "report.pdf", Path: "/tmp/report.pdf"}}

	loop := NewLoop(prov, "test-model", []tools.Tool{att}, 10)
	conv := NewConversation()
	conv.AppendUser("send me the report")
	emit, evs := newCollector()

	if _, err := loop.Run(context.Background(), conv, "SYS", emit); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	atts := eventsOfType(*evs, agent.EventAttachment)
	if len(atts) != 1 {
		t.Fatalf("EventAttachment count = %d, want 1", len(atts))
	}
	if atts[0].Attachment == nil || atts[0].Attachment.Filename != "report.pdf" {
		t.Fatalf("attachment event = %+v, want filename report.pdf", atts[0].Attachment)
	}

	// The model receives a delivery acknowledgement as the tool result, not bytes.
	var toolResult string
	for _, m := range conv.Messages() {
		if m.Role == llm.RoleTool && m.ToolCallID == "a1" {
			toolResult = m.Text
		}
	}
	if !strings.Contains(toolResult, "report.pdf") || !strings.Contains(strings.ToLower(toolResult), "delivered") {
		t.Fatalf("tool result fed to model = %q, want a delivery ack naming the file", toolResult)
	}
}
