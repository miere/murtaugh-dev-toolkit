package ask

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
	_, err := New(nil).Invoke(locatedCtx(), map[string]any{"question": "q", "options": []any{"a", "b"}})
	if err == nil {
		t.Fatal("expected an error when the broker is unwired")
	}
}

func TestInvoke_RequiresSlackLocation(t *testing.T) {
	broker := interaction.NewWith((&slacktest.FakeAPI{}).LazyClient())
	_, err := New(broker).Invoke(context.Background(), map[string]any{"question": "q", "options": []any{"a", "b"}})
	if err == nil || !strings.Contains(err.Error(), "Slack conversation") {
		t.Fatalf("expected a Slack-conversation error, got %v", err)
	}
}

func TestInvoke_RequiresTwoOptions(t *testing.T) {
	broker := interaction.NewWith((&slacktest.FakeAPI{}).LazyClient())
	_, err := New(broker).Invoke(locatedCtx(), map[string]any{"question": "q", "options": []any{"only one"}})
	if err == nil {
		t.Fatal("expected an error for fewer than two options")
	}
}

func TestInvoke_PostsToTurnLocationAndReturnsChoice(t *testing.T) {
	sig := &signalingAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	broker := interaction.NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }))
	tool := New(broker)

	resultCh := make(chan Result, 1)
	go func() {
		out, err := tool.Invoke(locatedCtx(), map[string]any{"question": "Ship it?", "options": []any{"Approve", "Deny"}})
		if err != nil {
			t.Errorf("Invoke error: %v", err)
			resultCh <- Result{}
			return
		}
		resultCh <- out.(Result)
	}()

	posted := <-sig.posted
	// The question is asked in the turn's own thread, not somewhere the model guessed.
	if posted.ChannelID != "C1" || posted.ThreadTS != "t1" {
		t.Fatalf("posted to %q/%q, want C1/t1", posted.ChannelID, posted.ThreadTS)
	}
	corr := corrFrom(t, posted.Blocks)
	if !broker.Resolve(corr, interaction.Decision{OptionID: "Approve", Label: "Approve", UserID: "U1"}) {
		t.Fatal("Resolve found no pending ask")
	}

	got := <-resultCh
	if !got.Answered || got.Choice != "Approve" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestInvoke_DismissedOnCancel(t *testing.T) {
	sig := &signalingAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	broker := interaction.NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }))
	ctx, cancel := context.WithCancel(locatedCtx())

	resultCh := make(chan Result, 1)
	go func() {
		out, _ := New(broker).Invoke(ctx, map[string]any{"question": "q", "options": []any{"a", "b"}})
		resultCh <- out.(Result)
	}()

	<-sig.posted
	cancel()
	got := <-resultCh
	if got.Answered {
		t.Fatalf("expected an unanswered result on cancel, got %+v", got)
	}
}

func TestInvoke_MultiQuestionRoutesToForm(t *testing.T) {
	sig := &signalingAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	broker := interaction.NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }))
	tool := New(broker)

	resultCh := make(chan Result, 1)
	go func() {
		out, err := tool.Invoke(locatedCtx(), map[string]any{
			"title": "Deploy",
			"questions": []any{
				map[string]any{"label": "Env?", "options": []any{"Staging", "Production"}},
				map[string]any{"label": "Regions?", "multiSelect": true, "options": []any{"US", "EU"}},
				map[string]any{"label": "Notes?", "freeText": true},
			},
		})
		if err != nil {
			t.Errorf("Invoke error: %v", err)
			resultCh <- Result{}
			return
		}
		resultCh <- out.(Result)
	}()

	posted := <-sig.posted
	if posted.ChannelID != "C1" || posted.ThreadTS != "t1" {
		t.Fatalf("posted to %q/%q, want C1/t1", posted.ChannelID, posted.ThreadTS)
	}
	corr := formCorrFrom(t, posted.Blocks)

	// Keys are assigned positionally (q0, q1, q2) by the tool.
	resp := interaction.FormResponse{
		Answers:   map[string][]string{"q0": {"Production"}, "q1": {"US", "EU"}},
		FreeText:  map[string]string{"q2": "ship carefully"},
		UserID:    "U1",
		Submitted: true,
	}
	if !broker.ResolveForm(corr, resp) {
		t.Fatal("ResolveForm found no pending form")
	}

	got := <-resultCh
	if !got.Answered || len(got.Answers) != 3 {
		t.Fatalf("unexpected result: %+v", got)
	}
	if got.Answers[0].Question != "Env?" || len(got.Answers[0].Choices) != 1 || got.Answers[0].Choices[0] != "Production" {
		t.Fatalf("env answer wrong: %+v", got.Answers[0])
	}
	if len(got.Answers[1].Choices) != 2 {
		t.Fatalf("regions answer wrong: %+v", got.Answers[1])
	}
	if got.Answers[2].Text != "ship carefully" {
		t.Fatalf("notes answer wrong: %+v", got.Answers[2])
	}
}

