package toolset

import (
	"context"
	"os"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/miere/murtaugh/internal/tools"
	"github.com/miere/murtaugh/internal/tools/files"
)

// mustRoot builds a *files.Root for tests, failing the test on error.
func mustRoot(t *testing.T, dir string) *files.Root {
	t.Helper()
	r, err := files.NewRoot(dir)
	if err != nil {
		t.Fatalf("files.NewRoot(%q): %v", dir, err)
	}
	return r
}

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
	got, _, err := Resolve([]string{"ping", "slack"}, nil, Deps{Registry: reg})
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
	got, probs, err := Resolve([]string{"files", "terminal", "skills"}, nil, Deps{Root: mustRoot(t, dir), ManagedSkillsFS: os.DirFS(dir)})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(probs) != 0 {
		t.Fatalf("expected no problems with a root + skills FS, got %v", probs)
	}
	have := names(got)
	// 4 file tools + terminal + skills = 6
	if len(got) != 6 {
		t.Fatalf("expected 6 native tools, got %d: %v", len(got), have)
	}
}

func TestResolve_AttachGroup(t *testing.T) {
	dir := t.TempDir()
	got, probs, err := Resolve([]string{"attach"}, nil, Deps{Root: mustRoot(t, dir)})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(probs) != 0 {
		t.Fatalf("expected no problems, got %v", probs)
	}
	if !names(got)["attach"] {
		t.Fatalf("expected attach tool, got %v", names(got))
	}
}

// TestResolve_MissingWorkdirDegrades asserts the decided policy: a workdir-rooted
// group requested without a Root is DROPPED with a Problem (agent stays alive),
// not a fatal error. skills with no managed FS degrades the same way.
func TestResolve_MissingWorkdirDegrades(t *testing.T) {
	for _, group := range []string{"files", "terminal", "attach"} {
		got, probs, err := Resolve([]string{group, "ping"}, nil, Deps{AgentName: "coder", RootReason: "no workdir is set", Registry: newRegistry("ping")})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", group, err)
		}
		if len(probs) != 1 || probs[0].Group != group || probs[0].Agent != "coder" {
			t.Fatalf("%s: expected one Problem naming the group+agent, got %v", group, probs)
		}
		if probs[0].Reason != "no workdir is set" {
			t.Fatalf("%s: expected the supplied RootReason, got %q", group, probs[0].Reason)
		}
		if names(got)[group] {
			t.Fatalf("%s: dropped group must be absent from the toolset, got %v", group, names(got))
		}
		if !names(got)["ping"] {
			t.Fatalf("%s: the rest of the toolset must survive, got %v", group, names(got))
		}
	}
	// skills without a managed FS degrades too.
	_, probs, err := Resolve([]string{"skills"}, nil, Deps{})
	if err != nil {
		t.Fatalf("skills: unexpected error: %v", err)
	}
	if len(probs) != 1 || probs[0].Group != "skills" {
		t.Fatalf("skills: expected one skills Problem, got %v", probs)
	}
}

func TestResolve_IncludesMCPToolsAndDedupes(t *testing.T) {
	reg := newRegistry("ping")
	mcp := []tools.Tool{fakeTool{name: "vaultre__get_contact"}, fakeTool{name: "ping"}}
	got, _, err := Resolve([]string{"ping"}, mcp, Deps{Registry: reg})
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
