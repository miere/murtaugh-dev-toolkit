// Package ls implements the `files.ls` tool: list the entries of a directory,
// or match files against a glob pattern, within the agent's root directory.
package ls

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/tools/files"
)

// Tool is the `files.ls` capability, rooted at a directory.
type Tool struct {
	root *files.Root
}

// New constructs an ls Tool anchored at root.
func New(root *files.Root) *Tool {
	return &Tool{root: root}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "files.ls" }

// Description returns the human-facing summary used by MCP/CLI clients.
func (t *Tool) Description() string {
	return "List a directory's entries, or match files against a glob, within the workspace."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"path": {Type: "string", Description: "Directory to list, relative to the workspace root. Defaults to the root."},
			"glob": {Type: "string", Description: "Glob pattern (relative to the workspace root) to match files instead of listing a directory, e.g. '*.go' or 'cmd/**/*.go'."},
		},
	}
}

// Entry describes one listed item.
type Entry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// Result is the structured payload returned by Invoke. The MCP frontend
// JSON-marshals it; the CLI frontend renders it via String().
type Result struct {
	Dir     string  `json:"dir"`
	Glob    string  `json:"glob,omitempty"`
	Entries []Entry `json:"entries"`
}

// String renders one entry per line, directories suffixed with "/".
func (r Result) String() string {
	if len(r.Entries) == 0 {
		return "(no entries)"
	}
	var b strings.Builder
	for i, e := range r.Entries {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(e.Path)
		if e.IsDir {
			b.WriteByte('/')
		}
	}
	return b.String()
}

// Invoke lists a directory or evaluates a glob. When glob is set it takes
// precedence over path. Paths/patterns that escape the root are rejected.
func (t *Tool) Invoke(_ context.Context, args map[string]any) (any, error) {
	glob, _ := args["glob"].(string)
	if glob != "" {
		return t.glob(glob)
	}
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	return t.list(path)
}

func (t *Tool) list(path string) (any, error) {
	abs, err := t.root.Resolve(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("Error: %v", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("Error: %q is not a directory", path)
	}
	dirents, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("Error: %v", err)
	}
	entries := make([]Entry, 0, len(dirents))
	for _, de := range dirents {
		var size int64
		if fi, err := de.Info(); err == nil {
			size = fi.Size()
		}
		entries = append(entries, Entry{
			Name:  de.Name(),
			Path:  t.root.Rel(filepath.Join(abs, de.Name())),
			IsDir: de.IsDir(),
			Size:  size,
		})
	}
	sortEntries(entries)
	return Result{Dir: t.root.Rel(abs), Entries: entries}, nil
}

func (t *Tool) glob(pattern string) (any, error) {
	// Resolve the pattern against the root so matches stay in-root; reject a
	// pattern whose fixed prefix already escapes.
	abs, err := t.root.Resolve(pattern)
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(abs)
	if err != nil {
		return nil, fmt.Errorf("Error: %v", err)
	}
	entries := make([]Entry, 0, len(matches))
	for _, m := range matches {
		// Defence in depth: drop anything that escaped the root via the pattern.
		clean, err := t.root.Resolve(m)
		if err != nil {
			continue
		}
		fi, err := os.Stat(clean)
		if err != nil {
			continue
		}
		entries = append(entries, Entry{
			Name:  filepath.Base(clean),
			Path:  t.root.Rel(clean),
			IsDir: fi.IsDir(),
			Size:  fi.Size(),
		})
	}
	sortEntries(entries)
	return Result{Dir: t.root.Rel(t.root.Dir()), Glob: pattern, Entries: entries}, nil
}

// sortEntries orders directories first, then by path, for stable output.
func sortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Path < entries[j].Path
	})
}
