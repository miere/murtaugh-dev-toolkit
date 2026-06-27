// Package write implements the `files.write` tool: create a new file or
// overwrite an existing one within the agent's root directory. Overwriting an
// existing file requires that it was read first and has not drifted since
// (read-before-write guard); creating a brand-new file does not.
package write

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/tools/files"
)

// Tool is the `files.write` capability, rooted at a directory and sharing the
// read-before-write stamp store with the read/edit tools.
type Tool struct {
	root  *files.Root
	state *files.ReadState
}

// New constructs a write Tool anchored at root, consulting/updating state.
func New(root *files.Root, state *files.ReadState) *Tool {
	return &Tool{root: root, state: state}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "files.write" }

// Description returns the human-facing summary used by MCP/CLI clients.
func (t *Tool) Description() string {
	return "Create a new file or overwrite an existing one within the workspace."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"path":    {Type: "string", Description: "File path relative to the workspace root (absolute paths must stay inside it)."},
			"content": {Type: "string", Description: "Full file content to write."},
		},
		Required: []string{"path", "content"},
	}
}

// Result is the structured payload returned by Invoke. The MCP frontend
// JSON-marshals it; the CLI frontend renders it via String().
type Result struct {
	Path    string `json:"path"`
	Bytes   int    `json:"bytes"`
	Created bool   `json:"created"`
}

// String renders a single CLI-visible line describing the write.
func (r Result) String() string {
	verb := "Wrote"
	if r.Created {
		verb = "Created"
	}
	return fmt.Sprintf("%s %s (%d bytes).", verb, r.Path, r.Bytes)
}

// Invoke writes content to the file at path. A new file is created (parent dirs
// are created as needed). An existing file is overwritten only if it was read
// first and has not changed since (stale-write guard). After a successful write
// the stamp store is updated so a subsequent edit/write sees a fresh baseline.
func (t *Tool) Invoke(_ context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("Error: path is required")
	}
	content, ok := args["content"].(string)
	if !ok {
		return nil, fmt.Errorf("Error: content is required")
	}

	abs, err := t.root.Resolve(path)
	if err != nil {
		return nil, err
	}

	info, statErr := os.Stat(abs)
	switch {
	case statErr == nil && info.IsDir():
		return nil, fmt.Errorf("Error: %q is a directory", path)
	case statErr == nil:
		// Overwriting an existing file: it must have been read and be unchanged.
		if err := t.state.Verify(abs); err != nil {
			return nil, fmt.Errorf("Error: %v", err)
		}
	case errors.Is(statErr, os.ErrNotExist):
		// New file: no read-before-write requirement.
	default:
		return nil, fmt.Errorf("Error: %v", statErr)
	}

	created := errors.Is(statErr, os.ErrNotExist)
	if created {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return nil, fmt.Errorf("Error: %v", err)
		}
	}

	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("Error: %v", err)
	}

	// Refresh the baseline so a follow-up edit/write does not see false drift.
	if err := t.state.Mark(abs); err != nil {
		return nil, fmt.Errorf("Error: %v", err)
	}

	return Result{Path: t.root.Rel(abs), Bytes: len(content), Created: created}, nil
}
