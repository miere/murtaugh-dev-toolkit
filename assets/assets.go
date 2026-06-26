package assets

import (
	"embed"
	"io/fs"
	"sort"
)

// FS contains reference Slack assets that are also used as built-in defaults:
// the seed config files, the Block Kit templates under templates/, the bundled
// agent skills under skills/ (each a SKILL.md + reference/ + examples/ tree),
// and cli-help.md (the canonical CLI/MCP command reference surfaced by
// `murtaugh help`). Both templates and skills are embedded recursively, as is
// troubleshoot/ (the diagnostics-bundle instructions surfaced by
// `troubleshoot.bundle`).
//
//go:embed slack.yaml agents.yaml jobs.yaml journal.yaml env.example system-prompt.md AGENTS.md cli-help.md templates skills troubleshoot
var FS embed.FS

// skillsRoot is the embedded directory holding one subdirectory per bundled
// (murtaugh-*) skill.
const skillsRoot = "skills"

// Skills returns an fs.FS rooted at the embedded skills tree, so each bundled
// skill is a top-level directory (e.g. "murtaugh-slack/SKILL.md"). This is the
// managed, in-binary source the skills tool serves from — the bundled skills
// never need to touch disk. Returns nil only if the embed is malformed (a build
// error), which never happens in practice.
func Skills() fs.FS {
	sub, err := fs.Sub(FS, skillsRoot)
	if err != nil {
		return nil
	}
	return sub
}

// SkillNames returns the sorted directory names of the bundled skills (e.g.
// "murtaugh-slack"). It is the authority for validating an agent's
// export_skills_to_fs list and for the export reconcile.
func SkillNames() []string {
	entries, err := fs.ReadDir(FS, skillsRoot)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}
