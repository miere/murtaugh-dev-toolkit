package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"text/template"

	"github.com/miere/murtaugh-dev-toolkit/assets"
	"github.com/miere/murtaugh-dev-toolkit/internal/agentdelegate"
	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/slack-go/slack"
)

// AgentDelegator runs a delegate-to-agent action. A reply-to-slack trigger
// captures JSON output via RunForJSON and posts it; a top-level trigger is
// fire-and-forget via RunAndForget. *agentdelegate.Runner satisfies it.
type AgentDelegator interface {
	RunForJSON(ctx context.Context, agent, prompt string) ([]byte, error)
	RunAndForget(ctx context.Context, agent, prompt string) error
}

type Engine struct {
	rules       []Rule
	poster      ResponsePoster
	runner      CommandRunner
	delegator   AgentDelegator
	templateDir string
	templateFS  fs.FS
	logger      *slog.Logger
}

type Rule struct {
	Name   string
	Config config.WorkflowRuleConfig
}

type Options struct {
	Poster      ResponsePoster
	Runner      CommandRunner
	Delegator   AgentDelegator
	TemplateDir string
	TemplateFS  fs.FS
	Logger      *slog.Logger
}

func NewEngine(cfg config.Config, opts Options) *Engine {
	rulesConfig := cfg.WorkflowRules
	templateFS := opts.TemplateFS
	if templateFS == nil {
		templateFS = assets.FS
	}
	if len(rulesConfig) == 0 {
		rulesConfig = defaultWorkflowRules()
	}

	names := make([]string, 0, len(rulesConfig))
	for name := range rulesConfig {
		names = append(names, name)
	}
	sort.Strings(names)

	rules := make([]Rule, 0, len(names))
	for _, name := range names {
		rules = append(rules, Rule{Name: name, Config: rulesConfig[name]})
	}

	templateDir := opts.TemplateDir
	if templateDir == "" {
		templateDir = cfg.BaseDir
	}
	if templateDir == "" {
		templateDir = "."
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	poster := opts.Poster
	if poster == nil {
		poster = HTTPResponsePoster{}
	}
	runner := opts.Runner
	if runner == nil {
		runner = OSCommandRunner{}
	}

	return &Engine{rules: rules, poster: poster, runner: runner, delegator: opts.Delegator, templateDir: templateDir, templateFS: templateFS, logger: logger}
}

func defaultWorkflowRules() map[string]config.WorkflowRuleConfig {
	return map[string]config.WorkflowRuleConfig{
		"ping-pong": {
			RequestEvent: "interactive",
			Match: map[string]any{
				"type":    "block_actions",
				"actions": []any{map[string]any{"action_id": "ping"}},
			},
			Triggers: []config.TriggerConfig{{
				Type:         "reply-to-slack",
				ReplyToSlack: &config.ReplyToSlackTriggerConfig{Template: "templates/ping/02-pong.json"},
			}},
		},
	}
}

func (e *Engine) Execute(ctx context.Context, interaction slack.InteractionCallback) error {
	payload, err := payloadMap(interaction)
	if err != nil {
		return err
	}
	payloadJSON, err := json.Marshal(interaction)
	if err != nil {
		return fmt.Errorf("marshal interaction payload: %w", err)
	}

	for _, rule := range e.rules {
		if rule.Config.RequestEvent != "interactive" || !matches(rule.Config.Match, payload) {
			continue
		}
		e.logger.Info("workflow rule matched", "rule", rule.Name)
		return e.executeRule(ctx, rule, interaction.ResponseURL, payload, payloadJSON)
	}

	e.logger.Info(
		"interactive request had no matching workflow rule",
		"interaction_type", interaction.Type,
		"channel", interaction.Channel.Name,
		"callback_id", interaction.CallbackID,
		"action_ids", blockActionIDs(interaction.ActionCallback.BlockActions),
	)
	return nil
}

func (e *Engine) executeRule(ctx context.Context, rule Rule, responseURL string, payload map[string]any, payloadJSON []byte) error {
	for _, trigger := range rule.Config.Triggers {
		switch trigger.Type {
		case "reply-to-slack":
			body, err := e.renderReply(ctx, *trigger.ReplyToSlack, payload, payloadJSON)
			if err != nil {
				// A delegate-to-agent reply that produced non-JSON is not a hard
				// failure: the runner already logged a warning with the output.
				// Skip posting and move on rather than failing the whole rule.
				if errors.Is(err, agentdelegate.ErrNonJSONOutput) {
					continue
				}
				return fmt.Errorf("render Slack response for rule %s: %w", rule.Name, err)
			}
			if err := e.poster.Post(ctx, responseURL, body); err != nil {
				return fmt.Errorf("post Slack response for rule %s: %w", rule.Name, err)
			}
		case "run":
			if _, err := e.runner.Run(ctx, *trigger.Run, payloadJSON); err != nil {
				return fmt.Errorf("run command for rule %s: %w", rule.Name, err)
			}
		case "delegate-to-agent":
			if err := e.delegate(ctx, *trigger.DelegateToAgent, payload); err != nil {
				return fmt.Errorf("delegate-to-agent for rule %s: %w", rule.Name, err)
			}
		}
	}
	return nil
}

// delegate runs a fire-and-forget top-level delegate-to-agent trigger: it
// renders the prompt against the interaction payload and hands it to the agent,
// discarding any text output (the agent acts through its own tools).
func (e *Engine) delegate(ctx context.Context, cfg config.DelegateToAgentConfig, payload map[string]any) error {
	if e.delegator == nil {
		return errors.New("delegate-to-agent requires ACP to be enabled")
	}
	prompt, err := renderPrompt(cfg.Prompt, map[string]any{"Payload": payload})
	if err != nil {
		return err
	}
	return e.delegator.RunAndForget(ctx, cfg.Agent, prompt)
}

func (e *Engine) renderReply(ctx context.Context, trigger config.ReplyToSlackTriggerConfig, payload map[string]any, payloadJSON []byte) ([]byte, error) {
	if trigger.Run != nil {
		stdout, err := e.runner.Run(ctx, *trigger.Run, payloadJSON)
		if err != nil {
			return nil, err
		}
		return validJSON(stdout)
	}

	if trigger.DelegateToAgent != nil {
		if e.delegator == nil {
			return nil, errors.New("delegate-to-agent requires ACP to be enabled")
		}
		prompt, err := renderPrompt(trigger.DelegateToAgent.Prompt, map[string]any{"Payload": payload})
		if err != nil {
			return nil, err
		}
		// RunForJSON validates the agent output is JSON and, on failure, logs a
		// warning with the raw output and returns ErrNonJSONOutput — which the
		// caller treats as "skip posting", not a hard error.
		return e.delegator.RunForJSON(ctx, trigger.DelegateToAgent.Agent, prompt)
	}

	path := e.templatePath(trigger.Template)
	content, err := e.readTemplate(trigger.Template, path)
	if err != nil {
		return nil, fmt.Errorf("read template: %w", err)
	}
	tpl, err := template.New(filepath.Base(path)).Option("missingkey=error").Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}

	var rendered bytes.Buffer
	data := map[string]any{"Payload": payload}
	if err := tpl.Execute(&rendered, data); err != nil {
		return nil, fmt.Errorf("execute template: %w", err)
	}
	return validJSON(rendered.Bytes())
}

