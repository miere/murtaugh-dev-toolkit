package read

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/tools/files"
)

func setup(t *testing.T) (*Tool, *files.Root, *files.ReadState, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := files.NewRoot(dir)
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	st := files.NewReadState()
	return New(root, st), root, st, dir
}

func TestTool_Name(t *testing.T) {
	tool, _, _, _ := setup(t)
	if tool.Name() != "files.read" {
		t.Fatalf("Name = %q", tool.Name())
	}
	if tool.InputSchema() == nil {
		t.Fatal("InputSchema = nil")
	}
}

func TestInvoke_ReadsWholeFile(t *testing.T) {
	tool, _, st, dir := setup(t)
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("l1\nl2\nl3"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := tool.Invoke(context.Background(), map[string]any{"path": "a.txt"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	res := got.(Result)
	if res.Content != "l1\nl2\nl3" {
		t.Fatalf("Content = %q", res.Content)
	}
	if res.LineCount != 3 || res.Truncated {
		t.Fatalf("LineCount=%d Truncated=%v", res.LineCount, res.Truncated)
	}
	// Read must stamp the file for write/edit.
	if !st.Seen(f) {
		t.Fatal("read did not stamp the file")
	}
}

func TestInvoke_OffsetLimit(t *testing.T) {
	tool, _, _, dir := setup(t)
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("l1\nl2\nl3\nl4\nl5"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := tool.Invoke(context.Background(), map[string]any{
		"path": "a.txt", "offset": float64(2), "limit": float64(2),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	res := got.(Result)
	if res.Content != "l2\nl3" {
		t.Fatalf("Content = %q, want l2\\nl3", res.Content)
	}
	if res.StartLine != 2 || res.EndLine != 3 {
		t.Fatalf("Start=%d End=%d", res.StartLine, res.EndLine)
	}
	if !res.Truncated || res.LineCount != 5 {
		t.Fatalf("Truncated=%v LineCount=%d", res.Truncated, res.LineCount)
	}
	if !strings.Contains(res.String(), "lines 2-3 of 5") {
		t.Fatalf("String = %q", res.String())
	}
}

func TestInvoke_Errors(t *testing.T) {
	tool, _, _, dir := setup(t)
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args map[string]any
	}{
		{"missing path", map[string]any{}},
		{"traversal", map[string]any{"path": "../x"}},
		{"not found", map[string]any{"path": "nope.txt"}},
		{"directory", map[string]any{"path": "d"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tool.Invoke(context.Background(), tc.args); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}
