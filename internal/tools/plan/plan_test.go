package plan

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/agent"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/client/slacktest"
	"github.com/miere/murtaugh/internal/slack/interaction"
)

type signalingAPI struct {
	*slacktest.FakeAPI
	posted chan slacklib.PostMessageParams
}

func (s *signalingAPI) PostMessage(ctx context.Context, p slacklib.PostMessageParams) (slacklib.PostMessageResult, error) {
	res, err := s.FakeAPI.PostMessage(ctx, p)
	s.posted <- p
	return res, err
}

func locatedCtx() context.Context {
	return agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"})
}

func TestInvoke_NilBrokerErrors(t *testing.T) {
	_, err := New(nil).Invoke(locatedCtx(), map[string]any{"plan": "do the thing"})
	if err == nil {
		t.Fatal("expected an error when the broker is unwired")
	}
}

func TestInvoke_RequiresSlackLocation(t *testing.T) {
	broker := interaction.NewWith((&slacktest.FakeAPI{}).LazyClient())
	_, err := New(broker).Invoke(context.Background(), map[string]any{"plan": "do the thing"})
	if err == nil || !strings.Contains(err.Error(), "Slack conversation") {
		t.Fatalf("expected a Slack-conversation error, got %v", err)
	}
}

func TestInvoke_RequiresPlan(t *testing.T) {
	broker := interaction.NewWith((&slacktest.FakeAPI{}).LazyClient())
	_, err := New(broker).Invoke(locatedCtx(), map[string]any{"plan": "   "})
	if err == nil {
		t.Fatal("expected an error for an empty plan")
	}
}

func TestInvoke_PostsToTurnLocationAndProceeds(t *testing.T) {
	sig := &signalingAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	broker := interaction.NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }))
	tool := New(broker)

	resultCh := make(chan Result, 1)
	go func() {
		out, err := tool.Invoke(locatedCtx(), map[string]any{"plan": "1. step one\n2. step two"})
		if err != nil {
			t.Errorf("Invoke error: %v", err)
			resultCh <- Result{}
			return
		}
		resultCh <- out.(Result)
	}()

	posted := <-sig.posted
	// The plan is presented in the turn's own thread, not somewhere the model guessed.
	if posted.ChannelID != "C1" || posted.ThreadTS != "t1" {
		t.Fatalf("posted to %q/%q, want C1/t1", posted.ChannelID, posted.ThreadTS)
	}
	corr := corrFrom(t, posted.Blocks)
	if !broker.Resolve(corr, interaction.Decision{OptionID: "proceed", Label: "Proceed", UserID: "U1"}) {
		t.Fatal("Resolve found no pending prompt")
	}

	got := <-resultCh
	if !got.Approved || got.Choice != "Proceed" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestInvoke_CancelIsNotApproved(t *testing.T) {
	sig := &signalingAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	broker := interaction.NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }))
	tool := New(broker)

	resultCh := make(chan Result, 1)
	go func() {
		out, err := tool.Invoke(locatedCtx(), map[string]any{"plan": "ship it"})
		if err != nil {
			t.Errorf("Invoke error: %v", err)
			resultCh <- Result{}
			return
		}
		resultCh <- out.(Result)
	}()

	posted := <-sig.posted
	corr := corrFrom(t, posted.Blocks)
	if !broker.Resolve(corr, interaction.Decision{OptionID: "cancel", Label: "Cancel", UserID: "U1"}) {
		t.Fatal("Resolve found no pending prompt")
	}

	got := <-resultCh
	if got.Approved {
		t.Fatalf("expected a non-approved result on cancel, got %+v", got)
	}
}

func corrFrom(t *testing.T, raw []byte) string {
	t.Helper()
	var blocks slackgo.Blocks
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("blocks not valid JSON: %v", err)
	}
	for _, b := range blocks.BlockSet {
		if action, ok := b.(*slackgo.ActionBlock); ok && action.Elements != nil {
			for _, el := range action.Elements.ElementSet {
				if btn, ok := el.(*slackgo.ButtonBlockElement); ok {
					// action_id == "murtaugh_interaction:<corr>:<idx>"
					parts := strings.Split(btn.ActionID, ":")
					if len(parts) >= 3 {
						return parts[1]
					}
				}
			}
		}
	}
	t.Fatal("no broker button in posted blocks")
	return ""
}
