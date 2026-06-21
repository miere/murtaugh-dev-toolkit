package gateway

import "testing"

func TestMatchChannelAgent(t *testing.T) {
	patterns := map[string]string{
		"C123":            "id-agent",      // exact channel-ID key
		"general":         "general-agent", // exact channel-name key
		"feature-*":       "feature-agent",
		"feature-prod-*":  "feature-prod-agent",
		"*-prod":          "prod-agent",
		"release-?":       "release-agent", // ? is a valid path.Match glob too
		"design-channel*": "design-agent",
	}

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
			gotAgent, gotOK := matchChannelAgent(tt.channelID, tt.channelName, patterns)
			if gotAgent != tt.wantAgent || gotOK != tt.wantOK {
				t.Fatalf("matchChannelAgent(%q, %q) = (%q, %v), want (%q, %v)",
					tt.channelID, tt.channelName, gotAgent, gotOK, tt.wantAgent, tt.wantOK)
			}
		})
	}
}

func TestMatchChannelAgentEmptyPatterns(t *testing.T) {
	if agent, ok := matchChannelAgent("C123", "general", nil); ok || agent != "" {
		t.Fatalf("nil patterns: got (%q, %v), want (\"\", false)", agent, ok)
	}
}

// TestMatchChannelAgentPrecedenceIsScored guards the documented precedence:
// exact-name must beat a glob that also matches the same name, regardless of
// Go's (unordered) map iteration. Running it repeatedly makes a flaky
// iteration-order dependency surface.
func TestMatchChannelAgentPrecedenceIsScored(t *testing.T) {
	patterns := map[string]string{
		"feature-x": "exact-name-agent",
		"feature-*": "glob-agent",
	}
	for i := 0; i < 100; i++ {
		if agent, ok := matchChannelAgent("C1", "feature-x", patterns); !ok || agent != "exact-name-agent" {
			t.Fatalf("iteration %d: got (%q, %v), want exact-name-agent to win", i, agent, ok)
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
