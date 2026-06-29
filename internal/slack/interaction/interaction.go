// Package interaction is Murtaugh's native human-in-the-loop primitive: it posts
// a Block Kit prompt with option buttons into a Slack conversation and blocks the
// calling turn until the user clicks one (or the wait times out, or the turn is
// cancelled).
//
// It is the shared transport behind the `ask` tool (the agent asks the user a
// question) and — in a later PR — the tool-approval gate. The broker is agnostic
// about *why* it is asking: a caller hands it a PromptSpec and reads back a
// Decision.
//
// Correlation is by a random id minted per Ask and carried in the buttons'
// action_id namespace. The running gateway recognizes that namespace, routes the
// click to Resolve, and the blocked Ask wakes with the chosen option. Like
// internal/slack/restartcard, the action_id/block_id constants live here as the
// single source of truth the gateway router keys on.
package interaction

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	slackgo "github.com/slack-go/slack"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

const (
	// BlockID tags the actions block carrying the option buttons; the gateway
	// router recognizes a broker interaction by it (or the action_id prefix).
	BlockID = "murtaugh_interaction"
	// ActionPrefix namespaces every action_id a prompt emits. The correlation id
	// and option index are appended: "murtaugh_interaction:<corr>:<idx>".
	ActionPrefix = "murtaugh_interaction:"
)

// DefaultTimeout bounds a single Ask when the spec sets none. While Ask blocks,
// the native loop's heartbeat keeps the turn's idle watchdog alive, so this — not
// the watchdog — is the governing bound; on expiry the Decision reports TimedOut.
const DefaultTimeout = 10 * time.Minute

// Destination is the Slack conversation a prompt is posted to. When UserID is
// set the prompt is posted ephemerally — visible only to that user — instead of
// to the whole conversation; the outcome is then written back through the click's
// response_url (an ephemeral message cannot be edited any other way).
type Destination struct {
	ChannelID string
	ThreadTS  string
	UserID    string
}

// Option is a single selectable answer, rendered as a button.
type Option struct {
	ID    string // returned in Decision.OptionID; defaults to Label when empty
	Label string // button text
	Style string // "", "primary", or "danger"
}

// PromptSpec describes a single-question prompt. Multi-question, multi-select,
// and free-text answers are a later, modal-based extension; v1 is one question
// with a single pick.
type PromptSpec struct {
	Title    string
	Question string
	Options  []Option
	Timeout  time.Duration

	// OutcomeText, when set, renders the terminal message the prompt is rewritten
	// to once it resolves (answered, timed out, or cancelled), replacing the
	// default "<@user> chose *Label*" line. The approval gate uses it to show a
	// concise "Tool `x` approved/denied by <@user>" instead of echoing the
	// (code-laden) question. nil falls back to the default renderer.
	OutcomeText func(Decision) string

	// Markdown renders the title and question with Slack's `markdown` block
	// (full GitHub-flavored markdown, including syntax-highlighted fenced code)
	// rather than a section's legacy mrkdwn. The approval gate sets it so the
	// command being approved renders like the agent's own code blocks. When set,
	// the Title/Question must use GFM syntax (**bold**, not *bold*).
	Markdown bool
}

// Decision is the outcome of an Ask.
type Decision struct {
	OptionID  string // the chosen option's ID ("" when none chosen)
	Label     string // the chosen option's label
	UserID    string // who clicked
	TimedOut  bool   // no response within the timeout
	Cancelled bool   // the turn was cancelled (interrupt / idle) before a response
	// ResponseURL is the click's Slack response_url, carried so an ephemeral
	// prompt can be rewritten to its outcome (chat.update cannot touch it). Empty
	// on the timeout/cancel paths, where no click ever arrived.
	ResponseURL string
}

// Answered reports whether the user actually picked an option.
func (d Decision) Answered() bool { return !d.TimedOut && !d.Cancelled }

// clickValue is the JSON payload carried in each button's value, so a click
// round-trips both the stable option id and its human label.
type clickValue struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Broker posts interactive prompts and correlates the click back to the blocked
// Ask. One instance is shared between the `ask` tool (which calls Ask) and the
// gateway (which calls Resolve); the pending registry is the rendezvous.
type Broker struct {
	client *slacklib.LazyClient

	mu      sync.Mutex
	pending map[string]chan Decision
	// forms/formPending back the modal-based AskForm flow (see form.go): forms
	// holds the spec a pending "Answer" click will open into a modal, and
	// formPending is the rendezvous a submission resolves. Both are guarded by mu.
	forms       map[string]FormSpec
	formPending map[string]chan FormResponse
}

