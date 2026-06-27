package createchannel

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/client/slacktest"
)

func TestTool_Metadata(t *testing.T) {
	tool := New("")
	if tool.Name() != "slack.create-channel" {
		t.Fatalf("Name = %q, want slack.create-channel", tool.Name())
	}
	schema := tool.InputSchema()
	if schema == nil || schema.Type != "object" {
		t.Fatalf("InputSchema = %+v, want object schema", schema)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "name" {
		t.Fatalf("schema required = %v, want [name]", schema.Required)
	}
}

func TestInvoke_CreatesPublicChannel(t *testing.T) {
	fake := &slacktest.FakeAPI{
		CreateResult: slacklib.CreateChannelResult{Channel: slacklib.Channel{ID: "C100", Name: "launch"}},
	}
	tool := NewWith(fake.LazyClient(), &bytes.Buffer{})

	res, err := tool.Invoke(context.Background(), map[string]any{
		"name":    "#launch",
		"topic":   "ship it",
		"purpose": "coordinate the launch",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if !r.OK || r.Channel != "C100" || r.Name != "launch" || r.Private {
		t.Fatalf("Result = %+v", r)
	}
	if len(fake.Created) != 1 {
		t.Fatalf("Created calls = %d, want 1", len(fake.Created))
	}
	c := fake.Created[0]
	if c.Name != "launch" || c.Private || c.Topic != "ship it" || c.Purpose != "coordinate the launch" {
		t.Fatalf("Created = %+v", c)
	}
	if r.String() != "Created public channel #launch (C100)." {
		t.Fatalf("String = %q", r.String())
	}
}

func TestInvoke_PrivateFlag(t *testing.T) {
	fake := &slacktest.FakeAPI{
		CreateResult: slacklib.CreateChannelResult{Channel: slacklib.Channel{ID: "G200", Name: "secret"}},
	}
	tool := NewWith(fake.LazyClient(), &bytes.Buffer{})

	res, err := tool.Invoke(context.Background(), map[string]any{
		"name":    "secret",
		"private": true,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !fake.Created[0].Private {
		t.Fatalf("Created.Private = false, want true")
	}
	if r := res.(Result); !r.Private || !strings.Contains(r.String(), "private channel #secret") {
		t.Fatalf("Result = %+v / %q", r, r.String())
	}
}

func TestInvoke_ResolvesInviteHandlesAndPassesIDs(t *testing.T) {
	fake := &slacktest.FakeAPI{
		Users:        []slacklib.User{{ID: "U99", Name: "ada"}},
		CreateResult: slacklib.CreateChannelResult{Channel: slacklib.Channel{ID: "C1", Name: "team"}},
	}
	warn := &bytes.Buffer{}
	tool := NewWith(fake.LazyClient(), warn)

	if _, err := tool.Invoke(context.Background(), map[string]any{
		"name":   "team",
		"invite": []any{"@ada", "U777", "@ghost"},
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	got := fake.Created[0].Invite
	if len(got) != 2 || got[0] != "U99" || got[1] != "U777" {
		t.Fatalf("Invite = %v, want [U99 U777]", got)
	}
	if !strings.Contains(warn.String(), "@ghost") {
		t.Fatalf("expected warning about @ghost, got %q", warn.String())
	}
}

func TestInvoke_SurfacesInviteErrorsWithoutAborting(t *testing.T) {
	fake := &slacktest.FakeAPI{
		CreateResult: slacklib.CreateChannelResult{
			Channel:      slacklib.Channel{ID: "C1", Name: "team"},
			InviteErrors: []string{"U404: Slack error (conversations.invite): user_not_found"},
		},
	}
	tool := NewWith(fake.LazyClient(), &bytes.Buffer{})

	res, err := tool.Invoke(context.Background(), map[string]any{
		"name":   "team",
		"invite": []any{"U404"},
	})
	if err != nil {
		t.Fatalf("Invoke should not abort on invite failure: %v", err)
	}
	r := res.(Result)
	if !r.OK || len(r.InviteErrors) != 1 {
		t.Fatalf("Result = %+v, want OK with one invite error", r)
	}
	if !strings.Contains(r.String(), "follow-up action(s) failed") {
		t.Fatalf("String = %q", r.String())
	}
}

func TestInvoke_MissingNameFails(t *testing.T) {
	fake := &slacktest.FakeAPI{}
	tool := NewWith(fake.LazyClient(), &bytes.Buffer{})

	if _, err := tool.Invoke(context.Background(), map[string]any{}); err == nil {
		t.Fatalf("Invoke without name should fail")
	}
	if _, err := tool.Invoke(context.Background(), map[string]any{"name": "  #  "}); err == nil {
		t.Fatalf("Invoke with blank name should fail")
	}
	if len(fake.Created) != 0 {
		t.Fatalf("expected no CreateChannel call on missing name, got %+v", fake.Created)
	}
}

func TestInvoke_ResultIsJSONSerialisable(t *testing.T) {
	fake := &slacktest.FakeAPI{
		CreateResult: slacklib.CreateChannelResult{Channel: slacklib.Channel{ID: "C1", Name: "team"}},
	}
	tool := NewWith(fake.LazyClient(), &bytes.Buffer{})

	res, err := tool.Invoke(context.Background(), map[string]any{"name": "team"})
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
	if got["ok"] != true || got["channel"] != "C1" || got["name"] != "team" {
		t.Fatalf("JSON = %v", got)
	}
}
