package files

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewRoot_RequiresDir(t *testing.T) {
	if _, err := NewRoot(""); err == nil {
		t.Fatal("NewRoot(\"\") = nil error, want error")
	}
}

func TestRoot_Resolve(t *testing.T) {
	dir := t.TempDir()
	root, err := NewRoot(dir)
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"simple relative", "a.txt", false},
		{"nested relative", "sub/b.txt", false},
		{"dot", ".", false},
		{"absolute inside root", filepath.Join(dir, "c.txt"), false},
		{"empty", "", true},
		{"parent traversal", "../escape.txt", true},
		{"deep traversal", "sub/../../escape.txt", true},
		{"absolute outside root", "/etc/passwd", true},
		{"prefix sibling not inside", dir + "-sibling/x", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := root.Resolve(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Resolve(%q) = %q, want error", tc.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q) error: %v", tc.path, err)
			}
			if !filepath.IsAbs(got) {
				t.Fatalf("Resolve(%q) = %q, want absolute", tc.path, got)
			}
		})
	}
}

func TestRoot_Rel(t *testing.T) {
	dir := t.TempDir()
	root, _ := NewRoot(dir)
	abs := filepath.Join(dir, "sub", "x.txt")
	if got := root.Rel(abs); got != "sub/x.txt" {
		t.Fatalf("Rel = %q, want %q", got, "sub/x.txt")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestReadState_VerifyRequiresRead(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	writeFile(t, f, "hello")

	st := NewReadState()
	if err := st.Verify(f); err == nil {
		t.Fatal("Verify before Mark = nil, want error (read-before-write)")
	}
	if st.Seen(f) {
		t.Fatal("Seen = true before Mark")
	}

	if err := st.Mark(f); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if !st.Seen(f) {
		t.Fatal("Seen = false after Mark")
	}
	if err := st.Verify(f); err != nil {
		t.Fatalf("Verify after Mark: %v", err)
	}
}

func TestReadState_VerifyDetectsDrift(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	writeFile(t, f, "hello")

	st := NewReadState()
	if err := st.Mark(f); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	// Change content and bump modtime so drift is unambiguous on every FS.
	writeFile(t, f, "hello world changed")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(f, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if err := st.Verify(f); err == nil {
		t.Fatal("Verify after drift = nil, want error")
	}
}

func TestReadState_VerifyVanishedFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	writeFile(t, f, "hello")

	st := NewReadState()
	if err := st.Mark(f); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if err := os.Remove(f); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := st.Verify(f); err == nil {
		t.Fatal("Verify after delete = nil, want error")
	}
}

func TestReadState_MarkMissingFile(t *testing.T) {
	st := NewReadState()
	if err := st.Mark(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Fatal("Mark missing file = nil, want error")
	}
}
