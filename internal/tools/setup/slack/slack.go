// Package slack implements the `setup.slack` tool: write the Slack OAuth and
// runtime config block (slack.yaml) the daemon depends on. The tool replaces
// the inline `write_slack_yaml` helper that lived in install.sh.
//
// The tool is deliberately narrow: it only touches slack.yaml. Agent and ACP
// configuration is owned by `setup.agents`, MCP wiring by `setup.mcp-register`.
package slack

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
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/setup/internal/envfile"
)

// Env variable names slack.yaml references for the Slack credentials. The actual
// tokens live in ~/.config/murtaugh/.env, never in the YAML — so a shared config
// file (or a troubleshoot bundle) carries only the ${VAR} references.
const (
	appTokenVar = "SLACK_APP_TOKEN"
	botTokenVar = "SLACK_BOT_TOKEN"
)

// PathProvider returns the absolute path of slack.yaml. A closure over the
// loaded config dir is supplied by the composition root so the same path is
// observed whether the tool runs via the CLI, MCP, or a direct test.
type PathProvider func() string

// Tool is the `setup.slack` capability.
type Tool struct {
	path PathProvider
}

// New constructs a Tool that writes slack.yaml at the file path returned by
// path.
func New(path PathProvider) *Tool {
	return &Tool{path: path}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "setup.slack" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Write slack.yaml with OAuth tokens, admin user, and the /murtaugh slash command."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"app_token":     {Type: "string", Description: "Slack app-level token (must start with xapp-)."},
			"bot_token":     {Type: "string", Description: "Slack bot OAuth token (must start with xoxb-)."},
			"admin_user":    {Type: "string", Description: "Slack admin handle (@name) or user ID (U…)."},
			"default_agent": {Type: "string", Description: "Optional agents.yaml key wired into chat.default_agent."},
		},
		Required: []string{"app_token", "bot_token", "admin_user"},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	Path       string `json:"path"`
	Created    bool   `json:"created"`
	BackupPath string `json:"backup_path,omitempty"`
	// EnvPath is the .env the Slack tokens were written to (referenced from
	// slack.yaml as ${SLACK_APP_TOKEN}/${SLACK_BOT_TOKEN}).
	EnvPath string `json:"env_path,omitempty"`
}

// String renders a one-line CLI confirmation. It never echoes the tokens.
func (r Result) String() string {
	verb := "updated"
	if r.Created {
		verb = "created"
	}
	msg := fmt.Sprintf("%s %s (tokens → %s)", verb, r.Path, r.EnvPath)
	if r.BackupPath != "" {
		msg += " (backup: " + r.BackupPath + ")"
	}
	return msg
}

// document mirrors the on-disk YAML shape produced by the bash installer so
// existing fixtures and the running daemon see identical input.
type document struct {
	OAuth         oauthBlock         `yaml:"oauth"`
	Configuration configurationBlock `yaml:"configuration"`
	Chat          map[string]string  `yaml:"chat"`
	Commands      []commandEntry     `yaml:"commands"`
}

type oauthBlock struct {
	AppToken string `yaml:"app_token"`
	BotToken string `yaml:"bot_token"`
}

type configurationBlock struct {
	AdminUser string `yaml:"admin_user"`
	Debug     bool   `yaml:"debug"`
}

type commandEntry struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// Invoke validates arguments, builds the slack.yaml document, and writes it
// to disk with 0600 perms. An existing file is backed up before being
// replaced.
func (t *Tool) Invoke(_ context.Context, args map[string]any) (any, error) {
	appToken, _ := args["app_token"].(string)
	botToken, _ := args["bot_token"].(string)
	adminUser, _ := args["admin_user"].(string)
	defaultAgent, _ := args["default_agent"].(string)

	if !strings.HasPrefix(appToken, "xapp-") {
		return nil, errors.New("app_token must start with xapp-")
	}
	if !strings.HasPrefix(botToken, "xoxb-") {
		return nil, errors.New("bot_token must start with xoxb-")
	}
	if strings.TrimSpace(adminUser) == "" {
		return nil, errors.New("admin_user is required")
	}

	path := t.path()
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("slack.yaml path is not configured")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("ensure config dir: %w", err)
	}

	// Secrets go to the .env sibling; slack.yaml only references them. This is
	// what keeps tokens out of a shareable config / troubleshoot bundle.
	envPath := filepath.Join(filepath.Dir(path), ".env")
	if _, err := envfile.Merge(envPath, map[string]string{
		appTokenVar: appToken,
		botTokenVar: botToken,
	}); err != nil {
		return nil, fmt.Errorf("write Slack tokens to .env: %w", err)
	}

	doc := document{
		OAuth:         oauthBlock{AppToken: "${" + appTokenVar + "}", BotToken: "${" + botTokenVar + "}"},
		Configuration: configurationBlock{AdminUser: adminUser, Debug: false},
		Chat:          map[string]string{},
		Commands: []commandEntry{
			{Name: "/murtaugh", Description: "Entrypoint for Murtaugh commands"},
		},
	}
	if strings.TrimSpace(defaultAgent) != "" {
		doc.Chat["default_agent"] = defaultAgent
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal slack.yaml: %w", err)
	}

	backupPath, err := backup.IfExists(path)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return nil, fmt.Errorf("write %q: %w", path, err)
	}
	return Result{Path: path, Created: backupPath == "", BackupPath: backupPath, EnvPath: envPath}, nil
}
