package interaction

import (
	"context"
	"fmt"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
)

// GateApprover is the Slack-backed approval gate: it asks the user to confirm a
// side-effecting tool call via the broker's Approve/Deny prompt, in the turn's
// own conversation. It satisfies the native loop's Approver interface
// structurally, so the native package stays free of any Slack dependency.
type GateApprover struct {
	broker *Broker
}

// NewApprover builds a GateApprover over the shared broker.
func NewApprover(broker *Broker) *GateApprover { return &GateApprover{broker: broker} }

// Approve asks the user to confirm running toolName with the given summary,
// posting Approve/Deny buttons into the current Slack thread and blocking until
// they answer. It returns whether to run the tool and, when not, a note for the
// model.
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

	question := fmt.Sprintf("The agent wants to run the `%s` tool:\n```%s```\nApprove?", toolName, summary)
	decision, err := g.broker.Ask(ctx, Destination{ChannelID: loc.ChannelID, ThreadTS: loc.ThreadTS}, PromptSpec{
		Title:    ":lock: Approval needed",
		Question: question,
		Options: []Option{
			{ID: "approve", Label: "Approve", Style: "primary"},
			{ID: "deny", Label: "Deny", Style: "danger"},
		},
	})
	if err != nil {
		return false, fmt.Sprintf("Skipped: couldn't ask for approval (%v). Not run.", err)
	}
	switch {
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
