package native

import (
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/llm"
)

func TestConversation_AppendHelpersBuildAlternatingArray(t *testing.T) {
	c := NewConversation()
	c.AppendUser("hello")
	c.AppendAssistant("on it", []llm.ToolCall{{ID: "1", Name: "t"}})
	c.AppendToolResult("1", "t", "result")
	c.AppendAssistant("done", nil)

	msgs := c.Messages()
	if len(msgs) != 4 {
		t.Fatalf("len = %d, want 4", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser {
		t.Errorf("msg0 role = %q", msgs[0].Role)
	}
	if msgs[1].Role != llm.RoleAssistant || len(msgs[1].ToolCalls) != 1 {
		t.Errorf("msg1 not an assistant tool-call turn: %#v", msgs[1])
	}
	if msgs[2].Role != llm.RoleTool || msgs[2].ToolCallID != "1" || msgs[2].ToolName != "t" {
		t.Errorf("msg2 not a correlated tool result: %#v", msgs[2])
	}
	if err := assertNoConsecutiveUserAfterTool(msgs); err != nil {
		t.Fatalf("clean array flagged: %v", err)
	}
}

func TestConversation_AppendUserHealsDanglingToolResult(t *testing.T) {
	// Tail is a tool-result (a prior turn ended on max_turns / error / cancel).
	c := NewConversation()
	c.AppendUser("q1")
	c.AppendAssistant("on it", []llm.ToolCall{{ID: "1", Name: "t"}})
	c.AppendToolResult("1", "t", "result") // <- dangling: no closing assistant turn

	c.AppendUser("q2")

	msgs := c.Messages()
	// A synthetic assistant message must have been inserted between the
	// tool-result and the new user turn.
	if msgs[len(msgs)-2].Role != llm.RoleAssistant {
		t.Fatalf("expected a synthetic assistant turn before the user message, got %#v", msgs[len(msgs)-2])
	}
	if msgs[len(msgs)-1].Role != llm.RoleUser || msgs[len(msgs)-1].Text != "q2" {
		t.Fatalf("expected the user turn last, got %#v", msgs[len(msgs)-1])
	}
	if err := assertNoConsecutiveUserAfterTool(msgs); err != nil {
		t.Fatalf("heal failed, array still malformed: %v", err)
	}
}

func TestConversation_AppendUserNoHealAfterAssistant(t *testing.T) {
	// When the tail is already an assistant turn, no synthetic message is added.
	c := NewConversation()
	c.AppendUser("q1")
	c.AppendAssistant("answer", nil)
	before := c.Len()
	c.AppendUser("q2")
	if c.Len() != before+1 {
		t.Fatalf("expected exactly one message appended, len %d -> %d", before, c.Len())
	}
}

func TestConversation_MessagesReturnsCopy(t *testing.T) {
	c := NewConversation()
	c.AppendUser("hi")
	got := c.Messages()
	got[0].Text = "mutated"
	if c.Messages()[0].Text != "hi" {
		t.Fatal("Messages() must return a copy, not the backing array")
	}
}

func TestAssertNoConsecutiveUserAfterTool(t *testing.T) {
	tests := []struct {
		name    string
		msgs    []llm.Message
		wantErr bool
	}{
		{
			name: "clean alternation",
			msgs: []llm.Message{
				{Role: llm.RoleUser, Text: "q"},
				{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1"}}},
				{Role: llm.RoleTool, ToolCallID: "1", Text: "r"},
				{Role: llm.RoleAssistant, Text: "a"},
			},
		},
		{
			name: "tool result then standalone user (the Goose MOIM bug)",
			msgs: []llm.Message{
				{Role: llm.RoleTool, ToolCallID: "1", Text: "r"},
				{Role: llm.RoleUser, Text: "<info-msg>..."},
			},
			wantErr: true,
		},
		{
			name: "user-effective tool result (RoleUser+ToolCallID) then user",
			msgs: []llm.Message{
				{Role: llm.RoleUser, ToolCallID: "1", Text: "tool result as user role"},
				{Role: llm.RoleUser, Text: "next"},
			},
			wantErr: true,
		},
		{
			name: "two tool results in a row are fine",
			msgs: []llm.Message{
				{Role: llm.RoleTool, ToolCallID: "1", Text: "r1"},
				{Role: llm.RoleTool, ToolCallID: "2", Text: "r2"},
			},
		},
		{
			name: "consecutive plain users (not after tool) is allowed by this guard",
			msgs: []llm.Message{
				{Role: llm.RoleUser, Text: "a"},
				{Role: llm.RoleUser, Text: "b"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := assertNoConsecutiveUserAfterTool(tt.msgs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}