func TestInvoke_SingleFreeTextRoutesToForm(t *testing.T) {
	sig := &signalingAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	broker := interaction.NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }))

	resultCh := make(chan Result, 1)
	go func() {
		out, err := New(broker).Invoke(locatedCtx(), map[string]any{
			"questions": []any{map[string]any{"label": "Why?", "freeText": true}},
		})
		if err != nil {
			t.Errorf("Invoke error: %v", err)
			resultCh <- Result{}
			return
		}
		resultCh <- out.(Result)
	}()

	posted := <-sig.posted
	corr := formCorrFrom(t, posted.Blocks)
	if !broker.ResolveForm(corr, interaction.FormResponse{FreeText: map[string]string{"q0": "because"}, Submitted: true}) {
		t.Fatal("ResolveForm found no pending form")
	}
	got := <-resultCh
	if !got.Answered || len(got.Answers) != 1 || got.Answers[0].Text != "because" {
		t.Fatalf("unexpected free-text result: %+v", got)
	}
}

func TestInvoke_SinglePlainQuestionUsesButtonPath(t *testing.T) {
	sig := &signalingAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	broker := interaction.NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }))

	resultCh := make(chan Result, 1)
	go func() {
		// One plain single-select question expressed via `questions` should still
		// ride the simpler button path (no modal).
		out, err := New(broker).Invoke(locatedCtx(), map[string]any{
			"questions": []any{map[string]any{"label": "Ship?", "options": []any{"Yes", "No"}}},
		})
		if err != nil {
			t.Errorf("Invoke error: %v", err)
			resultCh <- Result{}
			return
		}
		resultCh <- out.(Result)
	}()

	posted := <-sig.posted
	// It is a button prompt (interaction.go namespace), resolved by a click.
	corr := corrFrom(t, posted.Blocks)
	if !broker.Resolve(corr, interaction.Decision{OptionID: "Yes", Label: "Yes", UserID: "U1"}) {
		t.Fatal("Resolve found no pending ask")
	}
	got := <-resultCh
	if !got.Answered || got.Choice != "Yes" || len(got.Answers) != 0 {
		t.Fatalf("expected a button-path choice, got %+v", got)
	}
}

// formCorrFrom recovers the correlation id from a posted form announce message's
// "Answer" button (the murtaugh_interaction_form: namespace).
func formCorrFrom(t *testing.T, raw []byte) string {
	t.Helper()
	var blocks slackgo.Blocks
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("blocks not valid JSON: %v", err)
	}
	for _, b := range blocks.BlockSet {
		if action, ok := b.(*slackgo.ActionBlock); ok && action.Elements != nil {
			for _, el := range action.Elements.ElementSet {
				if btn, ok := el.(*slackgo.ButtonBlockElement); ok {
					if corr, ok := interaction.IsFormAnswerClick(slackgo.InteractionCallback{
						Type: slackgo.InteractionTypeBlockActions,
						ActionCallback: slackgo.ActionCallbacks{
							BlockActions: []*slackgo.BlockAction{{ActionID: btn.ActionID}},
						},
					}); ok {
						return corr
					}
				}
			}
		}
	}
	t.Fatal("no form Answer button in posted blocks")
	return ""
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
