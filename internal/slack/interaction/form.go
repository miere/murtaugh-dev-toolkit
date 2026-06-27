// form.go extends the interaction broker with a modal-based form flow: the agent
// can ask SEVERAL questions at once — each single-select, multi-select, or
// free-text — collected in one Slack modal behind a Submit button.
//
// Buttons (the single-question Ask path in interaction.go) can express only a
// one-shot single pick, so the richer shape rides on Slack modals instead. A
// modal cannot be opened cold: views.open requires a fresh trigger_id from a
// user interaction. The flow is therefore two-step:
//
//  1. AskForm posts a message carrying ONE "Answer" button in a distinct
//     action_id namespace (ActionFormPrefix) and blocks on a rendezvous channel.
//  2. The user clicks "Answer" → the gateway recognizes the namespace
//     (IsFormAnswerClick) and calls OpenForm with the click's trigger_id, which
//     builds the modal from the stored FormSpec and calls views.open.
//  3. The user submits → the gateway receives a view_submission
//     (ParseViewSubmission reads the answers + correlation id) and calls
//     ResolveForm, waking the blocked AskForm.
//
// Correlation, mutex discipline, and the edit-on-terminal pattern mirror Ask.
package interaction

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	slackgo "github.com/slack-go/slack"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

const (
	// FormBlockID tags the actions block carrying the single "Answer" button.
	FormBlockID = "murtaugh_interaction_form"
	// ActionFormPrefix namespaces the "Answer" button's action_id. The
	// correlation id is appended: "murtaugh_interaction_form:<corr>". It is a
	// distinct namespace from ActionPrefix so the gateway can tell a form-answer
	// click apart from a plain single-question button click.
	ActionFormPrefix = "murtaugh_interaction_form:"
	// FormCallbackID is the stable callback_id stamped on every modal. The
	// per-form correlation id rides in PrivateMetadata, not here.
	FormCallbackID = "murtaugh_interaction_form_modal"
	// inputPrefix namespaces each modal input block's block_id/action_id so the
	// question key can be recovered from view.State.Values on submission.
	inputPrefix = "murtaugh_q:"
)

// Question is one prompt inside a FormSpec. Exactly one input widget is rendered
// per question, chosen by the flags below:
//   - FreeText        → a plain_text_input (multiline)
//   - MultiSelect     → a checkboxes group over Options
//   - otherwise       → a radio_buttons group over Options (single select)
//
// FreeText takes precedence over the select flags when both are set.
type Question struct {
	Key         string   // stable identifier; answers are keyed by it
	Label       string   // shown above the input
	Options     []Option // choices for select questions (ignored for FreeText)
	MultiSelect bool     // render checkboxes instead of radio buttons
	FreeText    bool     // render a text input instead of a select
}

// FormSpec describes a multi-question modal form.
type FormSpec struct {
	Title     string        // modal title (and message fallback)
	Questions []Question    // one input widget each, in order
	Timeout   time.Duration // 0 → DefaultTimeout
}

// FormResponse is the outcome of an AskForm. On a successful submit, Answers
// holds the selected option labels per question key (one entry for single-select,
// many for multi-select) and FreeText holds the typed text per question key.
type FormResponse struct {
	Answers   map[string][]string // question key → chosen option labels (selects)
	FreeText  map[string]string   // question key → typed text (free-text inputs)
	UserID    string              // who submitted
	Submitted bool                // the user submitted the modal
	Cancelled bool                // the turn was cancelled before a submit
	TimedOut  bool                // no submit within the timeout
}

// Completed reports whether the user actually submitted the form.
func (r FormResponse) Completed() bool { return r.Submitted && !r.TimedOut && !r.Cancelled }

// AskForm posts the "Answer" button to dest and blocks until the user submits the
// modal, the wait times out, or ctx is cancelled. It always edits the posted
// message to a terminal, button-less state before returning.
func (b *Broker) AskForm(ctx context.Context, dest Destination, spec FormSpec) (FormResponse, error) {
	if strings.TrimSpace(dest.ChannelID) == "" {
		return FormResponse{}, fmt.Errorf("interaction: no Slack channel to ask in")
	}
	if len(spec.Questions) == 0 {
		return FormResponse{}, fmt.Errorf("interaction: form has no questions")
	}
	for i := range spec.Questions {
		q := &spec.Questions[i]
		if strings.TrimSpace(q.Key) == "" {
			q.Key = fmt.Sprintf("q%d", i)
		}
		if !q.FreeText && len(q.Options) == 0 {
			return FormResponse{}, fmt.Errorf("interaction: select question %q has no options", q.Key)
		}
	}
	api, err := b.client.Get()
	if err != nil {
		return FormResponse{}, err
	}
	corr, err := newCorrelationID()
	if err != nil {
		return FormResponse{}, err
	}
	blocks, err := json.Marshal(slackgo.Blocks{BlockSet: buildFormAnnounceBlocks(corr, spec)})
	if err != nil {
		return FormResponse{}, fmt.Errorf("interaction: encode form prompt: %w", err)
	}

	ch := make(chan FormResponse, 1)
	b.mu.Lock()
	b.forms[corr] = spec
	b.formPending[corr] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.forms, corr)
		delete(b.formPending, corr)
		b.mu.Unlock()
	}()

	posted, err := api.PostMessage(ctx, slacklib.PostMessageParams{
		ChannelID: dest.ChannelID,
		Text:      formFallback(spec),
		ThreadTS:  dest.ThreadTS,
		Blocks:    blocks,
	})
	if err != nil {
		return FormResponse{}, fmt.Errorf("interaction: post form prompt: %w", err)
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var resp FormResponse
	select {
	case resp = <-ch:
	case <-timer.C:
		resp = FormResponse{TimedOut: true}
	case <-ctx.Done():
		resp = FormResponse{Cancelled: true}
	}

	b.editFormOutcome(api, posted.Channel, posted.TS, spec, resp)
	return resp, nil
}

