// Package update implements the `setup.update` tool: replace the running
// Murtaugh binary with the matching asset from a GitHub release. Mirrors
// install.sh's install_or_update_binary semantics:
//
//   - "dev" builds are refused by default — they are likely a local checkout
//     and silently overwriting them would surprise the developer. Pass
//     force=true to override.
//   - Already-current installs short-circuit with a Skipped result.
//   - The fetched asset is verified before it replaces the running binary;
//     a failed verify leaves the original in place.
//   - The previous binary is backed up alongside the install path before the
//     swap.
package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// HTTPGet performs a GET against url and returns the body. Injected so tests
// can stub network access; production wiring is httpGet.
type HTTPGet func(ctx context.Context, url string) ([]byte, error)

// Deps is the explicit dependency bundle passed to New.
type Deps struct {
	CurrentVersion func() string
	CurrentBinary  func() (string, error)
	GOOS, GOARCH   string
	HTTPGet        HTTPGet
	VerifyBinary   func(path string) error
	Owner, Repo    string
}

// Tool is the `setup.update` capability.
type Tool struct {
	deps Deps
}

// New constructs a Tool from the supplied dependencies.
func New(deps Deps) *Tool { return &Tool{deps: deps} }

// Name returns the registry key.
func (t *Tool) Name() string { return "setup.update" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Update the running Murtaugh binary from a GitHub release asset."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"version":          {Type: "string", Description: "Optional release tag to install (default: latest)."},
			"force":            {Type: "boolean", Description: "Replace the binary even when version is \"dev\" or already current."},
			"release_json_url": {Type: "string", Description: "Override the release JSON URL; primarily for local fixtures and tests."},
		},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	CurrentVersion string `json:"current_version"`
	TargetVersion  string `json:"target_version"`
	BinaryPath     string `json:"binary_path"`
	BackupPath     string `json:"backup_path,omitempty"`
	Skipped        bool   `json:"skipped"`
}

// String renders a one-line CLI confirmation.
func (r Result) String() string {
	if r.Skipped {
		return fmt.Sprintf("already running %s; nothing to do", r.CurrentVersion)
	}
	if r.BackupPath != "" {
		return fmt.Sprintf("updated %s from %s to %s (backup: %s)", r.BinaryPath, r.CurrentVersion, r.TargetVersion, r.BackupPath)
	}
	return fmt.Sprintf("installed %s at %s", r.TargetVersion, r.BinaryPath)
}

// HTTPGetter returns the default HTTPGet implementation: a plain http.Get
// with a context-aware request. Composition root wires this in.
func HTTPGetter() HTTPGet {
	return func(ctx context.Context, url string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: status %d", url, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}
}

// Invoke is the entry point that orchestrates fetch + verify + swap.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	force, _ := args["force"].(bool)
	target, _ := args["version"].(string)
	override, _ := args["release_json_url"].(string)

	current := t.deps.CurrentVersion()
	if current == "dev" && !force {
		return nil, errors.New("refusing to update a dev binary; pass force=true to override")
	}

	url := strings.TrimSpace(override)
	if url == "" {
		url = releaseURL(t.deps.Owner, t.deps.Repo, target)
	}
	releaseBody, err := t.deps.HTTPGet(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("fetch release: %w", err)
	}
	tag, assetURL, err := findAsset(releaseBody, t.deps.GOOS, t.deps.GOARCH)
	if err != nil {
		return nil, err
	}
	if equalVersions(current, tag) && !force {
		bin, _ := t.deps.CurrentBinary()
		return Result{CurrentVersion: current, TargetVersion: tag, BinaryPath: bin, Skipped: true}, nil
	}

	asset, err := t.deps.HTTPGet(ctx, assetURL)
	if err != nil {
		return nil, fmt.Errorf("download asset: %w", err)
	}
	return t.installAsset(current, tag, asset)
}
