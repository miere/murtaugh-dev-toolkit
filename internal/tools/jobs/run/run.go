// Package run implements the `jobs.run` tool: execute a job defined in
// jobs.yaml. The tool resolves the job by name against the loaded
// configuration, applies the per-job timeout (default 10 minutes), and runs
// the command with the configured args / workdir.
//
// Stdout and stderr from the executed process are streamed to the supplied
// writers — in the CLI frontend that is the user's terminal; the MCP
// frontend captures them so they appear in the JSON result.
package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/miere/murtaugh-dev-toolkit/internal/journal"
)

// defaultTimeout matches the previous hand-rolled subcommand: jobs without an
// explicit timeout run up to 10 minutes.
const defaultTimeout = 10 * time.Minute

// JobLookup returns the JobProfile registered under name, if any. The
// composition root supplies a closure over the loaded config.
type JobLookup func(name string) (config.JobProfile, bool)

// AgentDelegator runs an agent-delegated job: it starts the named agent in an
// isolated one-shot session and sends the rendered prompt, discarding the
// agent's text output (jobs are fire-and-forget — the agent acts through its
// own tools). *agentdelegate.Runner satisfies it.
type AgentDelegator interface {
	RunAndForget(ctx context.Context, agent, prompt string) error
}

// Tool is the `jobs.run` capability.
type Tool struct {
	lookup    JobLookup
	delegator AgentDelegator
	stdout    io.Writer
	stderr    io.Writer
	recorder  journal.Recorder
}

// New constructs a Tool that resolves jobs through lookup and streams the
// child process stdout/stderr to the calling process's stdout/stderr. The
// CLI frontend gets a live console; the MCP frontend wraps it with a Tool
// configured via NewWith to capture output into the result instead.
func New(lookup JobLookup) *Tool {
	return &Tool{lookup: lookup, stdout: os.Stdout, stderr: os.Stderr, recorder: journal.NopRecorder{}}
}

// NewWith returns a Tool whose stdout/stderr are redirected to the supplied
// writers. Intended for tests and for frontends that need to capture output.
func NewWith(lookup JobLookup, stdout, stderr io.Writer) *Tool {
	return &Tool{lookup: lookup, stdout: stdout, stderr: stderr, recorder: journal.NopRecorder{}}
}

// WithDelegator wires the agent runner used by agent-delegated jobs (jobs that
// set `agent`/`prompt` instead of `command`). Without it, such a job fails with
// a clear error. Returns the receiver for fluent wiring.
func (t *Tool) WithDelegator(d AgentDelegator) *Tool {
	t.delegator = d
	return t
}

// WithRecorder wires the journal recorder that receives a job-stream `job.run`
// event for each invocation (command or agent), recording the outcome and
// duration. A nil recorder is ignored, leaving the no-op default in place.
// Returns the receiver for fluent wiring.
func (t *Tool) WithRecorder(recorder journal.Recorder) *Tool {
	if recorder != nil {
		t.recorder = recorder
	}
	return t
}

// record emits a job-stream `job.run` event keyed by job name.
func (t *Tool) record(ctx context.Context, level journal.Level, summary, name string, payload any) {
	t.recorder.Record(ctx, journal.Event{
		Stream:  journal.StreamJob,
		Kind:    "job.run",
		Level:   level,
		Summary: summary,
		CorrID:  journal.CorrIDFromContext(ctx),
		Keys:    journal.Keys{JobName: name},
		Payload: payload,
	})
}

// Name returns the registry key.
func (t *Tool) Name() string { return "jobs.run" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Run a job defined in jobs.yaml by name."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name": {Type: "string", Description: "Name of the job as keyed in jobs.yaml."},
			"args": {
				Type:        "array",
				Items:       &jsonschema.Schema{Type: "string"},
				Description: "Positional arguments forwarded to an agent-delegated job's prompt placeholders ({{ 1 }}, {{ 2 }}, ...). Ignored by command jobs.",
			},
		},
		Required: []string{"name"},
	}
}