// OpenForm builds the modal for the stored FormSpec identified by corr and opens
// it with views.open against triggerID. The gateway calls this when it sees the
// "Answer" button click. Returns an error when no form is pending for corr or the
// API call fails.
func (b *Broker) OpenForm(ctx context.Context, corr, triggerID string) error {
	b.mu.Lock()
	spec, ok := b.forms[corr]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("interaction: no pending form for %q", corr)
	}
	api, err := b.client.Get()
	if err != nil {
		return err
	}
	return api.OpenView(ctx, triggerID, buildModal(corr, spec))
}

// ResolveForm delivers a submission to the blocked AskForm identified by corr. It
// returns false when no form is waiting (a late, duplicate, or unknown submit).
// Non-blocking; the pending entry is removed so a second submit cannot
// double-deliver.
func (b *Broker) ResolveForm(corr string, resp FormResponse) bool {
	b.mu.Lock()
	ch, ok := b.formPending[corr]
	if ok {
		delete(b.formPending, corr)
		delete(b.forms, corr)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- resp:
	default:
	}
	return true
}

// IsFormAnswerClick reports whether ic is a click on a form "Answer" button and,
// if so, returns its correlation id. The gateway uses it to route the click to
// OpenForm.
func IsFormAnswerClick(ic slackgo.InteractionCallback) (corr string, ok bool) {
	if ic.Type != slackgo.InteractionTypeBlockActions {
		return "", false
	}
	for _, a := range ic.ActionCallback.BlockActions {
		if a == nil || !strings.HasPrefix(a.ActionID, ActionFormPrefix) {
			continue
		}
		return strings.TrimPrefix(a.ActionID, ActionFormPrefix), true
	}
	return "", false
}

// ParseViewSubmission reads a view_submission callback into a FormResponse. The
// correlation id comes from view.PrivateMetadata and the answers from
// view.State.Values. ok is false when this is not one of our modals.
func ParseViewSubmission(ic slackgo.InteractionCallback) (corr string, resp FormResponse, ok bool) {
	if ic.Type != slackgo.InteractionTypeViewSubmission {
		return "", FormResponse{}, false
	}
	if ic.View.CallbackID != FormCallbackID {
		return "", FormResponse{}, false
	}
	corr = ic.View.PrivateMetadata
	resp = FormResponse{
		Answers:   map[string][]string{},
		FreeText:  map[string]string{},
		UserID:    ic.User.ID,
		Submitted: true,
	}
	if ic.View.State == nil {
		return corr, resp, true
	}
	for blockID, actions := range ic.View.State.Values {
		key := strings.TrimPrefix(blockID, inputPrefix)
		for _, action := range actions {
			switch {
			case action.Value != "":
				if t := strings.TrimSpace(action.Value); t != "" {
					resp.FreeText[key] = t
				}
			case len(action.SelectedOptions) > 0:
				labels := make([]string, 0, len(action.SelectedOptions))
				for _, opt := range action.SelectedOptions {
					labels = append(labels, opt.Value)
				}
				resp.Answers[key] = labels
			case action.SelectedOption.Value != "":
				resp.Answers[key] = []string{action.SelectedOption.Value}
			}
		}
	}
	return corr, resp, true
}

// buildFormAnnounceBlocks renders the announce message: the form's title/question
// summary plus the single "Answer" button that opens the modal.
func buildFormAnnounceBlocks(corr string, spec FormSpec) []slackgo.Block {
	var blocks []slackgo.Block
	if t := strings.TrimSpace(spec.Title); t != "" {
		blocks = append(blocks, slackgo.NewSectionBlock(markdown("*"+t+"*"), nil, nil))
	}
	blocks = append(blocks, slackgo.NewSectionBlock(markdown(formSummary(spec)), nil, nil))

	btn := slackgo.NewButtonBlockElement(
		ActionFormPrefix+corr,
		corr,
		slackgo.NewTextBlockObject(slackgo.PlainTextType, "Answer", false, false),
	)
	btn.Style = slackgo.StylePrimary
	blocks = append(blocks, slackgo.NewActionBlock(FormBlockID, btn))
	return blocks
}

