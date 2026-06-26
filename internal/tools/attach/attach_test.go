package attach

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/files"
)

func newRoot(t *testing.T, dir string) *files.Root {
	t.Helper()
	root, err := files.NewRoot(dir)
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	return root
}

func TestInvoke_ReturnsAttachment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A workspace-relative path resolves under the root.
	res, err := New(newRoot(t, dir)).Invoke(context.Background(), map[string]any{
		"path": "out.txt", "title": "Report", "comment": "see attached",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	a, ok := res.(*agent.AttachmentEvent)
	if !ok {
		t.Fatalf("result type %T, want *agent.AttachmentEvent", res)
	}
	if a.Path != filepath.Join(dir, "out.txt") || a.Filename != "out.txt" || a.Title != "Report" || a.Comment != "see attached" {
		t.Fatalf("attachment = %+v", a)
	}
	if len(a.Data) != 0 {
		t.Fatalf("attach must not buffer bytes; got %d", len(a.Data))
	}
}

func TestInvoke_RejectsPathOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	// A real, non-empty secret outside the workspace.
	secret := filepath.Join(t.TempDir(), "secret.env")
	if err := os.WriteFile(secret, []byte("API_KEY=xyz"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := New(newRoot(t, dir))

	for _, p := range []string{secret, "../" + filepath.Base(secret), "/etc/passwd"} {
		if _, err := tool.Invoke(context.Background(), map[string]any{"path": p}); err == nil {
			t.Fatalf("expected rejection for path escaping root: %q", p)
		}
	}
}

func TestInvoke_Errors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tool := New(newRoot(t, dir))

	cases := map[string]map[string]any{
		"missing path": {},
		"nonexistent":  {"path": "nope"},
		"directory":    {"path": "."},
		"empty file":   {"path": "empty"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := tool.Invoke(context.Background(), args); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestNew_NilRootInvokeErrors(t *testing.T) {
	if _, err := New(nil).Invoke(context.Background(), map[string]any{"path": "x"}); err == nil {
		t.Fatal("expected error when no root is configured")
	}
}
