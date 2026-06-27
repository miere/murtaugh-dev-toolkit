// Package read implements the `files.read` tool: read a text file within the
// agent's root directory, optionally a window of lines, and stamp it so a later
// write or edit knows the file was read first.
package read

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/tools/files"
)

// defaultLimit caps how many lines a read returns when the caller does not ask
// for a specific window, keeping a stray "read this huge file" from flooding the
// model context.
const defaultLimit = 2000

// Tool is the `files.read` capability, rooted at a directory and sharing a
// read-before-write stamp store with the write/edit tools.
type Tool struct {
	root  *files.Root
	state *files.ReadState
}

// New constructs a read Tool anchored at root, recording reads into state.
func New(root *files.Root, state *files.ReadState) *Tool {
	return &Tool{root: root, state: state}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "files.read" }

// Description returns the human-facing summary used by MCP/CLI clients.
func (t *Tool) Description() string {
	return "Read a text file (optionally a window of lines) within the workspace."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"path":   {Type: "string", Description: "File path relative to the workspace root (absolute paths must stay inside it)."},
			"offset": {Type: "integer", Description: "1-based line number to start reading from (default 1)."},
			"limit":  {Type: "integer", Description: "Maximum number of lines to return (default 2000)."},
		},
		Required: []string{"path"},
	}
}

// Result is the structured payload returned by Invoke. The MCP frontend
// JSON-marshals it; the CLI frontend renders it via String().
type Result struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	LineCount int    `json:"line_count"`
	Truncated bool   `json:"truncated"`
}

// String renders the file content with a short trailing note when the window
// did not cover the whole file.
func (r Result) String() string {
	if r.Truncated {
		return fmt.Sprintf("%s\n\n[lines %d-%d of %d shown]", r.Content, r.StartLine, r.EndLine, r.LineCount)
	}
	return r.Content
}

// Invoke reads the requested file (or window of lines), records the read in the
// shared stamp store, and returns the content. Paths that escape the root are
// rejected by Root.Resolve.
func (t *Tool) Invoke(_ context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("Error: path is required")
	}
	offset := toInt(args["offset"])
	limit := toInt(args["limit"])
	if offset <= 0 {
		offset = 1
	}
	if limit <= 0 {
		limit = defaultLimit
	}

	abs, err := t.root.Resolve(path)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("Error: %v", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("Error: %q is a directory; use files.ls", path)
	}

	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("Error: %v", err)
	}
	defer f.Close()

	var (
		b         strings.Builder
		lineNum   int
		emitted   int
		endLine   int
		truncated bool
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		lineNum++
		if lineNum < offset {
			continue
		}
		if emitted >= limit {
			truncated = true
			break
		}
		if emitted > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(sc.Text())
		emitted++
		endLine = lineNum
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("Error: %v", err)
	}
	// Count any remaining lines to report total + truncation accurately.
	if truncated {
		for sc.Scan() {
			lineNum++
		}
	}

	// Record the read against the file's current on-disk identity so write/edit
	// can detect drift; use the FileInfo we already have.
	t.state.MarkInfo(abs, info)

	start := offset
	if emitted == 0 {
		start = 0
		endLine = 0
	}
	return Result{
		Path:      t.root.Rel(abs),
		Content:   b.String(),
		StartLine: start,
		EndLine:   endLine,
		LineCount: lineNum,
		Truncated: truncated,
	}, nil
}

// toInt coerces an integer arg that may arrive as int (CLI) or float64 (MCP JSON).
func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}
