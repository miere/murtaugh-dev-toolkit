// Package agents implements the `setup.agents` tool: write agents.yaml with the
// runtime tuning block and a single named agent. It supports both backends — a
// native LLM agent (kind: native, the default) and an external ACP agent
// (kind: acp) — so the installer can configure either from one tool.
//
// Secrets are never written here: a native profile only records api_key_env (the
// .env variable name); the key value goes to .env via setup.env.
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
	return "Write agents.yaml with the runtime block and a native (default) or ACP agent."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"agent_name": {Type: "string", Description: "Key under which the agent is registered. Defaults to \"default\"."},
			"kind":       {Type: "string", Description: "Backend: \"native\" (default) or \"acp\". Inferred from the flags when omitted."},
			// ACP backend.
			"command": {Type: "string", Description: "ACP only: absolute path to the ACP-speaking binary."},
			"args":    {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "ACP only: arguments passed to command."},
			// Native backend.
			"provider":           {Type: "string", Description: "Native: provider family — gemini, anthropic, or openai."},
			"model":              {Type: "string", Description: "Native: provider model id (e.g. gemini-2.5-pro)."},
			"base_url":           {Type: "string", Description: "Native: endpoint override for compat providers (Z.ai/DeepSeek/Kimi)."},
			"api_key_env":        {Type: "string", Description: "Native: name of the .env variable holding the API key."},
			"tools":              {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "Native: tool allowlist (files, terminal, skills, and registry namespaces)."},
			"mcp_servers":        {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "Native: names of mcp_servers entries to attach."},
			"system_prompt_file": {Type: "string", Description: "Native: path (relative to config dir) to the system prompt file."},
			"context_limit":      {Type: "integer", Description: "Native: token budget for compaction. 0 uses a per-family default."},
			"compaction":         {Type: "string", Description: "Native: \"truncate\" (default) or \"summarize\"."},
			"cache_retention":    {Type: "string", Description: "Native: prompt-cache TTL — \"5m\" (default), \"1h\", or \"off\"."},
		},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	Path       string `json:"path"`
	Created    bool   `json:"created"`
	BackupPath string `json:"backup_path,omitempty"`
	Enabled    bool   `json:"enabled"`
	AgentName  string `json:"agent_name,omitempty"`
	Kind       string `json:"kind,omitempty"`
}

// String renders a one-line CLI confirmation.
func (r Result) String() string {
	verb := "updated"
	if r.Created {
		verb = "created"
	}
	state := "chat=disabled"
	if r.Enabled {
		state = fmt.Sprintf("chat=enabled agent=%s kind=%s", r.AgentName, r.Kind)
	}
	if r.BackupPath != "" {
		return fmt.Sprintf("%s %s (%s, backup: %s)", verb, r.Path, state, r.BackupPath)
	}
	return fmt.Sprintf("%s %s (%s)", verb, r.Path, state)
}

// acpDefaults captures the runtime tuning baked into every fresh agents.yaml.
// It applies to both backends (timeouts, streaming) and is emitted under the
// back-compatible `acp:` key.
var acpDefaults = acpBlock{
	Enabled:              false,
	StartupTimeout:       "10s",
	RequestTimeout:       "10m",
	SessionIdleTimeout:   "30m",
	MaxSessions:          100,
	StreamAppendInterval: "750ms",
	StreamMinChunkChars:  96,
	StreamFinalFeedback:  false,
	ProgressDisplay:      "simplified",
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
	ProgressDisplay      string `yaml:"progress_display"`
}

// profileBlock is the union of ACP and native fields; omitempty keeps each
// written profile minimal to its kind.
type profileBlock struct {
	Kind             string   `yaml:"kind,omitempty"`
	Command          string   `yaml:"command,omitempty"`
	Args             []string `yaml:"args,omitempty"`
	Provider         string   `yaml:"provider,omitempty"`
	Model            string   `yaml:"model,omitempty"`
	BaseURL          string   `yaml:"base_url,omitempty"`
	APIKeyEnv        string   `yaml:"api_key_env,omitempty"`
	Tools            []string `yaml:"tools,omitempty"`
	MCPServers       []string `yaml:"mcp_servers,omitempty"`
	SystemPromptFile string   `yaml:"system_prompt_file,omitempty"`
	ContextLimit     int      `yaml:"context_limit,omitempty"`
	Compaction       string   `yaml:"compaction,omitempty"`
	CacheRetention   string   `yaml:"cache_retention,omitempty"`
}

