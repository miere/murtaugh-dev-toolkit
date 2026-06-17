package toolset

import (
	"context"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

// fakeTool is a minimal tools.Tool for registry-selection tests.
type fakeTool struct{ name string }

func (f fakeTool) Name() string                    { return f.name }
func (f fakeTool) Description() string             { return f.name + " desc" }
func (f fakeTool) InputSchema() *jsonschema.Schema { return nil }
func (f fakeTool) Invoke(context.Context, map[string]any) (any, error) {
	return "ok", nil
}

func names(ts []tools.Tool) map[string]bool {
	m := make(map[string]bool, len(ts))
	for _, t := range ts {
		m[t.Name()] = true
	}
	return m
}

func newRegistry(toolNames ...string) *tools.Registry {
	reg := tools.NewRegistry()
	for _, n := range toolNames {
		reg.Register(fakeTool{name: n})
	}
	return reg
}

func TestResolve_RegistrySelectionByNameAndNamespace(t *testing.T) {
	reg := newRegistry("ping", "slack.send-msg", "slack.fetch-msgs", "jobs.run", "restart")
	got, err := Resolve([]string{"ping", "slack"}, nil, Deps{Registry: reg})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	have := names(got)
	for _, want := range []string{"ping", "slack.send-msg", "slack.fetch-msgs"} {
		if !have[want] {
			t.Errorf("expected %q in toolset, got %v", want, have)
		}
	}
	if have["jobs.run"] || have["restart"] {
		t.Errorf("allowlist leaked unlisted tools: %v", have)
	}
}

func TestResolve_NativeGroups(t *testing.T) {
	dir := t.TempDir()
	got, err := Resolve([]string{"files", "terminal", "skills"}, nil, Deps{WorkDir: dir, SkillsDir: dir})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	have := names(got)
	// 4 file tools + terminal + skills = 6
	if len(got) != 6 {
		t.Fatalf("expected 6 native tools, got %d: %v", len(got), have)
	}
}

func TestResolve_MissingWorkdirForFiles(t *testing.T) {
	if _, err := Resolve([]string{"files"}, nil, Deps{}); err == nil {
		t.Fatal("expected error when files group requested without a workdir")
	}
	if _, err := Resolve([]string{"terminal"}, nil, Deps{}); err == nil {
		t.Fatal("expected error when terminal requested without a workdir")
	}
	if _, err := Resolve([]string{"skills"}, nil, Deps{}); err == nil {
		t.Fatal("expected error when skills requested without a skills_dir")
	}
}

func TestResolve_IncludesMCPToolsAndDedupes(t *testing.T) {
	reg := newRegistry("ping")
	mcp := []tools.Tool{fakeTool{name: "vaultre__get_contact"}, fakeTool{name: "ping"}}
	got, err := Resolve([]string{"ping"}, mcp, Deps{Registry: reg})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	have := names(got)
	if !have["vaultre__get_contact"] {
		t.Errorf("expected MCP tool included, got %v", have)
	}
	// "ping" appears in both registry and MCP; must be deduped to one entry.
	count := 0
	for _, tl := range got {
		if tl.Name() == "ping" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected ping deduped to 1, got %d", count)
	}
}
