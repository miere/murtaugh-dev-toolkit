package mcpregister

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/miere/murtaugh-dev-toolkit/internal/tools/setup/internal/backup"
)

// writeOpencode merges the murtaugh stdio entry into ~/.config/opencode/
// opencode.json, preserving every other key. The file is written with 0644
// to match opencode's own conventions (it is not a secrets file).
func writeOpencode(home, binary string) (Result, error) {
	target := filepath.Join(home, ".config", "opencode", "opencode.json")
	doc, err := readJSON(target)
	if err != nil {
		return Result{}, err
	}
	if _, ok := doc["$schema"]; !ok {
		doc["$schema"] = "https://opencode.ai/config.json"
	}
	mcp, _ := doc["mcp"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
		doc["mcp"] = mcp
	}
	mcp["murtaugh"] = map[string]any{
		"type":    "local",
		"command": []any{binary, "mcp"},
		"enabled": true,
	}
	return writeJSON(target, doc, "opencode")
}

// writeAuggie merges the murtaugh stdio entry into ~/.augment/settings.json.
func writeAuggie(home, binary string) (Result, error) {
	target := filepath.Join(home, ".augment", "settings.json")
	doc, err := readJSON(target)
	if err != nil {
		return Result{}, err
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		doc["mcpServers"] = servers
	}
	servers["murtaugh"] = map[string]any{
		"command": binary,
		"args":    []any{"mcp"},
	}
	return writeJSON(target, doc, "auggie")
}

// writeGoose merges the murtaugh stdio extension into ~/.config/goose/
// config.yaml, preserving every other top-level key and every other entry
// under extensions. The shape follows the upstream config-file guide
// (extensions.<name>: { name, type, cmd, args, enabled, timeout }).
func writeGoose(home, binary string) (Result, error) {
	target := filepath.Join(home, ".config", "goose", "config.yaml")
	doc, err := readYAML(target)
	if err != nil {
		return Result{}, err
	}
	exts, _ := doc["extensions"].(map[string]any)
	if exts == nil {
		exts = map[string]any{}
		doc["extensions"] = exts
	}
	exts["murtaugh"] = map[string]any{
		"name":    "murtaugh",
		"type":    "stdio",
		"enabled": true,
		"cmd":     binary,
		"args":    []any{"mcp"},
		"timeout": 300,
	}

	wasThere := existed(target)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return Result{}, fmt.Errorf("ensure goose config dir: %w", err)
	}
	backupPath, err := backup.IfExists(target)
	if err != nil {
		return Result{}, err
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return Result{}, fmt.Errorf("marshal goose config: %w", err)
	}
	if err := os.WriteFile(target, out, 0o644); err != nil {
		return Result{}, fmt.Errorf("write %q: %w", target, err)
	}
	return Result{Client: "goose", Path: target, BackupPath: backupPath, Created: !wasThere}, nil
}

// readJSON returns the parsed object at path. A missing file yields an empty
// map so callers can populate it without branching.
func readJSON(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

// readYAML is the YAML twin of readJSON.
func readYAML(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

// writeJSON serialises doc as 2-space-indented JSON, taking a backup first.
// client is recorded on the Result so the caller can tell opencode and auggie
// apart in the rendered confirmation.
func writeJSON(path string, doc map[string]any, client string) (Result, error) {
	wasThere := existed(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{}, fmt.Errorf("ensure %s dir: %w", client, err)
	}
	backupPath, err := backup.IfExists(path)
	if err != nil {
		return Result{}, err
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("marshal %s config: %w", client, err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return Result{}, fmt.Errorf("write %q: %w", path, err)
	}
	return Result{Client: client, Path: path, BackupPath: backupPath, Created: !wasThere}, nil
}
