// Package skills implements the `skills` tool: it lets a native agent discover
// and read the skills available to it.
//
// Two sources, merged. Murtaugh's own skills (the `murtaugh-*` namespace) are
// served from an embedded filesystem baked into the binary — they never touch
// disk by default, so neither the file tools nor a terminal can read them; the
// gated `skills` tool is the only reader. The user's own bespoke skills live on
// disk (an optional second source) and are layered in. The managed source is
// authoritative: a name present in the embed is always served from the embed,
// so an on-disk copy (e.g. one exported for an external Claude agent to
// discover) never shadows or supersedes it.
//
// Capability gating. A skill (and each of its files) may declare a `requires:`
// list of capability tokens in SKILL.md frontmatter. The tool is constructed
// with the agent's granted tokens (its agents.yaml `tools:` allowlist); a unit
// is visible iff the agent holds at least one of its required tokens (any-of),
// and a unit with no `requires` is always visible. This is the same allowlist
// currency `toolset.Resolve` uses for tools. Three layers cooperate: the
// listing/index filter (L1), the templated SKILL.md body render that drops
// out-of-scope rows (L2, opt-in via `templated: true`), and the per-file gate on
// the inventory and file-serve path (L3).
package skills

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"
)

// skillFile is the conventional name of a skill's entrypoint document.
const skillFile = "SKILL.md"

// filesMarker is the placeholder a `templated: true` SKILL.md body uses to mark
// where the capability-filtered file table should be generated. It is replaced
// (on the read path only) with a table of just the files the reading agent may
// open; a body without the marker is returned unchanged.
const filesMarker = "{{FILES}}"

// Tool is the skills capability. It reads from a managed source (the embedded
// murtaugh-* skills) and an optional bespoke source (the user's on-disk skills),
// carrying the reading agent's granted capability tokens (`have`) for gating.
type Tool struct {
	managed fs.FS
	bespoke fs.FS // may be nil
	have    map[string]bool
}

// New constructs a Tool from the managed skills FS (the embedded murtaugh-*
// tree) and an optional bespoke FS (the user's on-disk skills; nil for none).
// have is the agent's granted capability tokens (its agents.yaml `tools:`
// allowlist); pass none for an ungated view.
func New(managed, bespoke fs.FS, have ...string) *Tool {
	return &Tool{managed: managed, bespoke: bespoke, have: toSet(have)}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "skills" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Discover and read available skills. Call with no arguments to list " +
		"all skills (name + description); pass name to read that skill's SKILL.md " +
		"and file inventory; pass name and file to read one reference/example file."
}

