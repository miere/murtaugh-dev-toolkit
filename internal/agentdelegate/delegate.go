// Package agentdelegate runs a one-shot, isolated ACP agent session: it spins
// up a fresh agent process, sends a single prompt, waits for the turn to
// finish, and (optionally) collects the agent's full text output.
//
// Unlike the long-lived chat sessions managed by agent.SessionManager, each
// delegation gets its own process and session with no shared conversation
// memory. That isolation is exactly what the config-driven surfaces want: a
// job, a workflow trigger, or an unfurl that just needs an agent to do one
// thing and either render its JSON output or act through its own tools.
//
// Two call shapes sit on top of the shared Run:
//   - RunForJSON — capture the output and require it to be a valid JSON
//     document (a Slack message / attachment). Used where the surface renders
//     the result: a reply-to-slack trigger or an unfurl action.
//   - RunAndForget — discard the output; the agent is expected to act through
//     its own tools/MCP. Used by jobs and top-level workflow triggers.
package agentdelegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/agentbuild"
	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

// ErrNonJSONOutput is returned by RunForJSON when the agent completed its turn
// but its output was not a valid JSON document. The Runner logs a warning with
// the raw output before returning it, so callers should simply skip rendering.
var ErrNonJSONOutput = errors.New("delegate-to-agent: agent output was not valid JSON")

// ClientFactory builds a fresh client for a single one-shot session. Production
// wires the kind-aware agentbuild.Client (ACP or native); tests inject a fake.
type ClientFactory func(profile config.AgentProfile, logger *slog.Logger) agent.Client

// Runner resolves agent profiles by name and drives isolated one-shot sessions.
type Runner struct {
	agents      map[string]config.AgentProfile
	baseDir     string
	idleTimeout time.Duration
	newClient   ClientFactory
	logger      *slog.Logger
	// registry and mcpServers are consulted only when building a native agent:
	// the registry backs its `tools:` allowlist and mcpServers
	// resolves its MCP references. Wired by the composition root via
	// WithBuildContext; nil leaves a native agent with only its synthesized and
	// MCP tools (unresolved MCP refs are skipped).
	registry   *tools.Registry
	mcpServers map[string]config.MCPServerConfig
}

// NewRunner builds a Runner over the configured agents. idleTimeout is taken
// from the ACP request timeout: a turn is bounded by inactivity, not total
// wall-clock, so a long but productive delegation never trips it. baseDir is
// used as the working directory for any agent that leaves workdir unset, so
// delegated agents start where the bundled skills and templates live.
func NewRunner(agents map[string]config.AgentProfile, defaults config.RuntimeDefaults, baseDir string, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	r := &Runner{
		agents:      agents,
		baseDir:     baseDir,
		idleTimeout: defaults.EffectiveRequestTimeout(),
		logger:      logger,
	}
	r.newClient = r.defaultClient
	return r
}

// WithClientFactory overrides how the underlying client is constructed.
// Intended for tests; a nil factory is ignored. Returns the receiver.
func (r *Runner) WithClientFactory(f ClientFactory) *Runner {
	if f != nil {
		r.newClient = f
	}
	return r
}

// WithBuildContext supplies the registry and MCP server definitions a native
// delegated agent needs (its tool allowlist and MCP references). ACP agents
// ignore both. Returns the receiver for fluent wiring.
func (r *Runner) WithBuildContext(registry *tools.Registry, mcpServers map[string]config.MCPServerConfig) *Runner {
	r.registry = registry
	r.mcpServers = mcpServers
	return r
}

// defaultClient builds the backend for a one-shot delegation, branching on the
// profile's kind (ACP or native) via agentbuild. A build error is deferred to
// the client's Initialize (ErrorClient) so the factory keeps its no-error
// signature and the failure surfaces on the same path as a runtime error.
func (r *Runner) defaultClient(profile config.AgentProfile, logger *slog.Logger) agent.Client {
	client, err := agentbuild.Client(profile, agentbuild.Deps{
		Registry:   r.registry,
		MCPServers: r.mcpServers,
		BaseDir:    r.baseDir,
		Logger:     logger,
	})
	if err != nil {
		return agentbuild.ErrorClient(err)
	}
	return client
}

