// Package edit implements the `files.edit` tool: an exact string replacement in
// a file within the agent's root directory. The match must be unique unless
// replace_all is set, and the file must have been read first and be unchanged
// since (read-before-write guard).
package edit

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/tools/files"
)

// Tool is the `files.edit` capability, rooted at a directory and sharing the
// read-before-write stamp store with the read/write tools.
type Tool struct {
	root  *files.Root
	state *files.ReadState
}

// New constructs an edit Tool anchored at root, consulting/updating state.
func New(root *files.Root, state *files.ReadState) *Tool {
	return &Tool{root: root, state: state}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "files.edit" }

// Description returns the human-facing summary used by MCP/CLI clients.
func (t *Tool) Description() string {
	return "Replace an exact string in a file within the workspace (unique match unless replace_all)."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"path":        {Type: "string", Description: "File path relative to the workspace root (absolute paths must stay inside it)."},
			"old_string":  {Type: "string", Description: "Exact text to replace. Must be unique unless replace_all is true."},
			"new_string":  {Type: "string", Description: "Replacement text."},
			"replace_all": {Type: "boolean", Description: "Replace every occurrence instead of requiring a unique match (default false)."},
		},
		Required: []string{"path", "old_string", "new_string"},
	}
}

// Result is the structured payload returned by Invoke. The MCP frontend
// JSON-marshals it; the CLI frontend renders it via String().
type Result struct {
	Path         string `json:"path"`
	Replacements int    `json:"replacements"`
}

// String renders a single CLI-visible line describing the edit.
func (r Result) String() string {
	plural := ""
	if r.Replacements != 1 {
		plural = "s"
	}
	return fmt.Sprintf("Made %d replacement%s in %s.", r.Replacements, plural, r.Path)
}

// Invoke applies the replacement. It refuses when the file was not read first or
// drifted since (stale-write guard), when old_string equals new_string, when
// old_string is absent, or when it is non-unique and replace_all is false. On
// success the stamp store is refreshed so a follow-up edit sees a fresh baseline.
func (t *Tool) Invoke(_ context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("Error: path is required")
	}
	oldStr, _ := args["old_string"].(string)
	if oldStr == "" {
		return nil, fmt.Errorf("Error: old_string is required")
	}
	newStr, _ := args["new_string"].(string)
	if oldStr == newStr {
		return nil, fmt.Errorf("Error: old_string and new_string are identical")
	}
	replaceAll, _ := args["replace_all"].(bool)

	abs, err := t.root.Resolve(path)
	if err != nil {
		return nil, err
	}

	if err := t.state.Verify(abs); err != nil {
		return nil, fmt.Errorf("Error: %v", err)
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("Error: %v", err)
	}
	content := string(data)

	count := strings.Count(content, oldStr)
	if count == 0 {
		return nil, fmt.Errorf("Error: old_string not found in %s", path)
	}
	if count > 1 && !replaceAll {
		return nil, fmt.Errorf("Error: old_string is not unique in %s (%d matches); pass replace_all or include more context", path, count)
	}

	var updated string
	if replaceAll {
		updated = strings.ReplaceAll(content, oldStr, newStr)
	} else {
		updated = strings.Replace(content, oldStr, newStr, 1)
		count = 1
	}

	// Preserve the file's existing permissions.
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("Error: %v", err)
	}
	if err := os.WriteFile(abs, []byte(updated), info.Mode().Perm()); err != nil {
		return nil, fmt.Errorf("Error: %v", err)
	}

	// Refresh the baseline so a follow-up edit does not see false drift.
	if err := t.state.Mark(abs); err != nil {
		return nil, fmt.Errorf("Error: %v", err)
	}

	return Result{Path: t.root.Rel(abs), Replacements: count}, nil
}
