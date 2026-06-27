package config

import (
	"strings"
	"testing"
)

func TestEffectiveProgressDisplayDefaultsToSimplified(t *testing.T) {
	cfg := Config{Agents: map[string]AgentProfile{"coder": {ACP: &ACPProfile{Command: "x"}}}}
	if got := cfg.EffectiveProgressDisplay("coder"); got != ProgressDisplaySimplified {
		t.Fatalf("expected simplified by default, got %q", got)
	}
	// An unknown agent also defaults to simplified.
	if got := cfg.EffectiveProgressDisplay("missing"); got != ProgressDisplaySimplified {
		t.Fatalf("expected simplified for unknown agent, got %q", got)
	}
}

func TestEffectiveProgressDisplayGlobalDefault(t *testing.T) {
	cfg := Config{
		Defaults: RuntimeDefaults{Rendering: RenderingDefaults{ProgressDisplay: "tasks"}},
		Agents:   map[string]AgentProfile{"coder": {ACP: &ACPProfile{Command: "x"}}},
	}
	if got := cfg.EffectiveProgressDisplay("coder"); got != ProgressDisplayTasks {
		t.Fatalf("expected global tasks default, got %q", got)
	}
}

func TestEffectiveProgressDisplayAgentOverridesGlobal(t *testing.T) {
	cfg := Config{
		Defaults: RuntimeDefaults{Rendering: RenderingDefaults{ProgressDisplay: "tasks"}},
		Agents: map[string]AgentProfile{
			"coder":  {ACP: &ACPProfile{Command: "x"}},                                // inherits global → tasks
			"helper": {ACP: &ACPProfile{Command: "x"}, ProgressDisplay: "simplified"}, // overrides → simplified
		},
	}
	if got := cfg.EffectiveProgressDisplay("coder"); got != ProgressDisplayTasks {
		t.Fatalf("coder should inherit global tasks, got %q", got)
	}
	if got := cfg.EffectiveProgressDisplay("helper"); got != ProgressDisplaySimplified {
		t.Fatalf("helper should override to simplified, got %q", got)
	}
}

func TestProgressDisplayValidationRejectsUnknown(t *testing.T) {
	cfg := Config{
		OAuth:  OAuthConfig{AppToken: "a", BotToken: "b"},
		Agents: map[string]AgentProfile{"coder": {ACP: &ACPProfile{Command: "x"}, ProgressDisplay: "verbose"}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "progress_display") {
		t.Fatalf("expected a progress_display validation error, got %v", err)
	}
}

func TestProgressDisplayValidationRejectsUnknownGlobal(t *testing.T) {
	cfg := Config{
		OAuth:    OAuthConfig{AppToken: "a", BotToken: "b"},
		Defaults: RuntimeDefaults{Rendering: RenderingDefaults{ProgressDisplay: "loud"}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "defaults.rendering.progress_display") {
		t.Fatalf("expected a defaults.rendering.progress_display validation error, got %v", err)
	}
}

func TestProgressDisplayValidationAllowsKnownAndBlank(t *testing.T) {
	cfg := Config{
		OAuth:    OAuthConfig{AppToken: "a", BotToken: "b"},
		Defaults: RuntimeDefaults{Rendering: RenderingDefaults{ProgressDisplay: "tasks"}},
		Agents: map[string]AgentProfile{
			"a": {ACP: &ACPProfile{Command: "x"}, ProgressDisplay: "simplified"},
			"b": {ACP: &ACPProfile{Command: "x"}}, // blank inherits, allowed
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected known/blank values to validate, got %v", err)
	}
}
