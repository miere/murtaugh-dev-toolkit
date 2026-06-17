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
	"strings"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/agent/native"
	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

// Deps carries the shared context needed to build either backend. Registry and
// MCPServers are only consulted for native agents; BaseDir is the workdir
// fallback (both kinds) and the skills/system-prompt-file root (native).
type Deps struct {
	Registry   *tools.Registry
	MCPServers map[string]config.MCPServerConfig
	BaseDir    string
	Logger     *slog.Logger
}

// Client builds the backend for profile. It does no network/process I/O — both
// backends defer that to Initialize.
func Client(profile config.AgentProfile, deps Deps) (agent.Client, error) {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	switch profile.ResolvedKind() {
	case config.AgentKindNative:
		return native.Build(profile, native.BuildDeps{
			Registry:   deps.Registry,
			MCPServers: deps.MCPServers,
			BaseDir:    deps.BaseDir,
			Logger:     logger,
		})
	case config.AgentKindACP:
		workDir := strings.TrimSpace(profile.WorkDir)
		if workDir == "" {
			workDir = deps.BaseDir
		}
		return agent.NewProcessClient(agent.ProcessOptions{
			Command: profile.Command,
			Args:    profile.Args,
			WorkDir: workDir,
			Env:     profile.EnvOverrides(),
			Logger:  logger,
		}), nil
	default:
		return nil, fmt.Errorf("agentbuild: unknown agent kind %q", profile.ResolvedKind())
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
