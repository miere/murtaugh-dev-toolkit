package restart

import (
	"context"
	"encoding/json"
	"testing"

	slackgo "github.com/slack-go/slack"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/client/slacktest"
	"github.com/miere/murtaugh/internal/slack/restartcard"
)

// cardActionIDs decodes the posted Block Kit JSON and returns the action_ids
// of every button it carries, so a test can assert the gateway router would
// recognise the card (it keys on restartcard.Action*).
func cardActionIDs(t *testing.T, raw []byte) []string {
	t.Helper()
	var blocks slackgo.Blocks
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("posted blocks are not valid Block Kit JSON: %v", err)
	}
	var ids []string
	for _, b := range blocks.BlockSet {
		action, ok := b.(*slackgo.ActionBlock)
		if !ok || action.Elements == nil {
			continue
		}
		for _, el := range action.Elements.ElementSet {
			if btn, ok := el.(*slackgo.ButtonBlockElement); ok {
				ids = append(ids, btn.ActionID)
			}
		}
	}
	return ids
}

func TestInvokeFallsBackToAdminDM(t *testing.T) {
	fake := &slacktest.FakeAPI{
		DMFor:      map[string]string{"UADMIN00": "DADMIN00"},
		PostResult: slacklib.PostMessageResult{Channel: "DADMIN00", TS: "1700000000.000100"},
	}
	tool := NewWith(fake.LazyClient(), "UADMIN00")

	res, err := tool.Invoke(context.Background(), map[string]any{"reason": "stuck on boot"})
	if err != nil {
		t.Fatalf("Invoke error: %v", err)
	}
	if len(fake.Posted) != 1 {
		t.Fatalf("expected exactly one post, got %d", len(fake.Posted))
	}
	posted := fake.Posted[0]
	if posted.ChannelID != "DADMIN00" {
		t.Fatalf("expected card posted to admin DM DADMIN00, got %q", posted.ChannelID)
	}
	if posted.ThreadTS != "" {
		t.Fatalf("expected no thread for admin DM, got %q", posted.ThreadTS)
	}
	ids := cardActionIDs(t, posted.Blocks)
	if !contains(ids, restartcard.ActionConfirm) || !contains(ids, restartcard.ActionDismiss) {
		t.Fatalf("posted card missing recognised action_ids, got %v", ids)
	}
	if r, ok := res.(Result); !ok || !r.OK || r.Channel != "DADMIN00" {
		t.Fatalf("unexpected result: %#v", res)
	}
}

func TestInvokePostsToExplicitChannelAndThread(t *testing.T) {
	fake := &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C42", TS: "1.0"}}
	tool := NewWith(fake.LazyClient(), "UADMIN00")

	if _, err := tool.Invoke(context.Background(), map[string]any{
		"channel": "C42",
		"thread":  "1699999999.000001",
		"reason":  "agent asked for it",
	}); err != nil {
		t.Fatalf("Invoke error: %v", err)
	}
	if len(fake.Posted) != 1 {
		t.Fatalf("expected exactly one post, got %d", len(fake.Posted))
	}
	posted := fake.Posted[0]
	if posted.ChannelID != "C42" || posted.ThreadTS != "1699999999.000001" {
		t.Fatalf("expected post to C42 in-thread, got channel=%q thread=%q", posted.ChannelID, posted.ThreadTS)
	}
	// An explicit channel must never open the admin DM.
	if len(fake.DMFor) != 0 {
		t.Fatal("test misconfigured: DMFor should be empty")
	}
}

func TestInvokeErrorsWhenNoChannelAndNoAdmin(t *testing.T) {
	fake := &slacktest.FakeAPI{}
	tool := NewWith(fake.LazyClient(), "")

	if _, err := tool.Invoke(context.Background(), nil); err == nil {
		t.Fatal("expected an error when there is no channel and no admin to ask")
	}
	if len(fake.Posted) != 0 {
		t.Fatalf("expected no post when destination is unresolved, got %d", len(fake.Posted))
	}
}

func TestInvokeResolvesHandleAdminViaAtMention(t *testing.T) {
	fake := &slacktest.FakeAPI{
		Users:      []slacklib.User{{ID: "UADMIN00", Name: "alice"}},
		DMFor:      map[string]string{"UADMIN00": "DADMIN00"},
		PostResult: slacklib.PostMessageResult{Channel: "DADMIN00", TS: "1.0"},
	}
	// admin configured as a bare handle, not a U… id.
	tool := NewWith(fake.LazyClient(), "alice")

	if _, err := tool.Invoke(context.Background(), nil); err != nil {
		t.Fatalf("Invoke error: %v", err)
	}
	if len(fake.Posted) != 1 || fake.Posted[0].ChannelID != "DADMIN00" {
		t.Fatalf("expected card posted to resolved admin DM, got %+v", fake.Posted)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
