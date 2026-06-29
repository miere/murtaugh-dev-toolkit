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
	kindByID := make(map[string]string, len(req.Options))
	for _, o := range req.Options {
		label := o.Name
		if label == "" {
			label = o.ID
		}
		options = append(options, Option{ID: o.ID, Label: label, Style: styleForPermissionKind(o.Kind)})
		kindByID[o.ID] = o.Kind
	}
	if len(options) == 0 {
		return "", nil
	}
	// Mirror the native approval gate: name the tool concisely and, when the agent
	// supplied a title (for an execute call, the command line), render it in a
	// language-hinted fenced code block via Slack's markdown block — the same
	// syntax-highlighted treatment the agent's own code output gets — rather than
	// echoing the whole command inline.
	name := friendlyToolName(req.ToolKind)
	detail := strings.TrimRight(req.ToolTitle, "\n")
	question := fmt.Sprintf("The agent wants to use the `%s` tool. Allow?", name)
	if detail != "" {
		question = fmt.Sprintf("The agent wants to use the `%s` tool:\n\n```%s\n%s\n```\n\nAllow?", name, codeLang(name), detail)
	}
	decision, err := g.broker.Ask(ctx, Destination{ChannelID: loc.ChannelID, ThreadTS: loc.ThreadTS, UserID: loc.UserID}, PromptSpec{
		Title:       ":lock: Permission needed",
		Question:    question,
		Markdown:    true,
		Options:     options,
		OutcomeText: permissionOutcome(name, kindByID),
	})
	if err != nil {
		return "", err
	}
	if decision.Answered() {
		return decision.OptionID, nil
	}
	return "", nil
}

// permissionOutcome renders the terminal line an ACP permission prompt is
// rewritten to, mirroring the native approval gate: an allow_* choice shows a
// check, a reject_* choice is struck through, and both name the decider. The
// option kinds are agent-defined, so an unrecognised kind falls back to naming
// the chosen option plainly rather than guessing allow vs deny.
func permissionOutcome(toolName string, kindByID map[string]string) func(Decision) string {
	return func(d Decision) string {
		switch {
		case d.TimedOut:
			return fmt.Sprintf(":hourglass_flowing_sand: Permission for `%s` timed out", toolName)
		case d.Cancelled:
			return fmt.Sprintf(":no_entry_sign: Permission for `%s` dismissed", toolName)
		}
		kind := kindByID[d.OptionID]
		switch {
		case strings.HasPrefix(kind, "allow"):
			return fmt.Sprintf("✓ Tool `%s` approved%s", toolName, decidedBy(d.UserID))
		case strings.HasPrefix(kind, "reject"):
			return fmt.Sprintf("~Tool `%s` denied%s~", toolName, decidedBy(d.UserID))
		default:
			label := d.Label
			if label == "" {
				label = d.OptionID
			}
			return fmt.Sprintf("✓ Tool `%s`: *%s*%s", toolName, label, decidedBy(d.UserID))
		}
	}
}

// friendlyToolName maps an ACP toolCall kind to the short, stable label shown in
// the permission prompt and its outcome line. The execute kind is surfaced as
// "terminal" so the ACP gate reads identically to the native one (whose tool is
// literally named "terminal", and which codeLang keys on for bash highlighting);
// other known kinds use the kind verbatim, and an empty/unknown kind falls back to
// a neutral "tool".
func friendlyToolName(kind string) string {
	switch k := strings.ToLower(strings.TrimSpace(kind)); k {
	case "":
		return "tool"
	case "execute":
		return "terminal"
	default:
		return k
	}
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
