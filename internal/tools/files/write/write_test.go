package write

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/tools/files"
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

func TestTool_Name(t *testing.T) {
	tool, _, _ := setup(t)
	if tool.Name() != "files.write" {
		t.Fatalf("Name = %q", tool.Name())
	}
}

func TestInvoke_CreatesNewFileNoReadNeeded(t *testing.T) {
	tool, st, dir := setup(t)
	got, err := tool.Invoke(context.Background(), map[string]any{
		"path": "sub/new.txt", "content": "hi",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	res := got.(Result)
	if !res.Created || res.Bytes != 2 {
		t.Fatalf("res = %+v", res)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sub", "new.txt"))
	if err != nil || string(data) != "hi" {
		t.Fatalf("file content = %q err=%v", data, err)
	}
	// After a write the file is stamped, so an immediate overwrite is allowed.
	if !st.Seen(filepath.Join(dir, "sub", "new.txt")) {
		t.Fatal("write did not stamp the file")
	}
}

func TestInvoke_OverwriteRequiresRead(t *testing.T) {
	tool, st, dir := setup(t)
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Overwriting an existing, never-read file must fail.
	if _, err := tool.Invoke(context.Background(), map[string]any{
		"path": "a.txt", "content": "new",
	}); err == nil {
		t.Fatal("overwrite without read = nil, want error")
	}

	// After stamping (as a read would), overwrite succeeds.
	if err := st.Mark(f); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Invoke(context.Background(), map[string]any{
		"path": "a.txt", "content": "new",
	}); err != nil {
		t.Fatalf("overwrite after read: %v", err)
	}
	data, _ := os.ReadFile(f)
	if string(data) != "new" {
		t.Fatalf("content = %q", data)
	}
}

func TestInvoke_OverwriteDetectsDrift(t *testing.T) {
	tool, st, dir := setup(t)
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.Mark(f); err != nil {
		t.Fatal(err)
	}
	// Drift the file after the stamp.
	if err := os.WriteFile(f, []byte("changed externally"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(f, future, future)

	if _, err := tool.Invoke(context.Background(), map[string]any{
		"path": "a.txt", "content": "new",
	}); err == nil {
		t.Fatal("overwrite after drift = nil, want error")
	}
}

func TestInvoke_Errors(t *testing.T) {
	tool, _, dir := setup(t)
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args map[string]any
	}{
		{"missing path", map[string]any{"content": "x"}},
		{"missing content", map[string]any{"path": "a.txt"}},
		{"traversal", map[string]any{"path": "../x", "content": "x"}},
		{"is directory", map[string]any{"path": "d", "content": "x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tool.Invoke(context.Background(), tc.args); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}
