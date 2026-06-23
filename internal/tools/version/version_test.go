package version

import (
	"context"
	"testing"
)

func TestTool_Name(t *testing.T) {
	if got := New("v1.2.3").Name(); got != "version" {
		t.Fatalf("Name() = %q, want %q", got, "version")
	}
}

func TestTool_InputSchema_IsNil(t *testing.T) {
	if New("v1.2.3").InputSchema() != nil {
		t.Fatal("InputSchema() = non-nil, want nil")
	}
}

func TestTool_Invoke_ReturnsConfiguredVersion(t *testing.T) {
	const want = "v9.9.9"
	got, err := New(want).Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	res, ok := got.(Result)
	if !ok {
		t.Fatalf("Invoke returned %T, want Result", got)
	}
	if res.Version != want {
		t.Fatalf("Result.Version = %q, want %q", res.Version, want)
	}
	if res.String() != want {
		t.Fatalf("Result.String() = %q, want %q", res.String(), want)
	}
}
