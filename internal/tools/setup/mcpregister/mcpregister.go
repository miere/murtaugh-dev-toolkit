// Package mcpregister implements the `setup.mcp-register` tool: register
// Murtaugh as an MCP server in a downstream client's config file. Three
// clients are supported as first-class targets:
//
//   - opencode: ~/.config/opencode/opencode.json (JSON merge)
//   - auggie:   ~/.augment/settings.json         (JSON merge)
//   - goose:    ~/.config/goose/config.yaml      (YAML merge)
//
// Each writer reads the existing file (when present), merges Murtaugh into the
// client-specific extensions block while preserving every other key verbatim,
// and writes the file back through the shared backup helper.
package mcpregister

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// HomeResolver returns the user home directory. The composition root supplies
// os.UserHomeDir; tests inject a temp directory.
type HomeResolver func() (string, error)

// Tool is the `setup.mcp-register` capability.
type Tool struct {
	home HomeResolver
}

// New constructs a Tool that resolves client config paths under the home
// directory returned by home.
func New(home HomeResolver) *Tool {
	return &Tool{home: home}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "setup.mcp-register" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Register Murtaugh as an MCP server in opencode, auggie, or goose."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"client":      {Type: "string", Enum: []any{"opencode", "auggie", "goose"}, Description: "Downstream MCP client to configure."},
			"binary_path": {Type: "string", Description: "Absolute path to the murtaugh binary used as the MCP command."},
		},
		Required: []string{"client", "binary_path"},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	Client     string `json:"client"`
	Path       string `json:"path"`
	BackupPath string `json:"backup_path,omitempty"`
	Created    bool   `json:"created"`
}

// String renders a one-line CLI confirmation.
func (r Result) String() string {
	verb := "updated"
	if r.Created {
		verb = "created"
	}
	if r.BackupPath != "" {
		return fmt.Sprintf("%s %s for %s (backup: %s)", verb, r.Path, r.Client, r.BackupPath)
	}
	return fmt.Sprintf("%s %s for %s", verb, r.Path, r.Client)
}

// Invoke dispatches to the per-client writer.
func (t *Tool) Invoke(_ context.Context, args map[string]any) (any, error) {
	client, _ := args["client"].(string)
	binary, _ := args["binary_path"].(string)
	if strings.TrimSpace(binary) == "" {
		return nil, errors.New("binary_path is required")
	}
	home, err := t.home()
	if err != nil {
		return nil, fmt.Errorf("resolve home: %w", err)
	}
	switch client {
	case "opencode":
		return writeOpencode(home, binary)
	case "auggie":
		return writeAuggie(home, binary)
	case "goose":
		return writeGoose(home, binary)
	default:
		return nil, fmt.Errorf("unsupported client %q (want opencode, auggie, or goose)", client)
	}
}

// existed reports whether path was on disk before the write — used to
// distinguish a create from an update without consulting backup.IfExists
// twice.
func existed(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
