package native

import (
	"fmt"

	"github.com/miere/murtaugh-dev-toolkit/internal/llm"
)

// Conversation owns the provider-facing message array for one native session.
//
// It is the structural heart of the empty-reply fix. Goose's MOIM bug
// (see ignore/goose_moim_and_hermes_consecutive_msg_merger.md) was caused by
// injecting per-turn context (time, cwd, skills index, conversation-context) as
// a STANDALONE user message positioned immediately after a tool result. That
// produced a tool_result(user-effective) → user pair that providers answer with
// an empty completion.
//
// Conversation prevents that class of bug by construction: it exposes ONLY
// Append helpers for the three legitimate message kinds (user turn, assistant
// turn, tool result). There is deliberately no API to append per-turn context as
// a message — that context lives in the system prompt (see prompt.go), which is
// rebuilt every turn and passed via llm.Request.System. The defensive
// assertNoConsecutiveUserAfterTool guard makes the invariant testable.
type Conversation struct {
	messages []llm.Message
}

// NewConversation returns an empty Conversation.
func NewConversation() *Conversation {
	return &Conversation{}
}

// Messages returns the owned message array as sent to the provider. The returned
// slice is a copy of the header, but callers must treat it as read-only.
func (c *Conversation) Messages() []llm.Message {
	out := make([]llm.Message, len(c.messages))
	copy(out, c.messages)
	return out
}

// Len reports how many messages the conversation holds.
func (c *Conversation) Len() int { return len(c.messages) }

// AppendUser appends a genuine user turn (the human's Slack message). This is
// the ONLY way a RoleUser message enters the array — per-turn system context is
// never appended here; it belongs in the system prompt.
func (c *Conversation) AppendUser(text string) {
	c.messages = append(c.messages, llm.Message{
		Role: llm.RoleUser,
		Text: text,
	})
}

// AppendAssistant appends an assistant turn carrying optional text and/or the
// tool calls the model requested. Either may be empty; an assistant turn with
// tool calls is what the following tool-result messages correlate against.
func (c *Conversation) AppendAssistant(text string, toolCalls []llm.ToolCall) {
	c.messages = append(c.messages, llm.Message{
		Role:      llm.RoleAssistant,
		Text:      text,
		ToolCalls: toolCalls,
	})
}

// AppendToolResult appends the result of one tool invocation, correlated to the
// assistant's ToolCall.ID. RoleTool is the effective "tool" role; it must follow
// an assistant turn that requested the call and must never be followed by a
// standalone user message (only by another tool result or an assistant turn).
func (c *Conversation) AppendToolResult(id, name, text string) {
	c.messages = append(c.messages, llm.Message{
		Role:       llm.RoleTool,
		ToolCallID: id,
		ToolName:   name,
		Text:       text,
	})
}

// effectiveRole mirrors Goose/Hermes' notion of effective role: a tool result is
// "tool", everything else maps to its declared role. Used by the guard.
func effectiveRole(m llm.Message) llm.Role {
	if m.Role == llm.RoleTool || m.ToolCallID != "" {
		return llm.RoleTool
	}
	return m.Role
}

// assertNoConsecutiveUserAfterTool is the defensive guard that encodes the
// entire point of going native: the message array sent to a provider must never
// contain a tool-result (effective role "tool") immediately followed by a
// standalone user message. That exact sequence is the Goose MOIM bug. It returns
// an error describing the offending index pair, or nil when the array is clean.
//
// It is exported-for-tests via the package-internal test files and is also safe
// to call in production as a cheap invariant check before a provider call.
func assertNoConsecutiveUserAfterTool(msgs []llm.Message) error {
	for i := 1; i < len(msgs); i++ {
		prev := effectiveRole(msgs[i-1])
		cur := effectiveRole(msgs[i])
		if prev == llm.RoleTool && cur == llm.RoleUser {
			return fmt.Errorf(
				"native: malformed message array — tool-result at index %d immediately followed by standalone user message at index %d (this is the Goose MOIM consecutive-user empty-reply bug)",
				i-1, i,
			)
		}
	}
	return nil
}
