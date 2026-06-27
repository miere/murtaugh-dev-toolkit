package skills_test

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/tools/skills"
)

// TestMurtaughSlack_NoManageLeakToChatAgent is the maintainability guard from
// the design: a typical chat agent (slack/ask, no `manage`) must never receive
// the operator sections of the merged murtaugh-slack skill — not in the rendered
// body, not in the file inventory, and not via a direct file fetch. It runs
// against the real embedded skills FS.
func TestMurtaughSlack_NoManageLeakToChatAgent(t *testing.T) {
	managed := assets.Skills()
	chat := skills.New(managed, nil, "slack", "ask", "present_plan", "files", "terminal", "skills")

	got, err := chat.Invoke(context.Background(), map[string]any{"name": "murtaugh-slack"})
	if err != nil {
		t.Fatalf("read murtaugh-slack: %v", err)
	}
	res := got.(skills.ReadResult)

	for _, leaked := range []string{"workflow-rules.md", "unfurl.md", "automations.md"} {
		if strings.Contains(res.Content, leaked) {
			t.Errorf("manage section %q leaked into a chat agent's body:\n%s", leaked, res.Content)
		}
	}
	for _, want := range []string{"messaging.md", "asking.md", "blocks.md"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("runtime row %q missing from body:\n%s", want, res.Content)
		}
	}
	for _, f := range res.Files {
		if strings.Contains(f, "workflow-rules") || strings.Contains(f, "unfurl") || strings.Contains(f, "automations") {
			t.Errorf("manage file %q leaked into inventory: %v", f, res.Files)
		}
	}
	if _, err := chat.Invoke(context.Background(), map[string]any{"name": "murtaugh-slack", "file": "reference/workflow-rules.md"}); err == nil {
		t.Error("chat agent fetched reference/workflow-rules.md — gate bypassed")
	}

	// A manage agent, by contrast, sees the operator sections.
	op := skills.New(managed, nil, "slack", "manage")
	gotOp, err := op.Invoke(context.Background(), map[string]any{"name": "murtaugh-slack"})
	if err != nil {
		t.Fatalf("read as manage: %v", err)
	}
	if !strings.Contains(gotOp.(skills.ReadResult).Content, "workflow-rules.md") {
		t.Errorf("manage agent should see the workflow-rules row:\n%s", gotOp.(skills.ReadResult).Content)
	}
	if _, err := op.Invoke(context.Background(), map[string]any{"name": "murtaugh-slack", "file": "reference/unfurl.md"}); err != nil {
		t.Errorf("manage agent could not read reference/unfurl.md: %v", err)
	}
}

// TestAllShippedSkills_FrontmatterParses sanity-checks that every bundled skill's
// frontmatter is well-formed enough to list (names + descriptions resolve), read
// straight from the embedded FS.
func TestAllShippedSkills_FrontmatterParses(t *testing.T) {
	all := skills.New(assets.Skills(), nil,
		"slack", "ask", "present_plan", "jobs", "journal", "setup", "restart", "manage", "files", "terminal", "skills")
	got, err := all.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	res := got.(skills.ListResult)
	if len(res.Skills) < 7 {
		t.Errorf("expected the 7 shipped skills, got %d: %+v", len(res.Skills), res.Skills)
	}
	for _, s := range res.Skills {
		if s.Name == "" || s.Description == "" {
			t.Errorf("skill with empty name/description: %+v", s)
		}
	}
}