// RunForJSON runs a delegation and requires the agent's output to be a single
// valid JSON document. When the output is not valid JSON it logs a warning
// naming the agent and the raw output, then returns ErrNonJSONOutput so the
// caller can skip rendering without treating it as a hard failure.
func (r *Runner) RunForJSON(ctx context.Context, agentName, prompt string) ([]byte, error) {
	out, err := r.Run(ctx, agentName, prompt)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(out)
	if !json.Valid([]byte(trimmed)) {
		r.logger.Warn("delegate-to-agent expected a JSON response but the agent produced something else; skipping render",
			"agent", agentName, "output", trimmed)
		return nil, ErrNonJSONOutput
	}
	return []byte(trimmed), nil
}

// RunAndForget runs a delegation and discards the agent's text output — the
// agent is expected to act through its own tools/MCP (it may, for example, post
// to Slack itself). Only errors are surfaced; any captured text is logged at
// debug level for troubleshooting.
func (r *Runner) RunAndForget(ctx context.Context, agentName, prompt string) error {
	out, err := r.Run(ctx, agentName, prompt)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		r.logger.Debug("delegate-to-agent discarding fire-and-forget output", "agent", agentName, "bytes", len(out))
	}
	return nil
}

// Run drives one isolated turn: it starts a fresh agent, opens a session, sends
// prompt, and returns the agent's accumulated text once the turn finishes. The
// turn is bounded by inactivity (the ACP request timeout): the idle timer
// resets on every event the agent emits, so a long turn that keeps making
// progress is never killed mid-flight. The agent process is always torn down
// before Run returns.
func (r *Runner) Run(ctx context.Context, agentName, prompt string) (string, error) {
	profile, ok := r.agents[agentName]
	if !ok {
		return "", fmt.Errorf("delegate-to-agent: unknown agent %q", agentName)
	}

	client := r.newClient(profile, r.logger.With("agent", agentName))
	defer func() {
		if err := client.Close(); err != nil {
			r.logger.Warn("delegate-to-agent failed to close agent client", "agent", agentName, "error", err)
		}
	}()

	if err := client.Initialize(ctx); err != nil {
		return "", fmt.Errorf("delegate-to-agent: initialize agent %q: %w", agentName, err)
	}
	session, err := client.NewSession(ctx, agent.SessionMetadata{Source: "delegate"})
	if err != nil {
		return "", fmt.Errorf("delegate-to-agent: create session for agent %q: %w", agentName, err)
	}

	// Drive the prompt under a child context we can cancel ourselves so the
	// idle watchdog can unblock the in-flight request without disturbing ctx.
	promptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := client.Prompt(promptCtx, session.ID, agent.PromptRequest{Text: prompt})
	if err != nil {
		return "", fmt.Errorf("delegate-to-agent: prompt agent %q: %w", agentName, err)
	}

	var buf strings.Builder
	idle := time.NewTimer(r.idleTimeout)
	defer idle.Stop()
	for {
		select {
		case <-idle.C:
			// The agent went silent for the whole idle window. Unblock the
			// in-flight request and drain so the client tears down cleanly.
			cancel()
			for range events {
			}
			return buf.String(), fmt.Errorf("delegate-to-agent: agent %q went idle for %s", agentName, r.idleTimeout)
		case event, ok := <-events:
			if !ok {
				// Channel closed without an explicit completion event: treat the
				// accumulated output as the result.
				return buf.String(), nil
			}
			resetIdleTimer(idle, r.idleTimeout)
			switch event.Type {
			case agent.EventText:
				// Only the agent's reply text is captured. EventStatus is
				// progress/meta (e.g. compaction) and must not pollute the
				// captured output, which a caller may parse as JSON.
				buf.WriteString(event.Text)
			case agent.EventError:
				return buf.String(), fmt.Errorf("delegate-to-agent: agent %q failed: %w", agentName, event.Error)
			case agent.EventComplete:
				return buf.String(), nil
			}
		}
	}
}

// resetIdleTimer restarts t for another idle window, draining an already-fired
// timer first so the next select does not observe a stale tick.
func resetIdleTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
