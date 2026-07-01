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
	"testing/fstest"

	"github.com/miere/murtaugh/internal/agentdelegate"
	"github.com/miere/murtaugh/internal/config"
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

	if err := engine.Execute(context.Background(), approvalInteraction(), nil); err != nil {
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

func TestEngineRendersRunArgsAndPipesRawStdin(t *testing.T) {
	cfg := config.Config{WorkflowRules: map[string]config.WorkflowRuleConfig{
		"pr-approve": {
			RequestEvent: "interactive",
			Match:        map[string]any{"actions": []any{map[string]any{"action_id": "approve_only"}}},
			Triggers: []config.TriggerConfig{
				{Type: "run", Run: &config.RunTriggerConfig{
					Cmd:  "bash",
					Args: []string{"-c", "gh pr review {{ (index .Payload.actions 0).value }}"},
				}},
			},
		},
	}}
	runner := &recordingRunner{}
	engine := NewEngine(cfg, Options{Runner: runner})

	interaction := slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{{
			ActionID: "approve_only",
			Value:    "owner/repo#123",
		}}},
	}
	raw := []byte(`{"verbatim":"exactly-what-slack-sent"}`)
	if err := engine.Execute(context.Background(), interaction, raw); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("expected one run command, got %d", len(runner.commands))
	}
	// run args are template-rendered against .Payload
	if args := runner.commands[0].Args; len(args) != 2 || args[1] != "gh pr review owner/repo#123" {
		t.Fatalf("run args not rendered against .Payload: %#v", args)
	}
	// the run command receives the RAW Slack payload on stdin, verbatim
	if string(runner.inputs[0]) != string(raw) {
		t.Fatalf("stdin = %q, want raw payload %q", runner.inputs[0], raw)
	}
}

