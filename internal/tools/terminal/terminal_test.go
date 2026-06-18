package terminal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTool_Name_And_Schema(t *testing.T) {
	tool := New(t.TempDir())
	if got := tool.Name(); got != "terminal" {
		t.Fatalf("Name() = %q, want %q", got, "terminal")
	}
	schema := tool.InputSchema()
	if schema == nil {
		t.Fatal("InputSchema() = nil, want schema")
	}
	if _, ok := schema.Properties["command"]; !ok {
		t.Fatal("schema missing command property")
	}
	if len(schema.Required) != 1 || schema.Required[0] != "command" {
		t.Fatalf("Required = %v, want [command]", schema.Required)
	}
}

func invoke(t *testing.T, tool *Tool, args map[string]any) Result {
	t.Helper()
	got, err := tool.Invoke(context.Background(), args)
	if err != nil {
		t.Fatalf("Invoke error: %v", err)
	}
	res, ok := got.(Result)
	if !ok {
		t.Fatalf("Invoke returned %T, want Result", got)
	}
	return res
}

func TestInvoke_Success(t *testing.T) {
	tool := New(t.TempDir())
	res := invoke(t, tool, map[string]any{"command": "printf 'hello'"})
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.TimedOut {
		t.Fatal("TimedOut = true, want false")
	}
	if res.Output != "hello" {
		t.Fatalf("Output = %q, want %q", res.Output, "hello")
	}
}

func TestInvoke_CombinedStdoutStderr(t *testing.T) {
	tool := New(t.TempDir())
	res := invoke(t, tool, map[string]any{"command": "printf out; printf err 1>&2"})
	if !strings.Contains(res.Output, "out") || !strings.Contains(res.Output, "err") {
		t.Fatalf("Output = %q, want both stdout and stderr", res.Output)
	}
}

func TestInvoke_NonZeroExit(t *testing.T) {
	tool := New(t.TempDir())
	res := invoke(t, tool, map[string]any{"command": "echo boom; exit 7"})
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", res.ExitCode)
	}
	if res.TimedOut {
		t.Fatal("TimedOut = true, want false")
	}
	if !strings.Contains(res.Output, "boom") {
		t.Fatalf("Output = %q, want to contain boom", res.Output)
	}
}

func TestInvoke_Timeout(t *testing.T) {
	tool := New(t.TempDir())
	start := time.Now()
	res := invoke(t, tool, map[string]any{"command": "sleep 5", "timeout": "200ms"})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Invoke took %v, expected to be killed near the timeout", elapsed)
	}
	if !res.TimedOut {
		t.Fatal("TimedOut = false, want true")
	}
	if res.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1 on timeout", res.ExitCode)
	}
}

func TestInvoke_TimeoutKillsBackgroundedChild(t *testing.T) {
	tool := New(t.TempDir())
	start := time.Now()
	// The shell backgrounds a long sleep and exits immediately, but the child
	// inherits and holds the output pipe. Without killing the whole process group
	// (and the WaitDelay backstop), cmd.Run would block until the child finishes,
	// defeating the timeout. This reproduces the Linux grandchild case on every
	// platform.
	res := invoke(t, tool, map[string]any{"command": "sleep 30 &", "timeout": "200ms"})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Invoke took %v; the timeout did not reap the backgrounded child", elapsed)
	}
	if !res.TimedOut {
		t.Fatal("TimedOut = false, want true")
	}
}

