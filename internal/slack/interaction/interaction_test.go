package interaction

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	slacklib "github.com/miere/murtaugh-dev-toolkit/internal/slack/client"
	"github.com/miere/murtaugh-dev-toolkit/internal/slack/client/slacktest"
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
