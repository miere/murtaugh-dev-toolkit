package slack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
)

func validArgs() map[string]any {
	return map[string]any{
		"app_token":  "xapp-1-test",
		"bot_token":  "xoxb-test",
		"admin_user": "@admin",
	}
}

func TestTool_Metadata(t *testing.T) {
	tl := New(func() string { return "" })
	if tl.Name() != "setup.slack" {
		t.Fatalf("Name() = %q, want setup.slack", tl.Name())
	}
	schema := tl.InputSchema()
	if schema == nil {
		t.Fatal("InputSchema must not be nil")
	}
	required := map[string]bool{}
	for _, r := range schema.Required {
		required[r] = true
	}
	for _, want := range []string{"app_token", "bot_token", "admin_user"} {
		if !required[want] {
			t.Fatalf("required missing %q (have %v)", want, schema.Required)
		}
	}
}

func TestInvoke_RejectsBadInputs(t *testing.T) {
	tl := New(func() string { return filepath.Join(t.TempDir(), "slack.yaml") })
	cases := []map[string]any{
		{},
		{"app_token": "no-prefix", "bot_token": "xoxb-x", "admin_user": "@a"},
		{"app_token": "xapp-x", "bot_token": "no-prefix", "admin_user": "@a"},
		{"app_token": "xapp-x", "bot_token": "xoxb-x", "admin_user": ""},
	}
	for i, args := range cases {
		if _, err := tl.Invoke(context.Background(), args); err == nil {
			t.Fatalf("case %d: Invoke returned nil, want error for %+v", i, args)
		}
	}
}

func TestInvoke_FirstWriteCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "slack.yaml")
	tl := New(func() string { return path })

	res, err := tl.Invoke(context.Background(), validArgs())
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if !r.Created {
		t.Fatal("Result.Created = false, want true on first write")
	}
	if r.BackupPath != "" {
		t.Fatalf("BackupPath = %q, want empty on fresh write", r.BackupPath)
	}
	cfg := loadSlack(t, path)
	// slack.yaml must reference the tokens, not embed them.
	if cfg.OAuth.AppToken != "${SLACK_APP_TOKEN}" || cfg.OAuth.BotToken != "${SLACK_BOT_TOKEN}" {
		t.Fatalf("oauth = %+v, want ${VAR} references, not literal tokens", cfg.OAuth)
	}
	// The actual tokens must land in the .env sibling.
	if r.EnvPath == "" {
		t.Fatal("Result.EnvPath empty; tokens were not routed to .env")
	}
	envData, err := os.ReadFile(r.EnvPath)
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	if !strings.Contains(string(envData), "SLACK_APP_TOKEN=xapp-1-test") ||
		!strings.Contains(string(envData), "SLACK_BOT_TOKEN=xoxb-test") {
		t.Fatalf(".env missing tokens:\n%s", envData)
	}
	// The yaml itself must NOT contain the raw token values.
	rawYAML, _ := os.ReadFile(path)
	if strings.Contains(string(rawYAML), "xapp-1-test") || strings.Contains(string(rawYAML), "xoxb-test") {
		t.Fatalf("raw token leaked into slack.yaml:\n%s", rawYAML)
	}
	if cfg.Access.AdminUser != "@admin" {
		t.Fatalf("admin_user = %q, want @admin", cfg.Access.AdminUser)
	}
	if cfg.Access.Debug {
		t.Fatal("debug must default to false")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 0600", st.Mode().Perm())
	}
}

func TestInvoke_SecondWriteBacksUpExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "slack.yaml")
	if err := os.WriteFile(path, []byte("oauth:\n  app_token: 'old'\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tl := New(func() string { return path })

	res, err := tl.Invoke(context.Background(), validArgs())
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if r.Created {
		t.Fatal("Result.Created = true, want false when overwriting")
	}
	if r.BackupPath == "" {
		t.Fatal("BackupPath must be set when overwriting an existing file")
	}
	if _, err := os.Stat(r.BackupPath); err != nil {
		t.Fatalf("backup missing at %q: %v", r.BackupPath, err)
	}
}

func TestInvoke_DefaultAgentSwitchesChatBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "slack.yaml")
	tl := New(func() string { return path })

	args := validArgs()
	args["default_agent"] = "default"
	if _, err := tl.Invoke(context.Background(), args); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	cfg := loadSlack(t, path)
	if cfg.Chat.DefaultAgent != "default" {
		t.Fatalf("chat.default_agent = %q, want default", cfg.Chat.DefaultAgent)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "default_agent: default") {
		t.Fatalf("slack.yaml missing default_agent line:\n%s", raw)
	}
}

func loadSlack(t *testing.T, path string) config.Config {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return cfg
}
