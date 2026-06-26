package agentbuild

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/agent/native"
	"github.com/miere/murtaugh-dev-toolkit/internal/frontends/mcp"
	"github.com/miere/murtaugh-dev-toolkit/internal/mcpbridge"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
	"github.com/miere/murtaugh-dev-toolkit/internal/toolset"
)

// acpAggregator is the concrete agent.Aggregator for an ACP agent. It serves the
// agent's resolved built-in toolset (its tools: allowlist, minus the native-only
// synthesized groups and the host-config-mutating tools) over the gateway's
// shared bridge socket, gated by the same human approver the native loop uses.
//
// Proxying the agent's external MCP servers through the aggregator lands with the
// authoritative-MCP work (spec 015 §4); for now the surface is Murtaugh's own
// built-ins, which is the headline parity win (ask, present_plan, slack.*, ...).
type acpAggregator struct {
	server   *mcpbridge.Server
	binary   string
	toolset  []tools.Tool
	approver mcp.Approver
}

// newACPAggregator resolves the agent's built-in toolset once and returns an
// aggregator bound to server. allow is the agent's tools: allowlist; approver
// (may be nil) gates side-effecting calls.
func newACPAggregator(server *mcpbridge.Server, registry *tools.Registry, allow []string, approver mcp.Approver) (*acpAggregator, error) {
	ts, err := resolveBuiltins(registry, allow)
	if err != nil {
		return nil, err
	}
	binary, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve murtaugh binary for bridge: %w", err)
	}
	return &acpAggregator{server: server, binary: binary, toolset: ts, approver: approver}, nil
}

// RegisterSession registers this session's toolset under a fresh token and
// returns the stdio bridge server to advertise. The session's Slack location is
// injected into every tool-call context so the approver posts in the right
// thread.
func (a *acpAggregator) RegisterSession(meta agent.SessionMetadata) (agent.MCPServerSpec, func(), error) {
	decorate := locationDecorator(meta)
	token, err := a.server.Register(mcpbridge.Session{
		Tools:       a.toolset,
		Approver:    a.approver,
		WithContext: decorate,
	})
	if err != nil {
		return agent.MCPServerSpec{}, nil, err
	}
	spec := agent.MCPServerSpec{
		Name:    "murtaugh",
		Command: a.binary,
		Args:    []string{mcpbridge.Subcommand},
		Env: map[string]string{
			mcpbridge.EnvSocket: a.server.SocketPath(),
			mcpbridge.EnvToken:  token,
		},
	}
	return spec, func() { a.server.Unregister(token) }, nil
}

// locationDecorator returns a context decorator that carries the session's Slack
// location, or nil when there is none (non-chat sessions stay ungated by
// location, matching GateApprover's headless behaviour).
func locationDecorator(meta agent.SessionMetadata) func(context.Context) context.Context {
	if strings.TrimSpace(meta.ChannelID) == "" {
		return nil
	}
	loc := agent.TurnLocation{ChannelID: meta.ChannelID, ThreadTS: meta.ThreadTS}
	return func(ctx context.Context) context.Context {
		return agent.WithTurnLocation(ctx, loc)
	}
}

// resolveBuiltins resolves the registry tools an ACP agent's allowlist selects,
// excluding the native-only synthesized groups (files/terminal/skills — the
// agent has its own) and the bridge-unsafe tools (setup.*, which mutate
// Murtaugh's own configuration and must never be handed to an external agent).
func resolveBuiltins(registry *tools.Registry, allow []string) ([]tools.Tool, error) {
	filtered := make([]string, 0, len(allow))
	for _, a := range allow {
		switch strings.TrimSpace(a) {
		case toolset.GroupFiles, toolset.GroupTerminal, toolset.GroupSkills:
			continue
		default:
			filtered = append(filtered, a)
		}
	}
	ts, err := toolset.Resolve(filtered, nil, toolset.Deps{Registry: registry})
	if err != nil {
		return nil, err
	}
	out := ts[:0]
	for _, t := range ts {
		if bridgeUnsafe(t.Name()) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// bridgeUnsafe reports whether a tool must never be exposed to an external ACP
// agent regardless of the allowlist. The setup.* family writes Murtaugh's own
// config files (slack.yaml, .env, agents.yaml); handing those to an outside
// agent would let it reconfigure the host.
func bridgeUnsafe(name string) bool {
	return name == "setup" || strings.HasPrefix(name, "setup.")
}

// mcpApprover adapts a native.Approver into the mcp.Approver the aggregator
// expects. Both have the same method, so this just reuses the gateway's existing
// GateApprover (the same Slack approval gate the native loop uses). A nil inner
// approver means ungated.
type mcpApprover struct{ inner native.Approver }

func (m mcpApprover) Approve(ctx context.Context, toolName, summary string) (bool, string) {
	if m.inner == nil {
		return true, ""
	}
	return m.inner.Approve(ctx, toolName, summary)
}
