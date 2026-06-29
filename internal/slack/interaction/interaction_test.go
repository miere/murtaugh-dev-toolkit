package interaction

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/client/slacktest"
)

// signalingAPI wraps the shared fake to announce each post, so a test can learn
// the (randomly minted) correlation id and resolve the prompt without racing on
// the fake's recorded-call slices.
type signalingAPI struct {
	*slacktest.FakeAPI
	posted chan slacklib.PostMessageParams
}

func (s *signalingAPI) PostMessage(ctx context.Context, p slacklib.PostMessageParams) (slacklib.PostMessageResult, error) {
	res, err := s.FakeAPI.PostMessage(ctx, p)
	s.posted <- p
	return res, err
}

// PostEphemeral records the ephemeral post and signals on the same channel
// (as an equivalent PostMessageParams) so a test can read the correlation id
// regardless of which transport the broker chose.
func (s *signalingAPI) PostEphemeral(ctx context.Context, p slacklib.PostEphemeralParams) (string, error) {
	ts, err := s.FakeAPI.PostEphemeral(ctx, p)
	s.posted <- slacklib.PostMessageParams{ChannelID: p.ChannelID, ThreadTS: p.ThreadTS, Blocks: p.Blocks}
	return ts, err
}

func newSignalingBroker(t *testing.T) (*Broker, *signalingAPI) {
	t.Helper()
	sig := &signalingAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.0001"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	broker := NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }))
	return broker, sig
}

// corrFromPosted parses the correlation id back out of the posted prompt's first
// button, mirroring what the gateway router does on a click.
func corrFromPosted(t *testing.T, p slacklib.PostMessageParams) string {
	t.Helper()
	var blocks slackgo.Blocks
	if err := json.Unmarshal(p.Blocks, &blocks); err != nil {
		t.Fatalf("posted blocks are not valid Block Kit JSON: %v", err)
	}
	for _, b := range blocks.BlockSet {
		action, ok := b.(*slackgo.ActionBlock)
		if !ok || action.Elements == nil {
			continue
		}
		for _, el := range action.Elements.ElementSet {
			if btn, ok := el.(*slackgo.ButtonBlockElement); ok {
				return correlationFromActionID(btn.ActionID)
			}
		}
	}
	t.Fatal("no button found in posted prompt")
	return ""
}

func TestAsk_ResolvedByClick(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	spec := PromptSpec{Question: "Proceed?", Options: []Option{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}}

	resultCh := make(chan Decision, 1)
	go func() {
		d, err := broker.Ask(context.Background(), Destination{ChannelID: "C1", ThreadTS: "t1"}, spec)
		if err != nil {
			t.Errorf("Ask error: %v", err)
		}
		resultCh <- d
	}()

	posted := <-sig.posted
	if posted.ChannelID != "C1" || posted.ThreadTS != "t1" {
		t.Fatalf("prompt posted to wrong destination: %q / %q", posted.ChannelID, posted.ThreadTS)
	}
	corr := corrFromPosted(t, posted)
	if !broker.Resolve(corr, Decision{OptionID: "yes", Label: "Yes", UserID: "U9"}) {
		t.Fatal("Resolve reported no pending Ask")
	}

	d := <-resultCh
	if !d.Answered() || d.OptionID != "yes" || d.Label != "Yes" || d.UserID != "U9" {
		t.Fatalf("unexpected decision: %+v", d)
	}
	// The prompt is rewritten to a terminal, button-less state.
	if len(sig.Updated) != 1 {
		t.Fatalf("expected the prompt to be edited once on resolution, got %d updates", len(sig.Updated))
	}
}

// TestAsk_EphemeralUsesResponseURL verifies that a Destination with a UserID
// posts the prompt ephemerally and writes the outcome back through the click's
// response_url (chat.update cannot touch an ephemeral message).
func TestAsk_EphemeralUsesResponseURL(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	spec := PromptSpec{Question: "Proceed?", Options: []Option{{ID: "yes", Label: "Yes"}}}

	resultCh := make(chan Decision, 1)
	go func() {
		d, err := broker.Ask(context.Background(), Destination{ChannelID: "C1", ThreadTS: "t1", UserID: "U7"}, spec)
		if err != nil {
			t.Errorf("Ask error: %v", err)
		}
		resultCh <- d
	}()

	posted := <-sig.posted
	if len(sig.Ephemeral) != 1 || sig.Ephemeral[0].UserID != "U7" {
		t.Fatalf("expected one ephemeral post to U7, got %+v", sig.Ephemeral)
	}
	if len(sig.Posted) != 0 {
		t.Fatalf("ephemeral prompt must not post a channel message, got %d", len(sig.Posted))
	}
	corr := corrFromPosted(t, posted)
	broker.Resolve(corr, Decision{OptionID: "yes", Label: "Yes", UserID: "U7", ResponseURL: "https://hooks.slack/x"})
	<-resultCh

	if len(sig.Webhooks) != 1 {
		t.Fatalf("expected outcome written via response_url once, got %d", len(sig.Webhooks))
	}
	if sig.Webhooks[0].ResponseURL != "https://hooks.slack/x" || !sig.Webhooks[0].Params.ReplaceOriginal {
		t.Fatalf("outcome should replace the original via the click's response_url, got %+v", sig.Webhooks[0])
	}
	if len(sig.Updated) != 0 {
		t.Fatalf("ephemeral outcome must not use chat.update, got %d", len(sig.Updated))
	}
}

