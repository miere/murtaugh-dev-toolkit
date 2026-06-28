package agentbuild

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

func acpProfile(workdir string, tools ...string) config.AgentProfile {
	return config.AgentProfile{
		WorkDir: workdir,
		Tools:   tools,
		ACP:     &config.ACPProfile{Command: "/bin/true"},
	}
}

func problemGroups(r ResolvedAgent) []string {
	var out []string
	for _, p := range r.Problems() {
		out = append(out, p.Group)
	}
	return out
}

// TestResolve_WorkdirSet: an explicit workdir is rooted, no problems.
func TestResolve_WorkdirSet(t *testing.T) {
	dir := t.TempDir()
	r, err := Resolve("a", acpProfile(dir, "attach", "slack"), "/some/workspace")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Root() == nil || r.Dir() != filepath.Clean(dir) {
		t.Fatalf("expected root at %q, got %q (root=%v)", dir, r.Dir(), r.Root())
	}
	if len(r.Problems()) != 0 {
		t.Fatalf("expected no problems, got %v", r.Problems())
	}
	if !slices.Equal(r.Tools(), []string{"attach", "slack"}) {
		t.Fatalf("tools should pass through unchanged, got %v", r.Tools())
	}
}

// TestResolve_FallsBackToWorkspace: no workdir → the workspace dir roots it.
func TestResolve_FallsBackToWorkspace(t *testing.T) {
	ws := t.TempDir()
	r, err := Resolve("a", acpProfile("", "attach"), ws)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Dir() != filepath.Clean(ws) {
		t.Fatalf("expected fallback to workspace %q, got %q", ws, r.Dir())
	}
	if len(r.Problems()) != 0 {
		t.Fatalf("expected no problems, got %v", r.Problems())
	}
}

// TestResolve_NoWorkspaceNoRootedTools: an agent without a workspace that needs
// no workdir-rooted tool builds cleanly (nil root, no problems, tools intact).
func TestResolve_NoWorkspaceNoRootedTools(t *testing.T) {
	r, err := Resolve("a", acpProfile("", "slack", "ask"), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Root() != nil {
		t.Fatalf("expected nil root, got %v", r.Root())
	}
	if len(r.Problems()) != 0 {
		t.Fatalf("expected no problems, got %v", r.Problems())
	}
	if !slices.Equal(r.Tools(), []string{"slack", "ask"}) {
		t.Fatalf("tools should pass through, got %v", r.Tools())
	}
}

// TestResolve_DropsRootedToolsWithoutWorkspace is the decided degrade behaviour:
// each workdir-rooted group is dropped with a Problem naming agent+group+reason,
// the agent still resolves, and the non-rooted tools survive.
func TestResolve_DropsRootedToolsWithoutWorkspace(t *testing.T) {
	r, err := Resolve("coder", acpProfile("", "attach", "files", "terminal", "slack"), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Root() != nil {
		t.Fatalf("expected nil root, got %v", r.Root())
	}
	gotDropped := problemGroups(r)
	slices.Sort(gotDropped)
	if !slices.Equal(gotDropped, []string{"attach", "files", "terminal"}) {
		t.Fatalf("expected attach/files/terminal dropped, got %v", gotDropped)
	}
	for _, p := range r.Problems() {
		if p.Agent != "coder" || p.Reason == "" {
			t.Fatalf("problem must name the agent and a reason, got %+v", p)
		}
	}
	if !slices.Equal(r.Tools(), []string{"slack"}) {
		t.Fatalf("only the non-rooted tool should survive, got %v", r.Tools())
	}
}

// TestRegression_ACPAttachNoWorkdirStillBuilds is the original bug: a coder ACP
// agent listing `attach` with no resolvable workdir must build and answer (NOT be
// disabled). attach is dropped with a recorded problem; the rest survives.
func TestRegression_ACPAttachNoWorkdirStillBuilds(t *testing.T) {
	r, err := Resolve("coder", acpProfile("", "skills", "slack", "ask", "present_plan", "attach"), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if groups := problemGroups(r); !slices.Equal(groups, []string{"attach"}) {
		t.Fatalf("expected exactly attach dropped, got %v", groups)
	}
	if slices.Contains(r.Tools(), "attach") {
		t.Fatalf("attach must be pruned from the effective allowlist, got %v", r.Tools())
	}
	// The agent still builds a client (no error, not disabled). No Bridge here, so
	// the ACP path builds the ProcessClient without an aggregator.
	client, err := Client(r, Deps{})
	if err != nil {
		t.Fatalf("Client must build the degraded agent, got error: %v", err)
	}
	if client == nil {
		t.Fatal("expected a non-nil client")
	}
}