// New builds a Broker that posts with the given Slack bot token.
func New(token string) *Broker {
	return newBroker(slacklib.NewLazyClient(token))
}

// NewWith builds a Broker against an injected client, for tests.
func NewWith(client *slacklib.LazyClient) *Broker {
	return newBroker(client)
}

func newBroker(client *slacklib.LazyClient) *Broker {
	return &Broker{
		client:      client,
		pending:     make(map[string]chan Decision),
		forms:       make(map[string]FormSpec),
		formPending: make(map[string]chan FormResponse),
	}
}

// Ask posts the prompt to dest and blocks until the user clicks an option, the
// wait times out, or ctx is cancelled (the turn was interrupted). It always edits
// the posted message to a terminal state before returning, so no stale,
// still-clickable prompt is left behind.
func (b *Broker) Ask(ctx context.Context, dest Destination, spec PromptSpec) (Decision, error) {
	if strings.TrimSpace(dest.ChannelID) == "" {
		return Decision{}, fmt.Errorf("interaction: no Slack channel to ask in")
	}
	if len(spec.Options) == 0 {
		return Decision{}, fmt.Errorf("interaction: prompt has no options")
	}
	api, err := b.client.Get()
	if err != nil {
		return Decision{}, err
	}
	corr, err := newCorrelationID()
	if err != nil {
		return Decision{}, err
	}
	blocks, err := json.Marshal(slackgo.Blocks{BlockSet: buildPromptBlocks(corr, spec)})
	if err != nil {
		return Decision{}, fmt.Errorf("interaction: encode prompt: %w", err)
	}

	ch := make(chan Decision, 1)
	b.mu.Lock()
	b.pending[corr] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, corr)
		b.mu.Unlock()
	}()

	var postedChannel, postedTS string
	if dest.UserID != "" {
		// Ephemeral: visible only to the triggering user. Slack returns a
		// timestamp but it is not addressable by chat.update, so the outcome is
		// written back via the click's response_url (see editOutcome).
		if postedTS, err = api.PostEphemeral(ctx, slacklib.PostEphemeralParams{
			ChannelID: dest.ChannelID,
			UserID:    dest.UserID,
			Text:      promptFallback(spec),
			ThreadTS:  dest.ThreadTS,
			Blocks:    blocks,
		}); err != nil {
			b.notePromptUndeliverable(api, dest)
			return Decision{}, fmt.Errorf("interaction: post prompt: %w", err)
		}
	} else {
		posted, err := api.PostMessage(ctx, slacklib.PostMessageParams{
			ChannelID: dest.ChannelID,
			Text:      promptFallback(spec),
			ThreadTS:  dest.ThreadTS,
			Blocks:    blocks,
		})
		if err != nil {
			b.notePromptUndeliverable(api, dest)
			return Decision{}, fmt.Errorf("interaction: post prompt: %w", err)
		}
		postedChannel, postedTS = posted.Channel, posted.TS
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var decision Decision
	select {
	case decision = <-ch:
	case <-timer.C:
		decision = Decision{TimedOut: true}
	case <-ctx.Done():
		decision = Decision{Cancelled: true}
	}

	b.editOutcome(api, dest, postedChannel, postedTS, spec, decision)
	return decision, nil
}

// Resolve delivers a click to the blocked Ask identified by corr. It returns
// false when no Ask is waiting (a late, duplicate, or unknown click) so the
// caller can ignore it. Non-blocking: the rendezvous channel is buffered, and the
// pending entry is removed so a second click cannot double-deliver.
func (b *Broker) Resolve(corr string, d Decision) bool {
	b.mu.Lock()
	ch, ok := b.pending[corr]
	if ok {
		delete(b.pending, corr)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- d:
	default:
	}
	return true
}

