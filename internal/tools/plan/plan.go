// Package plan implements the `present_plan` tool: the agent's way to lay a
// concrete plan in front of the user as a Slack message with Proceed / Revise /
// Cancel buttons and WAIT for their sign-off before doing multi-step work. It is
// an ExitPlanMode-style consumer of the shared interaction broker
// (internal/slack/interaction), mirroring the `ask` tool.
//
// Like `ask`, it only works inside a Slack conversation: the turn's location is
// read from the context the native client stashes per turn, so the plan is shown
// in the same thread the agent is talking in — not the admin DM, and not wherever
// the model guesses. Outside a chat turn (CLI/MCP) it returns an error rather than
// blocking.
package plan

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/interaction"
)

// Tool is the `present_plan` capability.
type Tool struct {
	broker *interaction.Broker
}

// New constructs a present_plan Tool against the shared interaction broker. A nil
// broker leaves the tool registered but inert (it returns an error when invoked),
// which is the right behaviour in CLI/MCP processes that have no gateway to route
// the click back.
func New(broker *interaction.Broker) *Tool { return &Tool{broker: broker} }

// Name returns the registry key.
func (t *Tool) Name() string { return "present_plan" }

// Description is the model-facing summary. It is deliberately explicit that the
// tool blocks for a real decision and that the agent must not proceed on its own.
func (t *Tool) Description() string {
	return "Present a plan to the user as a Slack message with Proceed / Revise / Cancel " +
		"buttons and WAIT for their decision before doing multi-step work. Use this to get " +
		"sign-off on a plan you intend to execute — never start until the user picks Proceed, " +
		"and never treat silence as approval. Returns whether they approved, plus a note on " +
		"what to do next. Only works inside a Slack conversation."
}

// InputSchema declares the arguments: the plan itself, plus an optional heading.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"plan": {
				Type:        "string",
				Description: "The plan to present, as multi-line markdown the user can review.",
			},
			"title": {Type: "string", Description: "Optional short heading shown above the plan."},
		},
		Required: []string{"plan"},
	}
}

// Result is the structured outcome. The MCP frontend JSON-marshals it; the loop
// and CLI render it via String().
type Result struct {
	Approved bool   `json:"approved"`
	Choice   string `json:"choice,omitempty"`
	Note     string `json:"note,omitempty"`
}

// String renders the line fed back to the model / shown in the CLI.
func (r Result) String() string {
	if r.Note != "" {
		return r.Note
	}
	if r.Approved {
		return "The user approved the plan."
	}
	return "The user did not approve the plan."
}

// Invoke posts the plan to the current Slack thread and blocks until the user
// decides (or the wait times out / is cancelled).
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	if t.broker == nil {
		return nil, fmt.Errorf("Error: interactive plan approval is not available in this context")
	}
	loc, ok := agent.TurnLocationFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("Error: the present_plan tool only works inside a Slack conversation")
	}
	planText := strings.TrimSpace(stringArg(args, "plan"))
	if planText == "" {
		return nil, fmt.Errorf("Error: a plan is required")
	}

	title := strings.TrimSpace(stringArg(args, "title"))
	if title == "" {
		title = ":clipboard: Plan — approve?"
	}

	decision, err := t.broker.Ask(ctx, interaction.Destination{ChannelID: loc.ChannelID, ThreadTS: loc.ThreadTS}, interaction.PromptSpec{
		Title:    title,
		Question: planText,
		Options: []interaction.Option{
			{ID: "proceed", Label: "Proceed", Style: "primary"},
			{ID: "revise", Label: "Revise"},
			{ID: "cancel", Label: "Cancel", Style: "danger"},
		},
	})
	if err != nil {
		return nil, err
	}

	switch {
	case decision.TimedOut:
		return Result{Approved: false, Note: "No response in time. Do not assume approval — ask again or stop."}, nil
	case decision.Cancelled:
		return Result{Approved: false, Note: "The plan prompt was dismissed before they answered."}, nil
	}
	switch decision.OptionID {
	case "proceed":
		return Result{Approved: true, Choice: decision.Label, Note: "Approved — proceed with the plan as presented."}, nil
	case "revise":
		return Result{Approved: false, Choice: decision.Label, Note: "The user wants changes before you proceed. Ask what to adjust; do not start yet."}, nil
	case "cancel":
		return Result{Approved: false, Choice: decision.Label, Note: "The user cancelled. Do not proceed."}, nil
	default:
		return Result{Approved: false, Choice: decision.Label}, nil
	}
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}
