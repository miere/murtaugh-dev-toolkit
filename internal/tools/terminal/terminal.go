// Package terminal implements Murtaugh's terminal tool: run a shell command in
// a sandboxed working directory, bounded by a timeout and an output-size cap.
//
// Commands run via `sh -c` (darwin/linux). The working directory defaults to
// the tool's root and may be narrowed to a subdirectory via the optional
// workdir argument; any attempt to escape the root is rejected.
package terminal

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

const (
	// DefaultTimeout is applied when the caller omits the timeout argument.
	DefaultTimeout = 30 * time.Second
	// MaxTimeout caps the caller-supplied timeout.
	MaxTimeout = 5 * time.Minute
	// MaxOutputBytes caps the captured combined stdout+stderr. Output beyond
	// this is dropped and a truncation notice is appended.
	MaxOutputBytes = 64 * 1024

	truncationNotice = "\n\n[output truncated: exceeded 64KB cap]"
)

// Tool is the terminal capability. It runs shell commands rooted at root.
type Tool struct {
	root string
}

// New constructs a terminal Tool rooted at root. Commands run with a working
// directory defaulting to root; the optional workdir argument may select a
// subdirectory of root but never a path outside it. root is cleaned to an
// absolute path so traversal checks are reliable.
func New(root string) *Tool {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = filepath.Clean(root)
	}
	return &Tool{root: abs}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "terminal" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Run a shell command in the agent's workspace and capture its combined output."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"command": {Type: "string", Description: "Shell command to run via `sh -c`."},
			"workdir": {Type: "string", Description: "Working directory relative to the workspace root. Defaults to the root; must stay within it."},
			"timeout": {Type: "string", Description: "Max run time as a Go duration (e.g. 45s, 2m). Defaults to 30s, capped at 5m."},
		},
		Required: []string{"command"},
	}
}

// Result is the structured payload returned by Invoke. The MCP frontend
// JSON-marshals it; the CLI frontend renders it via String().
type Result struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
	TimedOut bool   `json:"timed_out"`
}

// String renders a CLI-visible summary followed by the captured output.
func (r Result) String() string {
	var b strings.Builder
	if r.TimedOut {
		fmt.Fprintf(&b, "Command timed out (exit %d).\n", r.ExitCode)
	} else {
		fmt.Fprintf(&b, "Command exited %d.\n", r.ExitCode)
	}
	b.WriteString(r.Output)
	return b.String()
}

// Invoke runs command in the resolved working directory under a context
// deadline. It captures combined stdout+stderr (capped at MaxOutputBytes) and
// reports the exit code. A non-zero exit is reported in Result, not as an
// error; an error is returned only for invalid arguments or a failure to start
// the process. On timeout, Result.TimedOut is set and ExitCode is -1.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	command, _ := args["command"].(string)
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("Error: command is required")
	}

	workdir, _ := args["workdir"].(string)
	dir, err := t.resolveWorkdir(workdir)
	if err != nil {
		return nil, err
	}

	timeout := DefaultTimeout
	if raw, _ := args["timeout"].(string); strings.TrimSpace(raw) != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("Error: invalid timeout %q: %w", raw, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("Error: timeout must be positive, got %q", raw)
		}
		if d > MaxTimeout {
			d = MaxTimeout
		}
		timeout = d
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	cmd.Dir = dir

	var buf cappedBuffer
	buf.limit = MaxOutputBytes
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	runErr := cmd.Run()

	res := Result{Output: buf.String()}
	if buf.truncated {
		res.Output += truncationNotice
	}

	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		res.TimedOut = true
		res.ExitCode = -1
	case runErr == nil:
		res.ExitCode = 0
	default:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		} else {
			// Process failed to start (e.g. sh missing). Surface as an error.
			return nil, fmt.Errorf("Error: failed to run command: %w", runErr)
		}
	}

	return res, nil
}

// resolveWorkdir cleans workdir relative to the root and rejects any path that
// escapes the root. An empty workdir resolves to the root itself.
func (t *Tool) resolveWorkdir(workdir string) (string, error) {
	if strings.TrimSpace(workdir) == "" {
		return t.root, nil
	}
	candidate := workdir
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(t.root, candidate)
	}
	candidate = filepath.Clean(candidate)

	if candidate != t.root && !strings.HasPrefix(candidate, t.root+string(filepath.Separator)) {
		return "", fmt.Errorf("Error: workdir %q escapes the workspace root", workdir)
	}
	return candidate, nil
}
