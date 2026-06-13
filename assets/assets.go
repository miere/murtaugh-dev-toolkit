package assets

import "embed"

// FS contains reference Slack assets that are also used as built-in defaults:
// the seed config files, the Block Kit templates under templates/, the bundled
// agent skills under skills/ (each a SKILL.md + reference/ + examples/ tree),
// and cli-help.md (the canonical CLI/MCP command reference surfaced by
// `murtaugh help`). Both templates and skills are embedded recursively.
//
//go:embed slack.yaml agents.yaml jobs.yaml journal.yaml cli-help.md templates skills
var FS embed.FS
