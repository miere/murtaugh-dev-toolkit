package envfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestMerge_CreatesFileSorted(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	backup, err := Merge(path, map[string]string{"GEMINI_API_KEY": "g", "SLACK_BOT_TOKEN": "xoxb-1"})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if backup != "" {
		t.Errorf("no backup expected for a new file, got %q", backup)
	}
	got := read(t, path)
	if !strings.Contains(got, "GEMINI_API_KEY=g\n") || !strings.Contains(got, "SLACK_BOT_TOKEN=xoxb-1\n") {
		t.Fatalf("unexpected content:\n%s", got)
	}
	// Sorted append: GEMINI before SLACK.
	if strings.Index(got, "GEMINI_API_KEY") > strings.Index(got, "SLACK_BOT_TOKEN") {
		t.Errorf("expected sorted order, got:\n%s", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("expected 0600 perms, got %v", info.Mode().Perm())
	}
}

func TestMerge_PreservesAndOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	seed := "# my secrets\nGEMINI_API_KEY=old\n\n# slack\nSLACK_APP_TOKEN=xapp-keep\nCUSTOM=leave-me\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	backup, err := Merge(path, map[string]string{"GEMINI_API_KEY": "new", "ANTHROPIC_API_KEY": "a"})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if backup == "" {
		t.Error("expected a backup of the existing file")
	}
	got := read(t, path)
	if !strings.Contains(got, "GEMINI_API_KEY=new\n") {
		t.Errorf("override failed:\n%s", got)
	}
	if strings.Contains(got, "GEMINI_API_KEY=old") {
		t.Errorf("old value still present:\n%s", got)
	}
	// Comments and unrelated keys preserved.
	for _, want := range []string{"# my secrets", "# slack", "SLACK_APP_TOKEN=xapp-keep", "CUSTOM=leave-me"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost preserved line %q:\n%s", want, got)
		}
	}
	// New key appended.
	if !strings.Contains(got, "ANTHROPIC_API_KEY=a\n") {
		t.Errorf("new key not appended:\n%s", got)
	}
	// GEMINI override happened in place (before CUSTOM, where it was seeded).
	if strings.Index(got, "GEMINI_API_KEY=new") > strings.Index(got, "CUSTOM=leave-me") {
		t.Errorf("override moved the key out of place:\n%s", got)
	}
}

func TestMerge_RejectsBadInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if _, err := Merge(path, map[string]string{"": "x"}); err == nil {
		t.Error("expected error for blank key")
	}
	if _, err := Merge(path, map[string]string{"K": "a\nb"}); err == nil {
		t.Error("expected error for newline in value")
	}
}
