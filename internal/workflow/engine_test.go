package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/agentdelegate"
	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/slack-go/slack"
)

type recordingPoster struct {
	urls   []string
	bodies [][]byte
}

func (p *recordingPoster) Post(_ context.Context, url string, body []byte) error {
	p.urls = append(p.urls, url)
	p.bodies = append(p.bodies, append([]byte(nil), body...))
	return nil
}

type recordingRunner struct {
	commands []config.RunTriggerConfig
	inputs   [][]byte
	outputs  [][]byte
}

func (r *recordingRunner) Run(_ context.Context, command config.RunTriggerConfig, input []byte) ([]byte, error) {
	r.commands = append(r.commands, command)
	r.inputs = append(r.inputs, append([]byte(nil), input...))
	if len(r.outputs) == 0 {
		return nil, nil
	}
	output := r.outputs[0]
	r.outputs = r.outputs[1:]
	return output, nil
}

func TestEnginePostsTemplateResponseAndRunsBackgroundCommand(t *testing.T) {
	templateDir := t.TempDir()
	writeTemplate(t, templateDir, `{"replace_original":true,"text":"approved in {{ index .Payload.channel "name" }}"}`)

	poster := &recordingPoster{}
	runner := &recordingRunner{}
	engine := NewEngine(workflowConfig(), Options{Poster: poster, Runner: runner, TemplateDir: templateDir})

	if err := engine.Execute(context.Background(), approvalInteraction()); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if len(poster.bodies) != 1 || poster.urls[0] != "https://hooks.slack.test/response" {
		t.Fatalf("unexpected posted responses: urls=%#v bodies=%#v", poster.urls, poster.bodies)
	}
	if !strings.Contains(string(poster.bodies[0]), "approved in nc-code-reviews") {
		t.Fatalf("unexpected posted body: %s", poster.bodies[0])
	}
	if len(runner.commands) != 1 || runner.commands[0].Cmd != "/path/to/background" {
		t.Fatalf("expected background command to run, got: %#v", runner.commands)
	}
	if !json.Valid(runner.inputs[0]) {
		t.Fatalf("expected command input to be JSON")
	}
}

func TestEnginePostsCommandRenderedResponse(t *testing.T) {
	poster := &recordingPoster{}
	runner := &recordingRunner{outputs: [][]byte{[]byte(`{"text":"from command"}`)}}
	cfg := workflowConfig()
	rule := cfg.WorkflowRules["code-review-approval"]
	rule.Triggers = []config.TriggerConfig{{
		Type:         "reply-to-slack",
		ReplyToSlack: &config.ReplyToSlackTriggerConfig{Run: &config.RunTriggerConfig{Cmd: "/path/to/render"}},
	}}
	cfg.WorkflowRules["code-review-approval"] = rule

	engine := NewEngine(cfg, Options{Poster: poster, Runner: runner})
	if err := engine.Execute(context.Background(), approvalInteraction()); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if len(runner.commands) != 1 || runner.commands[0].Cmd != "/path/to/render" {
		t.Fatalf("expected render command to run, got: %#v", runner.commands)
	}
	if len(poster.bodies) != 1 || string(poster.bodies[0]) != `{"text":"from command"}` {
		t.Fatalf("unexpected posted command response: %#v", poster.bodies)
	}
}

type fakeDelegator struct {
	jsonOut   []byte
	jsonErr   error
	forgetErr error

	agents  []string
	prompts []string
	forgets int
}

func (f *fakeDelegator) RunForJSON(_ context.Context, agent, prompt string) ([]byte, error) {
	f.agents = append(f.agents, agent)
	f.prompts = append(f.prompts, prompt)
	return f.jsonOut, f.jsonErr
}

func (f *fakeDelegator) RunAndForget(_ context.Context, agent, prompt string) error {
	f.agents = append(f.agents, agent)
	f.prompts = append(f.prompts, prompt)
	f.forgets++
	return f.forgetErr
}

