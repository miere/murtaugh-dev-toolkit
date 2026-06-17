// Package skills implements the `skills` tool: it lets a native agent discover
// and read the skills bundled into its workspace.
//
// Murtaugh's bootstrap mirrors the embedded skills/ tree to
// <workspace>/.agents/skills/, where each skill is a directory containing a
// SKILL.md plus optional reference/ and examples/ subtrees. This tool lists
// those skills (name + description) when invoked with no name, and returns a
// single skill's SKILL.md body plus its file inventory when given a name.
package skills

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"
)

// skillFile is the conventional name of a skill's entrypoint document.
const skillFile = "SKILL.md"

// Tool is the skills capability. It is rooted at skillsDir, the directory that
// holds one subdirectory per skill.
type Tool struct {
	skillsDir string
}

// New constructs a Tool rooted at skillsDir — the directory holding skill
// subdirectories (e.g. <workspace>/.agents/skills).
func New(skillsDir string) *Tool { return &Tool{skillsDir: skillsDir} }

// Name returns the registry key.
func (t *Tool) Name() string { return "skills" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Discover and read bundled skills. Call with no arguments to list all " +
		"skills (name + description); pass name to read that skill's SKILL.md and " +
		"file inventory."
}

// InputSchema returns the JSON Schema for the tool's arguments. The single
// optional `name` switches between list mode (empty) and read mode (set).
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name": {
				Type:        "string",
				Description: "Skill name (its directory name). When omitted, all skills are listed.",
			},
		},
	}
}

// SkillSummary is one entry in a list result: the skill's directory name and
// its parsed description.
type SkillSummary struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ListResult is returned when Invoke is called with no name. It is
// JSON-marshalable and renders a human line per skill via String().
type ListResult struct {
	Skills []SkillSummary `json:"skills"`
}

// String renders the CLI-visible listing.
func (r ListResult) String() string {
	if len(r.Skills) == 0 {
		return "No skills found."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d skill(s):\n", len(r.Skills))
	for _, s := range r.Skills {
		if s.Description != "" {
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
		} else {
			fmt.Fprintf(&b, "- %s\n", s.Name)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ReadResult is returned when Invoke is called with a name. It carries the
// skill's metadata, the full SKILL.md text, and the relative paths of files
// under its reference/ and examples/ subdirectories.
type ReadResult struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Content     string   `json:"content"`
	Files       []string `json:"files,omitempty"`
}

// String renders the CLI-visible view: a header, the file inventory, then the
// SKILL.md body.
func (r ReadResult) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", r.Name)
	if r.Description != "" {
		fmt.Fprintf(&b, "%s\n", r.Description)
	}
	if len(r.Files) > 0 {
		b.WriteString("\nFiles:\n")
		for _, f := range r.Files {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	b.WriteString("\n")
	b.WriteString(r.Content)
	return strings.TrimRight(b.String(), "\n")
}

// Invoke lists all skills when name is empty, or reads the named skill.
func (t *Tool) Invoke(_ context.Context, args map[string]any) (any, error) {
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return t.list()
	}
	return t.read(name)
}

// list scans skillsDir for subdirectories containing a SKILL.md and returns a
// sorted summary for each.
func (t *Tool) list() (ListResult, error) {
	entries, err := os.ReadDir(t.skillsDir)
	if err != nil {
		return ListResult{}, fmt.Errorf("Error: cannot read skills directory: %w", err)
	}
	var out []SkillSummary
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mdPath := filepath.Join(t.skillsDir, e.Name(), skillFile)
		data, err := os.ReadFile(mdPath)
		if err != nil {
			continue // not a skill directory
		}
		meta := parseMetadata(string(data))
		desc := meta.description
		if desc == "" {
			desc = meta.summary
		}
		name := meta.name
		if name == "" {
			name = e.Name()
		}
		out = append(out, SkillSummary{Name: name, Description: desc})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return ListResult{Skills: out}, nil
}

// read returns the named skill's SKILL.md content and file inventory. The name
// must resolve to a direct child of skillsDir (path-traversal guard).
func (t *Tool) read(name string) (ReadResult, error) {
	dir, err := t.skillDir(name)
	if err != nil {
		return ReadResult{}, err
	}

	mdPath := filepath.Join(dir, skillFile)
	data, err := os.ReadFile(mdPath)
	if err != nil {
		return ReadResult{}, fmt.Errorf("Error: skill %q not found", name)
	}
	content := string(data)
	meta := parseMetadata(content)

	desc := meta.description
	if desc == "" {
		desc = meta.summary
	}
	displayName := meta.name
	if displayName == "" {
		displayName = name
	}

	return ReadResult{
		Name:        displayName,
		Description: desc,
		Content:     content,
		Files:       listSkillFiles(dir),
	}, nil
}

// skillDir resolves name to a direct child directory of skillsDir, rejecting
// any name that contains path separators or escapes skillsDir.
func (t *Tool) skillDir(name string) (string, error) {
	// A skill name must be a single path element — no separators, no traversal.
	if name != filepath.Base(name) || name == "." || name == ".." || strings.ContainsRune(name, os.PathSeparator) || strings.ContainsRune(name, '/') {
		return "", fmt.Errorf("Error: invalid skill name %q", name)
	}
	dir := filepath.Join(t.skillsDir, name)

	// Defense in depth: confirm the cleaned path is a direct child of skillsDir.
	parent := filepath.Clean(t.skillsDir)
	if filepath.Dir(filepath.Clean(dir)) != parent {
		return "", fmt.Errorf("Error: invalid skill name %q", name)
	}

	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("Error: skill %q not found", name)
	}
	return dir, nil
}

// listSkillFiles returns relative paths (slash-separated) of regular files
// under the skill's reference/ and examples/ subdirectories, sorted.
func listSkillFiles(dir string) []string {
	var files []string
	for _, sub := range []string{"reference", "examples"} {
		root := filepath.Join(dir, sub)
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // subdir absent — skip
			}
			if d.IsDir() {
				return nil
			}
			rel, relErr := filepath.Rel(dir, path)
			if relErr != nil {
				return nil
			}
			files = append(files, filepath.ToSlash(rel))
			return nil
		})
	}
	sort.Strings(files)
	return files
}

