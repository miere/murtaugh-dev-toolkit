package updatemsg

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/client/slacktest"
)

func TestTool_Metadata(t *testing.T) {
	tool := New("")
	if tool.Name() != "slack.update-msg" {
		t.Fatalf("Name = %q", tool.Name())
	}
	schema := tool.InputSchema()
	req := map[string]bool{}
	for _, r := range schema.Required {
		req[r] = true
	}
	if !req["channel"] || !req["ts"] {
		t.Fatalf("schema required = %v, want channel + ts", schema.Required)
	}
}

func TestInvoke_PassesIDChannelThrough(t *testing.T) {
	fake := &slacktest.FakeAPI{
		UpdateResult: slacklib.PostMessageResult{Channel: "C9", TS: "111.222"},
	}
	tool := NewWith(fake.LazyClient())

	res, err := tool.Invoke(context.Background(), map[string]any{
		"channel": "C9",
		"ts":      "111.222",
		"body":    "edited",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fake.Updated) != 1 || fake.Updated[0].ChannelID != "C9" || fake.Updated[0].Text != "edited" {
		t.Fatalf("Updated = %+v", fake.Updated)
	}
	r := res.(Result)
	if r.String() != "Message 111.222 updated in C9." {
		t.Fatalf("String = %q", r.String())
	}
}

func TestInvoke_ResolvesHashChannel(t *testing.T) {
	fake := &slacktest.FakeAPI{
		Channels:     []slacklib.Channel{{ID: "C1", Name: "general"}},
		UpdateResult: slacklib.PostMessageResult{Channel: "C1", TS: "1.0"},
	}
	tool := NewWith(fake.LazyClient())

	res, err := tool.Invoke(context.Background(), map[string]any{
		"channel": "#general",
		"ts":      "1.0",
		"body":    "edited",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if fake.Updated[0].ChannelID != "C1" {
		t.Fatalf("ChannelID = %q, want C1", fake.Updated[0].ChannelID)
	}
	if got := res.(Result).String(); got != "Message 1.0 updated in #general." {
		t.Fatalf("String = %q (should echo original channel form)", got)
	}
}

func TestInvoke_DefaultBodyWhenOmitted(t *testing.T) {
	fake := &slacktest.FakeAPI{
		UpdateResult: slacklib.PostMessageResult{Channel: "C9", TS: "1.0"},
	}
	tool := NewWith(fake.LazyClient())

	if _, err := tool.Invoke(context.Background(), map[string]any{
		"channel": "C9",
		"ts":      "1.0",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if fake.Updated[0].Text != DefaultBody {
		t.Fatalf("Text = %q, want %q", fake.Updated[0].Text, DefaultBody)
	}
}

func TestInvoke_PassesBlocksJSONThrough(t *testing.T) {
	fake := &slacktest.FakeAPI{
		UpdateResult: slacklib.PostMessageResult{Channel: "C9", TS: "1.0"},
	}
	tool := NewWith(fake.LazyClient())
	blocks := `[{"type":"section","text":{"type":"mrkdwn","text":"hi"}}]`

	if _, err := tool.Invoke(context.Background(), map[string]any{
		"channel": "C9",
		"ts":      "1.0",
		"blocks":  blocks,
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(fake.Updated[0].Blocks) != blocks {
		t.Fatalf("Blocks = %q, want %q", fake.Updated[0].Blocks, blocks)
	}
}

func TestInvoke_BlocksFromFilePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blocks.json")
	body := `[{"type":"section","text":{"type":"mrkdwn","text":"from file"}}]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fake := &slacktest.FakeAPI{
		UpdateResult: slacklib.PostMessageResult{Channel: "C9", TS: "1.0"},
	}
	tool := NewWith(fake.LazyClient())

	if _, err := tool.Invoke(context.Background(), map[string]any{
		"channel": "C9",
		"ts":      "1.0",
		"blocks":  path,
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(fake.Updated[0].Blocks) != body {
		t.Fatalf("Blocks = %q, want %q", fake.Updated[0].Blocks, body)
	}
}

func TestInvoke_InvalidBlocksJSONErrors(t *testing.T) {
	tool := NewWith((&slacktest.FakeAPI{}).LazyClient())
	_, err := tool.Invoke(context.Background(), map[string]any{
		"channel": "C9",
		"ts":      "1.0",
		"blocks":  "{not valid",
	})
	if err == nil || !strings.Contains(err.Error(), "Error parsing blocks JSON") {
		t.Fatalf("Invoke err = %v, want blocks-json error", err)
	}
}

func TestInvoke_RequiresChannelAndTS(t *testing.T) {
	tool := NewWith((&slacktest.FakeAPI{}).LazyClient())
	if _, err := tool.Invoke(context.Background(), map[string]any{"channel": "C9"}); err == nil {
		t.Fatalf("Invoke without ts should fail")
	}
	if _, err := tool.Invoke(context.Background(), map[string]any{"ts": "1.0"}); err == nil {
		t.Fatalf("Invoke without channel should fail")
	}
}

func TestInvoke_ResultJSONShape(t *testing.T) {
	fake := &slacktest.FakeAPI{
		UpdateResult: slacklib.PostMessageResult{Channel: "C9", TS: "1.0"},
	}
	tool := NewWith(fake.LazyClient())
	res, err := tool.Invoke(context.Background(), map[string]any{"channel": "C9", "ts": "1.0"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got["ok"] != true || got["channel"] != "C9" || got["ts"] != "1.0" {
		t.Fatalf("JSON = %v, want ok/C9/1.0", got)
	}
	if _, present := got["originalChannel"]; present {
		t.Fatalf("originalChannel should not be JSON-serialised; got %v", got)
	}
}