func delegateReplyConfig(prompt string) config.Config {
	cfg := workflowConfig()
	rule := cfg.WorkflowRules["code-review-approval"]
	rule.Triggers = []config.TriggerConfig{{
		Type:         "reply-to-slack",
		ReplyToSlack: &config.ReplyToSlackTriggerConfig{DelegateToAgent: &config.DelegateToAgentConfig{Agent: "default", Prompt: prompt}},
	}}
	cfg.WorkflowRules["code-review-approval"] = rule
	return cfg
}

func TestEngineDelegateReplyPostsJSON(t *testing.T) {
	poster := &recordingPoster{}
	del := &fakeDelegator{jsonOut: []byte(`{"text":"from agent"}`)}
	cfg := delegateReplyConfig(`Summarise approval in {{ index .Payload.channel "name" }}`)

	engine := NewEngine(cfg, Options{Poster: poster, Delegator: del})
	if err := engine.Execute(context.Background(), approvalInteraction()); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if len(del.prompts) != 1 || del.agents[0] != "default" {
		t.Fatalf("unexpected delegation: agents=%#v prompts=%#v", del.agents, del.prompts)
	}
	if !strings.Contains(del.prompts[0], "nc-code-reviews") {
		t.Fatalf("prompt was not rendered with payload: %q", del.prompts[0])
	}
	if len(poster.bodies) != 1 || string(poster.bodies[0]) != `{"text":"from agent"}` {
		t.Fatalf("unexpected posted body: %#v", poster.bodies)
	}
}

func TestEngineDelegateReplyNonJSONSkipsPost(t *testing.T) {
	poster := &recordingPoster{}
	del := &fakeDelegator{jsonErr: agentdelegate.ErrNonJSONOutput}
	cfg := delegateReplyConfig("Summarise it")

	engine := NewEngine(cfg, Options{Poster: poster, Delegator: del})
	if err := engine.Execute(context.Background(), approvalInteraction()); err != nil {
		t.Fatalf("Execute should not error on non-JSON output, got: %v", err)
	}
	if len(poster.bodies) != 0 {
		t.Fatalf("expected nothing posted on non-JSON output, got: %#v", poster.bodies)
	}
}

func TestEngineTopLevelDelegateFireAndForget(t *testing.T) {
	poster := &recordingPoster{}
	del := &fakeDelegator{}
	cfg := workflowConfig()
	rule := cfg.WorkflowRules["code-review-approval"]
	rule.Triggers = []config.TriggerConfig{{
		Type:            "delegate-to-agent",
		DelegateToAgent: &config.DelegateToAgentConfig{Agent: "default", Prompt: "Act on it"},
	}}
	cfg.WorkflowRules["code-review-approval"] = rule

	engine := NewEngine(cfg, Options{Poster: poster, Delegator: del})
	if err := engine.Execute(context.Background(), approvalInteraction()); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if del.forgets != 1 || del.agents[0] != "default" {
		t.Fatalf("expected one fire-and-forget delegation, got forgets=%d agents=%#v", del.forgets, del.agents)
	}
	if len(poster.bodies) != 0 {
		t.Fatalf("fire-and-forget must not post, got: %#v", poster.bodies)
	}
}

func TestEngineSkipsWhenNoRuleMatches(t *testing.T) {
	poster := &recordingPoster{}
	runner := &recordingRunner{}
	engine := NewEngine(workflowConfig(), Options{Poster: poster, Runner: runner})
	interaction := approvalInteraction()
	interaction.Channel.Name = "other-channel"

	if err := engine.Execute(context.Background(), interaction); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(poster.bodies) != 0 || len(runner.commands) != 0 {
		t.Fatalf("expected no actions, got posts=%d commands=%d", len(poster.bodies), len(runner.commands))
	}
}