// InputSchema returns the JSON Schema for the tool's arguments. `name` switches
// between list mode (empty) and read mode (set); `file` (with `name`) reads a
// single reference/example file within that skill.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name": {
				Type:        "string",
				Description: "Skill name (its directory name). When omitted, all skills are listed.",
			},
			"file": {
				Type:        "string",
				Description: "Optional reference/example file path within the skill (e.g. reference/messaging.md) to read its body. Requires name.",
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
// skill's metadata, the SKILL.md text (rendered to the agent's capabilities when
// the skill is templated), and the relative paths of its visible files.
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

// FileResult is returned when Invoke is called with name + file: the body of a
// single reference/example file within a skill.
type FileResult struct {
	Skill   string `json:"skill"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

// String renders the file body verbatim.
func (r FileResult) String() string { return r.Content }

// Invoke lists visible skills when name is empty, reads the named skill, or —
// when file is also set — reads one file within that skill.
func (t *Tool) Invoke(_ context.Context, args map[string]any) (any, error) {
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	file, _ := args["file"].(string)
	file = strings.TrimSpace(file)
	if name == "" {
		return t.list()
	}
	if file != "" {
		return t.readFile(name, file)
	}
	return t.read(name)
}

// sourceFor returns the FS that owns name: the managed source if it has the
// skill (authoritative), else the bespoke source. ok is false if neither does.
func (t *Tool) sourceFor(name string) (fs.FS, bool) {
	if t.managed != nil && isSkillDir(t.managed, name) {
		return t.managed, true
	}
	if t.bespoke != nil && isSkillDir(t.bespoke, name) {
		return t.bespoke, true
	}
	return nil, false
}

// list returns the skills visible to this agent (sorted), filtered by each
// skill's top-level `requires:` (L1). Managed skills are listed first and a
// bespoke skill whose name collides with a managed one is dropped (managed
// authoritative).
func (t *Tool) list() (ListResult, error) {
	infos, err := t.listInfos()
	if err != nil {
		return ListResult{}, fmt.Errorf("Error: cannot read skills: %w", err)
	}
	var out []SkillSummary
	for _, in := range infos {
		if visible(in.requires, t.have) {
			out = append(out, in.SkillSummary)
		}
	}
	return ListResult{Skills: out}, nil
}

// skillInfo is a skill's summary plus its top-level capability requirement.
type skillInfo struct {
	SkillSummary
	requires []string
}

// listInfos scans both sources and returns each skill's summary + top-level
// `requires:`, sorted by name. Managed names win; a bespoke skill with the same
// name as a managed one is skipped.
func (t *Tool) listInfos() ([]skillInfo, error) {
	seen := make(map[string]bool)
	var out []skillInfo
	collect := func(fsys fs.FS) error {
		if fsys == nil {
			return nil
		}
		entries, err := fs.ReadDir(fsys, ".")
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			data, err := fs.ReadFile(fsys, path.Join(e.Name(), skillFile))
			if err != nil {
				continue // not a skill directory
			}
			seen[e.Name()] = true
			meta := parseMetadata(string(data))
			desc := meta.description
			if desc == "" {
				desc = meta.summary
			}
			name := meta.name
			if name == "" {
				name = e.Name()
			}
			out = append(out, skillInfo{SkillSummary{Name: name, Description: desc}, meta.requires})
		}
		return nil
	}
	// Managed first so it wins on name collisions; a missing managed source is
	// not fatal (tests may pass only bespoke).
	if err := collect(t.managed); err != nil {
		return nil, err
	}
	if err := collect(t.bespoke); err != nil && t.managed == nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListVisible returns the skills an agent holding `have` may use across the
// managed + bespoke sources — the L1 gate for the system-prompt skills index.
// Filtering by the static profile tokens keeps the index stable per profile (and
// so cache-safe). bespoke may be nil.
func ListVisible(managed, bespoke fs.FS, have []string) ([]SkillSummary, error) {
	t := New(managed, bespoke, have...)
	infos, err := t.listInfos()
	if err != nil {
		return nil, err
	}
	var out []SkillSummary
	for _, in := range infos {
		if visible(in.requires, t.have) {
			out = append(out, in.SkillSummary)
		}
	}
	return out, nil
}

// read returns the named skill's SKILL.md content and visible file inventory.
// For a templated skill the frontmatter is stripped and the body is rendered to
// the agent's capabilities (L2); the file inventory is filtered per-file (L3).
func (t *Tool) read(name string) (ReadResult, error) {
	fsys, err := t.skillFS(name)
	if err != nil {
		return ReadResult{}, err
	}
	data, err := fs.ReadFile(fsys, path.Join(name, skillFile))
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

	body := content
	if meta.templated {
		if _, rest, ok := splitFrontmatter(content); ok {
			body = strings.TrimLeft(rest, "\n")
		}
		body = renderFiles(body, meta.files, t.have)
	}

	return ReadResult{
		Name:        displayName,
		Description: desc,
		Content:     body,
		Files:       t.filterFiles(listSkillFiles(fsys, name), meta.files),
	}, nil
}

// readFile serves one reference/example file's body, gated by the file's
// `requires:` in the skill manifest (L3). A hidden or absent file is reported
// identically ("not found") so gating doesn't disclose that a file exists.
func (t *Tool) readFile(name, rel string) (FileResult, error) {
	fsys, err := t.skillFS(name)
	if err != nil {
		return FileResult{}, err
	}
	clean := path.Clean(strings.TrimSpace(rel))
	// Confine to the skill's reference/ and examples/ subtrees.
	if !strings.HasPrefix(clean, "reference/") && !strings.HasPrefix(clean, "examples/") {
		return FileResult{}, fmt.Errorf("Error: file %q not found in skill %q", rel, name)
	}
	full := path.Join(name, clean)
	if !fs.ValidPath(full) {
		return FileResult{}, fmt.Errorf("Error: file %q not found in skill %q", rel, name)
	}
	// Gate by the manifest entry, if any.
	meta := readMeta(fsys, name)
	if reqs, ok := fileRequires(meta.files, clean); ok && !visible(reqs, t.have) {
		return FileResult{}, fmt.Errorf("Error: file %q not found in skill %q", rel, name)
	}
	data, err := fs.ReadFile(fsys, full)
	if err != nil {
		return FileResult{}, fmt.Errorf("Error: file %q not found in skill %q", rel, name)
	}
	return FileResult{Skill: name, Path: clean, Content: string(data)}, nil
}

// filterFiles drops inventory entries whose manifest `requires:` the agent
// doesn't satisfy. Files absent from the manifest are kept (visible by default).
func (t *Tool) filterFiles(all []string, manifest []FileMeta) []string {
	if len(manifest) == 0 {
		return all
	}
	var out []string
	for _, f := range all {
		if reqs, ok := fileRequires(manifest, f); ok && !visible(reqs, t.have) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// renderFiles replaces the filesMarker line in a templated body with a table of
// just the files the agent may open. A body without the marker is unchanged.
func renderFiles(body string, files []FileMeta, have map[string]bool) string {
	var rows []string
	for _, f := range files {
		// Gating-only entries (e.g. a directory prefix) carry no summary and
		// never appear as a table row.
		if f.Summary == "" {
			continue
		}
		if visible(f.Requires, have) {
			rows = append(rows, fmt.Sprintf("| %s | `%s` |", f.Summary, f.Path))
		}
	}
	table := ""
	if len(rows) > 0 {
		table = "| When you need to… | Read |\n|---|---|\n" + strings.Join(rows, "\n")
	}
	lines := strings.Split(body, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) == filesMarker {
			lines[i] = table
			return strings.Join(lines, "\n")
		}
	}
	return body
}

// skillFS resolves the source FS that owns a valid skill name, rejecting names
// that aren't a single path element or aren't a skill directory.
func (t *Tool) skillFS(name string) (fs.FS, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return nil, fmt.Errorf("Error: invalid skill name %q", name)
	}
	if fsys, ok := t.sourceFor(name); ok {
		return fsys, nil
	}
	return nil, fmt.Errorf("Error: skill %q not found", name)
}

// isSkillDir reports whether fsys has a skill directory named name (a directory
// containing a SKILL.md).
func isSkillDir(fsys fs.FS, name string) bool {
	if info, err := fs.Stat(fsys, name); err != nil || !info.IsDir() {
		return false
	}
	if _, err := fs.Stat(fsys, path.Join(name, skillFile)); err != nil {
		return false
	}
	return true
}

// listSkillFiles returns relative paths (slash-separated) of regular files under
// the skill's reference/ and examples/ subdirectories, sorted.
func listSkillFiles(fsys fs.FS, name string) []string {
	var files []string
	for _, sub := range []string{"reference", "examples"} {
		root := path.Join(name, sub)
		_ = fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // subdir absent — skip
			}
			if d.IsDir() {
				return nil
			}
			rel := strings.TrimPrefix(p, name+"/")
			files = append(files, rel)
			return nil
		})
	}
	sort.Strings(files)
	return files
}

// FileMeta is one entry from a skill's `files:` frontmatter manifest: a
// reference/example path, the capability tokens that gate it, and a one-line
// summary used to generate the templated file table.
type FileMeta struct {
	Path     string
	Requires []string
	Summary  string
}

// metadata holds the fields parsed from a SKILL.md document.
type metadata struct {
	name        string
	description string
	summary     string // fallback: first heading text or first paragraph
	requires    []string
	templated   bool
	files       []FileMeta
}

// readMeta parses the SKILL.md of name in fsys, returning a zero metadata if
// unreadable.
func readMeta(fsys fs.FS, name string) metadata {
	data, err := fs.ReadFile(fsys, path.Join(name, skillFile))
	if err != nil {
		return metadata{}
	}
	return parseMetadata(string(data))
}

// fileRequires returns the gating tokens for a file path per the manifest: an
// exact entry wins, else the longest directory-prefix entry (a manifest key
// ending in "/"). ok is false when nothing matches (the file is then visible by
// default). Directory entries let one line gate a whole subtree (e.g.
// `examples/unfurl/`).
func fileRequires(manifest []FileMeta, p string) ([]string, bool) {
	var bestReq []string
	var bestLen int
	found := false
	for _, f := range manifest {
		if f.Path == p {
			return f.Requires, true
		}
		if strings.HasSuffix(f.Path, "/") && strings.HasPrefix(p, f.Path) && len(f.Path) > bestLen {
			bestReq, bestLen, found = f.Requires, len(f.Path), true
		}
	}
	return bestReq, found
}

// parseMetadata extracts name/description/requires/templated/files from YAML
// frontmatter when present. When frontmatter is absent (or lacks a
// description), summary holds the first markdown heading's text, else the first
// non-empty paragraph.
func parseMetadata(content string) metadata {
	var m metadata
	body := content

	if fm, rest, ok := splitFrontmatter(content); ok {
		var doc struct {
			Name        string    `yaml:"name"`
			Description string    `yaml:"description"`
			Requires    []string  `yaml:"requires"`
			Templated   bool      `yaml:"templated"`
			Files       yaml.Node `yaml:"files"`
		}
		if err := yaml.Unmarshal([]byte(fm), &doc); err == nil {
			m.name = strings.TrimSpace(doc.Name)
			m.description = strings.TrimSpace(doc.Description)
			m.requires = trimAll(doc.Requires)
			m.templated = doc.Templated
			m.files = parseFiles(&doc.Files)
		}
		body = rest
	}

	m.summary = firstSummary(body)
	return m
}

// parseFiles decodes the `files:` mapping node preserving author order (a Go map
// would shuffle it, which the generated table must not do).
func parseFiles(node *yaml.Node) []FileMeta {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	var out []FileMeta
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		var fm struct {
			Requires []string `yaml:"requires"`
			Summary  string   `yaml:"summary"`
		}
		_ = val.Decode(&fm)
		out = append(out, FileMeta{
			Path:     strings.TrimSpace(key.Value),
			Requires: trimAll(fm.Requires),
			Summary:  strings.TrimSpace(fm.Summary),
		})
	}
	return out
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

// visible reports whether an agent holding `have` may see a unit requiring
// `requires`: any-of semantics, with no requirement meaning always-visible.
func visible(requires []string, have map[string]bool) bool {
	if len(requires) == 0 {
		return true
	}
	for _, r := range requires {
		if have[strings.TrimSpace(r)] {
			return true
		}
	}
	return false
}

// toSet builds a lookup set from a token slice, dropping blanks.
func toSet(tokens []string) map[string]bool {
	set := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		if t = strings.TrimSpace(t); t != "" {
			set[t] = true
		}
	}
	return set
}

// trimAll trims each token and drops blanks.
func trimAll(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
