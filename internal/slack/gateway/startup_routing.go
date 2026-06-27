package gateway

import (
	"context"
	"fmt"
	"sort"

	"github.com/miere/murtaugh-dev-toolkit/internal/journal"
)

// startupAgent is one configured agent in the startup summary.
type startupAgent struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`             // native | acp
	Detail string `json:"detail,omitempty"` // model (native) or command (acp)
	Ready  bool   `json:"ready"`            // built successfully (will answer)
}

// startupRoute is one channel→agent routing entry in the startup summary.
type startupRoute struct {
	Key   string `json:"key"`   // channel ID, name, or name glob
	Agent string `json:"agent"` // the agent that key routes to
	Ready bool   `json:"ready"` // the target agent built successfully
}

// startupSummary is the at-startup picture of agents and chat routing. It is
// logged line-by-line and recorded as one journal event so a misroute or a
// failed-to-build agent is visible (and correlatable) without reproducing it.
type startupSummary struct {
	ChatEnabled  bool           `json:"chat_enabled"`
	DefaultAgent string         `json:"default_agent,omitempty"`
	DMAgent      string         `json:"dm_agent,omitempty"`
	Agents       []startupAgent `json:"agents"`
	Routes       []startupRoute `json:"channel_routes"`
	Problems     []string       `json:"problems,omitempty"`
}

// buildStartupSummary assembles the agents + routing picture from the captured
// config and the agents that actually built (chatSessions). An agent that is
// configured but absent from chatSessions failed to build and will not answer;
// a route or default/dm agent pointing at such an agent is flagged as a problem.
func (a *Gateway) buildStartupSummary() startupSummary {
	chatEnabled := a.chatSessions != nil
	built := func(name string) bool {
		if name == "" {
			return false
		}
		_, ok := a.chatSessions[name]
		return ok
	}

	s := startupSummary{
		ChatEnabled:  chatEnabled,
		DefaultAgent: a.chatRouting.DefaultAgent,
		DMAgent:      a.chatRouting.DMAgent,
	}

	for _, name := range sortedKeys(a.agentProfiles) {
		p := a.agentProfiles[name]
		var detail string
		if p.ACP != nil {
			detail = p.ACP.Command
		} else if p.Native != nil {
			detail = p.Native.Model
		}
		ready := built(name)
		s.Agents = append(s.Agents, startupAgent{Name: name, Kind: string(p.ResolvedKind()), Detail: detail, Ready: ready})
		if chatEnabled && !ready {
			s.Problems = append(s.Problems, fmt.Sprintf("agent %q is configured but failed to build — it will not answer (check its build error above)", name))
		}
	}

	for _, key := range sortedKeys(a.chatRouting.ChannelAgents) {
		target := a.chatRouting.ChannelAgents[key]
		ready := built(target)
		s.Routes = append(s.Routes, startupRoute{Key: key, Agent: target, Ready: ready})
		if chatEnabled && !ready {
			s.Problems = append(s.Problems, fmt.Sprintf("channel route %q → %q: that agent is not available, so mentions in this channel fall back to the default", key, target))
		}
	}

	if chatEnabled {
		if a.chatRouting.DefaultAgent != "" && !built(a.chatRouting.DefaultAgent) {
			s.Problems = append(s.Problems, fmt.Sprintf("default_agent %q is not available", a.chatRouting.DefaultAgent))
		}
		if a.chatRouting.DMAgent != "" && !built(a.chatRouting.DMAgent) {
			s.Problems = append(s.Problems, fmt.Sprintf("dm_agent %q is not available", a.chatRouting.DMAgent))
		}
	}
	return s
}

// logStartupRouting logs the agents + chat-routing picture at daemon start and
// records it as one journal event (gateway stream, kind "startup.routing"). It
// makes "which channels route to which agent" and "which configured agent did
// not come up" visible at a glance, so a misroute or a failed build can be
// correlated with later errors instead of inferred from silence.
func (a *Gateway) logStartupRouting(ctx context.Context) {
	s := a.buildStartupSummary()

	if !s.ChatEnabled {
		a.logger.Warn("startup routing: chat is disabled (agent.enabled: false) — DMs and mentions are ignored; no agent will answer")
	}
	a.logger.Info("startup routing: agents configured", "count", len(s.Agents), "chat_enabled", s.ChatEnabled)
	for _, ag := range s.Agents {
		status := "ready"
		switch {
		case !s.ChatEnabled:
			status = "configured (chat disabled)"
		case !ag.Ready:
			status = "DISABLED — failed to build"
		}
		a.logger.Info("startup routing: agent", "name", ag.Name, "kind", ag.Kind, "detail", ag.Detail, "status", status)
	}

	a.logger.Info("startup routing: chat routing",
		"default_agent", s.DefaultAgent, "dm_agent", s.DMAgent, "channel_routes", len(s.Routes))
	for _, r := range s.Routes {
		if s.ChatEnabled && !r.Ready {
			a.logger.Warn("startup routing: channel route → unavailable agent", "channel", r.Key, "agent", r.Agent)
		} else {
			a.logger.Info("startup routing: channel route", "channel", r.Key, "agent", r.Agent)
		}
	}
	for _, p := range s.Problems {
		a.logger.Warn("startup routing: problem", "detail", p)
	}

	level := journal.LevelInfo
	if !s.ChatEnabled || len(s.Problems) > 0 {
		level = journal.LevelWarn
	}
	a.record(ctx, "startup.routing", level, "gateway startup routing summary", journal.Keys{}, s)
}

// sortedKeys returns the keys of m sorted, for deterministic log/journal order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
