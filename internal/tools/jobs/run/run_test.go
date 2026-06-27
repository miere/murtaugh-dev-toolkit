package run

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

func lookupFrom(jobs map[string]config.JobProfile) JobLookup {
	return func(name string) (config.JobProfile, bool) {
		j, ok := jobs[name]
		return j, ok
	}
}

func TestTool_Metadata(t *testing.T) {
	tl := New(lookupFrom(nil))
	if tl.Name() != "jobs.run" {
		t.Fatalf("Name() = %q, want jobs.run", tl.Name())
	}
	schema := tl.InputSchema()
	if schema == nil || schema.Type != "object" {
		t.Fatalf("InputSchema = %+v, want object", schema)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "name" {
		t.Fatalf("required = %v, want [name]", schema.Required)
	}
}

func TestInvoke_MissingName(t *testing.T) {
	tl := New(lookupFrom(nil))
	if _, err := tl.Invoke(context.Background(), map[string]any{}); err == nil {
		t.Fatal("Invoke returned nil, want error for missing name")
	}
}

func TestInvoke_UnknownJob(t *testing.T) {
	tl := New(lookupFrom(map[string]config.JobProfile{}))
	_, err := tl.Invoke(context.Background(), map[string]any{"name": "missing"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Invoke err = %v, want 'not found'", err)
	}
}

func TestInvoke_RunsCommand_CapturesStdout(t *testing.T) {
	jobs := map[string]config.JobProfile{
		"hello": {Command: "/bin/echo", Args: []string{"hello", "world"}},
	}
	var stdout, stderr bytes.Buffer
	tl := NewWith(lookupFrom(jobs), &stdout, &stderr)

	res, err := tl.Invoke(context.Background(), map[string]any{"name": "hello"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r, ok := res.(Result)
	if !ok {
		t.Fatalf("Invoke returned %T, want Result", res)
	}
	if r.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", r.ExitCode)
	}
	if !strings.Contains(r.Stdout, "hello world") {
		t.Fatalf("Result.Stdout = %q, want it to contain 'hello world'", r.Stdout)
	}
	if !strings.Contains(stdout.String(), "hello world") {
		t.Fatalf("captured stdout = %q, want it to contain 'hello world'", stdout.String())
	}
}

func TestInvoke_NonZeroExit_ReturnsResult(t *testing.T) {
	jobs := map[string]config.JobProfile{
		"fail": {Command: "/bin/sh", Args: []string{"-c", "exit 3"}},
	}
	var stdout, stderr bytes.Buffer
	tl := NewWith(lookupFrom(jobs), &stdout, &stderr)

	res, err := tl.Invoke(context.Background(), map[string]any{"name": "fail"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if r.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", r.ExitCode)
	}
}

func TestResult_String(t *testing.T) {
	r := Result{Name: "demo", ExitCode: 0}
	got := r.String()
	if !strings.Contains(got, "demo") || !strings.Contains(got, "exit 0") {
		t.Fatalf("String() = %q, want it to mention demo and exit 0", got)
	}
}

func TestResult_String_Agent(t *testing.T) {
	r := Result{Name: "review", Agent: "default"}
	got := r.String()
	if !strings.Contains(got, "review") || !strings.Contains(got, "default") {
		t.Fatalf("String() = %q, want it to mention review and default", got)
	}
}

type fakeDelegator struct {
	agent  string
	prompt string
	err    error
	calls  int
}

func (f *fakeDelegator) RunAndForget(_ context.Context, agent, prompt string) error {
	f.calls++
	f.agent = agent
	f.prompt = prompt
	return f.err
}

func TestInvoke_AgentJob_RendersPositionalArgs(t *testing.T) {
	jobs := map[string]config.JobProfile{
		"review": {Agent: "default", Prompt: "Review PR {{ 1 }} in {{2}}"},
	}
	del := &fakeDelegator{}
	tl := New(lookupFrom(jobs)).WithDelegator(del)

	res, err := tl.Invoke(context.Background(), map[string]any{
		"name": "review",
		"args": []any{"42", "/repo"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if del.calls != 1 {
		t.Fatalf("delegator called %d times, want 1", del.calls)
	}
	if del.agent != "default" {
		t.Fatalf("agent = %q, want default", del.agent)
	}
	if del.prompt != "Review PR 42 in /repo" {
		t.Fatalf("prompt = %q, want positional substitution", del.prompt)
	}
	r := res.(Result)
	if r.Agent != "default" {
		t.Fatalf("Result.Agent = %q, want default", r.Agent)
	}
}

func TestInvoke_AgentJob_FallsBackToConfiguredArgs(t *testing.T) {
	jobs := map[string]config.JobProfile{
		"review": {Agent: "default", Prompt: "PR {{ 1 }}", Args: []string{"99"}},
	}
	del := &fakeDelegator{}
	tl := New(lookupFrom(jobs)).WithDelegator(del)

	// No runtime args supplied: the job's configured args should fill in.
	if _, err := tl.Invoke(context.Background(), map[string]any{"name": "review"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if del.prompt != "PR 99" {
		t.Fatalf("prompt = %q, want configured-arg fallback 'PR 99'", del.prompt)
	}
}

func TestInvoke_AgentJob_NoDelegator(t *testing.T) {
	jobs := map[string]config.JobProfile{
		"review": {Agent: "default", Prompt: "hi"},
	}
	tl := New(lookupFrom(jobs)) // no delegator wired

	_, err := tl.Invoke(context.Background(), map[string]any{"name": "review"})
	if err == nil || !strings.Contains(err.Error(), "agent delegation is unavailable") {
		t.Fatalf("Invoke err = %v, want unavailable-delegation error", err)
	}
}

func TestRenderPositional(t *testing.T) {
	got := renderPositional("a {{1}} b {{ 2 }} c {{3}}", []string{"X", "Y"})
	if got != "a X b Y c " {
		t.Fatalf("renderPositional = %q, want out-of-range to render empty", got)
	}
}
