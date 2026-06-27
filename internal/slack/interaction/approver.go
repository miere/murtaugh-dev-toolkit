package interaction

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/miere/murtaugh/internal/agent"
)

// GateApprover is the Slack-backed approval gate: it asks the user to confirm a
// side-effecting tool call via the broker's Approve/Deny prompt, in the turn's
// own conversation. It satisfies the native loop's Approver interface
// structurally, so the native package stays free of any Slack dependency.
//
// It also keeps an in-memory "always allow" set: when the user picks
// "Approve & always allow", the exact summary string is remembered and every
// later call with the same summary is approved silently, without re-prompting.
// The set is session-scoped — it lives on the GateApprover and resets when the
// daemon restarts; nothing is persisted to config. Matching is exact (after
// trimming surrounding whitespace), with no fuzzy/normalizing comparison.
type GateApprover struct {
	broker *Broker

	mu      sync.Mutex
	allowed map[string]bool // summaries the user chose to always allow this run
}

// NewApprover builds a GateApprover over the shared broker.
func NewApprover(broker *Broker) *GateApprover {
	return &GateApprover{broker: broker, allowed: make(map[string]bool)}
}

// Approve asks the user to confirm running toolName with the given summary,
// posting Approve / Approve & always allow / Deny buttons into the current Slack
// thread and blocking until they answer. It returns whether to run the tool and,
// when not, a note for the model.
//
// If the (trimmed) summary was previously marked "always allow" this run, the
// call is approved immediately with no prompt. The always-allow set is
// session-scoped and matched exactly on the summary string (for the terminal
// tool, that is the command line).
//
// When there is no Slack conversation on the context (a headless/delegated run),
// the call is NOT gated — the run was arranged without a human to ask, so it
// proceeds. The gate exists to catch eager behaviour in live chat; that is the
// only place a TurnLocation is set.
func (g *GateApprover) Approve(ctx context.Context, toolName, summary string) (bool, string) {
	if g == nil || g.broker == nil {
		return true, ""
	}
	loc, ok := agent.TurnLocationFromContext(ctx)
	if !ok {
		return true, ""
	}

	key := strings.TrimSpace(summary)
	if g.isAllowed(key) {
		return true, ""
	}

	question := fmt.Sprintf("The agent wants to run the `%s` tool:\n```%s```\nApprove?", toolName, summary)
	decision, err := g.broker.Ask(ctx, Destination{ChannelID: loc.ChannelID, ThreadTS: loc.ThreadTS}, PromptSpec{
		Title:    ":lock: Approval needed",
		Question: question,
		Options: []Option{
			{ID: "approve", Label: "Approve", Style: "primary"},
			{ID: "approve_always", Label: "Approve & always allow", Style: "primary"},
			{ID: "deny", Label: "Deny", Style: "danger"},
		},
	})
	if err != nil {
		return false, fmt.Sprintf("Skipped: couldn't ask for approval (%v). Not run.", err)
	}
	switch {
	case decision.OptionID == "approve_always":
		g.remember(key)
		return true, ""
	case decision.OptionID == "approve":
		return true, ""
	case decision.TimedOut:
		return false, "Skipped: no approval received in time. The action was not run — ask again if it is still needed."
	case decision.Cancelled:
		return false, "Skipped: the approval request was dismissed. The action was not run."
	default:
		return false, "Denied by the user. The action was not run; do not retry it without their go-ahead."
	}
}

// isAllowed reports whether key is in the session-scoped always-allow set.
func (g *GateApprover) isAllowed(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.allowed[key]
}

// remember adds key to the always-allow set for the rest of this run.
func (g *GateApprover) remember(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.allowed[key] = true
}
