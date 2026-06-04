package workflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