// Invoke validates arguments and writes the agents.yaml document.
func (t *Tool) Invoke(_ context.Context, args map[string]any) (any, error) {
	agentName, _ := args["agent_name"].(string)
	if strings.TrimSpace(agentName) == "" {
		agentName = "default"
	}
	kind := strings.ToLower(strings.TrimSpace(stringArg(args, "kind")))
	command := strings.TrimSpace(stringArg(args, "command"))
	provider := strings.TrimSpace(stringArg(args, "provider"))
	agentArgs, err := coerceStringSlice(args["args"])
	if err != nil {
		return nil, fmt.Errorf("args: %w", err)
	}

	// Infer the kind when not given: provider ⇒ native, command ⇒ acp.
	if kind == "" {
		switch {
		case provider != "":
			kind = "native"
		case command != "":
			kind = "acp"
		}
	}

	doc := document{ACP: acpDefaults, Agents: map[string]profileBlock{}}
	var resultKind string

	switch kind {
	case "native":
		profile, err := buildNative(args, provider)
		if err != nil {
			return nil, err
		}
		doc.ACP.Enabled = true
		doc.Agents[agentName] = profile
		resultKind = "native"
	case "acp":
		if command == "" {
			return nil, errors.New("kind acp requires --command")
		}
		doc.ACP.Enabled = true
		doc.Agents[agentName] = profileBlock{Kind: "acp", Command: command, Args: agentArgs}
		resultKind = "acp"
	case "":
		// No agent configured: write a disabled file (chat off). Stray agent
		// flags (e.g. --args without --command) are a mistake, not a silent skip.
		if hasNativeArgs(args) || command != "" || len(agentArgs) > 0 {
			return nil, errors.New("agent flags supplied but kind could not be determined; pass --kind, --command, or --provider")
		}
	default:
		return nil, fmt.Errorf("unknown kind %q (want native or acp)", kind)
	}

	path := t.path()
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("agents.yaml path is not configured")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("ensure config dir: %w", err)
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
		Enabled:    doc.ACP.Enabled,
		AgentName:  agentName,
		Kind:       resultKind,
	}, nil
}

// buildNative assembles and validates a native profile. provider is passed in
// already-trimmed since the caller used it for kind inference.
func buildNative(args map[string]any, provider string) (profileBlock, error) {
	model := strings.TrimSpace(stringArg(args, "model"))
	apiKeyEnv := strings.TrimSpace(stringArg(args, "api_key_env"))
	switch {
	case provider == "":
		return profileBlock{}, errors.New("kind native requires --provider")
	case model == "":
		return profileBlock{}, errors.New("kind native requires --model")
	case apiKeyEnv == "":
		return profileBlock{}, errors.New("kind native requires --api-key-env")
	}
	switch provider {
	case "gemini", "anthropic", "openai":
	default:
		return profileBlock{}, fmt.Errorf("provider %q must be gemini, anthropic, or openai", provider)
	}
	tools, err := coerceStringSlice(args["tools"])
	if err != nil {
		return profileBlock{}, fmt.Errorf("tools: %w", err)
	}
	mcpServers, err := coerceStringSlice(args["mcp_servers"])
	if err != nil {
		return profileBlock{}, fmt.Errorf("mcp_servers: %w", err)
	}
	contextLimit, err := coerceInt(args["context_limit"])
	if err != nil {
		return profileBlock{}, fmt.Errorf("context_limit: %w", err)
	}
	compaction := strings.ToLower(strings.TrimSpace(stringArg(args, "compaction")))
	switch compaction {
	case "", "truncate", "summarize":
	default:
		return profileBlock{}, fmt.Errorf("compaction %q must be truncate or summarize", compaction)
	}
	cacheRetention := strings.ToLower(strings.TrimSpace(stringArg(args, "cache_retention")))
	switch cacheRetention {
	case "", "off", "none", "5m", "short", "1h", "long":
	default:
		return profileBlock{}, fmt.Errorf("cache_retention %q must be one of 5m, 1h, or off", cacheRetention)
	}
	return profileBlock{
		Kind:             "native",
		Provider:         provider,
		Model:            model,
		BaseURL:          strings.TrimSpace(stringArg(args, "base_url")),
		APIKeyEnv:        apiKeyEnv,
		Tools:            tools,
		MCPServers:       mcpServers,
		SystemPromptFile: strings.TrimSpace(stringArg(args, "system_prompt_file")),
		ContextLimit:     contextLimit,
		Compaction:       compaction,
		CacheRetention:   cacheRetention,
	}, nil
}

func hasNativeArgs(args map[string]any) bool {
	for _, k := range []string{"provider", "model", "api_key_env", "base_url", "tools", "mcp_servers", "system_prompt_file", "compaction"} {
		if v, ok := args[k]; ok && v != nil {
			if s, isStr := v.(string); isStr && strings.TrimSpace(s) == "" {
				continue
			}
			return true
		}
	}
	return false
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}
