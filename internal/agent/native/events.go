package native

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/miere/murtaugh/internal/agent"
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

// eventAttachment builds an agent.Event carrying a file the agent is delivering
// to the user as part of its reply. The chat handler uploads it into the turn's
// thread; it is never fed back to the model.
func eventAttachment(a *agent.AttachmentEvent) agent.Event {
	return agent.Event{Type: agent.EventAttachment, Attachment: a}
}

// attachmentsFromResult extracts the attachment(s) a tool returned, if any. A
// tool that delivers a file returns *agent.AttachmentEvent (or a slice of them);
// every other result yields nil and falls through to the normal text tool-result
// path. A plain agent.AttachmentEvent value is tolerated too, so a tool need not
// remember to return a pointer.
func attachmentsFromResult(v any) []*agent.AttachmentEvent {
	switch a := v.(type) {
	case *agent.AttachmentEvent:
		if a != nil {
			return []*agent.AttachmentEvent{a}
		}
	case []*agent.AttachmentEvent:
		out := make([]*agent.AttachmentEvent, 0, len(a))
		for _, x := range a {
			if x != nil {
				out = append(out, x)
			}
		}
		return out
	case agent.AttachmentEvent:
		cp := a
		return []*agent.AttachmentEvent{&cp}
	}
	return nil
}

// attachmentAck is the tool-result text fed back to the model after a tool's
// attachments were delivered to the user. The model never sees the bytes — only
// that the files were sent — so it can continue or close the turn coherently.
func attachmentAck(atts []*agent.AttachmentEvent) string {
	names := make([]string, 0, len(atts))
	for _, a := range atts {
		name := a.Filename
		if name == "" {
			name = a.Title
		}
		if name == "" {
			name = "file"
		}
		names = append(names, name)
	}
	if len(names) == 1 {
		return fmt.Sprintf("Delivered attachment %q to the user.", names[0])
	}
	return fmt.Sprintf("Delivered %d attachments to the user: %s.", len(names), strings.Join(names, ", "))
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