func TestEngineRunArgTemplateErrorFailsRule(t *testing.T) {
	cfg := config.Config{WorkflowRules: map[string]config.WorkflowRuleConfig{
		"bad": {
			RequestEvent: "interactive",
			Match:        map[string]any{"actions": []any{map[string]any{"action_id": "approve_only"}}},
			Triggers: []config.TriggerConfig{
				{Type: "run", Run: &config.RunTriggerConfig{Cmd: "bash", Args: []string{"{{ .Payload.missing.field }}"}}},
			},
		},
	}}
	runner := &recordingRunner{}
	engine := NewEngine(cfg, Options{Runner: runner})

	interaction := slack.InteractionCallback{
		Type:           slack.InteractionTypeBlockActions,
		ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{{ActionID: "approve_only"}}},
	}
	if err := engine.Execute(context.Background(), interaction, nil); err == nil {
		t.Fatal("expected an unresolved-placeholder template error to fail the rule")
	}
	if len(runner.commands) != 0 {
		t.Fatalf("command should not run when its args fail to render: %#v", runner.commands)
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
	if err := engine.Execute(context.Background(), approvalInteraction(), nil); err != nil {
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
	jsonOut []byte
	jsonErr error

	agents  []string
	prompts []string
}

func (f *fakeDelegator) RunForJSON(_ context.Context, agent, prompt string) ([]byte, error) {
	f.agents = append(f.agents, agent)
	f.prompts = append(f.prompts, prompt)
	return f.jsonOut, f.jsonErr
}

// fakeChatStarter captures the chat turns a delegate-to-agent trigger starts.
type fakeChatStarter struct {
	specs []ChatSpec
	err   error
}

func (f *fakeChatStarter) StartChat(_ context.Context, spec ChatSpec) error {
	f.specs = append(f.specs, spec)
	return f.err
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
	if err := engine.Execute(context.Background(), approvalInteraction(), nil); err != nil {
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
	if err := engine.Execute(context.Background(), approvalInteraction(), nil); err != nil {
		t.Fatalf("Execute should not error on non-JSON output, got: %v", err)
	}
	if len(poster.bodies) != 0 {
		t.Fatalf("expected nothing posted on non-JSON output, got: %#v", poster.bodies)
	}
}

func TestEngineTopLevelDelegateStartsChat(t *testing.T) {
	poster := &recordingPoster{}
	chat := &fakeChatStarter{}
	cfg := workflowConfig()
	rule := cfg.WorkflowRules["code-review-approval"]
	rule.Triggers = []config.TriggerConfig{{
		Type:            "delegate-to-agent",
		DelegateToAgent: &config.DelegateToAgentConfig{Agent: "default", Prompt: "Review {{ index .Payload.channel \"name\" }}"},
	}}
	cfg.WorkflowRules["code-review-approval"] = rule

	engine := NewEngine(cfg, Options{Poster: poster})
	engine.SetChatStarter(chat)
	if err := engine.Execute(context.Background(), approvalInteraction(), nil); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(chat.specs) != 1 {
		t.Fatalf("expected one chat turn, got %#v", chat.specs)
	}
	got := chat.specs[0]
	if got.ChannelID != "C0REVIEW" || got.ThreadTS != "1782886620.260769" {
		t.Fatalf("chat turn did not target the button's thread: %#v", got)
	}
	if got.Agent != "default" {
		t.Fatalf("expected explicit agent override, got %q", got.Agent)
	}
	if got.Source != "workflow:code-review-approval" {
		t.Fatalf("unexpected source: %q", got.Source)
	}
	if !strings.Contains(got.Text, "nc-code-reviews") {
		t.Fatalf("prompt was not rendered with payload: %q", got.Text)
	}
	if len(poster.bodies) != 0 {
		t.Fatalf("delegate-to-agent must not post directly, got: %#v", poster.bodies)
	}
}

func TestEngineTopLevelDelegateRequiresChatStarter(t *testing.T) {
	cfg := workflowConfig()
	rule := cfg.WorkflowRules["code-review-approval"]
	rule.Triggers = []config.TriggerConfig{{
		Type:            "delegate-to-agent",
		DelegateToAgent: &config.DelegateToAgentConfig{Agent: "default", Prompt: "Act on it"},
	}}
	cfg.WorkflowRules["code-review-approval"] = rule

	engine := NewEngine(cfg, Options{}) // no chat starter wired (chat disabled)
	err := engine.Execute(context.Background(), approvalInteraction(), nil)
	if err == nil || !strings.Contains(err.Error(), "chat to be enabled") {
		t.Fatalf("expected chat-required error, got %v", err)
	}
}

func TestEngineSkipsWhenNoRuleMatches(t *testing.T) {
	poster := &recordingPoster{}
	runner := &recordingRunner{}
	engine := NewEngine(workflowConfig(), Options{Poster: poster, Runner: runner})
	interaction := approvalInteraction()
	interaction.Channel.Name = "other-channel"

	if err := engine.Execute(context.Background(), interaction, nil); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(poster.bodies) != 0 || len(runner.commands) != 0 {
		t.Fatalf("expected no actions, got posts=%d commands=%d", len(poster.bodies), len(runner.commands))
	}
}

// TestEngineHasNoDefaultRules pins the contract that the engine ships with no
// built-in rules: the ping → pong self-test is now owned by the gateway (in Go),
// so a ping click reaching the engine must produce nothing. A regression that
// reinstated a default rule here would resurrect the template-driven path this
// change removed.
func TestEngineHasNoDefaultRules(t *testing.T) {
	poster := &recordingPoster{}
	engine := NewEngine(config.Config{}, Options{Poster: poster})

	if err := engine.Execute(context.Background(), pingInteraction(), nil); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(poster.bodies) != 0 {
		t.Fatalf("expected no default rule to fire, got posts=%#v", poster.bodies)
	}
}

func TestEngineUsesEmbeddedFallbackTemplateForConfiguredRule(t *testing.T) {
	poster := &recordingPoster{}
	// A user-configured rule may still resolve its template from the embedded FS
	// when it is absent on disk. Use an injected TemplateFS so the test exercises
	// that fallback without depending on any particular shipped asset.
	fsys := fstest.MapFS{
		"templates/custom/reply.json": &fstest.MapFile{Data: []byte(
			`{"response_type":"in_channel","replace_original":false,` +
				`"thread_ts":"{{ .Payload.message.ts }}",` +
				`"blocks":[{"type":"section","text":{"type":"mrkdwn","text":":recycle: fallback works."}}]}`,
		)},
	}
	engine := NewEngine(config.Config{WorkflowRules: map[string]config.WorkflowRuleConfig{
		"reply-rule": {
			RequestEvent: "interactive",
			Match: map[string]any{
				"type":    "block_actions",
				"actions": []any{map[string]any{"action_id": "ping"}},
			},
			Triggers: []config.TriggerConfig{{
				Type:         "reply-to-slack",
				ReplyToSlack: &config.ReplyToSlackTriggerConfig{Template: "templates/custom/reply.json"},
			}},
		},
	}}, Options{Poster: poster, TemplateFS: fsys})

	if err := engine.Execute(context.Background(), pingInteraction(), nil); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(poster.bodies) != 1 || !strings.Contains(string(poster.bodies[0]), ":recycle: fallback works.") {
		t.Fatalf("unexpected fallback template response: %#v", poster.bodies)
	}
	var response map[string]any
	if err := json.Unmarshal(poster.bodies[0], &response); err != nil {
		t.Fatalf("fallback body is not JSON: %v", err)
	}
	if response["thread_ts"] != "1717450123.000100" {
		t.Fatalf("expected rendered thread_ts from payload, got %v", response["thread_ts"])
	}
}

func TestEngineLogsInfoWhenNoRuleMatches(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	engine := NewEngine(workflowConfig(), Options{Logger: logger})
	interaction := pingInteraction()

	if err := engine.Execute(context.Background(), interaction, nil); err != nil {
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
		Team:        slack.Team{ID: "T0TEAM"},
		User:        slack.User{ID: "U0ADMIN"},
		Channel: slack.Channel{GroupConversation: slack.GroupConversation{
			Conversation: slack.Conversation{ID: "C0REVIEW"},
			Name:         "nc-code-reviews",
		}},
		// The container message carrying the button is the thread a delegated
		// chat turn replies in.
		Message: slack.Message{Msg: slack.Msg{Timestamp: "1782886620.260769"}},
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
