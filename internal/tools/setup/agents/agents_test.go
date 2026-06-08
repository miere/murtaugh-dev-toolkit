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
		StreamFinalFeedback  bool   `yaml:"stream_final_feedback"`
	} `yaml:"acp"`
	Agents map[string]struct {
		Command string   `yaml:"command"`
		Args    []string `yaml:"args"`
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
	if agent.Command != "/usr/local/bin/auggie" {
		t.Fatalf("command = %q, want auggie path", agent.Command)
	}
	want := []string{"--acp", "--allow-indexing"}
	if len(agent.Args) != 2 || agent.Args[0] != want[0] || agent.Args[1] != want[1] {
		t.Fatalf("args = %v, want %v", agent.Args, want)
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

func TestInvoke_RejectsArgsWithoutCommand(t *testing.T) {
	tl := New(func() string { return filepath.Join(t.TempDir(), "agents.yaml") })
	_, err := tl.Invoke(context.Background(), map[string]any{
		"args": []any{"--foo"},
	})
	if err == nil {
		t.Fatal("Invoke should reject args without command")
	}
}