// buildModal turns a FormSpec into a views.open ModalViewRequest, one input block
// per question. The correlation id rides in PrivateMetadata so the submission can
// be routed back; the callback_id is stable (FormCallbackID).
func buildModal(corr string, spec FormSpec) slackgo.ModalViewRequest {
	title := strings.TrimSpace(spec.Title)
	if title == "" {
		title = "A few questions"
	}
	blocks := make([]slackgo.Block, 0, len(spec.Questions))
	for _, q := range spec.Questions {
		label := strings.TrimSpace(q.Label)
		if label == "" {
			label = q.Key
		}
		labelObj := slackgo.NewTextBlockObject(slackgo.PlainTextType, label, false, false)
		blockID := inputPrefix + q.Key
		actionID := inputPrefix + q.Key

		var element slackgo.BlockElement
		switch {
		case q.FreeText:
			in := slackgo.NewPlainTextInputBlockElement(nil, actionID)
			in.Multiline = true
			element = in
		case q.MultiSelect:
			element = slackgo.NewCheckboxGroupsBlockElement(actionID, optionObjects(q.Options)...)
		default:
			element = slackgo.NewRadioButtonsBlockElement(actionID, optionObjects(q.Options)...)
		}
		blocks = append(blocks, slackgo.NewInputBlock(blockID, labelObj, nil, element))
	}

	return slackgo.ModalViewRequest{
		Type:            slackgo.VTModal,
		Title:           slackgo.NewTextBlockObject(slackgo.PlainTextType, truncate(title, 24), false, false),
		Submit:          slackgo.NewTextBlockObject(slackgo.PlainTextType, "Submit", false, false),
		Close:           slackgo.NewTextBlockObject(slackgo.PlainTextType, "Cancel", false, false),
		Blocks:          slackgo.Blocks{BlockSet: blocks},
		CallbackID:      FormCallbackID,
		PrivateMetadata: corr,
	}
}

// optionObjects maps interaction Options to Slack option objects. The option's
// stable ID (falling back to its label) is the value so ParseViewSubmission can
// echo a meaningful answer; the label is the displayed text.
func optionObjects(opts []Option) []*slackgo.OptionBlockObject {
	out := make([]*slackgo.OptionBlockObject, 0, len(opts))
	for _, opt := range opts {
		value := opt.ID
		if value == "" {
			value = opt.Label
		}
		out = append(out, slackgo.NewOptionBlockObject(
			value,
			slackgo.NewTextBlockObject(slackgo.PlainTextType, opt.Label, false, false),
			nil,
		))
	}
	return out
}

// editFormOutcome rewrites the announce message to a terminal, button-less state.
func (b *Broker) editFormOutcome(api slacklib.SlackAPI, channel, ts string, spec FormSpec, r FormResponse) {
	if channel == "" || ts == "" {
		return
	}
	text := formOutcomeText(spec, r)
	blocks, err := json.Marshal(slackgo.Blocks{BlockSet: []slackgo.Block{slackgo.NewSectionBlock(markdown(text), nil, nil)}})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = api.UpdateMessage(ctx, slacklib.UpdateMessageParams{
		ChannelID: channel,
		TS:        ts,
		Text:      text,
		Blocks:    blocks,
	})
}

func formOutcomeText(spec FormSpec, r FormResponse) string {
	title := strings.TrimSpace(spec.Title)
	if title == "" {
		title = "form"
	}
	switch {
	case r.TimedOut:
		return fmt.Sprintf(":hourglass_flowing_sand: _No response to: %s_", title)
	case r.Cancelled:
		return fmt.Sprintf(":no_entry_sign: _Form dismissed: %s_", title)
	default:
		who := ""
		if r.UserID != "" {
			who = fmt.Sprintf("<@%s> ", r.UserID)
		}
		return fmt.Sprintf(":white_check_mark: %sanswered *%s*", who, title)
	}
}

func formSummary(spec FormSpec) string {
	if len(spec.Questions) == 1 {
		q := strings.TrimSpace(spec.Questions[0].Label)
		if q == "" {
			q = "1 question"
		}
		return q + "\n_Click *Answer* to respond._"
	}
	return fmt.Sprintf("%d questions to answer.\n_Click *Answer* to respond._", len(spec.Questions))
}

func formFallback(spec FormSpec) string {
	if t := strings.TrimSpace(spec.Title); t != "" {
		return t
	}
	if len(spec.Questions) > 0 {
		return strings.TrimSpace(spec.Questions[0].Label)
	}
	return "A few questions"
}

// truncate caps s to n runes (Slack modal titles are limited to 24 chars).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
