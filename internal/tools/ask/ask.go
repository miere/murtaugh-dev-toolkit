// Package ask implements the `ask` tool: the agent's way to put a question with
// a few options in front of the user as clickable Slack buttons and WAIT for the
// answer, instead of assuming one. It is the model-driven consumer of the shared
// interaction broker (internal/slack/interaction).
//
// It only works inside a Slack conversation: the turn's location is read from the
// context the native client stashes per turn, so the question is asked in the
// same thread the agent is talking in — not the admin DM, and not wherever the
// model guesses. Outside a chat turn (CLI/MCP) it returns an error rather than
// blocking.
package ask

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/slack/interaction"
)

// Tool is the `ask` capability.
type Tool struct {
	broker *interaction.Broker
}

// New constructs an ask Tool against the shared interaction broker. A nil broker
// leaves the tool registered but inert (it returns an error when invoked), which
// is the right behaviour in CLI/MCP processes that have no gateway to route the
// click back.
func New(broker *interaction.Broker) *Tool { return &Tool{broker: broker} }

// Name returns the registry key.
func (t *Tool) Name() string { return "ask" }

// Description is the model-facing summary. It is deliberately explicit that the
// tool blocks for a real answer and must not be second-guessed.
func (t *Tool) Description() string {
	return "Ask the user a question with a few options, shown as clickable buttons in the " +
		"current Slack conversation, and WAIT for their answer. Use this whenever you need a " +
		"decision or confirmation before acting — never assume the answer or treat silence as " +
		"approval. Returns the option the user picked, or a note that they did not respond. " +
		"Only works inside a Slack conversation."
}

// InputSchema declares the arguments: a question and 2+ options, plus an optional
// heading.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"question": {Type: "string", Description: "The question to ask the user."},
			"options": {
				Type:        "array",
				Description: "The answer options, shown as buttons (provide at least two).",
				Items:       &jsonschema.Schema{Type: "string"},
			},
			"title": {Type: "string", Description: "Optional short heading shown above the question."},
		},
		Required: []string{"question", "options"},
	}
}

// Result is the structured outcome. The MCP frontend JSON-marshals it; the loop
// and CLI render it via String().
type Result struct {
	Answered bool   `json:"answered"`
	Choice   string `json:"choice,omitempty"`
	Note     string `json:"note,omitempty"`
}

// String renders the line fed back to the model / shown in the CLI.
func (r Result) String() string {
	if r.Answered {
		return "The user chose: " + r.Choice
	}
	if r.Note != "" {
		return r.Note
	}
	return "The user did not answer."
}

// Invoke posts the question to the current Slack thread and blocks until the user
// answers (or the wait times out / is cancelled).
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	if t.broker == nil {
		return nil, fmt.Errorf("Error: interactive questions are not available in this context")
	}
	loc, ok := agent.TurnLocationFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("Error: the ask tool only works inside a Slack conversation")
	}
	question := strings.TrimSpace(stringArg(args, "question"))
	if question == "" {
		return nil, fmt.Errorf("Error: a question is required")
	}
	options := parseOptions(args["options"])
	if len(options) < 2 {
		return nil, fmt.Errorf("Error: provide at least two options")
	}

	decision, err := t.broker.Ask(ctx, interaction.Destination{ChannelID: loc.ChannelID, ThreadTS: loc.ThreadTS}, interaction.PromptSpec{
		Title:    strings.TrimSpace(stringArg(args, "title")),
		Question: question,
		Options:  options,
	})
	if err != nil {
		return nil, err
	}
	switch {
	case decision.TimedOut:
		return Result{Answered: false, Note: "The user did not respond in time. Do not assume an answer — ask again or stop and wait."}, nil
	case decision.Cancelled:
		return Result{Answered: false, Note: "The question was dismissed before the user answered."}, nil
	default:
		return Result{Answered: true, Choice: decision.Label}, nil
	}
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

func parseOptions(raw any) []interaction.Option {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]interaction.Option, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		out = append(out, interaction.Option{ID: s, Label: s})
	}
	return out
}
