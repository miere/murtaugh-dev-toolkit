package gateway

import (
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

func TestBuildStartupSummary_FlagsUnavailableTargets(t *testing.T) {
	a := &Gateway{
		agentProfiles: map[string]config.AgentProfile{
			"default": {Native: &config.NativeProfile{Provider: "gemini", Model: "gemini-2.5-pro"}},
			"broken":  {ACP: &config.ACPProfile{Command: "/x"}},
		},
		// Only "default" built; "broken" failed (absent = not built).
		chatSessions: map[string]ChatSessionManager{"default": nil},
		chatRouting: config.ChatConfig{
			DefaultAgent:  "default",
			DMAgent:       "support", // configured nowhere → unavailable
			ChannelAgents: map[string]string{"C0ENG1": "broken", "support-*": "default"},
		},
	}

	s := a.buildStartupSummary()

	if !s.ChatEnabled {
		t.Fatal("chat should be enabled when chatSessions is non-nil")
	}
	// Agents sorted: broken (acp, not ready), default (native, ready).
	if len(s.Agents) != 2 || s.Agents[0].Name != "broken" || s.Agents[1].Name != "default" {
		t.Fatalf("unexpected agents: %+v", s.Agents)
	}
	if s.Agents[0].Ready || s.Agents[0].Kind != "acp" || s.Agents[0].Detail != "/x" {
		t.Fatalf("broken agent should be acp/not-ready/detail=/x: %+v", s.Agents[0])
	}
	if !s.Agents[1].Ready || s.Agents[1].Kind != "native" || s.Agents[1].Detail != "gemini-2.5-pro" {
		t.Fatalf("default agent should be native/ready/detail=model: %+v", s.Agents[1])
	}
	// Routes sorted: C0ENG1 → broken (not ready), support-* → default (ready).
	if len(s.Routes) != 2 || s.Routes[0].Key != "C0ENG1" || s.Routes[0].Ready {
		t.Fatalf("C0ENG1 route should target an unavailable agent: %+v", s.Routes)
	}
	if !s.Routes[1].Ready {
		t.Fatalf("support-* route should be ready: %+v", s.Routes[1])
	}
	// Problems: broken failed to build, C0ENG1→broken unavailable, dm_agent unavailable.
	joined := strings.Join(s.Problems, "\n")
	for _, want := range []string{`agent "broken"`, `channel route "C0ENG1"`, `dm_agent "support"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected a problem mentioning %s; got:\n%s", want, joined)
		}
	}
	// default_agent is built → must NOT be a problem.
	if strings.Contains(joined, "default_agent") {
		t.Errorf("default_agent is available and should not be flagged; got:\n%s", joined)
	}
}

func TestBuildStartupSummary_ChatDisabled(t *testing.T) {
	a := &Gateway{
		agentProfiles: map[string]config.AgentProfile{
			"default": {Native: &config.NativeProfile{Model: "m"}},
		},
		chatSessions: nil, // chat disabled (acp.enabled: false)
		chatRouting:  config.ChatConfig{DefaultAgent: "default"},
	}

	s := a.buildStartupSummary()

	if s.ChatEnabled {
		t.Fatal("chat should be disabled when chatSessions is nil")
	}
	if len(s.Agents) != 1 {
		t.Fatalf("agents should still be listed when chat is disabled: %+v", s.Agents)
	}
	// Nothing answers when chat is off, so we don't flag per-agent/route build problems.
	if len(s.Problems) != 0 {
		t.Fatalf("no build/route problems should be flagged while chat is disabled: %v", s.Problems)
	}
}
