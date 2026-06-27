package interaction

import (
	"context"
	"fmt"
	"strings"

	"github.com/miere/murtaugh/internal/agent"
)

// PermissionGate answers an ACP agent's session/request_permission by posting the
// agent's offered options as buttons in the turn's Slack thread and returning the
// chosen optionId. It satisfies agent.PermissionAsker structurally, so the agent
// package stays free of any Slack dependency. It is the ACP analogue of
// GateApprover (which gates the native loop's tool calls) over the same broker.
type PermissionGate struct {
	broker *Broker
}

// NewPermissionGate builds a PermissionGate over the shared broker.
func NewPermissionGate(broker *Broker) *PermissionGate {
	return &PermissionGate{broker: broker}
}

// AskPermission posts the agent's offered options as buttons in loc's thread and
// blocks until the user picks one (or the wait times out / is cancelled). It
// returns the chosen option's ID, or "" when the user did not choose — which the
// ACP client maps to a "cancelled" outcome. With no broker or no Slack location it
// returns "" (deny), leaving the caller to fail fast rather than block.
func (g *PermissionGate) AskPermission(ctx context.Context, loc agent.TurnLocation, req agent.PermissionRequest) (string, error) {
	if g == nil || g.broker == nil || loc.ChannelID == "" {
		return "", nil
	}
	options := make([]Option, 0, len(req.Options))
	for _, o := range req.Options {
		label := o.Name
		if label == "" {
			label = o.ID
		}
		options = append(options, Option{ID: o.ID, Label: label, Style: styleForPermissionKind(o.Kind)})
	}
	if len(options) == 0 {
		return "", nil
	}
	tool := req.ToolName
	if tool == "" {
		tool = "a tool"
	}
	decision, err := g.broker.Ask(ctx, Destination{ChannelID: loc.ChannelID, ThreadTS: loc.ThreadTS}, PromptSpec{
		Title:    ":lock: Permission needed",
		Question: fmt.Sprintf("The agent wants to use *%s*. Allow?", tool),
		Options:  options,
	})
	if err != nil {
		return "", err
	}
	if decision.Answered() {
		return decision.OptionID, nil
	}
	return "", nil
}

// styleForPermissionKind maps an ACP PermissionOptionKind to a button style:
// allow_* renders primary (green), reject_* danger (red), unknown neutral.
func styleForPermissionKind(kind string) string {
	switch {
	case strings.HasPrefix(kind, "allow"):
		return "primary"
	case strings.HasPrefix(kind, "reject"):
		return "danger"
	default:
		return ""
	}
}
