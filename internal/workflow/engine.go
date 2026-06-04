package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"text/template"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/slack-go/slack"
)

type Engine struct {
	rules       []Rule
	poster      ResponsePoster
	runner      CommandRunner
	templateDir string
	logger      *slog.Logger
}

type Rule struct {
	Name   string
	Config config.WorkflowRuleConfig
}

type Options struct {
	Poster      ResponsePoster
	Runner      CommandRunner
	TemplateDir string
	Logger      *slog.Logger
}

func NewEngine(cfg config.Config, opts Options) *Engine {
	names := make([]string, 0, len(cfg.WorkflowRules))
	for name := range cfg.WorkflowRules {
		names = append(names, name)
	}
	sort.Strings(names)

	rules := make([]Rule, 0, len(names))
	for _, name := range names {
		rules = append(rules, Rule{Name: name, Config: cfg.WorkflowRules[name]})
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

	return &Engine{rules: rules, poster: poster, runner: runner, templateDir: templateDir, logger: logger}
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

	e.logger.Debug("no workflow rule matched")
	return nil
}

func (e *Engine) executeRule(ctx context.Context, rule Rule, responseURL string, payload map[string]any, payloadJSON []byte) error {
	for _, trigger := range rule.Config.Triggers {
		switch trigger.Type {
		case "reply-to-slack":
			body, err := e.renderReply(ctx, *trigger.ReplyToSlack, payload, payloadJSON)
			if err != nil {
				return fmt.Errorf("render Slack response for rule %s: %w", rule.Name, err)
			}
			if err := e.poster.Post(ctx, responseURL, body); err != nil {
				return fmt.Errorf("post Slack response for rule %s: %w", rule.Name, err)
			}
		case "run":
			if _, err := e.runner.Run(ctx, *trigger.Run, payloadJSON); err != nil {
				return fmt.Errorf("run command for rule %s: %w", rule.Name, err)
			}
		}
	}
	return nil
}

func (e *Engine) renderReply(ctx context.Context, trigger config.ReplyToSlackTriggerConfig, payload map[string]any, payloadJSON []byte) ([]byte, error) {
	if trigger.Run != nil {
		stdout, err := e.runner.Run(ctx, *trigger.Run, payloadJSON)
		if err != nil {
			return nil, err
		}
		return validJSON(stdout)
	}

	path := e.templatePath(trigger.Template)
	content, err := os.ReadFile(path)
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

func validJSON(body []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if !json.Valid(trimmed) {
		return nil, fmt.Errorf("rendered Slack response must be valid JSON")
	}
	return trimmed, nil
}
