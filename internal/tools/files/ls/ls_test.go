package ls

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/tools/files"
)

func setup(t *testing.T) (*Tool, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := files.NewRoot(dir)
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	return New(root), dir
}

func mk(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTool_Name(t *testing.T) {
	tool, _ := setup(t)
	if tool.Name() != "files.ls" {
		t.Fatalf("Name = %q", tool.Name())
	}
}

func TestInvoke_ListsDirectory(t *testing.T) {
	tool, dir := setup(t)
	mk(t, filepath.Join(dir, "a.txt"))
	mk(t, filepath.Join(dir, "b.txt"))
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := tool.Invoke(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	res := got.(Result)
	if len(res.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(res.Entries))
	}
	// Directories sort first.
	if !res.Entries[0].IsDir || res.Entries[0].Name != "sub" {
		t.Fatalf("first entry = %+v, want sub dir first", res.Entries[0])
	}
}

func TestInvoke_ListSubdir(t *testing.T) {
	tool, dir := setup(t)
	mk(t, filepath.Join(dir, "sub", "x.txt"))

	got, err := tool.Invoke(context.Background(), map[string]any{"path": "sub"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	res := got.(Result)
	if len(res.Entries) != 1 || res.Entries[0].Path != "sub/x.txt" {
		t.Fatalf("entries = %+v", res.Entries)
	}
}

func TestInvoke_Glob(t *testing.T) {
	tool, dir := setup(t)
	mk(t, filepath.Join(dir, "a.go"))
	mk(t, filepath.Join(dir, "b.go"))
	mk(t, filepath.Join(dir, "c.txt"))

	got, err := tool.Invoke(context.Background(), map[string]any{"glob": "*.go"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	res := got.(Result)
	if len(res.Entries) != 2 {
		t.Fatalf("glob entries = %d, want 2", len(res.Entries))
	}
	for _, e := range res.Entries {
		if filepath.Ext(e.Path) != ".go" {
			t.Fatalf("unexpected glob match %q", e.Path)
		}
	}
}

func TestInvoke_Errors(t *testing.T) {
	tool, dir := setup(t)
	mk(t, filepath.Join(dir, "a.txt"))
	tests := []struct {
		name string
		args map[string]any
	}{
		{"traversal", map[string]any{"path": "../"}},
		{"glob traversal", map[string]any{"glob": "../*"}},
		{"not a dir", map[string]any{"path": "a.txt"}},
		{"missing dir", map[string]any{"path": "nope"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tool.Invoke(context.Background(), tc.args); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}
