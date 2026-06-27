package sendmsg

import (
	"bytes"
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
	tool := New("", "")
	if tool.Name() != "slack.send-msg" {
		t.Fatalf("Name = %q, want slack.send-msg", tool.Name())
	}
	schema := tool.InputSchema()
	if schema == nil || schema.Type != "object" {
		t.Fatalf("InputSchema = %+v, want object schema", schema)
	}
	for _, req := range []string{"body", "to"} {
		found := false
		for _, r := range schema.Required {
			if r == req {
				found = true
			}
		}
		if !found {
			t.Fatalf("schema required missing %q; required=%v", req, schema.Required)
		}
	}
}

func TestInvoke_PostsToChannelByName(t *testing.T) {
	fake := &slacktest.FakeAPI{
		Channels:   []slacklib.Channel{{ID: "C123", Name: "general"}},
		PostResult: slacklib.PostMessageResult{Channel: "C123", TS: "111.222"},
	}
	tool := NewWith(fake.LazyClient(), nil, &bytes.Buffer{})

	res, err := tool.Invoke(context.Background(), map[string]any{
		"body": "hello",
		"to":   "#general",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if !r.OK || r.Channel != "C123" || r.TS != "111.222" || r.To != "#general" {
		t.Fatalf("Result = %+v, want OK true / C123 / 111.222 / #general", r)
	}
	if r.String() != "Message sent to #general." {
		t.Fatalf("String = %q", r.String())
	}
	if len(fake.Posted) != 1 || fake.Posted[0].ChannelID != "C123" || fake.Posted[0].Text != "hello" {
		t.Fatalf("Posted = %+v", fake.Posted)
	}
}

func TestInvoke_AsAdminUsesUserTokenClient(t *testing.T) {
	bot := &slacktest.FakeAPI{
		Channels:   []slacklib.Channel{{ID: "C123", Name: "general"}},
		PostResult: slacklib.PostMessageResult{Channel: "C123", TS: "bot.ts"},
	}
	admin := &slacktest.FakeAPI{
		Channels:   []slacklib.Channel{{ID: "C123", Name: "general"}},
		PostResult: slacklib.PostMessageResult{Channel: "C123", TS: "admin.ts"},
	}
	tool := NewWith(bot.LazyClient(), admin.LazyClient(), &bytes.Buffer{})

	res, err := tool.Invoke(context.Background(), map[string]any{
		"body": "hello",
		"to":   "#general",
		"as":   "admin",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if r := res.(Result); r.TS != "admin.ts" {
		t.Fatalf("Result TS = %q, want admin.ts (admin client)", r.TS)
	}
	if len(admin.Posted) != 1 || admin.Posted[0].Text != "hello" {
		t.Fatalf("admin Posted = %+v, want one post", admin.Posted)
	}
	if len(bot.Posted) != 0 {
		t.Fatalf("expected no bot posts when as=admin, got %+v", bot.Posted)
	}
}

func TestInvoke_AsAdminWithoutUserTokenErrors(t *testing.T) {
	bot := &slacktest.FakeAPI{Channels: []slacklib.Channel{{ID: "C123", Name: "general"}}}
	tool := NewWith(bot.LazyClient(), nil, &bytes.Buffer{})

	_, err := tool.Invoke(context.Background(), map[string]any{
		"body": "hello",
		"to":   "#general",
		"as":   "admin",
	})
	if err == nil || !strings.Contains(err.Error(), "as=admin requires a Slack user token") {
		t.Fatalf("Invoke err = %v, want missing-user-token error", err)
	}
	if len(bot.Posted) != 0 {
		t.Fatalf("expected no fallback to bot, got %+v", bot.Posted)
	}
}

func TestInvoke_AsBotUnchanged(t *testing.T) {
	bot := &slacktest.FakeAPI{
		Channels:   []slacklib.Channel{{ID: "C123", Name: "general"}},
		PostResult: slacklib.PostMessageResult{Channel: "C123", TS: "bot.ts"},
	}
	admin := &slacktest.FakeAPI{
		Channels:   []slacklib.Channel{{ID: "C123", Name: "general"}},
		PostResult: slacklib.PostMessageResult{Channel: "C123", TS: "admin.ts"},
	}
	tool := NewWith(bot.LazyClient(), admin.LazyClient(), &bytes.Buffer{})

	for _, as := range []map[string]any{
		{"body": "hi", "to": "#general"},
		{"body": "hi", "to": "#general", "as": "bot"},
	} {
		res, err := tool.Invoke(context.Background(), as)
		if err != nil {
			t.Fatalf("Invoke(%v): %v", as, err)
		}
		if r := res.(Result); r.TS != "bot.ts" {
			t.Fatalf("Result TS = %q, want bot.ts", r.TS)
		}
	}
	if len(admin.Posted) != 0 {
		t.Fatalf("expected no admin posts, got %+v", admin.Posted)
	}
	if len(bot.Posted) != 2 {
		t.Fatalf("bot Posted = %d, want 2", len(bot.Posted))
	}
}

func TestInvoke_ResolvesMentionsInBody(t *testing.T) {
	fake := &slacktest.FakeAPI{
		Channels:   []slacklib.Channel{{ID: "C1", Name: "general"}},
		Users:      []slacklib.User{{ID: "U99", Name: "ada"}},
		PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1.0"},
	}
	tool := NewWith(fake.LazyClient(), nil, &bytes.Buffer{})

	_, err := tool.Invoke(context.Background(), map[string]any{
		"body": "hi @ada",
		"to":   "#general",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := fake.Posted[0].Text; got != "hi <@U99>" {
		t.Fatalf("Posted text = %q, want hi <@U99>", got)
	}
}

func TestInvoke_OpenDMForUserMention(t *testing.T) {
	fake := &slacktest.FakeAPI{
		Users:      []slacklib.User{{ID: "U987", Name: "miere"}},
		DMFor:      map[string]string{"U987": "D111"},
		PostResult: slacklib.PostMessageResult{Channel: "D111", TS: "2.0"},
	}
	tool := NewWith(fake.LazyClient(), nil, &bytes.Buffer{})

	res, err := tool.Invoke(context.Background(), map[string]any{
		"body": "hey",
		"to":   "@miere",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if fake.Posted[0].ChannelID != "D111" {
		t.Fatalf("ChannelID = %q, want D111", fake.Posted[0].ChannelID)
	}
	if r := res.(Result); r.To != "@miere" || r.Channel != "D111" {
		t.Fatalf("Result = %+v", r)
	}
}

func TestInvoke_UploadsAttachment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.md")
	if err := os.WriteFile(path, []byte("hi"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fake := &slacktest.FakeAPI{
		Channels:     []slacklib.Channel{{ID: "C1", Name: "general"}},
		UploadResult: slacklib.PostMessageResult{Channel: "C1", TS: "F123"},
	}
	tool := NewWith(fake.LazyClient(), nil, &bytes.Buffer{})

	if _, err := tool.Invoke(context.Background(), map[string]any{
		"body":            "report",
		"to":              "#general",
		"attachment":      path,
		"attachment_type": "markdown",
		"thread":          "1.2",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fake.Uploaded) != 1 {
		t.Fatalf("Uploaded calls = %d, want 1", len(fake.Uploaded))
	}
	up := fake.Uploaded[0]
	if up.ChannelID != "C1" || up.FilePath != path || up.Filename != "report.md" ||
		up.Title != "report.md" || up.InitialComment != "report" ||
		up.SnippetType != "markdown" || up.ThreadTS != "1.2" {
		t.Fatalf("Uploaded = %+v", up)
	}
	if len(fake.Posted) != 0 {
		t.Fatalf("expected no PostMessage when attachment is set, got %+v", fake.Posted)
	}
}

func TestInvoke_AttachmentMissingErrorsWithCLIMessage(t *testing.T) {
	fake := &slacktest.FakeAPI{Channels: []slacklib.Channel{{ID: "C1", Name: "general"}}}
	tool := NewWith(fake.LazyClient(), nil, &bytes.Buffer{})

	_, err := tool.Invoke(context.Background(), map[string]any{
		"body":       "x",
		"to":         "#general",
		"attachment": "/nope/missing.md",
	})
	if err == nil || !strings.Contains(err.Error(), "Error: attachment not found: /nope/missing.md") {
		t.Fatalf("Invoke err = %v, want attachment-not-found", err)
	}
}

func TestInvoke_MissingRequiredArgsFail(t *testing.T) {
	fake := &slacktest.FakeAPI{}
	tool := NewWith(fake.LazyClient(), nil, &bytes.Buffer{})
	if _, err := tool.Invoke(context.Background(), map[string]any{"to": "#g"}); err == nil {
		t.Fatalf("Invoke without body should fail")
	}
	if _, err := tool.Invoke(context.Background(), map[string]any{"body": "x"}); err == nil {
		t.Fatalf("Invoke without to should fail")
	}
}

func TestInvoke_ForwardsBlocksToPostMessage(t *testing.T) {
	fake := &slacktest.FakeAPI{
		Channels:   []slacklib.Channel{{ID: "C1", Name: "general"}},
		PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1.0"},
	}
	tool := NewWith(fake.LazyClient(), nil, &bytes.Buffer{})

	blocks := `[{"type":"section","text":{"type":"mrkdwn","text":"hi"}}]`
	if _, err := tool.Invoke(context.Background(), map[string]any{
		"body":   "hi",
		"to":     "#general",
		"blocks": blocks,
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fake.Posted) != 1 {
		t.Fatalf("Posted calls = %d, want 1", len(fake.Posted))
	}
	if got := string(fake.Posted[0].Blocks); got != blocks {
		t.Fatalf("Posted Blocks = %q, want %q", got, blocks)
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
		Channels:   []slacklib.Channel{{ID: "C1", Name: "general"}},
		PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1.0"},
	}
	tool := NewWith(fake.LazyClient(), nil, &bytes.Buffer{})

	if _, err := tool.Invoke(context.Background(), map[string]any{
		"body":   "hi",
		"to":     "#general",
		"blocks": path,
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(fake.Posted[0].Blocks) != body {
		t.Fatalf("Posted Blocks = %q, want %q", fake.Posted[0].Blocks, body)
	}
}

func TestInvoke_BlocksInvalidJSONFails(t *testing.T) {
	fake := &slacktest.FakeAPI{Channels: []slacklib.Channel{{ID: "C1", Name: "general"}}}
	tool := NewWith(fake.LazyClient(), nil, &bytes.Buffer{})

	_, err := tool.Invoke(context.Background(), map[string]any{
		"body":   "x",
		"to":     "#general",
		"blocks": "{not json",
	})
	if err == nil || !strings.Contains(err.Error(), "Error parsing blocks JSON") {
		t.Fatalf("Invoke err = %v, want blocks-parse error", err)
	}
	if len(fake.Posted) != 0 {
		t.Fatalf("expected no PostMessage on bad blocks, got %+v", fake.Posted)
	}
}

func TestInvoke_BlocksAndAttachmentMutuallyExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.md")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fake := &slacktest.FakeAPI{Channels: []slacklib.Channel{{ID: "C1", Name: "general"}}}
	tool := NewWith(fake.LazyClient(), nil, &bytes.Buffer{})

	_, err := tool.Invoke(context.Background(), map[string]any{
		"body":       "x",
		"to":         "#general",
		"attachment": path,
		"blocks":     `[{"type":"section","text":{"type":"mrkdwn","text":"hi"}}]`,
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Invoke err = %v, want mutually-exclusive error", err)
	}
	if len(fake.Uploaded) != 0 || len(fake.Posted) != 0 {
		t.Fatalf("expected no API calls; Uploaded=%+v Posted=%+v", fake.Uploaded, fake.Posted)
	}
}

func TestInvoke_ResultIsJSONSerialisable(t *testing.T) {
	fake := &slacktest.FakeAPI{
		Channels:   []slacklib.Channel{{ID: "C1", Name: "general"}},
		PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1.0"},
	}
	tool := NewWith(fake.LazyClient(), nil, &bytes.Buffer{})
	res, err := tool.Invoke(context.Background(), map[string]any{"body": "x", "to": "#general"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got["ok"] != true || got["channel"] != "C1" || got["ts"] != "1.0" || got["to"] != "#general" {
		t.Fatalf("JSON = %v", got)
	}
}
