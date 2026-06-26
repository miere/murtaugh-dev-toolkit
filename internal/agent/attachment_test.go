package agent

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func agentMessageUpdate(content []any) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"sessionId": "s1",
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       content,
		},
	})
	return raw
}

func TestExtractAttachments_ImageAndResourceBlob(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d}
	doc := []byte("%PDF-1.4 fake")
	raw := agentMessageUpdate([]any{
		map[string]any{"type": "text", "text": "here is the chart"},
		map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(png), "mimeType": "image/png"},
		map[string]any{"type": "resource", "resource": map[string]any{
			"uri": "file:///tmp/report.pdf", "mimeType": "application/pdf",
			"blob": base64.StdEncoding.EncodeToString(doc),
		}},
	})

	atts := extractAttachments(raw)
	if len(atts) != 2 {
		t.Fatalf("attachments = %d, want 2", len(atts))
	}
	if string(atts[0].Data) != string(png) || atts[0].Mimetype != "image/png" {
		t.Fatalf("image attachment = %+v", atts[0])
	}
	if atts[0].Filename != "image.png" {
		t.Fatalf("image filename = %q, want image.png (derived from mimetype)", atts[0].Filename)
	}
	if string(atts[1].Data) != string(doc) || atts[1].Filename != "report.pdf" {
		t.Fatalf("resource attachment = %+v, want report.pdf with the doc bytes", atts[1])
	}
}

func TestExtractAttachments_SkipsTextAndLinks(t *testing.T) {
	raw := agentMessageUpdate([]any{
		map[string]any{"type": "text", "text": "just words"},
		// A text resource carries no bytes to upload.
		map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///x.txt", "text": "inline text"}},
		// A resource_link is a pointer, not embedded bytes.
		map[string]any{"type": "resource_link", "uri": "https://example.com/a.png", "name": "a.png"},
	})
	if atts := extractAttachments(raw); len(atts) != 0 {
		t.Fatalf("attachments = %d, want 0 (text/links carry no bytes)", len(atts))
	}
}

func TestExtractAttachments_OnlyAgentMessages(t *testing.T) {
	// An image attached to a non-agent-message update (e.g. a tool call or the
	// echoed user message) is not the agent replying with a file.
	raw, _ := json.Marshal(map[string]any{
		"sessionId": "s1",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"content": []any{
				map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString([]byte("x")), "mimeType": "image/png"},
			},
		},
	})
	if atts := extractAttachments(raw); len(atts) != 0 {
		t.Fatalf("attachments = %d, want 0 (not an agent message)", len(atts))
	}
}
