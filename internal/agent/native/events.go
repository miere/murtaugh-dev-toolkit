package native

import (
	"encoding/json"
	"strings"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
)

// approvalSummary renders a short human description of a tool call for the
// approval prompt. A `command` argument (the terminal tool) is shown verbatim;
// otherwise the args are compactly JSON-encoded.
func approvalSummary(args map[string]any) string {
	if cmd, ok := args["command"].(string); ok {
		if cmd = strings.TrimSpace(cmd); cmd != "" {
			return cmd
		}
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeToolArgs unmarshals a model-produced ToolCall.Arguments payload into the
// map[string]any a tools.Tool.Invoke expects. An empty or null payload yields a
// nil map so tools that take no parameters don't need to nil-check. Mirrors
// internal/frontends/mcp/mcp.go decodeArgs.
func decodeToolArgs(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// eventText builds a streamed-text agent.Event carrying a delta of the
// assistant's reply.
func eventText(text string) agent.Event {
	return agent.Event{Type: agent.EventText, Text: text}
}

// eventStatus builds a human-readable status agent.Event (e.g. "running tool
// foo"). Status text is informational progress, not part of the assistant reply.
func eventStatus(text string) agent.Event {
	return agent.Event{Type: agent.EventStatus, Text: text}
}

// eventTask builds a task-progress agent.Event for a single tool invocation, so
// the gateway can render a live task card around each tool call.
func eventTask(id, title string, status agent.TaskStatus, output string) agent.Event {
	return agent.Event{
		Type: agent.EventTask,
		Task: &agent.TaskEvent{
			ID:     id,
			Title:  title,
			Status: status,
			Output: output,
		},
	}
}

// eventComplete builds the terminal success agent.Event, carrying the loop's
// stop reason so the chat handler can surface a non-"end_turn" ending.
func eventComplete(stopReason string) agent.Event {
	return agent.Event{Type: agent.EventComplete, StopReason: stopReason}
}

// eventError builds the terminal failure agent.Event.
func eventError(err error) agent.Event {
	return agent.Event{Type: agent.EventError, Error: err}
}

// toolResultString adapts an arbitrary tools.Tool result into the text payload
// fed back to the model as a tool result. It mirrors the rules in
// internal/frontends/mcp/mcp.go renderJSON: strings pass through unchanged to
// keep trivial tools' output uncluttered; everything else is JSON-marshalled. On
// a marshal failure it falls back to Go's default formatting so the loop always
// has something to feed back rather than aborting.
func toolResultString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fallbackString(v)
	}
	return string(b)
}

func fallbackString(v any) string {
	// Avoid importing fmt at the top for one path; keep behaviour explicit.
	type stringer interface{ String() string }
	if s, ok := v.(stringer); ok {
		return s.String()
	}
	return ""
}
