package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill creates dir/name/SKILL.md with the given content and returns the
// skill directory.
func writeSkill(t *testing.T, root, name, content string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, skillFile), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return dir
}

const withFrontmatter = `---
name: alpha-skill
description: Does alpha things very well.
---

# Skill: Alpha

Body text here.
`

const noFrontmatter = `# Beta Heading

Beta paragraph that should not win over the heading.
`

const onlyParagraph = `Just a leading paragraph, no heading and no frontmatter.

Second paragraph.
`

func TestInvoke_List(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "alpha", withFrontmatter)
	writeSkill(t, root, "beta", noFrontmatter)
	writeSkill(t, root, "gamma", onlyParagraph)
	// A non-skill directory (no SKILL.md) must be ignored.
	if err := os.MkdirAll(filepath.Join(root, "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A loose file must be ignored.
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := New(os.DirFS(root), nil).Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke list: %v", err)
	}
	res, ok := got.(ListResult)
	if !ok {
		t.Fatalf("Invoke returned %T, want ListResult", got)
	}
	if len(res.Skills) != 3 {
		t.Fatalf("got %d skills, want 3: %+v", len(res.Skills), res.Skills)
	}

	// Sorted by name; frontmatter name overrides dir name.
	want := []SkillSummary{
		{Name: "alpha-skill", Description: "Does alpha things very well."},
		{Name: "beta", Description: "Beta Heading"},
		{Name: "gamma", Description: "Just a leading paragraph, no heading and no frontmatter."},
	}
	for i, w := range want {
		if res.Skills[i] != w {
			t.Errorf("skill[%d] = %+v, want %+v", i, res.Skills[i], w)
		}
	}
}

func TestInvoke_ReadWithFrontmatter(t *testing.T) {
	root := t.TempDir()
	dir := writeSkill(t, root, "alpha", withFrontmatter)
	// Add reference/ and examples/ files.
	mustWrite(t, filepath.Join(dir, "reference", "a.md"), "a")
	mustWrite(t, filepath.Join(dir, "reference", "sub", "b.md"), "b")
	mustWrite(t, filepath.Join(dir, "examples", "ex.yaml"), "y")

	got, err := New(os.DirFS(root), nil).Invoke(context.Background(), map[string]any{"name": "alpha"})
	if err != nil {
		t.Fatalf("Invoke read: %v", err)
	}
	res, ok := got.(ReadResult)
	if !ok {
		t.Fatalf("Invoke returned %T, want ReadResult", got)
	}
	if res.Name != "alpha-skill" {
		t.Errorf("Name = %q, want alpha-skill", res.Name)
	}
	if res.Description != "Does alpha things very well." {
		t.Errorf("Description = %q", res.Description)
	}
	if res.Content != withFrontmatter {
		t.Errorf("Content not returned verbatim:\n%q", res.Content)
	}
	wantFiles := []string{"examples/ex.yaml", "reference/a.md", "reference/sub/b.md"}
	if len(res.Files) != len(wantFiles) {
		t.Fatalf("Files = %v, want %v", res.Files, wantFiles)
	}
	for i, w := range wantFiles {
		if res.Files[i] != w {
			t.Errorf("Files[%d] = %q, want %q", i, res.Files[i], w)
		}
	}
}

func TestInvoke_ReadNoFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "beta", noFrontmatter)

	got, err := New(os.DirFS(root), nil).Invoke(context.Background(), map[string]any{"name": "beta"})
	if err != nil {
		t.Fatalf("Invoke read: %v", err)
	}
	res := got.(ReadResult)
	// No frontmatter ⇒ display name falls back to dir name, description to heading.
	if res.Name != "beta" {
		t.Errorf("Name = %q, want beta", res.Name)
	}
	if res.Description != "Beta Heading" {
		t.Errorf("Description = %q, want Beta Heading", res.Description)
	}
	if len(res.Files) != 0 {
		t.Errorf("Files = %v, want empty", res.Files)
	}
}