// editOutcome rewrites the posted prompt to a terminal, button-less state so the
// thread shows what happened. Best-effort and on a fresh context: the ctx that
// drove Ask may already be cancelled on the interrupt path.
//
// For an ephemeral prompt (dest.UserID set) the rewrite must go through the
// click's response_url — chat.update cannot address an ephemeral message. When no
// click arrived (timeout/cancel) there is no response_url, so the prompt is left
// as-is; an ephemeral message vanishes on the next reload anyway.
func (b *Broker) editOutcome(api slacklib.SlackAPI, dest Destination, channel, ts string, spec PromptSpec, d Decision) {
	blocks, err := json.Marshal(slackgo.Blocks{BlockSet: buildOutcomeBlocks(spec, d)})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if dest.UserID != "" {
		if d.ResponseURL == "" {
			return
		}
		_ = api.RespondURL(ctx, d.ResponseURL, slacklib.WebhookParams{
			Text:            renderOutcomeText(spec, d),
			Blocks:          blocks,
			ReplaceOriginal: true,
		})
		return
	}

	if channel == "" || ts == "" {
		return
	}
	_, _ = api.UpdateMessage(ctx, slacklib.UpdateMessageParams{
		ChannelID: channel,
		TS:        ts,
		Text:      renderOutcomeText(spec, d),
		Blocks:    blocks,
	})
}

// IsInteraction reports whether ic is a click on a broker prompt. The gateway
// router uses it to dispatch the callback before the workflow engine sees it.
func IsInteraction(ic slackgo.InteractionCallback) bool {
	if ic.Type != slackgo.InteractionTypeBlockActions {
		return false
	}
	for _, a := range ic.ActionCallback.BlockActions {
		if a == nil {
			continue
		}
		if strings.HasPrefix(a.ActionID, ActionPrefix) || a.BlockID == BlockID {
			return true
		}
	}
	return false
}

// ParseClick extracts the correlation id and the chosen option from a broker
// interaction. ok is false when no broker action is present.
func ParseClick(ic slackgo.InteractionCallback) (corr string, d Decision, ok bool) {
	for _, a := range ic.ActionCallback.BlockActions {
		if a == nil || !strings.HasPrefix(a.ActionID, ActionPrefix) {
			continue
		}
		corr = correlationFromActionID(a.ActionID)
		var cv clickValue
		_ = json.Unmarshal([]byte(a.Value), &cv)
		return corr, Decision{OptionID: cv.ID, Label: cv.Label, UserID: ic.User.ID, ResponseURL: ic.ResponseURL}, true
	}
	return "", Decision{}, false
}

func buildPromptBlocks(corr string, spec PromptSpec) []slackgo.Block {
	var blocks []slackgo.Block
	if t := strings.TrimSpace(spec.Title); t != "" {
		if spec.Markdown {
			blocks = append(blocks, slackgo.NewMarkdownBlock("", clampText("**"+t+"**", slackMarkdownBlockLimit)))
		} else {
			blocks = append(blocks, slackgo.NewSectionBlock(markdown(clampText("*"+t+"*", slackSectionTextLimit)), nil, nil))
		}
	}
	if spec.Markdown {
		blocks = append(blocks, slackgo.NewMarkdownBlock("", clampText(spec.Question, slackMarkdownBlockLimit)))
	} else {
		blocks = append(blocks, slackgo.NewSectionBlock(markdown(clampText(spec.Question, slackSectionTextLimit)), nil, nil))
	}

	buttons := make([]slackgo.BlockElement, 0, len(spec.Options))
	for i, opt := range spec.Options {
		id := opt.ID
		if id == "" {
			id = opt.Label
		}
		value, _ := json.Marshal(clickValue{ID: id, Label: opt.Label})
		btn := slackgo.NewButtonBlockElement(
			fmt.Sprintf("%s%s:%d", ActionPrefix, corr, i),
			string(value),
			slackgo.NewTextBlockObject(slackgo.PlainTextType, clampButtonLabel(opt.Label), false, false),
		)
		switch opt.Style {
		case "primary":
			btn.Style = slackgo.StylePrimary
		case "danger":
			btn.Style = slackgo.StyleDanger
		}
		buttons = append(buttons, btn)
	}
	blocks = append(blocks, slackgo.NewActionBlock(BlockID, buttons...))
	return blocks
}

