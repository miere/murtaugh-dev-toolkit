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

	slacklib "github.com/miere/murtaugh-dev-toolkit/internal/slack/client"
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

// Destination is the Slack conversation a prompt is posted to.
type Destination struct {
	ChannelID string
	ThreadTS  string
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
}

// Decision is the outcome of an Ask.
type Decision struct {
	OptionID  string // the chosen option's ID ("" when none chosen)
	Label     string // the chosen option's label
	UserID    string // who clicked
	TimedOut  bool   // no response within the timeout
	Cancelled bool   // the turn was cancelled (interrupt / idle) before a response
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
}

// New builds a Broker that posts with the given Slack bot token.
func New(token string) *Broker {
	return &Broker{client: slacklib.NewLazyClient(token), pending: make(map[string]chan Decision)}
}

// NewWith builds a Broker against an injected client, for tests.
func NewWith(client *slacklib.LazyClient) *Broker {
	return &Broker{client: client, pending: make(map[string]chan Decision)}
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

	posted, err := api.PostMessage(ctx, slacklib.PostMessageParams{
		ChannelID: dest.ChannelID,
		Text:      promptFallback(spec),
		ThreadTS:  dest.ThreadTS,
		Blocks:    blocks,
	})
	if err != nil {
		return Decision{}, fmt.Errorf("interaction: post prompt: %w", err)
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

	b.editOutcome(api, posted.Channel, posted.TS, spec, decision)
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
func (b *Broker) editOutcome(api slacklib.SlackAPI, channel, ts string, spec PromptSpec, d Decision) {
	if channel == "" || ts == "" {
		return
	}
	blocks, err := json.Marshal(slackgo.Blocks{BlockSet: buildOutcomeBlocks(spec, d)})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = api.UpdateMessage(ctx, slacklib.UpdateMessageParams{
		ChannelID: channel,
		TS:        ts,
		Text:      outcomeText(spec, d),
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
		return corr, Decision{OptionID: cv.ID, Label: cv.Label, UserID: ic.User.ID}, true
	}
	return "", Decision{}, false
}

func buildPromptBlocks(corr string, spec PromptSpec) []slackgo.Block {
	var blocks []slackgo.Block
	if t := strings.TrimSpace(spec.Title); t != "" {
		blocks = append(blocks, slackgo.NewSectionBlock(markdown("*"+t+"*"), nil, nil))
	}
	blocks = append(blocks, slackgo.NewSectionBlock(markdown(spec.Question), nil, nil))

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
			slackgo.NewTextBlockObject(slackgo.PlainTextType, opt.Label, false, false),
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

func buildOutcomeBlocks(spec PromptSpec, d Decision) []slackgo.Block {
	return []slackgo.Block{slackgo.NewSectionBlock(markdown(outcomeText(spec, d)), nil, nil)}
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
