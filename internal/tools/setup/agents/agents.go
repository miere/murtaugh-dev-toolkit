// Package agents implements the `setup.agents` tool: write agents.yaml with
// an ACP block and a single named agent. Mirrors the on-disk layout the bash
// installer used to emit, so the running daemon and existing fixtures see no
// shape change.
package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"

	"github.com/miere/murtaugh-dev-toolkit/internal/tools/setup/internal/backup"
)

// PathProvider returns the absolute path of agents.yaml.
type PathProvider func() string

// Tool is the `setup.agents` capability.
type Tool struct {
	path PathProvider
}

// New constructs a Tool that writes agents.yaml at the path returned by path.
func New(path PathProvider) *Tool {
	return &Tool{path: path}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "setup.agents" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Write agents.yaml with the ACP block and an optional default agent."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"agent_name": {Type: "string", Description: "Key under which the agent is registered. Defaults to \"default\"."},
			"command":    {Type: "string", Description: "Absolute path to the ACP-speaking binary. Blank disables ACP."},
			"args":       {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "Arguments passed to command."},
		},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	Path       string `json:"path"`
	Created    bool   `json:"created"`
	BackupPath string `json:"backup_path,omitempty"`
	ACPEnabled bool   `json:"acp_enabled"`
	AgentName  string `json:"agent_name,omitempty"`
}

// String renders a one-line CLI confirmation.
func (r Result) String() string {
	verb := "updated"
	if r.Created {
		verb = "created"
	}
	state := "acp=disabled"
	if r.ACPEnabled {
		state = fmt.Sprintf("acp=enabled agent=%s", r.AgentName)
	}
	if r.BackupPath != "" {
		return fmt.Sprintf("%s %s (%s, backup: %s)", verb, r.Path, state, r.BackupPath)
	}
	return fmt.Sprintf("%s %s (%s)", verb, r.Path, state)
}

// acpDefaults captures the tuning the bash installer baked into every fresh
// agents.yaml. New tools that want to adjust these knobs should compose with
// setup.agents rather than duplicating its writer.
var acpDefaults = acpBlock{
	StartupTimeout:       "10s",
	RequestTimeout:       "10m",
	SessionIdleTimeout:   "30m",
	MaxSessions:          100,
	StreamAppendInterval: "750ms",
	StreamMinChunkChars:  96,
	StreamFinalFeedback:  false,
}

type document struct {
	ACP    acpBlock                `yaml:"acp"`
	Agents map[string]profileBlock `yaml:"agents"`
}

type acpBlock struct {
	Enabled              bool   `yaml:"enabled"`
	StartupTimeout       string `yaml:"startup_timeout"`
	RequestTimeout       string `yaml:"request_timeout"`
	SessionIdleTimeout   string `yaml:"session_idle_timeout"`
	MaxSessions          int    `yaml:"max_sessions"`
	StreamAppendInterval string `yaml:"stream_append_interval"`
	StreamMinChunkChars  int    `yaml:"stream_min_chunk_chars"`
	StreamFinalFeedback  bool   `yaml:"stream_final_feedback"`
}

type profileBlock struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
}

// Invoke validates arguments and writes the agents.yaml document.
func (t *Tool) Invoke(_ context.Context, args map[string]any) (any, error) {
	command, _ := args["command"].(string)
	agentName, _ := args["agent_name"].(string)
	if strings.TrimSpace(agentName) == "" {
		agentName = "default"
	}
	agentArgs, err := coerceStringSlice(args["args"])
	if err != nil {
		return nil, fmt.Errorf("args: %w", err)
	}
	if strings.TrimSpace(command) == "" && len(agentArgs) > 0 {
		return nil, errors.New("args is set but command is empty")
	}

	path := t.path()
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("agents.yaml path is not configured")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("ensure config dir: %w", err)
	}

	doc := document{ACP: acpDefaults, Agents: map[string]profileBlock{}}
	if strings.TrimSpace(command) != "" {
		doc.ACP.Enabled = true
		doc.Agents[agentName] = profileBlock{Command: command, Args: agentArgs}
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal agents.yaml: %w", err)
	}
	backupPath, err := backup.IfExists(path)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return nil, fmt.Errorf("write %q: %w", path, err)
	}
	return Result{
		Path:       path,
		Created:    backupPath == "",
		BackupPath: backupPath,
		ACPEnabled: doc.ACP.Enabled,
		AgentName:  agentName,
	}, nil
}
