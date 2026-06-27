package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// TestResolveSystemPrompt_Precedence covers the four-level fallback:
// inline > system_prompt_file > seeded <baseDir>/system-prompt.md > embedded.
func TestResolveSystemPrompt_Precedence(t *testing.T) {
	base := t.TempDir()

	// 4. Nothing configured + no seeded file ⇒ embedded floor (always a base).
	got, err := resolveSystemPrompt(config.AgentProfile{}, base)
	if err != nil {
		t.Fatalf("resolveSystemPrompt: %v", err)
	}
	if !strings.Contains(got, "Murtaugh agent") {
		t.Fatalf("expected the embedded default as the floor, got %q", got)
	}

	// 3. A seeded system-prompt.md overrides the embedded floor.
	if err := os.WriteFile(filepath.Join(base, config.DefaultSystemPromptFile), []byte("SEEDED BASE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := resolveSystemPrompt(config.AgentProfile{}, base); got != "SEEDED BASE" {
		t.Fatalf("seeded default not used: %q", got)
	}

	// 2. An explicit system_prompt_file overrides the seeded default.
	if err := os.WriteFile(filepath.Join(base, "custom.md"), []byte("CUSTOM FILE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := resolveSystemPrompt(config.AgentProfile{Native: &config.NativeProfile{SystemPromptFile: "custom.md"}}, base); got != "CUSTOM FILE" {
		t.Fatalf("system_prompt_file not used: %q", got)
	}

	// 1. An inline system_prompt wins over everything.
	if got, _ := resolveSystemPrompt(config.AgentProfile{Native: &config.NativeProfile{SystemPrompt: "INLINE", SystemPromptFile: "custom.md"}}, base); got != "INLINE" {
		t.Fatalf("inline system_prompt not preferred: %q", got)
	}
}
