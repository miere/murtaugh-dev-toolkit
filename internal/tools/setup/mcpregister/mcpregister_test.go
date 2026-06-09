package mcpregister

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestTool_Metadata(t *testing.T) {
	tl := New(stubResolver(t))
	if tl.Name() != "setup.mcp-register" {
		t.Fatalf("Name() = %q, want setup.mcp-register", tl.Name())
	}
	schema := tl.InputSchema()
	if schema == nil {
		t.Fatal("InputSchema must not be nil")
	}
	required := map[string]bool{}
	for _, r := range schema.Required {
		required[r] = true
	}
	for _, want := range []string{"client", "binary_path"} {
		if !required[want] {
			t.Fatalf("required missing %q", want)
		}
	}
}

func TestInvoke_OpencodeMergesIntoExistingJSON(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, ".config", "opencode", "opencode.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`{"theme":"dark","mcp":{"other":{"keep":true}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := New(homeResolver(home))

	res, err := tl.Invoke(context.Background(), map[string]any{
		"client":      "opencode",
		"binary_path": "/usr/local/bin/murtaugh",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if r.Path != target {
		t.Fatalf("Path = %q, want %q", r.Path, target)
	}
	if r.BackupPath == "" {
		t.Fatal("BackupPath must be set when overwriting")
	}

	var doc map[string]any
	raw, _ := os.ReadFile(target)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc["theme"] != "dark" {
		t.Fatalf("theme dropped: %+v", doc)
	}
	if doc["$schema"] != "https://opencode.ai/config.json" {
		t.Fatalf("$schema missing: %+v", doc)
	}
	mcp := doc["mcp"].(map[string]any)
	if _, ok := mcp["other"]; !ok {
		t.Fatalf("mcp.other dropped: %+v", mcp)
	}
	murtaugh := mcp["murtaugh"].(map[string]any)
	if murtaugh["type"] != "local" || murtaugh["enabled"] != true {
		t.Fatalf("murtaugh entry wrong: %+v", murtaugh)
	}
	cmd := murtaugh["command"].([]any)
	if len(cmd) != 2 || cmd[0] != "/usr/local/bin/murtaugh" || cmd[1] != "mcp" {
		t.Fatalf("command = %v, want [bin, mcp]", cmd)
	}
}

func TestInvoke_AuggieMergesIntoExistingJSON(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, ".augment", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`{"theme":"light"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := New(homeResolver(home))

	_, err := tl.Invoke(context.Background(), map[string]any{
		"client":      "auggie",
		"binary_path": "/usr/local/bin/murtaugh",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var doc map[string]any
	raw, _ := os.ReadFile(target)
	_ = json.Unmarshal(raw, &doc)
	servers := doc["mcpServers"].(map[string]any)
	murtaugh := servers["murtaugh"].(map[string]any)
	if murtaugh["command"] != "/usr/local/bin/murtaugh" {
		t.Fatalf("command = %v", murtaugh["command"])
	}
	args := murtaugh["args"].([]any)
	if len(args) != 1 || args[0] != "mcp" {
		t.Fatalf("args = %v", args)
	}
}

func TestInvoke_GoosePreservesExistingExtensionsAndWritesYAML(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, ".config", "goose", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := "GOOSE_PROVIDER: anthropic\nextensions:\n  developer:\n    enabled: true\n    type: builtin\n"
	if err := os.WriteFile(target, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := New(homeResolver(home))

	_, err := tl.Invoke(context.Background(), map[string]any{
		"client":      "goose",
		"binary_path": "/usr/local/bin/murtaugh",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	raw, _ := os.ReadFile(target)
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc["GOOSE_PROVIDER"] != "anthropic" {
		t.Fatalf("top-level setting dropped: %+v", doc)
	}
	exts := doc["extensions"].(map[string]any)
	if _, ok := exts["developer"]; !ok {
		t.Fatalf("developer extension dropped: %+v", exts)
	}
	murtaugh := exts["murtaugh"].(map[string]any)
	if murtaugh["type"] != "stdio" || murtaugh["enabled"] != true {
		t.Fatalf("murtaugh wrong: %+v", murtaugh)
	}
	if murtaugh["cmd"] != "/usr/local/bin/murtaugh" {
		t.Fatalf("cmd = %v", murtaugh["cmd"])
	}
}

func TestInvoke_GooseCreatesFreshConfigWhenMissing(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, ".config", "goose", "config.yaml")
	tl := New(homeResolver(home))

	res, err := tl.Invoke(context.Background(), map[string]any{
		"client":      "goose",
		"binary_path": "/usr/local/bin/murtaugh",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if r.BackupPath != "" {
		t.Fatalf("BackupPath should be empty on fresh write, got %q", r.BackupPath)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("config.yaml not created: %v", err)
	}
}

func TestInvoke_RejectsUnknownClient(t *testing.T) {
	tl := New(homeResolver(t.TempDir()))
	_, err := tl.Invoke(context.Background(), map[string]any{
		"client":      "claude",
		"binary_path": "/usr/local/bin/murtaugh",
	})
	if err == nil {
		t.Fatal("expected error for unknown client")
	}
}

func homeResolver(home string) HomeResolver {
	return func() (string, error) { return home, nil }
}

func stubResolver(t *testing.T) HomeResolver {
	t.Helper()
	return homeResolver(t.TempDir())
}
