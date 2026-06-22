// Package toolset assembles the concrete []tools.Tool a native agent may call,
// from its agents.yaml `tools:` allowlist plus the remote tools of any MCP
// servers it attaches. It is the join point where Murtaugh's three tool sources
// converge onto one currency (tools.Tool): the synthesized native tools
// (files/terminal/skills, rooted per agent), the in-process registry tools
// (slack/jobs/…, selected by the allowlist), and the external MCP tools.
//
// The native file/terminal tools are synthesized HERE, per agent, rather than
// registered globally: each is rooted at the agent's own workdir (a single
// global instance could not carry a per-agent root), and this keeps powerful
// tools like terminal/write off the CLI and external-MCP surfaces unless an
// operator explicitly lists them for an agent.
package toolset

import (
	"fmt"
	"strings"

	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/files"
	fedit "github.com/miere/murtaugh-dev-toolkit/internal/tools/files/edit"
	fls "github.com/miere/murtaugh-dev-toolkit/internal/tools/files/ls"
	fread "github.com/miere/murtaugh-dev-toolkit/internal/tools/files/read"
	fwrite "github.com/miere/murtaugh-dev-toolkit/internal/tools/files/write"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/skills"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/terminal"
)

// Synthesized native tool-group names recognised in an agent's `tools:` list.
// Any other entry is matched against the registry by exact name or namespace.
const (
	GroupFiles    = "files"
	GroupTerminal = "terminal"
	GroupSkills   = "skills"
)

// Deps carries the per-agent context the resolver needs to build native tools
// and select registry tools.
type Deps struct {
	// Registry holds Murtaugh's in-process tools (slack.*, jobs.*, ping, …).
	// nil means no registry tools are available (only native + MCP).
	Registry *tools.Registry
	// WorkDir roots the files/terminal tools. Required when the allowlist
	// includes "files" or "terminal".
	WorkDir string
	// SkillsDir backs the skills tool. Required when the allowlist includes
	// "skills".
	SkillsDir string
	// TerminalApproval is the approval policy applied to the terminal tool. The
	// zero value (empty Mode) leaves NewWithApproval's default (allowlist).
	TerminalApproval terminal.ApprovalPolicy
}

// Resolve builds the toolset for a native agent. allow is the agent's `tools:`
// allowlist; mcpTools are the already-resolved remote tools from its attached
// MCP servers (always included). Native groups (files/terminal/skills) are
// synthesized; every other allowlist entry selects registry tools whose name
// equals the entry or whose namespace (the part before the first '.') equals it
// — so "slack" pulls in slack.send-msg, slack.fetch-msgs, … and "ping" pulls in
// ping. Duplicates (by tool name) are removed, preserving first-seen order:
// native, then registry (allowlist order), then MCP.
func Resolve(allow []string, mcpTools []tools.Tool, deps Deps) ([]tools.Tool, error) {
	var out []tools.Tool
	seen := make(map[string]bool)
	add := func(t tools.Tool) {
		if t == nil || seen[t.Name()] {
			return
		}
		seen[t.Name()] = true
		out = append(out, t)
	}

	// Shared read-before-write state across this agent's file tools.
	var readState *files.ReadState
	var root *files.Root

	ensureFileRoot := func() error {
		if root != nil {
			return nil
		}
		if strings.TrimSpace(deps.WorkDir) == "" {
			return fmt.Errorf("toolset: workdir is required for the %q tool group", GroupFiles)
		}
		r, err := files.NewRoot(deps.WorkDir)
		if err != nil {
			return fmt.Errorf("toolset: root %q: %w", deps.WorkDir, err)
		}
		root = r
		readState = files.NewReadState()
		return nil
	}

	for _, raw := range allow {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		switch entry {
		case GroupFiles:
			if err := ensureFileRoot(); err != nil {
				return nil, err
			}
			add(fread.New(root, readState))
			add(fwrite.New(root, readState))
			add(fedit.New(root, readState))
			add(fls.New(root))
		case GroupTerminal:
			if strings.TrimSpace(deps.WorkDir) == "" {
				return nil, fmt.Errorf("toolset: workdir is required for the %q tool", GroupTerminal)
			}
			add(terminal.NewWithApproval(deps.WorkDir, deps.TerminalApproval))
		case GroupSkills:
			if strings.TrimSpace(deps.SkillsDir) == "" {
				return nil, fmt.Errorf("toolset: skills_dir is required for the %q tool", GroupSkills)
			}
			add(skills.New(deps.SkillsDir))
		default:
			for _, t := range registryMatches(deps.Registry, entry) {
				add(t)
			}
		}
	}

	for _, t := range mcpTools {
		add(t)
	}
	return out, nil
}

// registryMatches returns the registry tools selected by a single allowlist
// entry: an exact tool-name match, or a namespace match (the entry equals the
// substring before the tool name's first '.'). Returns nil when reg is nil or
// nothing matches.
func registryMatches(reg *tools.Registry, entry string) []tools.Tool {
	if reg == nil {
		return nil
	}
	var matched []tools.Tool
	for _, t := range reg.All() {
		name := t.Name()
		if name == entry {
			matched = append(matched, t)
			continue
		}
		if i := strings.IndexByte(name, '.'); i >= 0 && name[:i] == entry {
			matched = append(matched, t)
		}
	}
	return matched
}