// Slack's hard limits on the text fields of the blocks a prompt is built from.
// Exceeding any of them makes chat.postMessage reject the whole message with
// invalid_blocks — which, on the approval/permission path, the caller reads as
// "couldn't ask" and denies the tool. The values are agent-supplied (an ACP
// option name embeds the command/dir; a question echoes the command), so they
// are of attacker-controlled length and must be clamped before assembly.
const (
	slackButtonLabelLimit   = 75    // a button text object (plain_text)
	slackSectionTextLimit   = 3000  // a section block's text object (mrkdwn/plain_text)
	slackMarkdownBlockLimit = 12000 // a markdown block's text
)

// clampText shortens s to limit runes, appending an ellipsis so the truncation is
// visible. It counts runes (not bytes) so multibyte text isn't cut mid-character.
func clampText(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}

// clampButtonLabel clamps a button label to Slack's limit. The label is
// display-only: the chosen option's stable ID travels in the button value
// (clickValue.ID) and is what's returned to the caller, so truncating the label
// never changes which option a click selects.
func clampButtonLabel(label string) string { return clampText(label, slackButtonLabelLimit) }

// undeliverableNotice is the plain-text message posted when a prompt could not be
// delivered. It is deliberately generic — naming no command or path — so it is safe
// to fall back to a public channel post without leaking what the (possibly private)
// prompt was about.
const undeliverableNotice = "⚠️ Couldn't show an approval prompt here, so the action was not run. This is a display issue on our side, not your request — please ask again."

// notePromptUndeliverable makes a posting failure visible instead of letting it
// become a silent denial: when the prompt itself can't be delivered, the caller
// still returns "not run", but without this the thread shows nothing and the agent
// appears to stall. It posts a plain-text notice (no Block Kit — so it can't hit
// the same rejection that sank the prompt), preferring an ephemeral notice when the
// prompt was meant to be private and falling back to a channel message otherwise.
// Best-effort and on a fresh context: the ctx that drove Ask may be the failed one.
func (b *Broker) notePromptUndeliverable(api slacklib.SlackAPI, dest Destination) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if dest.UserID != "" {
		if _, err := api.PostEphemeral(ctx, slacklib.PostEphemeralParams{
			ChannelID: dest.ChannelID,
			UserID:    dest.UserID,
			ThreadTS:  dest.ThreadTS,
			Text:      undeliverableNotice,
		}); err == nil {
			return
		}
	}
	_, _ = api.PostMessage(ctx, slacklib.PostMessageParams{
		ChannelID: dest.ChannelID,
		ThreadTS:  dest.ThreadTS,
		Text:      undeliverableNotice,
	})
}

func buildOutcomeBlocks(spec PromptSpec, d Decision) []slackgo.Block {
	return []slackgo.Block{slackgo.NewSectionBlock(markdown(renderOutcomeText(spec, d)), nil, nil)}
}

// renderOutcomeText picks the caller's custom outcome renderer when the spec
// supplies one, otherwise the default. A custom renderer that returns "" falls
// back to the default so the message is never left blank.
func renderOutcomeText(spec PromptSpec, d Decision) string {
	if spec.OutcomeText != nil {
		if s := strings.TrimSpace(spec.OutcomeText(d)); s != "" {
			return s
		}
	}
	return outcomeText(spec, d)
}

func outcomeText(spec PromptSpec, d Decision) string {
	q := strings.TrimSpace(spec.Question)
	switch {
	case d.TimedOut:
		return fmt.Sprintf(":hourglass_flowing_sand: _No response to: %s_", q)
	case d.Cancelled:
		return fmt.Sprintf(":no_entry_sign: _Question dismissed: %s_", q)
	default:
		who := ""
		if d.UserID != "" {
			who = fmt.Sprintf("<@%s> ", d.UserID)
		}
		return fmt.Sprintf(":white_check_mark: %schose *%s*\n_%s_", who, d.Label, q)
	}
}

func promptFallback(spec PromptSpec) string {
	if t := strings.TrimSpace(spec.Title); t != "" {
		return t
	}
	return strings.TrimSpace(spec.Question)
}

func markdown(text string) *slackgo.TextBlockObject {
	return slackgo.NewTextBlockObject(slackgo.MarkdownType, text, false, false)
}

// correlationFromActionID pulls the <corr> out of "murtaugh_interaction:<corr>:<idx>".
func correlationFromActionID(actionID string) string {
	rest := strings.TrimPrefix(actionID, ActionPrefix)
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		return rest[:i]
	}
	return rest
}

func newCorrelationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("interaction: mint correlation id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