func TestInvoke_ReadErrors(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "alpha", withFrontmatter)
	// Create a sibling outside the skills dir to make traversal meaningful.
	outside := filepath.Join(filepath.Dir(root), "outside-secret")
	mustWrite(t, filepath.Join(outside, "SKILL.md"), "secret")
	t.Cleanup(func() { os.RemoveAll(outside) })

	tool := New(os.DirFS(root), nil)
	tests := []struct {
		name string
		arg  string
	}{
		{"missing", "does-not-exist"},
		{"parent traversal", ".."},
		{"nested traversal", "../outside-secret"},
		{"absolute path", "/etc"},
		{"dot", "."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tool.Invoke(context.Background(), map[string]any{"name": tc.arg}); err == nil {
				t.Fatalf("Invoke(%q) = nil error, want error", tc.arg)
			}
		})
	}
}

func TestInvoke_ListMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := New(os.DirFS(missing), nil).Invoke(context.Background(), nil); err == nil {
		t.Fatal("Invoke list on missing dir = nil error, want error")
	}
}

func TestStaticContract(t *testing.T) {
	tool := New(os.DirFS(t.TempDir()), nil)
	if tool.Name() != "skills" {
		t.Errorf("Name() = %q, want skills", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("Description() is empty")
	}
	sch := tool.InputSchema()
	if sch == nil {
		t.Fatal("InputSchema() = nil")
	}
	if _, ok := sch.Properties["name"]; !ok {
		t.Error("InputSchema missing 'name' property")
	}
	if len(sch.Required) != 0 {
		t.Errorf("Required = %v, want none (name is optional)", sch.Required)
	}
}

func TestResultStrings(t *testing.T) {
	list := ListResult{Skills: []SkillSummary{{Name: "a", Description: "does a"}, {Name: "b"}}}
	if list.String() == "" {
		t.Error("ListResult.String() empty")
	}
	if (ListResult{}).String() != "No skills found." {
		t.Errorf("empty ListResult.String() = %q", (ListResult{}).String())
	}
	read := ReadResult{Name: "a", Description: "d", Content: "# body", Files: []string{"reference/x.md"}}
	if read.String() == "" {
		t.Error("ReadResult.String() empty")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gatedSkill is a multi-audience skill: top-level requires is the union, and it
// is templated with a {{FILES}} marker plus a per-file manifest.
const gatedSkill = `---
name: gated
description: A gated, templated skill.
requires: [slack, manage]
templated: true
files:
  reference/messaging.md:      { requires: [slack],  summary: "post and read messages" }
  reference/workflow-rules.md: { requires: [manage], summary: "wire button clicks" }
---

# Skill: Gated

Read only the file your task needs.

{{FILES}}

## Boundary

Operator rows are config.
`

// writeGated writes the gated skill plus its two reference files.
func writeGated(t *testing.T, root string) {
	t.Helper()
	dir := writeSkill(t, root, "gated", gatedSkill)
	mustWrite(t, filepath.Join(dir, "reference", "messaging.md"), "MESSAGING BODY")
	mustWrite(t, filepath.Join(dir, "reference", "workflow-rules.md"), "WORKFLOW BODY")
}

func TestVisible(t *testing.T) {
	have := map[string]bool{"slack": true}
	cases := []struct {
		requires []string
		want     bool
	}{
		{nil, true},                         // ungated → always visible
		{[]string{"slack"}, true},           // any-of hit
		{[]string{"manage"}, false},         // miss
		{[]string{"manage", "slack"}, true}, // any-of: one hit is enough
		{[]string{"manage", "journal"}, false},
	}
	for _, c := range cases {
		if got := visible(c.requires, have); got != c.want {
			t.Errorf("visible(%v) = %v, want %v", c.requires, got, c.want)
		}
	}
}

func TestList_GatedByRequires(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "open", withFrontmatter) // ungated
	writeSkill(t, root, "ops", `---
name: ops
description: operator only.
requires: [manage]
---
body`)

	// A slack-only agent sees the ungated skill, not the manage-gated one.
	res, err := New(os.DirFS(root), nil, "slack").Invoke(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := skillNames(res.(ListResult))
	if _, ok := names["alpha-skill"]; !ok {
		t.Errorf("ungated skill missing from list: %v", names)
	}
	if _, ok := names["ops"]; ok {
		t.Errorf("manage-gated skill leaked to a slack-only agent: %v", names)
	}

	// A manage agent sees both.
	res2, _ := New(os.DirFS(root), nil, "slack", "manage").Invoke(context.Background(), nil)
	if _, ok := skillNames(res2.(ListResult))["ops"]; !ok {
		t.Errorf("manage agent should see the ops skill")
	}
}

func TestRead_TemplatedRendersVisibleRowsOnly(t *testing.T) {
	root := t.TempDir()
	writeGated(t, root)

	// slack-only: frontmatter stripped, only the messaging row in the table,
	// no mention of the workflow-rules file, inventory hides it.
	got, err := New(os.DirFS(root), nil, "slack").Invoke(context.Background(), map[string]any{"name": "gated"})
	if err != nil {
		t.Fatal(err)
	}
	res := got.(ReadResult)
	if strings.Contains(res.Content, "---") && strings.Contains(res.Content, "requires:") {
		t.Errorf("templated body still carries frontmatter:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "{{FILES}}") {
		t.Errorf("marker not rendered:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "reference/messaging.md") {
		t.Errorf("visible row missing:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "workflow-rules.md") {
		t.Errorf("manage row leaked to a slack-only agent:\n%s", res.Content)
	}
	if contains(res.Files, "reference/workflow-rules.md") {
		t.Errorf("manage file leaked into inventory: %v", res.Files)
	}
	if !contains(res.Files, "reference/messaging.md") {
		t.Errorf("visible file missing from inventory: %v", res.Files)
	}

	// manage agent sees both rows and both files.
	got2, _ := New(os.DirFS(root), nil, "slack", "manage").Invoke(context.Background(), map[string]any{"name": "gated"})
	res2 := got2.(ReadResult)
	if !strings.Contains(res2.Content, "workflow-rules.md") {
		t.Errorf("manage agent should see the workflow-rules row:\n%s", res2.Content)
	}
	if !contains(res2.Files, "reference/workflow-rules.md") {
		t.Errorf("manage agent inventory should include workflow-rules.md: %v", res2.Files)
	}
}

func TestReadFile_GatedServe(t *testing.T) {
	root := t.TempDir()
	writeGated(t, root)

	// Visible file serves its body.
	got, err := New(os.DirFS(root), nil, "slack").Invoke(context.Background(), map[string]any{"name": "gated", "file": "reference/messaging.md"})
	if err != nil {
		t.Fatalf("serve visible file: %v", err)
	}
	if got.(FileResult).Content != "MESSAGING BODY" {
		t.Errorf("unexpected body: %q", got.(FileResult).Content)
	}

	// Gated file is refused (as "not found") for a slack-only agent.
	if _, err := New(os.DirFS(root), nil, "slack").Invoke(context.Background(), map[string]any{"name": "gated", "file": "reference/workflow-rules.md"}); err == nil {
		t.Error("manage-gated file served to a slack-only agent")
	}

	// The manage agent can read it.
	got2, err := New(os.DirFS(root), nil, "slack", "manage").Invoke(context.Background(), map[string]any{"name": "gated", "file": "reference/workflow-rules.md"})
	if err != nil || got2.(FileResult).Content != "WORKFLOW BODY" {
		t.Errorf("manage agent could not read the gated file: %v / %v", err, got2)
	}

	// Path traversal / outside reference|examples is refused.
	for _, bad := range []string{"../../etc/passwd", "/etc/passwd", "SKILL.md", "reference/../../x"} {
		if _, err := New(os.DirFS(root), nil, "manage").Invoke(context.Background(), map[string]any{"name": "gated", "file": bad}); err == nil {
			t.Errorf("readFile(%q) = nil error, want refusal", bad)
		}
	}
}

func skillNames(r ListResult) map[string]bool {
	out := make(map[string]bool, len(r.Skills))
	for _, s := range r.Skills {
		out[s.Name] = true
	}
	return out
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
