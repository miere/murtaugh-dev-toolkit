// Package llm is Murtaugh's provider-agnostic LLM boundary. It defines the
// message/tool/stream types the native agent loop speaks and a single Provider
// interface that streams a completion. Concrete providers (gemini /
// anthropic-compat / openai-compat) wrap github.com/voocel/litellm and land in
// litellm.go (T1); this file is the contract every other Wave-1 task codes
// against.
//
// Design note (the whole reason we went native): the native loop OWNS this
// message array. Per-turn context (time, cwd, skills index, conversation
// context) is folded into Request.System — never appended as a standalone
// RoleUser message after a tool result. That is the structural fix for the
// Goose MOIM consecutive-`user` empty-completion bug.
package llm

import (
	"context"
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
)

// Role identifies the author of a conversation message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one entry in the provider conversation array.
//
//   - A user turn sets Role=RoleUser and Text.
//   - An assistant turn sets Role=RoleAssistant with Text and/or ToolCalls.
//   - A tool result sets Role=RoleTool, Text (the result payload), ToolCallID
//     (correlating back to the assistant's ToolCall.ID) and ToolName.
type Message struct {
	Role       Role
	Text       string
	ToolCalls  []ToolCall
	ToolCallID string
	ToolName   string
}

// ToolCall is a model request to invoke a tool. Arguments is the raw JSON object
// the model produced; the loop hands it to the matching tools.Tool unchanged.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ToolSpec advertises a callable tool to the provider. Schema is the tool's JSON
// Schema (the same *jsonschema.Schema a tools.Tool exposes via InputSchema).
type ToolSpec struct {
	Name        string
	Description string
	Schema      *jsonschema.Schema
}

// Request is a single streamed completion call. The loop rebuilds System every
// turn (see package doc); Messages is the owned conversation array.
type Request struct {
	Model       string
	System      string
	Messages    []Message
	Tools       []ToolSpec
	MaxTokens   int     // 0 = provider default
	Temperature float64 // 0 = provider default
}

// StreamEvent is one increment of a streamed completion. Exactly one of
// TextDelta, ToolCall, or Err is meaningful on a given event; Done marks the
// terminal event, on which StopReason and Usage are populated.
type StreamEvent struct {
	TextDelta  string
	ToolCall   *ToolCall
	StopReason string
	Usage      *Usage
	Done       bool
	Err        error
}

// Usage carries token accounting reported by the provider on the final event.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Provider streams a completion for a Request. Implementations are constructed
// per agent from its profile + .env credentials (see resolve.go, T1) and wrap
// litellm for the gemini / anthropic-compat / openai-compat families.
type Provider interface {
	Stream(ctx context.Context, req Request) (<-chan StreamEvent, error)
}
