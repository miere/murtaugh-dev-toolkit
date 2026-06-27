package bundle

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/troubleshoot"
)

func TestTool_Metadata(t *testing.T) {
	tl := New(func() troubleshoot.Sources { return troubleshoot.Sources{} }, nil)
	if tl.Name() != "troubleshoot.bundle" {
		t.Fatalf("Name() = %q", tl.Name())
	}
	if tl.InputSchema().Properties["include"] == nil || tl.InputSchema().Properties["note"] == nil {
		t.Fatalf("schema missing expected properties: %+v", tl.InputSchema().Properties)
	}
}

func TestTool_Invoke_BuildsBundle(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "b.zip")
	tl := New(func() troubleshoot.Sources {
		return troubleshoot.Sources{Version: "test", GOOS: "testos"}
	}, nil)

	got, err := tl.Invoke(context.Background(), map[string]any{
		"note": "silent replies",
		"out":  out,
		// MCP delivers arrays as []any and numbers as float64; exercise both.
		"include":       []any{"goose"},
		"max_log_bytes": float64(2048),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	res, ok := got.(Result)
	if !ok {
		t.Fatalf("Invoke returned %T, want Result", got)
	}
	if res.Path != out || !res.Redacted || res.FileCount == 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.Contains(res.String(), "redacted") {
		t.Errorf("String() should note redaction: %q", res.String())
	}
}

func TestTool_Invoke_RedactFalse(t *testing.T) {
	dir := t.TempDir()
	tl := New(func() troubleshoot.Sources {
		return troubleshoot.Sources{Version: "test", GOOS: "testos"}
	}, nil)
	got, err := tl.Invoke(context.Background(), map[string]any{
		"out":    filepath.Join(dir, "b.zip"),
		"redact": false,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got.(Result).Redacted {
		t.Fatalf("redact=false should disable redaction")
	}
	if !strings.Contains(got.(Result).String(), "UNREDACTED") {
		t.Errorf("String() should flag UNREDACTED: %q", got.(Result).String())
	}
}