func TestEngineInstallsDefaultPingPongRule(t *testing.T) {
	poster := &recordingPoster{}
	engine := NewEngine(config.Config{}, Options{Poster: poster})

	if err := engine.Execute(context.Background(), pingInteraction()); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(poster.bodies) != 1 || poster.urls[0] != "https://hooks.slack.test/ping" {
		t.Fatalf("unexpected posted responses: urls=%#v bodies=%#v", poster.urls, poster.bodies)
	}
	var response map[string]any
	if err := json.Unmarshal(poster.bodies[0], &response); err != nil {
		t.Fatalf("default pong body is not JSON: %v", err)
	}
	if response["thread_ts"] != "1717450123.000100" || !strings.Contains(string(poster.bodies[0]), ":recycle: The server communication is functional.") {
		t.Fatalf("unexpected default pong body: %s", poster.bodies[0])
	}
}

func TestEngineUsesEmbeddedFallbackTemplateForConfiguredRule(t *testing.T) {
	poster := &recordingPoster{}
	engine := NewEngine(config.Config{WorkflowRules: map[string]config.WorkflowRuleConfig{
		"startup-ping-pong": {
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
	}}, Options{Poster: poster})

	if err := engine.Execute(context.Background(), pingInteraction()); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(poster.bodies) != 1 || !strings.Contains(string(poster.bodies[0]), ":recycle: The server communication is functional.") {
		t.Fatalf("unexpected fallback template response: %#v", poster.bodies)
	}
}

func TestEngineLogsInfoWhenNoRuleMatches(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	engine := NewEngine(workflowConfig(), Options{Logger: logger})
	interaction := pingInteraction()

	if err := engine.Execute(context.Background(), interaction); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	output := logs.String()
	if !strings.Contains(output, "interactive request had no matching workflow rule") || !strings.Contains(output, "action_ids=[ping]") {
		t.Fatalf("expected info log for unmatched request, got %q", output)
	}
}

func TestMatchesPartialNestedArrays(t *testing.T) {
	actual := map[string]any{
		"channel": map[string]any{"name": "nc-code-reviews", "id": "C123"},
		"actions": []any{
			map[string]any{"block_id": "other", "action_id": "ignore"},
			map[string]any{"block_id": "github_pull_request", "action_id": "approve_only", "value": "42"},
		},
	}
	expected := map[string]any{
		"channel": map[string]any{"name": "nc-code-reviews"},
		"actions": []any{map[string]any{"block_id": "github_pull_request", "action_id": "approve_only"}},
	}
	if !matches(expected, actual) {
		t.Fatal("expected partial matcher to match")
	}
}

func workflowConfig() config.Config {
	return config.Config{WorkflowRules: map[string]config.WorkflowRuleConfig{
		"code-review-approval": {
			RequestEvent: "interactive",
			Match: map[string]any{
				"channel": map[string]any{"name": "nc-code-reviews"},
				"actions": []any{map[string]any{"block_id": "github_pull_request", "action_id": "approve_only"}},
			},
			Triggers: []config.TriggerConfig{
				{Type: "reply-to-slack", ReplyToSlack: &config.ReplyToSlackTriggerConfig{Template: "code-review/approved.json"}},
				{Type: "run", Run: &config.RunTriggerConfig{Cmd: "/path/to/background"}},
			},
		},
	}}
}

func approvalInteraction() slack.InteractionCallback {
	return slack.InteractionCallback{
		Type:        slack.InteractionTypeBlockActions,
		ResponseURL: "https://hooks.slack.test/response",
		Channel:     slack.Channel{GroupConversation: slack.GroupConversation{Name: "nc-code-reviews"}},
		ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{{
			BlockID:  "github_pull_request",
			ActionID: "approve_only",
		}}},
	}
}

func pingInteraction() slack.InteractionCallback {
	return slack.InteractionCallback{
		Type:        slack.InteractionTypeBlockActions,
		ResponseURL: "https://hooks.slack.test/ping",
		Message:     slack.Message{Msg: slack.Msg{Timestamp: "1717450123.000100"}},
		ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{{
			ActionID: "ping",
		}}},
	}
}

func writeTemplate(t *testing.T, baseDir string, content string) {
	t.Helper()
	dir := filepath.Join(baseDir, "code-review")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create template directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "approved.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}
}
