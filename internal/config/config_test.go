package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/assets"
)

const baseSlackYAML = `oauth:
  app_token: xapp-test
  bot_token: xoxb-test
`

func testConfig(extra string) []byte {
	return []byte(baseSlackYAML + extra)
}

func TestParseValidConfig(t *testing.T) {
	cfg, err := Parse(testConfig(`configuration:
  admin_user: '@admin'
chat:
  default_agent: default
  channel_agents:
    C12345: coding
  dm_agent: default
commands:
  - name: /murtaugh
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if cfg.OAuth.AppToken != "xapp-test" || cfg.OAuth.BotToken != "xoxb-test" || cfg.Configuration.AdminUser != "@admin" {
		t.Fatalf("unexpected Slack config parsed")
	}
	if cfg.Chat.DefaultAgent != "default" || cfg.Chat.DMAgent != "default" || cfg.Chat.ChannelAgents["C12345"] != "coding" {
		t.Fatalf("unexpected chat routing parsed: %#v", cfg.Chat)
	}
	if len(cfg.Commands) != 1 || cfg.Commands[0].Name != "/murtaugh" {
		t.Fatalf("unexpected commands parsed: %#v", cfg.Commands)
	}
}

func TestLoadACPConfigFromAgentsFile(t *testing.T) {
	baseDir := t.TempDir()
	configPath := filepath.Join(baseDir, "slack.yaml")
	if err := os.WriteFile(configPath, testConfig(`chat:
  default_agent: default
`), 0o644); err != nil {
		t.Fatalf("write slack config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "agents.yaml"), []byte(`acp:
  enabled: true
  request_timeout: 2m
  stream_append_interval: 100ms
  stream_min_chunk_chars: 12
agents:
  default:
    command: ls
`), 0o644); err != nil {
		t.Fatalf("write agents config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cfg.ACP.Enabled || cfg.ACP.EffectiveStreamMinChunkChars() != 12 {
		t.Fatalf("unexpected ACP config: %#v", cfg.ACP)
	}
	if _, ok := cfg.Agents["default"]; !ok {
		t.Fatalf("expected default agent to be loaded: %#v", cfg.Agents)
	}
}

func TestParseACPRequiresAgentsWhenEnabled(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.ACP.Enabled = true
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "no agents are defined") {
		t.Fatalf("expected ACP agents validation error, got: %v", err)
	}
}

func TestParseACPValidatesDurations(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.ACP.RequestTimeout = "nope"
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "acp.request_timeout") {
		t.Fatalf("expected ACP duration validation error, got: %v", err)
	}
}

func TestParseRequiresSlackTokens(t *testing.T) {
	cfg, err := Parse([]byte("oauth: {}\n"))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	message := err.Error()
	if !strings.Contains(message, "oauth.app_token") || !strings.Contains(message, "oauth.bot_token") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestParseValidatesSlashCommandNames(t *testing.T) {
	cfg, err := Parse(testConfig("commands:\n  - name: murtaugh\n"))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "must start with /") {
		t.Fatalf("expected slash command validation error, got: %v", err)
	}
}

func TestParseWorkflowRules(t *testing.T) {
	cfg, err := Parse(testConfig(`workflow-rules:
  code-review-approval:
    request_event: interactive
    match:
      channel: { name: nc-code-reviews }
      actions:
        - block_id: github_pull_request
          action_id: approve_only
    trigger:
      - reply-to-slack:
          template: code-review/02-approved.json
      - run:
          cmd: /path/to/cmd
          args: [param1, param2]
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	rule := cfg.WorkflowRules["code-review-approval"]
	if rule.RequestEvent != "interactive" || len(rule.Triggers) != 2 {
		t.Fatalf("unexpected workflow rule parsed: %#v", rule)
	}
	if rule.Triggers[0].ReplyToSlack.Template != "code-review/02-approved.json" {
		t.Fatalf("unexpected reply-to-slack trigger: %#v", rule.Triggers[0])
	}
	if rule.Triggers[1].Run.Cmd != "/path/to/cmd" || len(rule.Triggers[1].Run.Args) != 2 {
		t.Fatalf("unexpected run trigger: %#v", rule.Triggers[1])
	}
}

func TestParseWorkflowRuleValidatesReplyToSlackRenderer(t *testing.T) {
	cfg, err := Parse(testConfig(`workflow-rules:
  invalid:
    request_event: interactive
    match:
      type: block_actions
    trigger:
      - reply-to-slack:
          template: response.json
          run:
            cmd: /path/to/cmd
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "exactly one of template, run, or delegate-to-agent") {
		t.Fatalf("expected reply-to-slack validation error, got: %v", err)
	}
}

func TestParseWorkflowRuleValidatesRequestEvent(t *testing.T) {
	cfg, err := Parse(testConfig(`workflow-rules:
  invalid:
    request_event: slash_command
    match:
      type: block_actions
    trigger:
      - run:
          cmd: /path/to/cmd
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "request_event must be interactive") {
		t.Fatalf("expected request event validation error, got: %v", err)
	}
}

func TestParseValidUnfurlRule(t *testing.T) {
	cfg, err := Parse(testConfig(`unfurl-rules:
  github-pr:
    match:
      channels: [C0ENG]
      domain: github.com
      url_pattern: '/pull/(?P<number>\d+)'
    unfurl:
      template: templates/unfurl/github-pr.json
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	rule, ok := cfg.UnfurlRules["github-pr"]
	if !ok {
		t.Fatal("expected github-pr unfurl rule")
	}
	if rule.Match.Domain != "github.com" || rule.Unfurl.Template != "templates/unfurl/github-pr.json" {
		t.Fatalf("unexpected unfurl rule parsed: %#v", rule)
	}
	if len(rule.Match.Channels) != 1 || rule.Match.Channels[0] != "C0ENG" {
		t.Fatalf("unexpected channels parsed: %#v", rule.Match.Channels)
	}
}

func TestParseUnfurlRequiresMatchCondition(t *testing.T) {
	cfg, err := Parse(testConfig(`unfurl-rules:
  bad:
    match:
      channels: [C1]
    unfurl:
      template: t.json
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "at least one of domain") {
		t.Fatalf("expected match condition error, got: %v", err)
	}
}

func TestParseUnfurlRejectsTemplateAndRun(t *testing.T) {
	cfg, err := Parse(testConfig(`unfurl-rules:
  bad:
    match:
      domain: github.com
    unfurl:
      template: t.json
      run:
        cmd: echo
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "exactly one of template, run, or delegate-to-agent") {
		t.Fatalf("expected exclusivity error, got: %v", err)
	}
}

func TestParseJobsConfig(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	// Inject jobs manually (as Load() would, from jobs.yaml)
	cfg.Jobs = map[string]JobProfile{
		"cleanup-logs": {
			Command: "/usr/bin/find",
			Args:    []string{"/var/log", "-mtime", "+7", "-delete"},
			WorkDir: "/tmp",
			Timeout: "5m",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	job, ok := cfg.Jobs["cleanup-logs"]
	if !ok {
		t.Fatal("expected cleanup-logs job")
	}
	if job.Command != "/usr/bin/find" || len(job.Args) != 4 || job.Timeout != "5m" {
		t.Fatalf("unexpected job parsed: %#v", job)
	}
}

func TestJobValidationRequiresCommand(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Jobs = map[string]JobProfile{
		"bad-job": {Command: ""},
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "jobs[bad-job] requires either command or agent + prompt") {
		t.Fatalf("expected job command validation error, got: %v", err)
	}
}

func TestJobValidationRejectsBadTimeout(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Jobs = map[string]JobProfile{
		"bad-timeout": {Command: "/bin/echo", Timeout: "nope"},
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "jobs[bad-timeout].timeout") {
		t.Fatalf("expected job timeout validation error, got: %v", err)
	}
}

func TestJobValidationAcceptsOptionalFields(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Jobs = map[string]JobProfile{
		"minimal": {Command: "/bin/echo"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestJobScheduleKind(t *testing.T) {
	cases := []struct {
		name string
		job  JobProfile
		want ScheduleKind
	}{
		{"manual", JobProfile{Command: "/bin/echo"}, ScheduleManual},
		{"cron", JobProfile{Command: "/bin/echo", Schedule: "0 2 * * *"}, ScheduleCron},
		{"every", JobProfile{Command: "/bin/echo", Every: "1h"}, ScheduleEvery},
		{"blank-schedule-is-manual", JobProfile{Command: "/bin/echo", Schedule: "  "}, ScheduleManual},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.job.ScheduleKind(); got != tc.want {
				t.Fatalf("ScheduleKind() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestJobValidationAcceptsScheduleAndEverySeparately(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Jobs = map[string]JobProfile{
		"cron":  {Command: "/bin/echo", Schedule: "0 2 * * *"},
		"every": {Command: "/bin/echo", Every: "30m"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestJobValidationRejectsScheduleAndEveryTogether(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Jobs = map[string]JobProfile{
		"both": {Command: "/bin/echo", Schedule: "0 2 * * *", Every: "1h"},
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "jobs[both] sets both schedule and every") {
		t.Fatalf("expected mutual-exclusion error, got: %v", err)
	}
}

func TestJobValidationRejectsBadEvery(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Jobs = map[string]JobProfile{
		"bad":  {Command: "/bin/echo", Every: "nope"},
		"zero": {Command: "/bin/echo", Every: "0s"},
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected every validation error, got nil")
	}
	if !strings.Contains(err.Error(), "jobs[bad].every must be a valid duration") {
		t.Fatalf("expected invalid-duration error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "jobs[zero].every must be greater than zero") {
		t.Fatalf("expected non-positive-duration error, got: %v", err)
	}
}

func TestJobValidationAcceptsAgentPrompt(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Agents = map[string]AgentProfile{"default": {Command: "/bin/agent"}}
	cfg.Jobs = map[string]JobProfile{
		"review": {Agent: "default", Prompt: "Review PR {{ 1 }}"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestJobValidationRejectsCommandAndAgentTogether(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Agents = map[string]AgentProfile{"default": {Command: "/bin/agent"}}
	cfg.Jobs = map[string]JobProfile{
		"both": {Command: "/bin/echo", Agent: "default", Prompt: "hi"},
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "jobs[both] sets both command and agent/prompt") {
		t.Fatalf("expected command/agent exclusivity error, got: %v", err)
	}
}

func TestJobValidationRejectsAgentWithoutPrompt(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Agents = map[string]AgentProfile{"default": {Command: "/bin/agent"}}
	cfg.Jobs = map[string]JobProfile{
		"no-prompt": {Agent: "default"},
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "jobs[no-prompt].prompt is required when agent is set") {
		t.Fatalf("expected missing-prompt error, got: %v", err)
	}
}

func TestJobValidationRejectsUnknownAgent(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Jobs = map[string]JobProfile{
		"ghost": {Agent: "missing", Prompt: "hi"},
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `jobs[ghost].agent references unknown agent "missing"`) {
		t.Fatalf("expected unknown-agent error, got: %v", err)
	}
}

func TestParseWorkflowDelegateToAgentTrigger(t *testing.T) {
	cfg, err := Parse(testConfig(`workflow-rules:
  review:
    request_event: interactive
    match:
      type: block_actions
    trigger:
      - delegate-to-agent:
          agent: default
          prompt: "Do the thing"
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Agents = map[string]AgentProfile{"default": {Command: "/bin/agent"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	trig := cfg.WorkflowRules["review"].Triggers[0]
	if trig.Type != "delegate-to-agent" || trig.DelegateToAgent == nil || trig.DelegateToAgent.Agent != "default" {
		t.Fatalf("unexpected delegate-to-agent trigger parsed: %#v", trig)
	}
}

func TestParseReplyToSlackDelegateToAgent(t *testing.T) {
	cfg, err := Parse(testConfig(`workflow-rules:
  review:
    request_event: interactive
    match:
      type: block_actions
    trigger:
      - reply-to-slack:
          delegate-to-agent:
            agent: default
            prompt: "Return a JSON message"
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Agents = map[string]AgentProfile{"default": {Command: "/bin/agent"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	rts := cfg.WorkflowRules["review"].Triggers[0].ReplyToSlack
	if rts == nil || rts.DelegateToAgent == nil || rts.DelegateToAgent.Agent != "default" {
		t.Fatalf("unexpected reply-to-slack delegate parsed: %#v", rts)
	}
}

func TestParseReplyToSlackRejectsTemplateAndDelegate(t *testing.T) {
	cfg, err := Parse(testConfig(`workflow-rules:
  bad:
    request_event: interactive
    match:
      type: block_actions
    trigger:
      - reply-to-slack:
          template: t.json
          delegate-to-agent:
            agent: default
            prompt: "x"
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Agents = map[string]AgentProfile{"default": {Command: "/bin/agent"}}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "exactly one of template, run, or delegate-to-agent") {
		t.Fatalf("expected reply-to-slack exclusivity error, got: %v", err)
	}
}

func TestParseUnfurlDelegateToAgent(t *testing.T) {
	cfg, err := Parse(testConfig(`unfurl-rules:
  github-issues:
    match:
      domain: github.com
    unfurl:
      delegate-to-agent:
        agent: default
        prompt: "Summarise {{ .URL }}"
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Agents = map[string]AgentProfile{"default": {Command: "/bin/agent"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	action := cfg.UnfurlRules["github-issues"].Unfurl
	if action.DelegateToAgent == nil || action.DelegateToAgent.Agent != "default" {
		t.Fatalf("unexpected unfurl delegate parsed: %#v", action)
	}
}

func TestParseDelegateRejectsUnknownAgent(t *testing.T) {
	cfg, err := Parse(testConfig(`unfurl-rules:
  github-issues:
    match:
      domain: github.com
    unfurl:
      delegate-to-agent:
        agent: ghost
        prompt: "x"
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `delegate-to-agent references unknown agent "ghost"`) {
		t.Fatalf("expected unknown-agent error, got: %v", err)
	}
}

// TestEmbeddedExampleConfigValidates guards the shipped default config: the
// bundled slack.yaml / agents.yaml / jobs.yaml must parse and validate together
// as a unit. It catches indentation slips and dangling delegate-to-agent agent
// references before they reach a fresh install.
func TestEmbeddedExampleConfigValidates(t *testing.T) {
	// Credentials live in .env / the environment now; the shipped slack.yaml
	// references them as ${SLACK_APP_TOKEN}/${SLACK_BOT_TOKEN}. Provide them as a
	// real .env would so the bundled config validates as a unit.
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	baseDir := t.TempDir()
	for _, name := range []string{"slack.yaml", "agents.yaml", "jobs.yaml"} {
		data, err := assets.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(baseDir, name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if _, err := Load(filepath.Join(baseDir, "slack.yaml")); err != nil {
		t.Fatalf("bundled example config failed to load/validate: %v", err)
	}
}

func TestParseAllowedUsers(t *testing.T) {
	cfg, err := Parse(testConfig(`configuration:
  admin_user: U0ADMIN00
  allowed_users:
    - U0ALICE00
    - U0BOB0000
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if got, want := cfg.Configuration.AllowedUsers, []string{"U0ALICE00", "U0BOB0000"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("unexpected allowed_users parsed: %#v", got)
	}
}

func TestValidateAllowedUsersRejectsBlankEntries(t *testing.T) {
	cfg, err := Parse(testConfig(`configuration:
  allowed_users:
    - ""
    - "   "
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "configuration.allowed_users[0] must not be blank") {
		t.Fatalf("expected blank entry validation error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "configuration.allowed_users[1] must not be blank") {
		t.Fatalf("expected blank entry error for index 1, got: %v", err)
	}
}

func TestValidateAllowedUsersAcceptsHandlesAndIDs(t *testing.T) {
	// Validation must accept both Slack user IDs and handles; resolution from
	// handles to IDs happens at startup in the gateway layer.
	cfg, err := Parse(testConfig(`configuration:
  allowed_users:
    - "@alice"
    - "bob"
    - "U0CHARLIE"
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected handles and IDs to pass validation, got: %v", err)
	}
}

func TestIsAllowedUserMatchesAdminWhenConfiguredAsUserID(t *testing.T) {
	cfg, err := Parse(testConfig(`configuration:
  admin_user: U0ADMIN00
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if !cfg.Configuration.IsAllowedUser("U0ADMIN00") {
		t.Fatal("expected admin user ID to be allowed")
	}
	if cfg.Configuration.IsAllowedUser("U0OTHER00") {
		t.Fatal("expected non-admin user to be denied with empty allowed_users")
	}
}

func TestIsAllowedUserHandlesAdminPrefixedHandle(t *testing.T) {
	cfg, err := Parse(testConfig(`configuration:
  admin_user: "@U0ADMIN00"
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if !cfg.Configuration.IsAllowedUser("U0ADMIN00") {
		t.Fatal("expected admin user ID to be allowed when admin_user is prefixed with @")
	}
}

func TestIsAllowedUserSkipsAdminConfiguredAsHandle(t *testing.T) {
	cfg, err := Parse(testConfig(`configuration:
  admin_user: murtaugh-admin
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	// admin_user is a handle, not a user ID — helper cannot match by ID alone.
	if cfg.Configuration.IsAllowedUser("U0ADMIN00") {
		t.Fatal("expected helper to skip handle-based admin_user matching")
	}
}

func TestIsAllowedUserMatchesAllowedList(t *testing.T) {
	cfg, err := Parse(testConfig(`configuration:
  admin_user: U0ADMIN00
  allowed_users:
    - U0ALICE00
    - U0BOB0000
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	for _, id := range []string{"U0ADMIN00", "U0ALICE00", "U0BOB0000"} {
		if !cfg.Configuration.IsAllowedUser(id) {
			t.Errorf("expected %q to be allowed", id)
		}
	}
	if cfg.Configuration.IsAllowedUser("U0EVE0000") {
		t.Fatal("expected non-listed user to be denied")
	}
}

func TestIsAllowedUserRejectsBlankInput(t *testing.T) {
	cfg, err := Parse(testConfig(`configuration:
  admin_user: U0ADMIN00
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if cfg.Configuration.IsAllowedUser("") || cfg.Configuration.IsAllowedUser("   ") {
		t.Fatal("expected blank user ID to be denied")
	}
}

func TestIsAdminUserMatchesAdminID(t *testing.T) {
	cfg, err := Parse(testConfig(`configuration:
  admin_user: U0ADMIN00
  allowed_users:
    - U0ALICE00
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if !cfg.Configuration.IsAdminUser("U0ADMIN00") {
		t.Fatal("expected admin user ID to be recognized as admin")
	}
	if cfg.Configuration.IsAdminUser("U0ALICE00") {
		t.Fatal("expected allowed-but-non-admin user to be rejected by IsAdminUser")
	}
	if cfg.Configuration.IsAdminUser("") || cfg.Configuration.IsAdminUser("   ") {
		t.Fatal("expected blank input to be rejected")
	}
}

func TestIsAdminUserSkipsHandleConfiguredAdmin(t *testing.T) {
	cfg, err := Parse(testConfig(`configuration:
  admin_user: murtaugh-admin
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if cfg.Configuration.IsAdminUser("U0ADMIN00") {
		t.Fatal("expected helper to refuse handle-shaped admin_user matching")
	}
}

func TestParseUnfurlRejectsBadRegex(t *testing.T) {
	cfg, err := Parse(testConfig(`unfurl-rules:
  bad:
    match:
      domain: github.com
      url_pattern: '([a-z'
    unfurl:
      template: t.json
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "url_pattern") {
		t.Fatalf("expected url_pattern validation error, got: %v", err)
	}
}

func TestAgentEnvOverridesExpandsAndSorts(t *testing.T) {
	t.Setenv("MURTAUGH_TEST_HOME", "/home/murtaugh")
	profile := AgentProfile{Env: map[string]string{
		"ZZZ":  "last",
		"DATA": "${MURTAUGH_TEST_HOME}/data",
		"AAA":  "first",
		"":     "skip-blank-key",
	}}
	got := profile.EnvOverrides()
	want := []string{"AAA=first", "DATA=/home/murtaugh/data", "ZZZ=last"}
	if len(got) != len(want) {
		t.Fatalf("EnvOverrides returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("EnvOverrides[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestAgentEnvOverridesNilWhenEmpty(t *testing.T) {
	if got := (AgentProfile{}).EnvOverrides(); got != nil {
		t.Fatalf("expected nil for empty env, got %v", got)
	}
}

func TestValidateRejectsEnvKeyWithEquals(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Agents = map[string]AgentProfile{
		"default": {Command: "/bin/agent", Env: map[string]string{"BAD=KEY": "x"}},
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "must not contain '='") {
		t.Fatalf("expected env key validation error, got: %v", err)
	}
}
