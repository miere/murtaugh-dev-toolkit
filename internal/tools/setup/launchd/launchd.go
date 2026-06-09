// Package launchd implements the `setup.launchd` tool: write the
// dev.murtaugh LaunchAgent plist, optionally invoking launchctl to
// (re)bootstrap it. Mirrors the plist install.sh emitted, including the
// PATH environment block and the slack-mode ProgramArguments.
//
// The tool is registered on every platform but only operational on darwin;
// other GOOS values return a clean "unsupported on $GOOS" error so callers
// (MCP clients, scripted setups) see one consistent contract.
package launchd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh-dev-toolkit/internal/tools/setup/internal/backup"
)

// HomeResolver returns the user home directory.
type HomeResolver func() (string, error)

// CommandRunner invokes name with args. Used to abstract launchctl and plutil
// out of the tool body so tests can record invocations without touching the
// host launchd.
type CommandRunner func(ctx context.Context, name string, args ...string) error

// Deps is the explicit dependency bundle passed to New so the composition
// root and tests share one shape.
type Deps struct {
	Home      HomeResolver
	GOOS      string
	Plutil    CommandRunner
	Launchctl CommandRunner
}

// Tool is the `setup.launchd` capability.
type Tool struct {
	deps Deps
}

// New constructs a Tool from the supplied dependencies.
func New(deps Deps) *Tool { return &Tool{deps: deps} }

// Name returns the registry key.
func (t *Tool) Name() string { return "setup.launchd" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Write the dev.murtaugh LaunchAgent plist (macOS) and optionally load it."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"binary_path": {Type: "string", Description: "Absolute path to the murtaugh binary."},
			"load":        {Type: "boolean", Description: "When true, run launchctl bootout+bootstrap after writing."},
		},
		Required: []string{"binary_path"},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	Path       string `json:"path"`
	BackupPath string `json:"backup_path,omitempty"`
	Created    bool   `json:"created"`
	Loaded     bool   `json:"loaded"`
}

// String renders a multi-line CLI confirmation.
func (r Result) String() string {
	verb := "updated"
	if r.Created {
		verb = "created"
	}
	loadLine := "load: skipped"
	if r.Loaded {
		loadLine = "load: bootstrapped"
	}
	if r.BackupPath != "" {
		return fmt.Sprintf("%s %s (backup: %s, %s)", verb, r.Path, r.BackupPath, loadLine)
	}
	return fmt.Sprintf("%s %s (%s)", verb, r.Path, loadLine)
}

// Invoke validates arguments and writes (optionally loading) the LaunchAgent.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	if t.deps.GOOS != "darwin" {
		return nil, fmt.Errorf("setup.launchd is unsupported on %s", t.deps.GOOS)
	}
	binary, _ := args["binary_path"].(string)
	if strings.TrimSpace(binary) == "" {
		return nil, errors.New("binary_path is required")
	}
	load, _ := args["load"].(bool)

	home, err := t.deps.Home()
	if err != nil {
		return nil, fmt.Errorf("resolve home: %w", err)
	}

	plistPath := filepath.Join(home, "Library", "LaunchAgents", "dev.murtaugh.plist")
	logsDir := filepath.Join(home, "Library", "Logs", "murtaugh")
	for _, dir := range []string{filepath.Dir(plistPath), logsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("ensure %q: %w", dir, err)
		}
	}

	body, err := renderPlist(binary, home, logsDir)
	if err != nil {
		return nil, err
	}
	wasThere := pathExists(plistPath)
	backupPath, err := backup.IfExists(plistPath)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(plistPath, body, 0o644); err != nil {
		return nil, fmt.Errorf("write %q: %w", plistPath, err)
	}
	if err := t.deps.Plutil(ctx, "plutil", "-lint", plistPath); err != nil {
		return nil, fmt.Errorf("plutil -lint failed: %w", err)
	}

	loaded := false
	if load {
		gui := "gui/" + strconv.Itoa(os.Getuid())
		_ = t.deps.Launchctl(ctx, "launchctl", "bootout", gui, plistPath)
		if err := t.deps.Launchctl(ctx, "launchctl", "bootstrap", gui, plistPath); err != nil {
			return nil, fmt.Errorf("launchctl bootstrap failed: %w", err)
		}
		// bootstrap registers the agent but does not reliably honor
		// RunAtLoad: the job sits loaded-but-never-spawned (runs=0,
		// state=not running) and produces no output, so Slack is
		// unreachable. kickstart forces the first run; -k also restarts a
		// stale instance, keeping re-runs of the installer idempotent.
		if err := t.deps.Launchctl(ctx, "launchctl", "kickstart", "-k", gui+"/dev.murtaugh"); err != nil {
			return nil, fmt.Errorf("launchctl kickstart failed: %w", err)
		}
		loaded = true
	}
	return Result{Path: plistPath, BackupPath: backupPath, Created: !wasThere, Loaded: loaded}, nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
