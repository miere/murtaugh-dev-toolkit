// Package agentbuild constructs the agent.Client backend for an agent profile,
// branching on its kind: a kind:native profile yields the in-process LLM loop
// (internal/agent/native); a kind:acp profile yields the external-process
// ProcessClient. It is the single place the two backends are selected, shared by
// the Slack gateway (chat sessions) and the agentdelegate runner (jobs,
// workflows, unfurls) so both gain native support from one seam.
package agentbuild

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/native"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/frontends/mcp"
	"github.com/miere/murtaugh/internal/mcpbridge"
	"github.com/miere/murtaugh/internal/tools"
)

// Deps carries the shared context needed to build either backend. Registry and
// MCPServers are only consulted for native agents; WorkspaceDir is the
// workspace/config root for persona (SOUL.md), the system-prompt file, and the
// bespoke-skills dir. The agent workdir is NOT here — it is resolved once into
// the ResolvedAgent passed to Client, so it cannot be re-derived from a raw
// fallback at this seam.
type Deps struct {
	Registry     *tools.Registry
	MCPServers   map[string]config.MCPServerConfig
	WorkspaceDir string
	Logger       *slog.Logger
	// Approver gates a native agent's side-effecting tool calls behind human
	// approval. nil disables gating — set only on the interactive chat path,
	// never for headless/delegated agents. Ignored for ACP agents.
	Approver native.Approver
	// ACPPermissionAsker answers an ACP agent's session/request_permission
	// requests via a human in Slack. nil on headless/delegated paths, where the
	// agent's acp_permission policy still applies (auto-allow/auto-deny work;
	// "ask" denies). Ignored for native agents.
	ACPPermissionAsker agent.PermissionAsker
	// Bridge is the gateway's shared MCP aggregator server. When set, an ACP
	// agent is given a per-agent aggregator over it so it can reach Murtaugh's
	// own tools through `murtaugh mcp-bridge`. nil (CLI/delegate paths) leaves an
	// ACP agent with no Murtaugh tools, as before. Ignored for native agents,
	// which hold their toolset in-process.
	Bridge *mcpbridge.Server
}

// Client builds the backend for a resolved agent. It does no network/process I/O
// — both backends defer that to Initialize. The agent's workdir is taken from
// resolved (already validated); deps carries only the workspace/config root and
// the shared wiring.
func Client(resolved ResolvedAgent, deps Deps) (agent.Client, error) {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	profile := resolved.Profile
	switch resolved.Kind {
	case config.AgentKindNative:
		return native.Build(profile, native.BuildDeps{
			Registry:     deps.Registry,
			MCPServers:   deps.MCPServers,
			WorkspaceDir: deps.WorkspaceDir,
			Root:         resolved.Root(),
			Tools:        resolved.Tools(),
			Logger:       logger,
			Approver:     deps.Approver,
		})
	case config.AgentKindACP:
		var aggregator agent.Aggregator
		if deps.Bridge != nil {
			var approver mcp.Approver
			if deps.Approver != nil {
				approver = mcpApprover{inner: deps.Approver}
			}
			aggr, err := newACPAggregator(deps.Bridge, deps.Registry, resolved, approver, native.MCPServerConfigs(deps.MCPServers), logger)
			if err != nil {
				return nil, fmt.Errorf("agentbuild: build ACP aggregator: %w", err)
			}
			aggregator = aggr
		}
		return agent.NewProcessClient(agent.ProcessOptions{
			Command:          profile.ACP.Command,
			Args:             profile.ACP.Args,
			WorkDir:          resolved.Dir(),
			Env:              profile.EnvOverrides(),
			Logger:           logger,
			PermissionPolicy: profile.ResolvedACPPermission(),
			PermissionAsker:  deps.ACPPermissionAsker,
			Aggregator:       aggregator,
			// Share Murtaugh's persona with the ACP agent (it has no system role of
			// our making); read from the config/workspace dir where SOUL.md lives.
			Persona: native.ReadSoul(deps.WorkspaceDir),
		}), nil
	default:
		return nil, fmt.Errorf("agentbuild: unknown agent kind %q", resolved.Kind)
	}
}

// ErrorClient returns an agent.Client that fails every operation with err. It
// lets callers whose factory signature cannot return an error (e.g.
// agentdelegate.ClientFactory) surface a build failure at Initialize time
// instead of panicking on a nil client.
func ErrorClient(err error) agent.Client { return errClient{err: err} }

type errClient struct{ err error }

func (c errClient) Initialize(context.Context) error { return c.err }
func (c errClient) NewSession(context.Context, agent.SessionMetadata) (agent.Session, error) {
	return agent.Session{}, c.err
}
func (c errClient) Prompt(context.Context, string, agent.PromptRequest) (<-chan agent.Event, error) {
	return nil, c.err
}
func (c errClient) Cancel(context.Context, string) error { return c.err }
func (c errClient) Close() error                         { return nil }