func TestInvoke_TimeoutCappedAtMax(t *testing.T) {
	tool := New(t.TempDir())
	// A huge requested timeout is capped; the command itself finishes fast so
	// the test stays quick. We assert no error and a clean exit.
	res := invoke(t, tool, map[string]any{"command": "true", "timeout": "60m"})
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestInvoke_OutputTruncation(t *testing.T) {
	tool := New(t.TempDir())
	// Emit well over the 64KB cap.
	res := invoke(t, tool, map[string]any{
		"command": "head -c 200000 /dev/zero | tr '\\0' 'a'",
	})
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.HasSuffix(res.Output, truncationNotice) {
		t.Fatalf("Output missing truncation notice; tail = %q", tail(res.Output, 60))
	}
	body := strings.TrimSuffix(res.Output, truncationNotice)
	if len(body) != MaxOutputBytes {
		t.Fatalf("truncated body len = %d, want %d", len(body), MaxOutputBytes)
	}
}

func TestInvoke_NoTruncationUnderCap(t *testing.T) {
	tool := New(t.TempDir())
	res := invoke(t, tool, map[string]any{"command": "printf small"})
	if strings.Contains(res.Output, "truncated") {
		t.Fatalf("Output unexpectedly marked truncated: %q", res.Output)
	}
}

func TestInvoke_WorkdirSubdir(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "nested")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	tool := New(root)
	res := invoke(t, tool, map[string]any{"command": "pwd", "workdir": "nested"})
	// macOS tempdirs live under /private; resolve symlinks for comparison.
	gotDir := strings.TrimSpace(res.Output)
	wantReal, _ := filepath.EvalSymlinks(sub)
	gotReal, _ := filepath.EvalSymlinks(gotDir)
	if gotReal != wantReal {
		t.Fatalf("pwd = %q, want %q", gotReal, wantReal)
	}
}

func TestInvoke_WorkdirDefaultsToRoot(t *testing.T) {
	root := t.TempDir()
	tool := New(root)
	res := invoke(t, tool, map[string]any{"command": "pwd"})
	wantReal, _ := filepath.EvalSymlinks(root)
	gotReal, _ := filepath.EvalSymlinks(strings.TrimSpace(res.Output))
	if gotReal != wantReal {
		t.Fatalf("pwd = %q, want %q", gotReal, wantReal)
	}
}

func TestInvoke_WorkdirEscapeRejected(t *testing.T) {
	root := t.TempDir()
	tool := New(root)
	cases := []string{"..", "../sibling", "/etc", "nested/../../escape"}
	for _, wd := range cases {
		_, err := tool.Invoke(context.Background(), map[string]any{"command": "pwd", "workdir": wd})
		if err == nil {
			t.Fatalf("workdir %q: expected escape error, got nil", wd)
		}
		if !strings.Contains(err.Error(), "escapes the workspace root") {
			t.Fatalf("workdir %q: error = %v, want escape message", wd, err)
		}
	}
}

func TestInvoke_MissingCommand(t *testing.T) {
	tool := New(t.TempDir())
	for _, cmd := range []any{"", "   ", nil} {
		_, err := tool.Invoke(context.Background(), map[string]any{"command": cmd})
		if err == nil {
			t.Fatalf("command %v: expected error, got nil", cmd)
		}
	}
}

func TestInvoke_InvalidTimeout(t *testing.T) {
	tool := New(t.TempDir())
	for _, to := range []string{"notaduration", "-5s", "0"} {
		_, err := tool.Invoke(context.Background(), map[string]any{"command": "true", "timeout": to})
		if err == nil {
			t.Fatalf("timeout %q: expected error, got nil", to)
		}
	}
}

func TestResult_String(t *testing.T) {
	r := Result{ExitCode: 2, Output: "oops"}
	if got := r.String(); !strings.Contains(got, "exited 2") || !strings.Contains(got, "oops") {
		t.Fatalf("String() = %q", got)
	}
	r = Result{ExitCode: -1, Output: "stuck", TimedOut: true}
	if got := r.String(); !strings.Contains(got, "timed out") {
		t.Fatalf("String() timeout = %q", got)
	}
}

func TestResult_JSONMarshalable(t *testing.T) {
	b, err := json.Marshal(Result{ExitCode: 0, Output: "x", TimedOut: false})
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	var round Result
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if round.Output != "x" {
		t.Fatalf("round-trip Output = %q", round.Output)
	}
	for _, key := range []string{"exit_code", "output", "timed_out"} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("JSON %s missing key %q", b, key)
		}
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
