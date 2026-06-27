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

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/agentdelegate"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/journal"
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
	recorder    journal.Recorder
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
	// Recorder receives gateway-stream journal events for each interaction the
	// engine processes (match, no-match, per-trigger outcome). Nil defaults to
	// a no-op, so the engine records nothing unless wired with a real recorder.
	Recorder journal.Recorder
}

func NewEngine(cfg config.Config, opts Options) *Engine {
	rulesConfig := cfg.WorkflowRules
	templateFS := opts.TemplateFS
	if templateFS == nil {
		templateFS = assets.FS
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
	recorder := opts.Recorder
	if recorder == nil {
		recorder = journal.NopRecorder{}
	}

	return &Engine{rules: rules, poster: poster, runner: runner, delegator: opts.Delegator, templateDir: templateDir, templateFS: templateFS, logger: logger, recorder: recorder}
}

// Execute matches interaction against the configured rules and runs the
// triggers of the first match. rawPayload is the verbatim Slack interaction
// callback as delivered over the wire; it is what a `run` trigger receives on
// stdin (full fidelity, matching the docs). Callers that don't have the raw
// bytes (tests, synthetic events) may pass nil, in which case a marshaled form
// of interaction is used instead.
func (e *Engine) Execute(ctx context.Context, interaction slack.InteractionCallback, rawPayload []byte) error {
	payload, err := payloadMap(interaction)
	if err != nil {
		return err
	}
	runStdin := rawPayload
	if len(runStdin) == 0 {
		runStdin, err = json.Marshal(interaction)
		if err != nil {
			return fmt.Errorf("marshal interaction payload: %w", err)
		}
	}

	keys := journal.Keys{
		TeamID:    interaction.Team.ID,
		ChannelID: interaction.Channel.ID,
		UserID:    interaction.User.ID,
	}

	for _, rule := range e.rules {
		if rule.Config.RequestEvent != "interactive" || !matches(rule.Config.Match, payload) {
			continue
		}
		e.logger.Info("workflow rule matched", "rule", rule.Name)
		ruleKeys := keys
		ruleKeys.RuleID = rule.Name
		e.record(ctx, "workflow.matched", journal.LevelInfo, "matched workflow rule "+rule.Name, ruleKeys,
			map[string]any{"interaction_type": string(interaction.Type)})
		return e.executeRule(ctx, rule, interaction.ResponseURL, payload, runStdin, keys)
	}

	e.logger.Info(
		"interactive request had no matching workflow rule",
		"interaction_type", interaction.Type,
		"channel", interaction.Channel.Name,
		"callback_id", interaction.CallbackID,
		"action_ids", blockActionIDs(interaction.ActionCallback.BlockActions),
	)
	e.record(ctx, "workflow.no_match", journal.LevelDebug, "no workflow rule matched", keys, map[string]any{
		"interaction_type": string(interaction.Type),
		"callback_id":      interaction.CallbackID,
		"action_ids":       blockActionIDs(interaction.ActionCallback.BlockActions),
	})
	return nil
}

func (e *Engine) executeRule(ctx context.Context, rule Rule, responseURL string, payload map[string]any, runStdin []byte, keys journal.Keys) error {
	keys.RuleID = rule.Name
	for _, trigger := range rule.Config.Triggers {
		switch trigger.Type {
		case "reply-to-slack":
			body, err := e.renderReply(ctx, *trigger.ReplyToSlack, payload, runStdin)
			if err != nil {
				// A delegate-to-agent reply that produced non-JSON is not a hard
				// failure: the runner already logged a warning with the output.
				// Skip posting and move on rather than failing the whole rule.
				if errors.Is(err, agentdelegate.ErrNonJSONOutput) {
					e.record(ctx, "workflow.trigger", journal.LevelWarn, "reply-to-slack skipped: agent returned non-JSON", keys,
						map[string]any{"trigger": "reply-to-slack", "json_valid": false})
					continue
				}
				e.record(ctx, "workflow.trigger", journal.LevelError, "reply-to-slack render failed", keys,
					map[string]any{"trigger": "reply-to-slack", "error": err.Error()})
				return fmt.Errorf("render Slack response for rule %s: %w", rule.Name, err)
			}
			if err := e.poster.Post(ctx, responseURL, body); err != nil {
				e.record(ctx, "workflow.trigger", journal.LevelError, "reply-to-slack post failed", keys,
					map[string]any{"trigger": "reply-to-slack", "error": err.Error()})
				return fmt.Errorf("post Slack response for rule %s: %w", rule.Name, err)
			}
			e.record(ctx, "workflow.trigger", journal.LevelInfo, "reply-to-slack posted", keys,
				map[string]any{"trigger": "reply-to-slack"})
		case "run":
			runCfg, err := renderRunConfig(*trigger.Run, payload)
			if err != nil {
				e.record(ctx, "workflow.trigger", journal.LevelError, "run command template failed", keys,
					map[string]any{"trigger": "run", "error": err.Error()})
				return fmt.Errorf("render run command for rule %s: %w", rule.Name, err)
			}
			if _, err := e.runner.Run(ctx, runCfg, runStdin); err != nil {
				e.record(ctx, "workflow.trigger", journal.LevelError, "run command failed", keys,
					map[string]any{"trigger": "run", "error": err.Error()})
				return fmt.Errorf("run command for rule %s: %w", rule.Name, err)
			}
			e.record(ctx, "workflow.trigger", journal.LevelInfo, "run command executed", keys,
				map[string]any{"trigger": "run"})
		case "delegate-to-agent":
			if err := e.delegate(ctx, *trigger.DelegateToAgent, payload); err != nil {
				e.record(ctx, "workflow.trigger", journal.LevelError, "delegate-to-agent failed", keys,
					map[string]any{"trigger": "delegate-to-agent", "error": err.Error()})
				return fmt.Errorf("delegate-to-agent for rule %s: %w", rule.Name, err)
			}
			e.record(ctx, "workflow.trigger", journal.LevelInfo, "delegate-to-agent dispatched", keys,
				map[string]any{"trigger": "delegate-to-agent"})
		}
	}
	return nil
}

// record emits a gateway-stream journal event, stamping the correlation id the
// gateway minted for this interaction (carried on ctx). A nil-configured engine
// uses a no-op recorder, so this is always safe to call.
func (e *Engine) record(ctx context.Context, kind string, level journal.Level, summary string, keys journal.Keys, payload any) {
	e.recorder.Record(ctx, journal.Event{
		Stream:  journal.StreamGateway,
		Kind:    kind,
		Level:   level,
		Summary: summary,
		CorrID:  journal.CorrIDFromContext(ctx),
		Keys:    keys,
		Payload: payload,
	})
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

func (e *Engine) renderReply(ctx context.Context, trigger config.ReplyToSlackTriggerConfig, payload map[string]any, runStdin []byte) ([]byte, error) {
	if trigger.Run != nil {
		runCfg, err := renderRunConfig(*trigger.Run, payload)
		if err != nil {
			return nil, err
		}
		stdout, err := e.runner.Run(ctx, runCfg, runStdin)
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

// renderRunConfig renders a run trigger's cmd, args, and workdir through
// text/template against the interaction payload (under .Payload), so a rule can
// parameterise the command — e.g. `{{ (index .Payload.actions 0).value }}`.
// Timeout is left verbatim (it is a duration, never templated). A malformed or
// unresolved placeholder fails the rule loudly rather than executing a
// half-rendered command.
func renderRunConfig(cfg config.RunTriggerConfig, payload map[string]any) (config.RunTriggerConfig, error) {
	data := map[string]any{"Payload": payload}
	cmd, err := renderPrompt(cfg.Cmd, data)
	if err != nil {
		return config.RunTriggerConfig{}, fmt.Errorf("run cmd: %w", err)
	}
	var args []string
	if len(cfg.Args) > 0 {
		args = make([]string, len(cfg.Args))
		for i, a := range cfg.Args {
			rendered, err := renderPrompt(a, data)
			if err != nil {
				return config.RunTriggerConfig{}, fmt.Errorf("run arg %d: %w", i, err)
			}
			args[i] = rendered
		}
	}
	workdir, err := renderPrompt(cfg.WorkDir, data)
	if err != nil {
		return config.RunTriggerConfig{}, fmt.Errorf("run workdir: %w", err)
	}
	return config.RunTriggerConfig{Cmd: cmd, Args: args, Timeout: cfg.Timeout, WorkDir: workdir}, nil
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
