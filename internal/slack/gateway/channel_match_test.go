package gateway

import (
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

func agentChannels(m map[string]string) map[string]config.ChannelConfig {
	out := make(map[string]config.ChannelConfig, len(m))
	for k, v := range m {
		out[k] = config.ChannelConfig{Agent: v}
	}
	return out
}

func TestMatchChannel(t *testing.T) {
	channels := agentChannels(map[string]string{
		"C123":            "id-agent",      // exact channel-ID key
		"general":         "general-agent", // exact channel-name key
		"feature-*":       "feature-agent",
		"feature-prod-*":  "feature-prod-agent",
		"*-prod":          "prod-agent",
		"release-?":       "release-agent", // ? is a valid path.Match glob too
		"design-channel*": "design-agent",
	})

	tests := []struct {
		name        string
		channelID   string
		channelName string
		wantAgent   string
		wantOK      bool
	}{
		{
			name:      "exact channel-ID key wins over everything",
			channelID: "C123", channelName: "feature-anything",
			wantAgent: "id-agent", wantOK: true,
		},
		{
			name:      "exact channel-name key",
			channelID: "C999", channelName: "general",
			wantAgent: "general-agent", wantOK: true,
		},
		{
			name:      "single-star glob on name",
			channelID: "C999", channelName: "feature-login",
			wantAgent: "feature-agent", wantOK: true,
		},
		{
			name:      "longest-literal-prefix glob wins the tie-break",
			channelID: "C999", channelName: "feature-prod-deploy",
			wantAgent: "feature-prod-agent", wantOK: true,
		},
		{
			name:      "suffix glob on name",
			channelID: "C999", channelName: "anything-prod",
			wantAgent: "prod-agent", wantOK: true,
		},
		{
			name:      "no match falls through to default (ok=false)",
			channelID: "C999", channelName: "random",
			wantAgent: "", wantOK: false,
		},
		{
			name:      "empty name with no id match cannot glob-match",
			channelID: "C999", channelName: "",
			wantAgent: "", wantOK: false,
		},
		{
			name:      "empty name still matches an exact channel-ID key",
			channelID: "C123", channelName: "",
			wantAgent: "id-agent", wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotOK := matchChannel(tt.channelID, tt.channelName, channels)
			if got.Agent != tt.wantAgent || gotOK != tt.wantOK {
				t.Fatalf("matchChannel(%q, %q) = (%q, %v), want (%q, %v)",
					tt.channelID, tt.channelName, got.Agent, gotOK, tt.wantAgent, tt.wantOK)
			}
		})
	}
}

func TestMatchChannelEmptyChannels(t *testing.T) {
	if cc, ok := matchChannel("C123", "general", nil); ok || cc.Agent != "" {
		t.Fatalf("nil channels: got (%q, %v), want (\"\", false)", cc.Agent, ok)
	}
}

// TestMatchChannelReplyOnlyEntry covers an entry that overrides only the reply
// strategy: it matches (ok=true) with an empty Agent, so the caller falls back to
// the default agent while still honouring reply_on_thread.
func TestMatchChannelReplyOnlyEntry(t *testing.T) {
	off := false
	channels := map[string]config.ChannelConfig{
		"support-*": {ReplyOnThread: &off},
	}
	cc, ok := matchChannel("C1", "support-eu", channels)
	if !ok {
		t.Fatalf("expected a match for support-eu")
	}
	if cc.Agent != "" {
		t.Fatalf("expected empty agent (fall back to default), got %q", cc.Agent)
	}
	if cc.ReplyOnThread == nil || *cc.ReplyOnThread {
		t.Fatalf("expected reply_on_thread=false on the matched entry, got %v", cc.ReplyOnThread)
	}
}

// TestMatchChannelPrecedenceIsScored guards the documented precedence:
// exact-name must beat a glob that also matches the same name, regardless of
// Go's (unordered) map iteration. Running it repeatedly makes a flaky
// iteration-order dependency surface.
func TestMatchChannelPrecedenceIsScored(t *testing.T) {
	channels := agentChannels(map[string]string{
		"feature-x": "exact-name-agent",
		"feature-*": "glob-agent",
	})
	for i := 0; i < 100; i++ {
		if cc, ok := matchChannel("C1", "feature-x", channels); !ok || cc.Agent != "exact-name-agent" {
			t.Fatalf("iteration %d: got (%q, %v), want exact-name-agent to win", i, cc.Agent, ok)
		}
	}
}

func TestLiteralPrefixLen(t *testing.T) {
	tests := map[string]int{
		"feature-*":      8,
		"*-prod":         0,
		"feature-prod-*": 13,
		"no-glob":        7,
	}
	for pattern, want := range tests {
		if got := literalPrefixLen(pattern); got != want {
			t.Errorf("literalPrefixLen(%q) = %d, want %d", pattern, got, want)
		}
	}
}

func TestValidChannelAgentGlob(t *testing.T) {
	valid := []string{"C123", "general", "feature-*", "*-prod", "release-?", "[a-z]*"}
	for _, k := range valid {
		if !validChannelAgentGlob(k) {
			t.Errorf("validChannelAgentGlob(%q) = false, want true", k)
		}
	}
	// An unterminated character class is a malformed path.Match pattern.
	if validChannelAgentGlob("feature-[a-*") {
		t.Errorf("validChannelAgentGlob(%q) = true, want false (malformed glob)", "feature-[a-*")
	}
}