// metadata holds the fields parsed from a SKILL.md document.
type metadata struct {
	name        string
	description string
	summary     string // fallback: first heading text or first paragraph
}

// parseMetadata extracts name/description from YAML frontmatter when present.
// When frontmatter is absent (or lacks a description), summary holds the first
// markdown heading's text, else the first non-empty paragraph.
func parseMetadata(content string) metadata {
	var m metadata
	body := content

	if fm, rest, ok := splitFrontmatter(content); ok {
		var doc struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
		}
		if err := yaml.Unmarshal([]byte(fm), &doc); err == nil {
			m.name = strings.TrimSpace(doc.Name)
			m.description = strings.TrimSpace(doc.Description)
		}
		body = rest
	}

	m.summary = firstSummary(body)
	return m
}

// splitFrontmatter returns the YAML frontmatter block and the remaining body
// when content opens with a `---` fenced block. ok is false otherwise.
func splitFrontmatter(content string) (frontmatter, body string, ok bool) {
	s := strings.TrimLeft(content, "\uFEFF") // tolerate a leading BOM
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return "", content, false
	}
	// Drop the opening fence line.
	rest := s[strings.IndexByte(s, '\n')+1:]
	// Find the closing fence at the start of a line.
	lines := strings.Split(rest, "\n")
	for i, line := range lines {
		if strings.TrimRight(line, "\r") == "---" {
			fm := strings.Join(lines[:i], "\n")
			bodyOut := strings.Join(lines[i+1:], "\n")
			return fm, bodyOut, true
		}
	}
	return "", content, false
}

// firstSummary returns the text of the first markdown heading, or failing that
// the first non-empty, non-heading paragraph line.
func firstSummary(body string) string {
	var firstPara string
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
			if heading != "" {
				return heading
			}
			continue
		}
		if firstPara == "" {
			firstPara = line
		}
	}
	return firstPara
}
