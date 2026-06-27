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

// corrFromForm parses the correlation id back out of the posted form announce
// message's "Answer" button, mirroring what the gateway router does on a click.
func corrFromForm(t *testing.T, p slacklib.PostMessageParams) string {
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
				corr, ok := IsFormAnswerClick(slackgo.InteractionCallback{
					Type: slackgo.InteractionTypeBlockActions,
					ActionCallback: slackgo.ActionCallbacks{
						BlockActions: []*slackgo.BlockAction{{ActionID: btn.ActionID}},
					},
				})
				if ok {
					return corr
				}
			}
		}
	}
	t.Fatal("no form Answer button found in posted prompt")
	return ""
}

func sampleForm() FormSpec {
	return FormSpec{
		Title: "Deploy plan",
		Questions: []Question{
			{Key: "env", Label: "Which environment?", Options: []Option{{ID: "stg", Label: "Staging"}, {ID: "prod", Label: "Production"}}},
			{Key: "regions", Label: "Which regions?", MultiSelect: true, Options: []Option{{ID: "us", Label: "US"}, {ID: "eu", Label: "EU"}}},
			{Key: "notes", Label: "Anything else?", FreeText: true},
		},
	}
}

func TestAskForm_ResolvedBySubmit(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	spec := sampleForm()

	resultCh := make(chan FormResponse, 1)
	go func() {
		r, err := broker.AskForm(context.Background(), Destination{ChannelID: "C1", ThreadTS: "t1"}, spec)
		if err != nil {
			t.Errorf("AskForm error: %v", err)
		}
		resultCh <- r
	}()

	posted := <-sig.posted
	if posted.ChannelID != "C1" || posted.ThreadTS != "t1" {
		t.Fatalf("form posted to wrong destination: %q / %q", posted.ChannelID, posted.ThreadTS)
	}
	corr := corrFromForm(t, posted)

	// Simulate the "Answer" click → the broker opens a modal via views.open.
	if err := broker.OpenForm(context.Background(), corr, "trigger-123"); err != nil {
		t.Fatalf("OpenForm error: %v", err)
	}
	if len(sig.OpenedViews) != 1 {
		t.Fatalf("expected exactly one modal opened, got %d", len(sig.OpenedViews))
	}
	view := sig.OpenedViews[0]
	if view.CallbackID != FormCallbackID || view.PrivateMetadata != corr {
		t.Fatalf("modal callback/metadata = %q / %q, want %q / %q", view.CallbackID, view.PrivateMetadata, FormCallbackID, corr)
	}
	if len(view.Blocks.BlockSet) != 3 {
		t.Fatalf("expected 3 input blocks, got %d", len(view.Blocks.BlockSet))
	}
	if sig.OpenTriggers[0] != "trigger-123" {
		t.Fatalf("OpenView trigger = %q, want trigger-123", sig.OpenTriggers[0])
	}

	// Simulate the view_submission resolving the blocked AskForm.
	resp := FormResponse{
		Answers:   map[string][]string{"env": {"Production"}, "regions": {"US", "EU"}},
		FreeText:  map[string]string{"notes": "ship carefully"},
		UserID:    "U9",
		Submitted: true,
	}
	if !broker.ResolveForm(corr, resp) {
		t.Fatal("ResolveForm reported no pending form")
	}

	got := <-resultCh
	if !got.Completed() {
		t.Fatalf("expected a completed response, got %+v", got)
	}
	if got.UserID != "U9" {
		t.Fatalf("UserID = %q, want U9", got.UserID)
	}
	if len(got.Answers["regions"]) != 2 || got.FreeText["notes"] != "ship carefully" {
		t.Fatalf("unexpected answers: %+v", got)
	}
	// The announce message is rewritten to a terminal, button-less state.
	if len(sig.Updated) != 1 {
		t.Fatalf("expected the announce message edited once on resolution, got %d updates", len(sig.Updated))
	}
}

func TestAskForm_TimesOut(t *testing.T) {
	fake := &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.0001"}}
	broker := NewWith(fake.LazyClient())

	spec := sampleForm()
	spec.Timeout = 20 * time.Millisecond
	r, err := broker.AskForm(context.Background(), Destination{ChannelID: "C1"}, spec)
	if err != nil {
		t.Fatalf("AskForm error: %v", err)
	}
	if !r.TimedOut || r.Completed() {
		t.Fatalf("expected timed-out response, got %+v", r)
	}
}

func TestAskForm_CancelledByContext(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan FormResponse, 1)
	go func() {
		r, _ := broker.AskForm(ctx, Destination{ChannelID: "C1"}, sampleForm())
		resultCh <- r
	}()

	<-sig.posted
	cancel()
	r := <-resultCh
	if !r.Cancelled || r.Completed() {
		t.Fatalf("expected cancelled response, got %+v", r)
	}
}