// TestAsk_EphemeralTimeoutLeavesPrompt verifies that when an ephemeral prompt
// times out there is no response_url to write back through, so nothing is edited.
func TestAsk_EphemeralTimeoutLeavesPrompt(t *testing.T) {
	fake := &slacktest.FakeAPI{}
	broker := NewWith(fake.LazyClient())

	d, err := broker.Ask(context.Background(), Destination{ChannelID: "C1", UserID: "U7"}, PromptSpec{
		Question: "Proceed?",
		Options:  []Option{{Label: "Yes"}},
		Timeout:  20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Ask error: %v", err)
	}
	if !d.TimedOut {
		t.Fatalf("expected a timed-out decision, got %+v", d)
	}
	if len(fake.Webhooks) != 0 || len(fake.Updated) != 0 {
		t.Fatalf("a timed-out ephemeral prompt has no response_url to edit through, got webhooks=%d updates=%d", len(fake.Webhooks), len(fake.Updated))
	}
}

func TestAsk_TimesOut(t *testing.T) {
	fake := &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.0001"}}
	broker := NewWith(fake.LazyClient())

	d, err := broker.Ask(context.Background(), Destination{ChannelID: "C1"}, PromptSpec{
		Question: "Proceed?",
		Options:  []Option{{Label: "Yes"}, {Label: "No"}},
		Timeout:  20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Ask error: %v", err)
	}
	if !d.TimedOut || d.Answered() {
		t.Fatalf("expected timed-out decision, got %+v", d)
	}
}

func TestAsk_CancelledByContext(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan Decision, 1)
	go func() {
		d, _ := broker.Ask(ctx, Destination{ChannelID: "C1"}, PromptSpec{Question: "Proceed?", Options: []Option{{Label: "Yes"}, {Label: "No"}}})
		resultCh <- d
	}()

	<-sig.posted
	cancel()
	d := <-resultCh
	if !d.Cancelled || d.Answered() {
		t.Fatalf("expected cancelled decision, got %+v", d)
	}
}

func TestAsk_RejectsBadInput(t *testing.T) {
	broker := NewWith((&slacktest.FakeAPI{}).LazyClient())
	if _, err := broker.Ask(context.Background(), Destination{}, PromptSpec{Question: "q", Options: []Option{{Label: "a"}}}); err == nil {
		t.Fatal("expected error for missing channel")
	}
	if _, err := broker.Ask(context.Background(), Destination{ChannelID: "C1"}, PromptSpec{Question: "q"}); err == nil {
		t.Fatal("expected error for no options")
	}
}

func TestResolve_UnknownCorrelation(t *testing.T) {
	broker := NewWith((&slacktest.FakeAPI{}).LazyClient())
	if broker.Resolve("nope", Decision{}) {
		t.Fatal("Resolve should report false for an unknown correlation id")
	}
}

func TestParseClick_And_IsInteraction(t *testing.T) {
	spec := PromptSpec{Question: "Proceed?", Options: []Option{{ID: "yes", Label: "Yes", Style: "primary"}, {ID: "no", Label: "No", Style: "danger"}}}
	blocks := buildPromptBlocks("abc123", spec)

	// Pull the first button's action_id and value as Slack would echo them back.
	action := blocks[len(blocks)-1].(*slackgo.ActionBlock)
	btn := action.Elements.ElementSet[0].(*slackgo.ButtonBlockElement)

	ic := slackgo.InteractionCallback{Type: slackgo.InteractionTypeBlockActions, User: slackgo.User{ID: "U7"}}
	ic.ActionCallback.BlockActions = []*slackgo.BlockAction{{ActionID: btn.ActionID, BlockID: BlockID, Value: btn.Value}}

	if !IsInteraction(ic) {
		t.Fatal("IsInteraction should recognize a broker click")
	}
	corr, d, ok := ParseClick(ic)
	if !ok || corr != "abc123" {
		t.Fatalf("ParseClick corr = %q, ok = %v", corr, ok)
	}
	if d.OptionID != "yes" || d.Label != "Yes" || d.UserID != "U7" {
		t.Fatalf("unexpected parsed decision: %+v", d)
	}

	// A foreign callback is not ours.
	other := slackgo.InteractionCallback{Type: slackgo.InteractionTypeBlockActions}
	other.ActionCallback.BlockActions = []*slackgo.BlockAction{{ActionID: "something_else", BlockID: "other"}}
	if IsInteraction(other) {
		t.Fatal("IsInteraction should ignore a non-broker callback")
	}
}
