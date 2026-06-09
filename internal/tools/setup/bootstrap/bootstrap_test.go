package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTool_Metadata(t *testing.T) {
	tl := New(func() string { return "" })
	if tl.Name() != "setup.bootstrap" {
		t.Fatalf("Name() = %q, want setup.bootstrap", tl.Name())
	}
	if strings.TrimSpace(tl.Description()) == "" {
		t.Fatal("Description() must not be blank")
	}
	if tl.InputSchema() != nil {
		t.Fatalf("InputSchema() = %v, want nil", tl.InputSchema())
	}
}

func TestInvoke_FreshDirSeedsAndReportsCreated(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "slack.yaml")
	tl := New(func() string { return configPath })

	res, err := tl.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if len(r.Created) == 0 {
		t.Fatalf("Created should be non-empty on fresh dir, got %+v", r)
	}
	if len(r.Preserved) != 0 {
		t.Fatalf("Preserved should be empty on fresh dir, got %+v", r.Preserved)
	}
	for _, p := range r.Created {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("reported created file missing: %s (%v)", p, err)
		}
	}
	// slack.yaml at configPath is mandatory; all bootstrap reports it.
	if !containsPath(r.Created, configPath) {
		t.Fatalf("Created should mention %s, got %+v", configPath, r.Created)
	}
}

func TestInvoke_SecondRunPreservesEverything(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "slack.yaml")
	tl := New(func() string { return configPath })

	if _, err := tl.Invoke(context.Background(), nil); err != nil {
		t.Fatalf("first Invoke: %v", err)
	}
	res, err := tl.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("second Invoke: %v", err)
	}
	r := res.(Result)
	if len(r.Created) != 0 {
		t.Fatalf("Created should be empty on second run, got %+v", r.Created)
	}
	if len(r.Preserved) == 0 {
		t.Fatalf("Preserved should be non-empty on second run, got %+v", r)
	}
	if !containsPath(r.Preserved, configPath) {
		t.Fatalf("Preserved should mention %s, got %+v", configPath, r.Preserved)
	}
}

func TestInvoke_MixedReportsBothBuckets(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "slack.yaml")
	// Pre-seed slack.yaml only; agents/jobs should be created.
	if err := os.WriteFile(configPath, []byte("existing: true\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tl := New(func() string { return configPath })

	res, err := tl.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if !containsPath(r.Preserved, configPath) {
		t.Fatalf("Preserved should mention pre-existing %s, got %+v", configPath, r.Preserved)
	}
	if len(r.Created) == 0 {
		t.Fatalf("Created should mention newly-seeded files, got %+v", r.Created)
	}
}

func TestResult_String_Summarises(t *testing.T) {
	r := Result{Created: []string{"/tmp/a"}, Preserved: []string{"/tmp/b"}}
	got := r.String()
	if !strings.Contains(got, "1 created") || !strings.Contains(got, "1 preserved") {
		t.Fatalf("String() = %q, want it to mention counts", got)
	}
}

func containsPath(haystack []string, want string) bool {
	for _, p := range haystack {
		if p == want {
			return true
		}
	}
	return false
}
