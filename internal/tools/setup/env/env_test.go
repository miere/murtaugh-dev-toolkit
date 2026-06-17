package env

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInvoke_WritesKeysNeverEchoesValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	tool := New(func() string { return path })

	res, err := tool.Invoke(context.Background(), map[string]any{
		"set": []string{"GEMINI_API_KEY=supersecretvalue", "FOO=bar"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if !r.Created {
		t.Error("expected Created for a new file")
	}
	// The CLI/result rendering must never contain the secret value.
	if strings.Contains(r.String(), "supersecretvalue") {
		t.Errorf("result string leaked a secret: %q", r.String())
	}
	if got := strings.Join(r.Keys, ","); got != "FOO,GEMINI_API_KEY" {
		t.Errorf("keys = %q, want sorted FOO,GEMINI_API_KEY", got)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "GEMINI_API_KEY=supersecretvalue") {
		t.Errorf("value not written to .env:\n%s", data)
	}
}

func TestInvoke_RejectsBadPairs(t *testing.T) {
	tool := New(func() string { return filepath.Join(t.TempDir(), ".env") })
	for _, bad := range []string{"noequals", "=novalue"} {
		if _, err := tool.Invoke(context.Background(), map[string]any{"set": []string{bad}}); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
	if _, err := tool.Invoke(context.Background(), map[string]any{"set": []string{}}); err == nil {
		t.Error("expected error for empty set")
	}
}
