package agents

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

type loaded struct {
	ACP struct {
		Enabled              bool   `yaml:"enabled"`
		StartupTimeout       string `yaml:"startup_timeout"`
		RequestTimeout       string `yaml:"request_timeout"`
		SessionIdleTimeout   string `yaml:"session_idle_timeout"`
		MaxSessions          int    `yaml:"max_sessions"`
		StreamAppendInterval string `yaml:"stream_append_interval"`
		StreamMinChunkChars  int    `yaml:"stream_min_chunk_chars"`
	} `yaml:"acp"`
	Agents map[string]struct {
		Tools      []string `yaml:"tools"`
		MCPServers []string `yaml:"mcp_servers"`
		Native     *struct {
			Provider       string `yaml:"provider"`
			Model          string `yaml:"model"`
			APIKeyEnv      string `yaml:"api_key_env"`
			ContextLimit   int    `yaml:"context_limit"`
			Compaction     string `yaml:"compaction"`
			CacheRetention string `yaml:"cache_retention"`
		} `yaml:"native"`
		ACP *struct {
			Command string   `yaml:"command"`
			Args    []string `yaml:"args"`
		} `yaml:"acp"`
	} `yaml:"agents"`
}

func load(t *testing.T, path string) loaded {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc loaded
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

func TestTool_Metadata(t *testing.T) {
	tl := New(func() string { return "" })
	if tl.Name() != "setup.agents" {
		t.Fatalf("Name() = %q, want setup.agents", tl.Name())
	}
	if tl.InputSchema() == nil {
		t.Fatal("InputSchema must not be nil")
	}
}

func TestInvoke_NoCommandDisablesACP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.yaml")
	tl := New(func() string { return path })

	res, err := tl.Invoke(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if !r.Created {
		t.Fatal("Created must be true on fresh write")
	}
	doc := load(t, path)
	if doc.ACP.Enabled {
		t.Fatal("acp.enabled must be false when no command is supplied")
	}
	if len(doc.Agents) != 0 {
		t.Fatalf("agents must be empty when no command is supplied, got %+v", doc.Agents)
	}
	if doc.ACP.StartupTimeout != "10s" || doc.ACP.MaxSessions != 100 {
		t.Fatalf("acp defaults missing: %+v", doc.ACP)
	}
}

func TestInvoke_WithCommandRegistersAgentAndEnablesACP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.yaml")
	tl := New(func() string { return path })

	_, err := tl.Invoke(context.Background(), map[string]any{
		"command": "/usr/local/bin/auggie",
		"args":    []any{"--acp", "--allow-indexing"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	doc := load(t, path)
	if !doc.ACP.Enabled {
		t.Fatal("acp.enabled must be true when a command is provided")
	}
	agent, ok := doc.Agents["default"]
	if !ok {
		t.Fatalf("default agent missing in %+v", doc.Agents)
	}
	if agent.ACP == nil || agent.ACP.Command != "/usr/local/bin/auggie" {
		t.Fatalf("command wrong, want auggie path: %+v", agent.ACP)
	}
	want := []string{"--acp", "--allow-indexing"}
	if len(agent.ACP.Args) != 2 || agent.ACP.Args[0] != want[0] || agent.ACP.Args[1] != want[1] {
		t.Fatalf("args = %v, want %v", agent.ACP.Args, want)
	}
}

func TestInvoke_CustomAgentNameIsHonoured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.yaml")
	tl := New(func() string { return path })

	_, err := tl.Invoke(context.Background(), map[string]any{
		"agent_name": "ccode",
		"command":    "/usr/local/bin/claude",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	doc := load(t, path)
	if _, ok := doc.Agents["ccode"]; !ok {
		t.Fatalf("agents[ccode] missing in %+v", doc.Agents)
	}
}

func TestInvoke_BacksUpExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.yaml")
	if err := os.WriteFile(path, []byte("acp:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tl := New(func() string { return path })
	res, err := tl.Invoke(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if r.Created {
		t.Fatal("Created must be false when file existed")
	}
	if r.BackupPath == "" {
		t.Fatal("BackupPath must be populated when overwriting")
	}
	if _, err := os.Stat(r.BackupPath); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
}

func TestInvoke_NativeAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.yaml")
	tl := New(func() string { return path })

	res, err := tl.Invoke(context.Background(), map[string]any{
		"agent_name":      "emily",
		"provider":        "gemini",
		"model":           "gemini-2.5-pro",
		"api_key_env":     "GEMINI_API_KEY",
		"tools":           []any{"files", "terminal", "skills"},
		"mcp_servers":     []any{"vaultre"},
		"context_limit":   "200000",
		"compaction":      "summarize",
		"cache_retention": "1h",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if !r.Enabled || r.Kind != "native" || r.AgentName != "emily" {
		t.Fatalf("unexpected result: %+v", r)
	}
	doc := load(t, path)
	if !doc.ACP.Enabled {
		t.Fatal("runtime block must be enabled for a native agent")
	}
	a, ok := doc.Agents["emily"]
	if !ok {
		t.Fatalf("native agent missing: %+v", doc.Agents)
	}
	if a.Native == nil || a.Native.Provider != "gemini" || a.Native.Model != "gemini-2.5-pro" || a.Native.APIKeyEnv != "GEMINI_API_KEY" {
		t.Fatalf("native fields wrong: %+v", a.Native)
	}
	if a.ACP != nil {
		t.Errorf("native profile must not carry an acp block, got %+v", a.ACP)
	}
	if a.Native.ContextLimit != 200000 || a.Native.Compaction != "summarize" || a.Native.CacheRetention != "1h" {
		t.Errorf("context_limit/compaction/cache_retention wrong: %+v", a.Native)
	}
	if len(a.Tools) != 3 || len(a.MCPServers) != 1 {
		t.Errorf("tools/mcp_servers wrong: %+v", a)
	}
}

func TestInvoke_NativeValidation(t *testing.T) {
	tl := New(func() string { return filepath.Join(t.TempDir(), "agents.yaml") })
	cases := []map[string]any{
		{"provider": "gemini", "model": "m"},                                              // missing api_key_env
		{"provider": "gemini", "api_key_env": "K"},                                        // missing model
		{"kind": "native", "model": "m", "api_key_env": "K"},                              // missing provider
		{"provider": "cohere", "model": "m", "api_key_env": "K"},                          // bad provider
		{"provider": "gemini", "model": "m", "api_key_env": "K", "compaction": "shrink"},  // bad compaction
		{"provider": "gemini", "model": "m", "api_key_env": "K", "cache_retention": "2h"}, // bad cache_retention
	}
	for i, args := range cases {
		if _, err := tl.Invoke(context.Background(), args); err == nil {
			t.Errorf("case %d: expected error for %+v", i, args)
		}
	}
}

func TestInvoke_RejectsArgsWithoutCommand(t *testing.T) {
	tl := New(func() string { return filepath.Join(t.TempDir(), "agents.yaml") })
	_, err := tl.Invoke(context.Background(), map[string]any{
		"args": []any{"--foo"},
	})
	if err == nil {
		t.Fatal("Invoke should reject args without command")
	}
}
