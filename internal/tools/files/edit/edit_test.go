package edit

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/miere/murtaugh/internal/tools/files"
)

func setup(t *testing.T) (*Tool, *files.ReadState, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := files.NewRoot(dir)
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	st := files.NewReadState()
	return New(root, st), st, dir
}

func seed(t *testing.T, st *files.ReadState, dir, name, content string) string {
	t.Helper()
	f := filepath.Join(dir, name)
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.Mark(f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestTool_Name(t *testing.T) {
	tool, _, _ := setup(t)
	if tool.Name() != "files.edit" {
		t.Fatalf("Name = %q", tool.Name())
	}
}

func TestInvoke_UniqueReplacement(t *testing.T) {
	tool, st, dir := setup(t)
	f := seed(t, st, dir, "a.txt", "alpha beta gamma")

	got, err := tool.Invoke(context.Background(), map[string]any{
		"path": "a.txt", "old_string": "beta", "new_string": "BETA",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got.(Result).Replacements != 1 {
		t.Fatalf("replacements = %d", got.(Result).Replacements)
	}
	data, _ := os.ReadFile(f)
	if string(data) != "alpha BETA gamma" {
		t.Fatalf("content = %q", data)
	}
}

func TestInvoke_NonUniqueRequiresReplaceAll(t *testing.T) {
	tool, st, dir := setup(t)
	f := seed(t, st, dir, "a.txt", "x x x")

	if _, err := tool.Invoke(context.Background(), map[string]any{
		"path": "a.txt", "old_string": "x", "new_string": "y",
	}); err == nil {
		t.Fatal("non-unique without replace_all = nil, want error")
	}

	got, err := tool.Invoke(context.Background(), map[string]any{
		"path": "a.txt", "old_string": "x", "new_string": "y", "replace_all": true,
	})
	if err != nil {
		t.Fatalf("replace_all: %v", err)
	}
	if got.(Result).Replacements != 3 {
		t.Fatalf("replacements = %d, want 3", got.(Result).Replacements)
	}
	data, _ := os.ReadFile(f)
	if string(data) != "y y y" {
		t.Fatalf("content = %q", data)
	}
}

func TestInvoke_RequiresRead(t *testing.T) {
	tool, _, dir := setup(t)
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Never marked as read.
	if _, err := tool.Invoke(context.Background(), map[string]any{
		"path": "a.txt", "old_string": "hello", "new_string": "bye",
	}); err == nil {
		t.Fatal("edit without read = nil, want error")
	}
}

func TestInvoke_Errors(t *testing.T) {
	tool, st, dir := setup(t)
	seed(t, st, dir, "a.txt", "hello world")

	tests := []struct {
		name string
		args map[string]any
	}{
		{"missing path", map[string]any{"old_string": "a", "new_string": "b"}},
		{"missing old", map[string]any{"path": "a.txt", "new_string": "b"}},
		{"identical", map[string]any{"path": "a.txt", "old_string": "hello", "new_string": "hello"}},
		{"not found string", map[string]any{"path": "a.txt", "old_string": "zzz", "new_string": "b"}},
		{"traversal", map[string]any{"path": "../x", "old_string": "a", "new_string": "b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tool.Invoke(context.Background(), tc.args); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}
