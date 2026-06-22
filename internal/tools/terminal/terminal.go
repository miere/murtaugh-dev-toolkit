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

// Approval mode values for ApprovalPolicy.Mode.
const (
	ApprovalAllowlist = "allowlist" // auto-run recognized read-only commands; ask otherwise
	ApprovalPrompt    = "prompt"    // ask before every command
	ApprovalOff       = "off"       // never ask
)

// ApprovalPolicy controls whether a command needs human approval before it runs
// (consulted by the native loop's gate via RequiresApproval). Mode "off" — the
// default for the bare New constructor — disables gating entirely.
type ApprovalPolicy struct {
	Mode  string
	Allow []string // extra allowlist keys (argv0 or "binary subcommand")
}

// Tool is the terminal capability. It runs shell commands rooted at root.
type Tool struct {
	root     string
	approval ApprovalPolicy
}

// New constructs a terminal Tool rooted at root with approval gating OFF
// (the historical behaviour; CLI/MCP and tests). Use NewWithApproval to gate.
// Commands run with a working directory defaulting to root; the optional workdir
// argument may select a subdirectory of root but never a path outside it. root
// is cleaned to an absolute path so traversal checks are reliable.
func New(root string) *Tool {
	return NewWithApproval(root, ApprovalPolicy{Mode: ApprovalOff})
}

// NewWithApproval constructs a terminal Tool rooted at root with the given
// approval policy. An empty Mode defaults to allowlist (gating on).
func NewWithApproval(root string, policy ApprovalPolicy) *Tool {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = filepath.Clean(root)
	}
	if strings.TrimSpace(policy.Mode) == "" {
		policy.Mode = ApprovalAllowlist
	}
	return &Tool{root: abs, approval: policy}
}

// RequiresApproval reports whether THIS command needs the user's go-ahead before
// running, per the tool's approval policy. It satisfies tools.ApprovalClassifier
// so the native loop's gate can consult it. Off → never; prompt → always;
// allowlist (the default) → only when the command is not a recognized read-only
// one (fail closed).
func (t *Tool) RequiresApproval(args map[string]any) bool {
	switch t.approval.Mode {
	case ApprovalOff:
		return false
	case ApprovalPrompt:
		return true
	default: // allowlist
		command, _ := args["command"].(string)
		return !isReadOnly(command, t.approval.Allow)
	}
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
	// Kill the whole process group on timeout, not just the shell: a shell that
	// leaves a child holding the output pipe (a backgrounded process, or dash on
	// Linux forking the command rather than exec-ing it) would otherwise keep Run
	// blocked until that child exits on its own. WaitDelay is a backstop in case a
	// descendant still lingers after the group kill.
	configureProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second

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