// Result is the structured payload returned by Invoke. The MCP frontend
// JSON-marshals it; the CLI frontend renders it via String().
type Result struct {
	Name     string `json:"name"`
	Command  string `json:"command,omitempty"`
	Agent    string `json:"agent,omitempty"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

// String renders a one-line CLI confirmation.
func (r Result) String() string {
	if r.Agent != "" {
		return fmt.Sprintf("job %q completed via agent %q", r.Name, r.Agent)
	}
	return fmt.Sprintf("job %q completed (exit %d)", r.Name, r.ExitCode)
}

// Invoke resolves the named job and runs it. The job's configured timeout
// (default 10m) bounds the execution; stdout/stderr from the child process
// are streamed to the tool's writers and also captured into Result so the
// MCP frontend can surface them.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	name, _ := args["name"].(string)
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("name is required")
	}

	job, ok := t.lookup(name)
	if !ok {
		return nil, fmt.Errorf("job %q not found in jobs.yaml", name)
	}

	// Agent-delegated job: hand the rendered prompt to the agent runner instead
	// of spawning a command. Mutually exclusive with Command (enforced by config
	// validation), so this branch owns the whole invocation.
	if strings.TrimSpace(job.Agent) != "" {
		return t.invokeAgent(ctx, name, job, args)
	}

	if strings.TrimSpace(job.Command) == "" {
		return nil, fmt.Errorf("job %q has no command configured", name)
	}

	runCtx, cancel := context.WithTimeout(ctx, jobTimeout(job))
	defer cancel()

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd := exec.CommandContext(runCtx, job.Command, job.Args...)
	cmd.Dir = job.WorkDir
	cmd.Stdin = bytes.NewReader(nil)
	cmd.Stdout = io.MultiWriter(t.stdout, &stdoutBuf)
	cmd.Stderr = io.MultiWriter(t.stderr, &stderrBuf)

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	exit := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exit = exitErr.ExitCode()
		} else {
			t.record(ctx, journal.LevelError, fmt.Sprintf("job %q failed to start", name), name,
				map[string]any{"command": job.Command, "error": err.Error(), "duration_ms": elapsed.Milliseconds()})
			return nil, fmt.Errorf("job %q: %w", name, err)
		}
	}
	level, summary := journal.LevelInfo, fmt.Sprintf("job %q completed (exit %d)", name, exit)
	if exit != 0 {
		level, summary = journal.LevelError, fmt.Sprintf("job %q exited non-zero (exit %d)", name, exit)
	}
	t.record(ctx, level, summary, name,
		map[string]any{"command": job.Command, "exit_code": exit, "duration_ms": elapsed.Milliseconds()})
	return Result{
		Name:     name,
		Command:  job.Command,
		ExitCode: exit,
		Stdout:   stdoutBuf.String(),
		Stderr:   stderrBuf.String(),
	}, nil
}

// invokeAgent runs an agent-delegated job. It renders the prompt's positional
// placeholders from the runtime args (falling back to the job's configured
// args, so scheduled runs can bake them in) and delegates to the agent in a
// fire-and-forget session bounded by the job's timeout.
func (t *Tool) invokeAgent(ctx context.Context, name string, job config.JobProfile, args map[string]any) (any, error) {
	if t.delegator == nil {
		return nil, fmt.Errorf("job %q delegates to agent %q but agent delegation is unavailable (is ACP enabled?)", name, job.Agent)
	}

	runtimeArgs := stringArgs(args["args"])
	if len(runtimeArgs) == 0 {
		runtimeArgs = job.Args
	}
	prompt := renderPositional(job.Prompt, runtimeArgs)

	runCtx, cancel := context.WithTimeout(ctx, jobTimeout(job))
	defer cancel()

	start := time.Now()
	if err := t.delegator.RunAndForget(runCtx, job.Agent, prompt); err != nil {
		t.record(ctx, journal.LevelError, fmt.Sprintf("agent job %q failed", name), name,
			map[string]any{"agent": job.Agent, "error": err.Error(), "duration_ms": time.Since(start).Milliseconds()})
		return nil, fmt.Errorf("job %q: %w", name, err)
	}
	t.record(ctx, journal.LevelInfo, fmt.Sprintf("agent job %q completed", name), name,
		map[string]any{"agent": job.Agent, "duration_ms": time.Since(start).Milliseconds()})
	return Result{Name: name, Agent: job.Agent}, nil
}

// jobTimeout resolves the job's execution timeout, defaulting to 10 minutes
// when unset or unparseable.
func jobTimeout(job config.JobProfile) time.Duration {
	if job.Timeout != "" {
		if d, err := time.ParseDuration(job.Timeout); err == nil {
			return d
		}
	}
	return defaultTimeout
}

// positionalRef matches a positional prompt placeholder: {{ 1 }}, {{2}}, etc.
var positionalRef = regexp.MustCompile(`\{\{\s*(\d+)\s*\}\}`)

// renderPositional substitutes {{ N }} placeholders (1-based) with the
// corresponding runtime arg. Out-of-range references render as empty strings.
func renderPositional(prompt string, args []string) string {
	return positionalRef.ReplaceAllStringFunc(prompt, func(match string) string {
		n, _ := strconv.Atoi(positionalRef.FindStringSubmatch(match)[1])
		if n >= 1 && n <= len(args) {
			return args[n-1]
		}
		return ""
	})
}

// stringArgs coerces the tool's "args" input — which arrives as []any from the
// MCP/CLI frontends or []string from direct callers — into a string slice.
func stringArgs(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, fmt.Sprint(e))
		}
		return out
	default:
		return nil
	}
}
