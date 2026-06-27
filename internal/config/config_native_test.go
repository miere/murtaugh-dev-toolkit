package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile is a tiny helper for the dotenv/expansion tests.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadExpandsSlackTokensFromDotEnv(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "slack.yaml"), `
oauth:
  app_token: ${TEST_SLACK_APP_TOKEN}
  bot_token: ${TEST_SLACK_BOT_TOKEN}
`)
	writeFile(t, filepath.Join(dir, ".env"), "TEST_SLACK_APP_TOKEN=xapp-from-dotenv\nTEST_SLACK_BOT_TOKEN=xoxb-from-dotenv\n")

	// Ensure the vars are not pre-set in the environment so we prove the .env
	// is what supplied them.
	os.Unsetenv("TEST_SLACK_APP_TOKEN")
	os.Unsetenv("TEST_SLACK_BOT_TOKEN")

	cfg, err := Load(filepath.Join(dir, "slack.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OAuth.AppToken != "xapp-from-dotenv" {
		t.Errorf("app_token = %q, want expansion from .env", cfg.OAuth.AppToken)
	}
	if cfg.OAuth.BotToken != "xoxb-from-dotenv" {
		t.Errorf("bot_token = %q, want expansion from .env", cfg.OAuth.BotToken)
	}
}

func TestRealEnvWinsOverDotEnv(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "slack.yaml"), `
oauth:
  app_token: ${TEST_PRECEDENCE_APP}
  bot_token: xoxb-literal
`)
	writeFile(t, filepath.Join(dir, ".env"), "TEST_PRECEDENCE_APP=from-file\n")
	t.Setenv("TEST_PRECEDENCE_APP", "from-environment")

	cfg, err := Load(filepath.Join(dir, "slack.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OAuth.AppToken != "from-environment" {
		t.Errorf("app_token = %q, want the ambient environment to win over .env", cfg.OAuth.AppToken)
	}
	if cfg.OAuth.BotToken != "xoxb-literal" {
		t.Errorf("bot_token = %q, want literal value preserved", cfg.OAuth.BotToken)
	}
}

func baseValidConfig() Config {
	return Config{
		OAuth: OAuthConfig{AppToken: "xapp-x", BotToken: "xoxb-x"},
	}
}

func TestResolvedKind(t *testing.T) {
	cases := []struct {
		name string
		p    AgentProfile
		want AgentKind
	}{
		{"acp block ⇒ acp", AgentProfile{ACP: &ACPProfile{Command: "goose"}}, AgentKindACP},
		{"native block ⇒ native", AgentProfile{Native: &NativeProfile{Provider: "gemini"}}, AgentKindNative},
		{"neither ⇒ native", AgentProfile{}, AgentKindNative},
	}
	for _, tc := range cases {
		if got := tc.p.ResolvedKind(); got != tc.want {
			t.Errorf("%s: ResolvedKind = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestACPAgentPermissionValidation(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr string
	}{
		{"default empty ⇒ ask", "", ""},
		{"ask", "ask", ""},
		{"auto-allow", "auto-allow", ""},
		{"auto-deny", "auto-deny", ""},
		{"bad value", "yolo", "approval.requests must be"},
	}
	for _, tc := range cases {
		profile := AgentProfile{ACP: &ACPProfile{Command: "claude-code-acp-rs"}, Approval: ApprovalConfig{Requests: tc.value}}
		err := profile.Validate()
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: got %v, want substring %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestNativeAgentValidation(t *testing.T) {
	cases := []struct {
		name    string
		profile AgentProfile
		servers map[string]MCPServerConfig
		wantErr string // substring; "" means valid
	}{
		{
			name:    "valid",
			profile: AgentProfile{Native: &NativeProfile{Provider: "gemini", Model: "gemini-2.5-pro", APIKeyEnv: "GEMINI_API_KEY"}},
		},
		{
			name:    "missing provider",
			profile: AgentProfile{Native: &NativeProfile{Model: "m", APIKeyEnv: "K"}},
			wantErr: "provider is required",
		},
		{
			name:    "bad provider",
			profile: AgentProfile{Native: &NativeProfile{Provider: "cohere", Model: "m", APIKeyEnv: "K"}},
			wantErr: "must be one of gemini, anthropic, openai",
		},
		{
			name:    "missing model",
			profile: AgentProfile{Native: &NativeProfile{Provider: "openai", APIKeyEnv: "K"}},
			wantErr: "model is required",
		},
		{
			name:    "missing api_key_env",
			profile: AgentProfile{Native: &NativeProfile{Provider: "openai", Model: "m"}},
			wantErr: "api_key_env is required",
		},
		{
			name:    "both prompts",
			profile: AgentProfile{Native: &NativeProfile{Provider: "openai", Model: "m", APIKeyEnv: "K", SystemPrompt: "a", SystemPromptFile: "b"}},
			wantErr: "exactly one",
		},
		{
			name:    "unknown mcp ref",
			profile: AgentProfile{MCPServers: []string{"ghost"}, Native: &NativeProfile{Provider: "openai", Model: "m", APIKeyEnv: "K"}},
			wantErr: "unknown server",
		},
		{
			name:    "known mcp ref",
			profile: AgentProfile{MCPServers: []string{"vaultre"}, Native: &NativeProfile{Provider: "openai", Model: "m", APIKeyEnv: "K"}},
			servers: map[string]MCPServerConfig{"vaultre": {Command: "vaultre-mcp"}},
		},
		{
			name:    "negative context_limit",
			profile: AgentProfile{Native: &NativeProfile{Provider: "gemini", Model: "m", APIKeyEnv: "K", ContextLimit: -1}},
			wantErr: "context_limit must be greater than or equal to zero",
		},
		{
			name:    "bad compaction",
			profile: AgentProfile{Native: &NativeProfile{Provider: "gemini", Model: "m", APIKeyEnv: "K", Compaction: "shrink"}},
			wantErr: "compaction must be",
		},
		{
			name:    "valid summarize compaction",
			profile: AgentProfile{Native: &NativeProfile{Provider: "gemini", Model: "m", APIKeyEnv: "K", Compaction: "summarize", ContextLimit: 200000}},
		},
		{
			name:    "bad cache_retention",
			profile: AgentProfile{Native: &NativeProfile{Provider: "gemini", Model: "m", APIKeyEnv: "K", CacheRetention: "2h"}},
			wantErr: "cache_retention must be",
		},
		{
			name:    "valid cache_retention off",
			profile: AgentProfile{Native: &NativeProfile{Provider: "gemini", Model: "m", APIKeyEnv: "K", CacheRetention: "off"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseValidConfig()
			cfg.Agents = map[string]AgentProfile{"a": tc.profile}
			cfg.MCPServers = tc.servers
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestMCPServerValidation(t *testing.T) {
	cases := []struct {
		name    string
		server  MCPServerConfig
		wantErr string
	}{
		{"stdio", MCPServerConfig{Command: "x"}, ""},
		{"remote", MCPServerConfig{URL: "https://x"}, ""},
		{"neither", MCPServerConfig{}, "exactly one of command or url"},
		{"both", MCPServerConfig{Command: "x", URL: "https://x"}, "exactly one of command or url"},
		{"bad env key", MCPServerConfig{Command: "x", Env: map[string]string{"A=B": "v"}}, "must not contain '='"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseValidConfig()
			cfg.MCPServers = map[string]MCPServerConfig{"s": tc.server}
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
