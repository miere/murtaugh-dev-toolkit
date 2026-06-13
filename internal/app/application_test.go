package app

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRegistry_ContainsAllExpectedTools is the composition-root smoke test:
// it asserts that New wires up every tool the spec expects, with the right
// names and required-field schemas. Drift here means the binary ships with
// missing or mis-named tools.
func TestRegistry_ContainsAllExpectedTools(t *testing.T) {
	app := New(ModeCLI, nil, config.Config{}, "/tmp/slack.yaml", "v0.0.0-test", discardLogger(), nil)

	cases := []struct {
		name     string
		required []string
	}{
		{"ping", nil},
		{"jobs.run", []string{"name"}},
		{"jobs.define", []string{"name", "command"}},
		{"setup.bootstrap", nil},
		{"setup.slack", []string{"app_token", "bot_token", "admin_user"}},
		{"setup.agents", []string{}},
		{"setup.mcp-register", []string{"client", "binary_path"}},
		{"setup.launchd", []string{"binary_path"}},
		{"setup.update", []string{}},
		{"journal.query", []string{}},
		{"journal.stats", nil},
		{"journal.prune", nil},
	}

	for _, c := range cases {
		tool, ok := app.Registry().Get(c.name)
		if !ok {
			t.Errorf("registry missing %q", c.name)
			continue
		}
		schema := tool.InputSchema()
		if c.required == nil {
			if schema != nil {
				t.Errorf("%s: InputSchema = %+v, want nil", c.name, schema)
			}
			continue
		}
		if schema == nil || schema.Type != "object" {
			t.Errorf("%s: InputSchema type = %+v, want object", c.name, schema)
			continue
		}
		got := map[string]bool{}
		for _, r := range schema.Required {
			got[r] = true
		}
		for _, want := range c.required {
			if !got[want] {
				t.Errorf("%s: required missing %q (have %v)", c.name, want, schema.Required)
			}
		}
	}
}

// TestUsageLine_ListsFlatToolsNamespacesAndModes guards the bare-invocation
// help line. Regressions there mask missing tools or missing entry points.
func TestUsageLine_ListsFlatToolsNamespacesAndModes(t *testing.T) {
	line := New(ModeCLI, nil, config.Config{}, "/tmp/slack.yaml", "v0.0.0-test", discardLogger(), nil).UsageLine()

	for _, want := range []string{
		"ping",
		"jobs <define|run>",
		"setup <agents|bootstrap|launchd|mcp-register|slack|update>",
		"slack <fetch-msgs|fetch-reactions|gateway|send-msg|update-msg>",
		"mcp",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("UsageLine missing %q in:\n%s", want, line)
		}
	}
	if !strings.HasPrefix(line, "usage: murtaugh <command>; commands: ") {
		t.Errorf("UsageLine prefix wrong: %q", line)
	}
}
