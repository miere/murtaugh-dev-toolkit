package fetchmsgs

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/client/slacktest"
)

func TestTool_Metadata(t *testing.T) {
	tool := New("")
	if tool.Name() != "slack.fetch-msgs" {
		t.Fatalf("Name = %q", tool.Name())
	}
	schema := tool.InputSchema()
	if schema == nil || schema.Type != "object" || len(schema.Required) != 1 || schema.Required[0] != "channel" {
		t.Fatalf("InputSchema = %+v, want object with required=[channel]", schema)
	}
}

func TestInvoke_FetchesHistoryOldestFirst(t *testing.T) {
	loc, _ := time.LoadLocation(slacklib.SydneyTZ)
	t1 := slacklib.SlackTS(time.Date(2025, 1, 2, 10, 0, 0, 0, loc))
	t2 := slacklib.SlackTS(time.Date(2025, 1, 2, 11, 0, 0, 0, loc))
	fake := &slacktest.FakeAPI{
		Channels: []slacklib.Channel{{ID: "C1", Name: "general"}},
		// Slack returns newest-first; oldest-first output reverses this.
		History: []slacklib.Message{
			{TS: t2, User: "U2", Text: "second"},
			{TS: t1, User: "U1", Text: "first"},
		},
	}
	tool := NewWith(fake.LazyClient())

	res, err := tool.Invoke(context.Background(), map[string]any{"channel": "general"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if r.Channel != "C1" {
		t.Fatalf("Channel = %q, want C1", r.Channel)
	}
	if len(r.Messages) != 2 || r.Messages[0].Text != "first" || r.Messages[1].Text != "second" {
		t.Fatalf("Messages = %+v, want oldest-first", r.Messages)
	}
	want := "[10:00] @U1: first\n[11:00] @U2: second"
	if got := r.String(); got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
}

func TestInvoke_UsesGetRepliesWhenThreadSet(t *testing.T) {
	fake := &slacktest.FakeAPI{
		Channels: []slacklib.Channel{{ID: "C1", Name: "general"}},
		Replies:  []slacklib.Message{{TS: "2.0", Text: "a"}, {TS: "1.0", Text: "b"}},
	}
	tool := NewWith(fake.LazyClient())

	if _, err := tool.Invoke(context.Background(), map[string]any{
		"channel": "general",
		"thread":  "1234.5678",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fake.RepliesCalls) != 1 || fake.RepliesCalls[0].ThreadTS != "1234.5678" || fake.RepliesCalls[0].ChannelID != "C1" {
		t.Fatalf("RepliesCalls = %+v", fake.RepliesCalls)
	}
	if len(fake.HistoryCalls) != 0 {
		t.Fatalf("expected no GetHistory when --thread set; got %+v", fake.HistoryCalls)
	}
}

func TestInvoke_DefaultSinceIs24hAgoSydney(t *testing.T) {
	fake := &slacktest.FakeAPI{Channels: []slacklib.Channel{{ID: "C1", Name: "general"}}}
	tool := NewWith(fake.LazyClient())

	if _, err := tool.Invoke(context.Background(), map[string]any{"channel": "general"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fake.HistoryCalls) != 1 {
		t.Fatalf("HistoryCalls = %+v", fake.HistoryCalls)
	}
	// Parse the oldest ts back to a time and check it's ~24h ago.
	since, _ := slacklib.ParseSince("")
	got := fake.HistoryCalls[0].OldestTS
	want := slacklib.SlackTS(since)
	// Allow a few seconds of skew between the two calls to ParseSince.
	if absDiffSeconds(got, want) > 5 {
		t.Fatalf("oldestTS = %q, want ~%q (within 5s)", got, want)
	}
	if fake.HistoryCalls[0].Limit != HistoryLimit {
		t.Fatalf("Limit = %d, want %d", fake.HistoryCalls[0].Limit, HistoryLimit)
	}
}

func TestInvoke_ExplicitSinceParsedAsSydney(t *testing.T) {
	fake := &slacktest.FakeAPI{Channels: []slacklib.Channel{{ID: "C1", Name: "general"}}}
	tool := NewWith(fake.LazyClient())
	in := "2025-01-02 03:04:05"
	if _, err := tool.Invoke(context.Background(), map[string]any{
		"channel": "general",
		"since":   in,
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	parsed, _ := slacklib.ParseSince(in)
	want := slacklib.SlackTS(parsed)
	if got := fake.HistoryCalls[0].OldestTS; got != want {
		t.Fatalf("oldestTS = %q, want %q", got, want)
	}
}

func TestInvoke_InvalidSinceReturnsCLIMessage(t *testing.T) {
	fake := &slacktest.FakeAPI{Channels: []slacklib.Channel{{ID: "C1", Name: "general"}}}
	tool := NewWith(fake.LazyClient())
	_, err := tool.Invoke(context.Background(), map[string]any{
		"channel": "general",
		"since":   "yesterday",
	})
	if err == nil || !strings.Contains(err.Error(), "must be in format 'YYYY-MM-DD HH:mm:ss'") {
		t.Fatalf("Invoke err = %v", err)
	}
}

func TestInvoke_ChannelMissingErrors(t *testing.T) {
	tool := NewWith((&slacktest.FakeAPI{}).LazyClient())
	if _, err := tool.Invoke(context.Background(), map[string]any{}); err == nil {
		t.Fatalf("Invoke without channel should fail")
	}
}

func TestInvoke_ResultIsJSONSerialisable(t *testing.T) {
	fake := &slacktest.FakeAPI{
		Channels: []slacklib.Channel{{ID: "C1", Name: "general"}},
		History:  []slacklib.Message{{TS: "1.0", User: "U", Text: "x", Reactions: []slacklib.Reaction{{Name: "ok", Users: []string{"U2"}, Count: 1}}}},
	}
	tool := NewWith(fake.LazyClient())
	res, _ := tool.Invoke(context.Background(), map[string]any{"channel": "general"})
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"channel":"C1"`) || !strings.Contains(string(raw), `"reactions":[`) {
		t.Fatalf("JSON = %s", string(raw))
	}
}

// absDiffSeconds returns the absolute difference between two Slack-TS
// strings in seconds. Returns a large number on parse failure.
func absDiffSeconds(a, b string) float64 {
	pa, ea := strconv.ParseFloat(a, 64)
	pb, eb := strconv.ParseFloat(b, 64)
	if ea != nil || eb != nil {
		return 1e9
	}
	d := pa - pb
	if d < 0 {
		d = -d
	}
	return d
}