func (e *Engine) templatePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(e.templateDir, path)
}

func (e *Engine) readTemplate(templatePath string, resolvedPath string) ([]byte, error) {
	content, err := os.ReadFile(resolvedPath)
	if err == nil {
		return content, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if e.templateFS != nil && !filepath.IsAbs(templatePath) {
		return fs.ReadFile(e.templateFS, filepath.ToSlash(templatePath))
	}
	return nil, err
}

func blockActionIDs(actions []*slack.BlockAction) []string {
	ids := make([]string, 0, len(actions))
	for _, action := range actions {
		if action == nil {
			continue
		}
		ids = append(ids, action.ActionID)
	}
	return ids
}

// renderPrompt renders a delegate-to-agent prompt through text/template with
// the given data (the interaction payload under .Payload for workflow rules),
// using missingkey=error so a typo'd placeholder fails loudly rather than
// sending the agent a half-rendered prompt.
func renderPrompt(promptTemplate string, data map[string]any) (string, error) {
	tpl, err := template.New("prompt").Option("missingkey=error").Parse(promptTemplate)
	if err != nil {
		return "", fmt.Errorf("parse prompt template: %w", err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute prompt template: %w", err)
	}
	return buf.String(), nil
}

func validJSON(body []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if !json.Valid(trimmed) {
		return nil, fmt.Errorf("rendered Slack response must be valid JSON")
	}
	return trimmed, nil
}
