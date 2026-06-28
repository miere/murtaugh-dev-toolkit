package agentbuild

import (
	"fmt"
	"strings"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/tools/files"
	"github.com/miere/murtaugh/internal/toolset"
)

// ResolvedAgent is an AgentProfile whose hard preconditions have been resolved
// and validated exactly once, at the build seam where the profile and the
// runtime workspace directory first co-exist. Downstream code consumes this —
// never the raw profile + a base-dir fallback — so the agent workdir cannot be
// re-derived inconsistently or forgotten (an internal/archtest analyzer enforces
// that downstream packages don't read profile.WorkDir).
//
// A nil root models two cases identically: an agent that legitimately declares no
// workspace, and one whose configured workdir could not be turned into a Root
// (empty, or an invalid path). In the latter case the workdir-rooted tool groups
// it requested are dropped (recorded in problems) rather than failing the whole
// agent — the decided degrade-and-collect policy.
type ResolvedAgent struct {
	Profile config.AgentProfile
	Kind    config.AgentKind

	name       string
	root       *files.Root
	rootReason string
	tools      []string
	problems   []toolset.Problem
}

// Name returns the agent's name (empty on headless/delegate paths).
func (r ResolvedAgent) Name() string { return r.name }

// Root returns the constructed workspace root, or nil when none could be built.
func (r ResolvedAgent) Root() *files.Root { return r.root }

// RootReason returns the precise reason Root is nil (empty when Root is non-nil).
func (r ResolvedAgent) RootReason() string { return r.rootReason }

// Dir returns the absolute workspace dir, or "" when there is no root.
func (r ResolvedAgent) Dir() string {
	if r.root == nil {
		return ""
	}
	return r.root.Dir()
}

// Tools returns the EFFECTIVE allowlist: the profile's tools minus any
// workdir-rooted group dropped because no root could be built.
func (r ResolvedAgent) Tools() []string { return r.tools }

// Problems returns the tool groups dropped during resolution. The agent still
// builds and answers; these are surfaced at startup for diagnosis.
func (r ResolvedAgent) Problems() []toolset.Problem { return r.problems }

// Resolve resolves an agent's workspace at the build seam. workspaceDir is the
// runtime base/workspace directory used as the single workdir fallback. It
// applies that fallback once, builds the *files.Root when possible, and — per the
// degrade-and-collect policy — drops any workdir-rooted tool the root cannot
// satisfy (collecting a precise Problem per dropped group) while keeping the agent
// and the rest of its toolset alive. The error return is reserved for genuinely
// fatal states; a misconfigured workdir is never fatal here.
func Resolve(name string, profile config.AgentProfile, workspaceDir string) (ResolvedAgent, error) {
	resolved := ResolvedAgent{Profile: profile, Kind: profile.ResolvedKind(), name: name}

	dir := strings.TrimSpace(profile.WorkDir)
	if dir == "" {
		dir = strings.TrimSpace(workspaceDir)
	}
	switch {
	case dir == "":
		resolved.rootReason = "no workdir is set and no base workspace directory is configured"
	default:
		root, err := files.NewRoot(dir)
		if err != nil {
			resolved.rootReason = fmt.Sprintf("invalid workdir %q: %v", dir, err)
		} else {
			resolved.root = root
		}
	}

	// Prune workdir-rooted groups when no root could be built; collect a Problem
	// for each so it is visible at startup. Every other group passes through.
	effective := make([]string, 0, len(profile.Tools))
	for _, raw := range profile.Tools {
		group := strings.TrimSpace(raw)
		if group == "" {
			continue
		}
		if resolved.root == nil && toolset.IsWorkdirRooted(group) {
			resolved.problems = append(resolved.problems, toolset.Problem{
				Agent:  name,
				Group:  group,
				Reason: resolved.rootReason,
			})
			continue
		}
		effective = append(effective, raw)
	}
	resolved.tools = effective

	return resolved, nil
}
