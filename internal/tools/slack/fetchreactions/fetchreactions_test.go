package fetchreactions

import (
	"context"
	"strings"
	"testing"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/client/slacktest"
)

func TestTool_Metadata(t *testing.T) {
	tool := New("")
	if tool.Name() != "slack.fetch-reactions" {
		t.Fatalf("Name = %q", tool.Name())
	}
	schema := tool.InputSchema()
	if schema == nil || schema.Type != "object" {
		t.Fatalf("InputSchema = %+v", schema)
	}
	req := map[string]bool{}
	for _, r := range schema.Required {
		req[r] = true
	}
	for _, want := range []string{"from", "emoji", "channel"} {
		if !req[want] {
			t.Fatalf("schema required missing %q; required=%v", want, schema.Required)
		}
	}
}

func fixtureFake() *slacktest.FakeAPI {
	return &slacktest.FakeAPI{
		Channels: []slacklib.Channel{{ID: "C1", Name: "general"}},
		Users:    []slacklib.User{{ID: "U9", Name: "ada"}},
		History: []slacklib.Message{
			{TS: "3.0", User: "U1", Text: "matched", Reactions: []slacklib.Reaction{
				{Name: "thumbsup", Users: []string{"U9", "U3"}, Count: 2},
			}},
			{TS: "2.0", User: "U2", Text: "wrong emoji", Reactions: []slacklib.Reaction{
				{Name: "wave", Users: []string{"U9"}, Count: 1},
			}},
			{TS: "1.0", User: "U3", Text: "wrong user", Reactions: []slacklib.Reaction{
				{Name: "thumbsup", Users: []string{"U4"}, Count: 1},
			}},
		},
	}
}

func TestInvoke_FiltersByEmojiAndUser(t *testing.T) {
	fake := fixtureFake()
	tool := NewWith(fake.LazyClient())
	res, err := tool.Invoke(context.Background(), map[string]any{
		"from":    "@ada",
		"emoji":   "thumbsup",
		"channel": "general",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if len(r.Messages) != 1 || r.Messages[0].Text != "matched" {
		t.Fatalf("Messages = %+v, want one [matched]", r.Messages)
	}
	if r.Channel != "C1" {
		t.Fatalf("Channel = %q, want C1", r.Channel)
	}
}

func TestInvoke_StripsEmojiColons(t *testing.T) {
	fake := fixtureFake()
	tool := NewWith(fake.LazyClient())
	res, err := tool.Invoke(context.Background(), map[string]any{
		"from":    "ada",
		"emoji":   ":thumbsup:",
		"channel": "general",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(res.(Result).Messages) != 1 {
		t.Fatalf("Messages = %+v", res)
	}
}

func TestInvoke_NoMatchYieldsEmptyResult(t *testing.T) {
	fake := fixtureFake()
	tool := NewWith(fake.LazyClient())
	res, err := tool.Invoke(context.Background(), map[string]any{
		"from":    "ada",
		"emoji":   "rocket",
		"channel": "general",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if len(r.Messages) != 0 {
		t.Fatalf("Messages = %+v, want empty", r.Messages)
	}
	if r.String() != "" {
		t.Fatalf("String = %q, want empty", r.String())
	}
}

func TestInvoke_UserNotFoundBubbles(t *testing.T) {
	fake := fixtureFake()
	tool := NewWith(fake.LazyClient())
	_, err := tool.Invoke(context.Background(), map[string]any{
		"from":    "@ghost",
		"emoji":   "thumbsup",
		"channel": "general",
	})
	if err == nil || !strings.Contains(err.Error(), "User 'ghost' not found") {
		t.Fatalf("Invoke err = %v, want user-not-found", err)
	}
}

func TestInvoke_RequiresAllThreeArgs(t *testing.T) {
	fake := fixtureFake()
	tool := NewWith(fake.LazyClient())
	for _, args := range []map[string]any{
		{"emoji": "x", "channel": "c"},
		{"from": "x", "channel": "c"},
		{"from": "x", "emoji": "x"},
	} {
		if _, err := tool.Invoke(context.Background(), args); err == nil {
			t.Fatalf("Invoke(%v) should fail", args)
		}
	}
}
