// Package bootstrap implements the `setup.bootstrap` tool: seed the config
// directory with the embedded defaults (slack.yaml, agents.yaml, jobs.yaml,
// skills/, optional docs) the first time Murtaugh is installed.
//
// The tool wraps config.BootstrapWithReport so the installer (and any MCP
// client driving setup remotely) can report which files were freshly written
// versus which were preserved because the user already customised them.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
)

// PathProvider returns the path of the primary config file (slack.yaml). The
// config directory is derived from filepath.Dir of that value, matching the
// convention used by the rest of the codebase.
type PathProvider func() string

// Tool is the `setup.bootstrap` capability.
type Tool struct {
	path PathProvider
}

// New constructs a Tool that seeds the directory containing the file path
// returned by path.
func New(path PathProvider) *Tool {
	return &Tool{path: path}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "setup.bootstrap" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Seed the Murtaugh config directory with embedded defaults (idempotent)."
}

// InputSchema returns nil because the tool takes no arguments.
func (t *Tool) InputSchema() *jsonschema.Schema { return nil }

// Result is the structured payload returned by Invoke.
type Result struct {
	ConfigDir string   `json:"config_dir"`
	Created   []string `json:"created"`
	Preserved []string `json:"preserved"`
}

// String renders a multi-line CLI confirmation summarising the report.
func (r Result) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bootstrap: %d created, %d preserved in %s",
		len(r.Created), len(r.Preserved), r.ConfigDir)
	for _, p := range r.Created {
		fmt.Fprintf(&b, "\n  + %s", p)
	}
	for _, p := range r.Preserved {
		fmt.Fprintf(&b, "\n  = %s", p)
	}
	return b.String()
}

// Invoke seeds the config directory and returns a structured report of the
// result. Arguments are ignored.
func (t *Tool) Invoke(_ context.Context, _ map[string]any) (any, error) {
	path := t.path()
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("config path is not configured")
	}

	report, err := config.BootstrapWithReport(path)
	if err != nil {
		return nil, err
	}
	return Result{
		ConfigDir: filepath.Dir(path),
		Created:   report.Created,
		Preserved: report.Preserved,
	}, nil
}
