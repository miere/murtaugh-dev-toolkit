package skills

import (
	"context"
	"os"
	"path/filepath"
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

	got, err := New(root).Invoke(context.Background(), nil)
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

	got, err := New(root).Invoke(context.Background(), map[string]any{"name": "alpha"})
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

	got, err := New(root).Invoke(context.Background(), map[string]any{"name": "beta"})
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

	tool := New(root)
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
	if _, err := New(missing).Invoke(context.Background(), nil); err == nil {
		t.Fatal("Invoke list on missing dir = nil error, want error")
	}
}

func TestStaticContract(t *testing.T) {
	tool := New(t.TempDir())
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