func TestAskForm_RejectsBadInput(t *testing.T) {
	broker := NewWith((&slacktest.FakeAPI{}).LazyClient())
	if _, err := broker.AskForm(context.Background(), Destination{}, sampleForm()); err == nil {
		t.Fatal("expected error for missing channel")
	}
	if _, err := broker.AskForm(context.Background(), Destination{ChannelID: "C1"}, FormSpec{}); err == nil {
		t.Fatal("expected error for no questions")
	}
	if _, err := broker.AskForm(context.Background(), Destination{ChannelID: "C1"}, FormSpec{
		Questions: []Question{{Key: "x", Label: "no options"}},
	}); err == nil {
		t.Fatal("expected error for a select question with no options")
	}
}

func TestOpenForm_UnknownCorrelation(t *testing.T) {
	broker := NewWith((&slacktest.FakeAPI{}).LazyClient())
	if err := broker.OpenForm(context.Background(), "nope", "trig"); err == nil {
		t.Fatal("OpenForm should error for an unknown correlation id")
	}
}

func TestResolveForm_UnknownCorrelation(t *testing.T) {
	broker := NewWith((&slacktest.FakeAPI{}).LazyClient())
	if broker.ResolveForm("nope", FormResponse{}) {
		t.Fatal("ResolveForm should report false for an unknown correlation id")
	}
}

func TestParseViewSubmission_RoundTrip(t *testing.T) {
	corr := "abc123"
	ic := slackgo.InteractionCallback{
		Type: slackgo.InteractionTypeViewSubmission,
		User: slackgo.User{ID: "U7"},
		View: slackgo.View{
			CallbackID:      FormCallbackID,
			PrivateMetadata: corr,
			State: &slackgo.ViewState{
				Values: map[string]map[string]slackgo.BlockAction{
					inputPrefix + "env": {
						inputPrefix + "env": {SelectedOption: slackgo.OptionBlockObject{Value: "prod"}},
					},
					inputPrefix + "regions": {
						inputPrefix + "regions": {SelectedOptions: []slackgo.OptionBlockObject{{Value: "us"}, {Value: "eu"}}},
					},
					inputPrefix + "notes": {
						inputPrefix + "notes": {Value: "ship it"},
					},
				},
			},
		},
	}

	gotCorr, resp, ok := ParseViewSubmission(ic)
	if !ok {
		t.Fatal("ParseViewSubmission should recognize our modal submission")
	}
	if gotCorr != corr {
		t.Fatalf("corr = %q, want %q", gotCorr, corr)
	}
	if !resp.Submitted || resp.UserID != "U7" {
		t.Fatalf("unexpected response header: %+v", resp)
	}
	if got := resp.Answers["env"]; len(got) != 1 || got[0] != "prod" {
		t.Fatalf("env answer = %v, want [prod]", got)
	}
	if got := resp.Answers["regions"]; len(got) != 2 {
		t.Fatalf("regions answer = %v, want 2 entries", got)
	}
	if resp.FreeText["notes"] != "ship it" {
		t.Fatalf("notes free text = %q, want 'ship it'", resp.FreeText["notes"])
	}

	// A foreign callback (wrong type / callback id) is not ours.
	if _, _, ok := ParseViewSubmission(slackgo.InteractionCallback{Type: slackgo.InteractionTypeBlockActions}); ok {
		t.Fatal("ParseViewSubmission should ignore a non-submission callback")
	}
	if _, _, ok := ParseViewSubmission(slackgo.InteractionCallback{
		Type: slackgo.InteractionTypeViewSubmission,
		View: slackgo.View{CallbackID: "someone_elses_modal"},
	}); ok {
		t.Fatal("ParseViewSubmission should ignore a foreign modal")
	}
}

func TestIsFormAnswerClick(t *testing.T) {
	blocks := buildFormAnnounceBlocks("zzz", sampleForm())
	action := blocks[len(blocks)-1].(*slackgo.ActionBlock)
	btn := action.Elements.ElementSet[0].(*slackgo.ButtonBlockElement)

	ic := slackgo.InteractionCallback{Type: slackgo.InteractionTypeBlockActions}
	ic.ActionCallback.BlockActions = []*slackgo.BlockAction{{ActionID: btn.ActionID}}
	corr, ok := IsFormAnswerClick(ic)
	if !ok || corr != "zzz" {
		t.Fatalf("IsFormAnswerClick corr = %q, ok = %v", corr, ok)
	}

	// A plain single-question click (interaction.go namespace) is NOT a form click.
	other := slackgo.InteractionCallback{Type: slackgo.InteractionTypeBlockActions}
	other.ActionCallback.BlockActions = []*slackgo.BlockAction{{ActionID: ActionPrefix + "corr:0"}}
	if _, ok := IsFormAnswerClick(other); ok {
		t.Fatal("a single-question button click must not be seen as a form click")
	}
}
