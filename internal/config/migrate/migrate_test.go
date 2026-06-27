package migrate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
)

// legacyDir writes a representative pre-v1 config directory (the old shape) and
// returns its path.
func legacyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write(".env", "SLACK_APP_TOKEN=xapp-test\nSLACK_BOT_TOKEN=xoxb-test\n")
	write("slack.yaml", `oauth:
  app_token: ${SLACK_APP_TOKEN}
  bot_token: ${SLACK_BOT_TOKEN}
configuration:
  admin_user: U0ADMIN00
  allowed_users: [U0ALICE00]
  do_not_require_mention_from: [U0ALICE00]
  debug: false
chat:
  default_agent: default
  channel_do_not_require_mention:
    feature-*: [U0ALICE00]
commands:
  - name: /murtaugh
workflow-rules:
  review:
    request_event: interactive
    match: { type: block_actions }
    trigger:
      - run: { cmd: /bin/echo }
unfurl-rules:
  gh:
    match: { domain: github.com }
    unfurl: { template: t.json }
`)
	write("agents.yaml", `acp:
  enabled: true
  startup_timeout: 10s
  request_timeout: 10m
  session_idle_timeout: 30m
  max_sessions: 100
  stream_append_interval: 750ms
  stream_min_chunk_chars: 96
  stream_final_feedback: false
  cancel_grace_period: 2s
  progress_display: simplified
agents:
  default:
    kind: acp
    command: /usr/local/bin/auggie
    args: [--acp]
    acp_permission: ask
    workdir: /work
  emily:
    provider: gemini
    model: gemini-2.5-pro
    api_key_env: GEMINI_API_KEY
    tools: [files, terminal]
    max_turns: 40
    approval:
      terminal: allowlist
`)
	return dir
}

func TestRunV1MigratesLegacyConfig(t *testing.T) {
	dir := legacyDir(t)

	applied, err := Run(dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(applied) != 1 || applied[0] != 1 {
		t.Fatalf("applied = %v, want [1]", applied)
	}
	if Version(dir) != 1 {
		t.Fatalf("version stamp = %d, want 1", Version(dir))
	}

	// Legacy anchor gone; new files present.
	if fileExists(filepath.Join(dir, "slack.yaml")) {
		t.Fatal("slack.yaml should have been removed")
	}
	for _, name := range []string{"gateway.yaml", "agents.yaml", "workflow-rules.yaml", "unfurl-rules.yaml"} {
		if !fileExists(filepath.Join(dir, name)) {
			t.Fatalf("expected %s to exist after migration", name)
		}
	}

	// The migrated config loads and validates as the daemon would.
	cfg, err := config.Load(filepath.Join(dir, "gateway.yaml"))
	if err != nil {
		t.Fatalf("migrated config failed to load: %v", err)
	}

	// access (was configuration); no_mention fold; chat.enabled (was acp.enabled).
	if cfg.Access.AdminUser != "U0ADMIN00" {
		t.Errorf("access.admin_user = %q", cfg.Access.AdminUser)
	}
	if !cfg.Chat.Enabled {
		t.Error("chat.enabled should be true (carried from acp.enabled)")
	}
	if len(cfg.Chat.NoMention.Everywhere) != 1 || cfg.Chat.NoMention.Everywhere[0] != "U0ALICE00" {
		t.Errorf("no_mention.everywhere = %v", cfg.Chat.NoMention.Everywhere)
	}
	if len(cfg.Chat.NoMention.ByChannel["feature-*"]) != 1 {
		t.Errorf("no_mention.by_channel = %v", cfg.Chat.NoMention.ByChannel)
	}

	// defaults fan-out.
	if cfg.Defaults.Session.MaxConcurrent != 100 || cfg.Defaults.ACP.StartupTimeout != "10s" {
		t.Errorf("defaults wrong: %#v", cfg.Defaults)
	}

	// agent backends nested; acp_permission → approval.requests.
	def := cfg.Agents["default"]
	if def.ACP == nil || def.ACP.Command != "/usr/local/bin/auggie" {
		t.Errorf("default agent acp block wrong: %#v", def.ACP)
	}
	if def.Native != nil {
		t.Error("default (acp) agent must not have a native block")
	}
	if def.Approval.Requests != "ask" {
		t.Errorf("approval.requests = %q, want ask", def.Approval.Requests)
	}
	emily := cfg.Agents["emily"]
	if emily.Native == nil || emily.Native.Provider != "gemini" || emily.Native.Model != "gemini-2.5-pro" {
		t.Errorf("emily native block wrong: %#v", emily.Native)
	}
	if emily.ACP != nil {
		t.Error("emily (native) agent must not have an acp block")
	}

	// Rules landed in their own files.
	if _, ok := cfg.WorkflowRules["review"]; !ok {
		t.Error("workflow rule 'review' missing after migration")
	}
	if _, ok := cfg.UnfurlRules["gh"]; !ok {
		t.Error("unfurl rule 'gh' missing after migration")
	}

	// commands + stream_final_feedback dropped.
	gw := readYAML(filepath.Join(dir, "gateway.yaml"))
	if _, ok := gw["commands"]; ok {
		t.Error("commands block should be gone")
	}
	ag := readYAML(filepath.Join(dir, "agents.yaml"))
	if d, _ := asMap(ag["defaults"]); d != nil {
		if r, _ := asMap(d["rendering"]); r != nil {
			if _, ok := r["stream_final_feedback"]; ok {
				t.Error("stream_final_feedback should be dropped")
			}
		}
	}
}

func TestRunIsIdempotent(t *testing.T) {
	dir := legacyDir(t)
	if _, err := Run(dir); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	applied, err := Run(dir)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("second Run applied %v, want none", applied)
	}
}

func TestRunFreshDirStampsWithoutMigrating(t *testing.T) {
	// A dir with no legacy markers (e.g. a brand-new install) should just be
	// stamped current, not transformed.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gateway.yaml"),
		[]byte("oauth:\n  app_token: x\n  bot_token: y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	applied, err := Run(dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(applied) != 1 || Version(dir) != 1 {
		t.Fatalf("expected a no-op stamp to v1, applied=%v version=%d", applied, Version(dir))
	}
}

func TestRunRollsBackOnInvalidResult(t *testing.T) {
	// A structurally broken legacy config (agents as a scalar, not a map) survives
	// into a malformed agents.yaml that fails to parse into the config types;
	// Run must roll back and leave slack.yaml in place.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "slack.yaml"),
		[]byte("oauth:\n  app_token: xapp-x\n  bot_token: xoxb-x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents.yaml"),
		[]byte("acp:\n  enabled: true\nagents: not-a-map\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(dir)
	if err == nil {
		t.Fatal("expected migration to fail structural validation (agents is not a map)")
	}
	if !fileExists(filepath.Join(dir, "slack.yaml")) {
		t.Fatal("slack.yaml must be restored after a rolled-back migration")
	}
	if fileExists(filepath.Join(dir, "gateway.yaml")) {
		t.Fatal("gateway.yaml must be removed on rollback")
	}
	if Version(dir) != 0 {
		t.Fatalf("version must remain 0 after rollback, got %d", Version(dir))
	}
}
