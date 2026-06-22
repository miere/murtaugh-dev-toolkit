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
	return "Ask the user one or more questions in the current Slack conversation and WAIT for " +
		"their answer. Use this whenever you need a decision, confirmation, or input before " +
		"acting — never assume the answer or treat silence as approval. For a single quick " +
		"choice, pass `question` + `options` (rendered as clickable buttons). For several " +
		"questions at once, or multi-select / free-text answers, pass `questions` (rendered as " +
		"a form behind an Answer button). Returns what the user chose or typed, or a note that " +
		"they did not respond. Only works inside a Slack conversation."
}

// InputSchema declares two interchangeable shapes: the simple single-question
// button form (`question` + `options`), and the richer modal form (`questions`,
// each single-select / multi-select / free-text). Either may be supplied; when
// both are present the richer `questions` form wins.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"question": {Type: "string", Description: "A single question to ask (button form). Provide with `options`."},
			"options": {
				Type:        "array",
				Description: "The answer options for `question`, shown as buttons (provide at least two).",
				Items:       &jsonschema.Schema{Type: "string"},
			},
			"title": {Type: "string", Description: "Optional short heading shown above the question(s)."},
			"questions": {
				Type: "array",
				Description: "Two or more questions, or any multi-select / free-text question, collected " +
					"in one modal form with a Submit button. Use instead of `question`/`options` for " +
					"richer prompts.",
				Items: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"label": {Type: "string", Description: "The question text shown above the input."},
						"options": {
							Type:        "array",
							Description: "Choices for a select question (omit for free-text).",
							Items:       &jsonschema.Schema{Type: "string"},
						},
						"multiSelect": {Type: "boolean", Description: "Allow choosing more than one option (checkboxes instead of radio)."},
						"freeText":    {Type: "boolean", Description: "Render a free-text input instead of a list of options."},
					},
					Required: []string{"label"},
				},
			},
		},
	}
}

// Result is the structured outcome. The MCP frontend JSON-marshals it; the loop
// and CLI render it via String().
//
// The single-question button path sets Choice. The modal-form path sets Answers
// (one entry per question, in order) instead. Either way Answered/Note carry the
// terminal status.
type Result struct {
	Answered bool         `json:"answered"`
	Choice   string       `json:"choice,omitempty"`
	Answers  []FormAnswer `json:"answers,omitempty"`
	Note     string       `json:"note,omitempty"`
}

// FormAnswer is one question's answer in a modal-form Result. Choices holds the
// selected option label(s); Text holds a free-text answer. Exactly one is set.
type FormAnswer struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices,omitempty"`
	Text     string   `json:"text,omitempty"`
}

// String renders the line fed back to the model / shown in the CLI.
func (r Result) String() string {
	if r.Answered {
		if len(r.Answers) > 0 {
			var b strings.Builder
			b.WriteString("The user answered:")
			for _, a := range r.Answers {
				b.WriteString("\n- ")
				b.WriteString(a.Question)
				b.WriteString(": ")
				if a.Text != "" {
					b.WriteString(a.Text)
				} else if len(a.Choices) > 0 {
					b.WriteString(strings.Join(a.Choices, ", "))
				} else {
					b.WriteString("(no answer)")
				}
			}
			return b.String()
		}
		return "The user chose: " + r.Choice
	}
	if r.Note != "" {
		return r.Note
	}
	return "The user did not answer."
}

// Invoke posts the question(s) to the current Slack thread and blocks until the
// user answers (or the wait times out / is cancelled). It routes to the modal
// form when a `questions` array is supplied and demands it (more than one
// question, or any multi-select / free-text question); otherwise it uses the
// single-question button path unchanged.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	if t.broker == nil {
		return nil, fmt.Errorf("Error: interactive questions are not available in this context")
	}
	loc, ok := agent.TurnLocationFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("Error: the ask tool only works inside a Slack conversation")
	}
	dest := interaction.Destination{ChannelID: loc.ChannelID, ThreadTS: loc.ThreadTS}
	title := strings.TrimSpace(stringArg(args, "title"))

	if questions := parseQuestions(args["questions"]); len(questions) > 0 {
		if needsForm(questions) {
			return t.invokeForm(ctx, dest, title, questions)
		}
		// A single plain single-select question expressed via `questions` still
		// works fine as a button prompt; fold it into the simple path.
		if len(questions) == 1 && strings.TrimSpace(stringArg(args, "question")) == "" {
			args = map[string]any{
				"question": questions[0].Label,
				"options":  optionLabelsAny(questions[0].Options),
				"title":    title,
			}
		}
	}

	question := strings.TrimSpace(stringArg(args, "question"))
	if question == "" {
		return nil, fmt.Errorf("Error: a question is required")
	}
	options := parseOptions(args["options"])
	if len(options) < 2 {
		return nil, fmt.Errorf("Error: provide at least two options")
	}

	decision, err := t.broker.Ask(ctx, dest, interaction.PromptSpec{
		Title:    title,
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

// invokeForm runs the modal-form path: build a FormSpec, block on AskForm, and
// shape the submission into a Result listing each question's answer(s).
func (t *Tool) invokeForm(ctx context.Context, dest interaction.Destination, title string, questions []interaction.Question) (any, error) {
	resp, err := t.broker.AskForm(ctx, dest, interaction.FormSpec{Title: title, Questions: questions})
	if err != nil {
		return nil, err
	}
	switch {
	case resp.TimedOut:
		return Result{Answered: false, Note: "The user did not respond in time. Do not assume an answer — ask again or stop and wait."}, nil
	case resp.Cancelled:
		return Result{Answered: false, Note: "The form was dismissed before the user answered."}, nil
	}
	answers := make([]FormAnswer, 0, len(questions))
	for _, q := range questions {
		fa := FormAnswer{Question: q.Label}
		if q.FreeText {
			fa.Text = resp.FreeText[q.Key]
		} else {
			fa.Choices = resp.Answers[q.Key]
		}
		answers = append(answers, fa)
	}
	return Result{Answered: true, Answers: answers}, nil
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// needsForm reports whether the questions require the modal: more than one
// question, or any multi-select / free-text answer. A lone plain single-select
// question can still ride the simpler button path.
func needsForm(questions []interaction.Question) bool {
	if len(questions) > 1 {
		return true
	}
	for _, q := range questions {
		if q.MultiSelect || q.FreeText {
			return true
		}
	}
	return false
}

// parseQuestions reads the `questions` array into interaction.Question values.
// Each gets a stable key (q0, q1, …) so answers round-trip through the modal.
func parseQuestions(raw any) []interaction.Question {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]interaction.Question, 0, len(list))
	for i, v := range list {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		label := strings.TrimSpace(stringArg(m, "label"))
		if label == "" {
			continue
		}
		out = append(out, interaction.Question{
			Key:         fmt.Sprintf("q%d", i),
			Label:       label,
			Options:     parseOptions(m["options"]),
			MultiSelect: boolArg(m, "multiSelect"),
			FreeText:    boolArg(m, "freeText"),
		})
	}
	return out
}

func boolArg(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}

// optionLabelsAny re-expands parsed options into the []any of strings the simple
// button path's parseOptions expects.
func optionLabelsAny(opts []interaction.Option) []any {
	out := make([]any, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.Label)
	}
	return out
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
