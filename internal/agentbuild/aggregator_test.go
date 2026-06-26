package agentbuild

import (
	"context"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/mcpbridge"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

type fakeTool struct{ name string }

func (f fakeTool) Name() string                                        { return f.name }
func (f fakeTool) Description() string                                 { return "fake" }
func (f fakeTool) InputSchema() *jsonschema.Schema                     { return nil }
func (f fakeTool) Invoke(context.Context, map[string]any) (any, error) { return "ok", nil }

func registryWith(names ...string) *tools.Registry {
	reg := tools.NewRegistry()
	for _, n := range names {
		reg.Register(fakeTool{name: n})
	}
	return reg
}

func toolNames(ts []tools.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

func TestResolveBuiltinsCuratesAndStripsGroups(t *testing.T) {
	reg := registryWith("ping", "ask", "slack.send-msg", "slack.fetch-msgs", "setup.env", "restart")
	// Allow a namespace (slack), an exact tool (ask), a curated-out namespace
	// (setup), and a native-only group (files) that must not synthesize anything.
	got, err := resolveBuiltins(reg, []string{"ask", "slack", "setup", "files"})
	if err != nil {
		t.Fatalf("resolveBuiltins: %v", err)
	}
	names := toolNames(got)
	for _, want := range []string{"ask", "slack.send-msg", "slack.fetch-msgs"} {
		if !slices.Contains(names, want) {
			t.Fatalf("expected %q in resolved set, got %v", want, names)
		}
	}
	if slices.Contains(names, "setup.env") {
		t.Fatalf("setup.* must be curated out of the bridge surface, got %v", names)
	}
	if slices.Contains(names, "ping") {
		t.Fatalf("ping was not allowed, should be absent, got %v", names)
	}
	// "files" is a native-only synthesized group; nothing should appear for it.
	for _, n := range names {
		if n == "files" {
			t.Fatalf("the files group must not be served to an ACP agent, got %v", names)
		}
	}
}

func TestBridgeUnsafe(t *testing.T) {
	for _, n := range []string{"setup", "setup.env", "setup.slack"} {
		if !bridgeUnsafe(n) {
			t.Fatalf("%q should be bridge-unsafe", n)
		}
	}
	for _, n := range []string{"ask", "slack.send-msg", "setupx", "restart"} {
		if bridgeUnsafe(n) {
			t.Fatalf("%q should be bridge-safe", n)
		}
	}
}

func TestACPAggregatorRegisterSession(t *testing.T) {
	reg := registryWith("ask")
	srv := mcpbridge.NewServer("/tmp/murtaugh-test-agg.sock", nil)
	aggr, err := newACPAggregator(srv, reg, []string{"ask"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("newACPAggregator: %v", err)
	}

	spec, release, err := aggr.RegisterSession(agent.SessionMetadata{ChannelID: "C1", ThreadTS: "1.2"})
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	if spec.Name != "murtaugh" || len(spec.Args) != 1 || spec.Args[0] != mcpbridge.Subcommand {
		t.Fatalf("unexpected spec command shape: %+v", spec)
	}
	if spec.Env[mcpbridge.EnvSocket] != srv.SocketPath() {
		t.Fatalf("spec env socket = %q, want %q", spec.Env[mcpbridge.EnvSocket], srv.SocketPath())
	}
	if spec.Env[mcpbridge.EnvToken] == "" {
		t.Fatal("spec env is missing the session token")
	}
	if release == nil {
		t.Fatal("expected a non-nil release")
	}
	release() // must not panic; drops the token
}

func TestACPAggregatorToolsetAndClose(t *testing.T) {
	reg := registryWith("ask", "slack.send-msg")
	srv := mcpbridge.NewServer("/tmp/murtaugh-test-agg2.sock", nil)
	// No external MCP servers configured: the toolset is just the built-ins.
	aggr, err := newACPAggregator(srv, reg, []string{"ask", "slack"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("newACPAggregator: %v", err)
	}
	// Close before any session opened the manager must be a safe no-op.
	if err := aggr.Close(); err != nil {
		t.Fatalf("Close before use: %v", err)
	}
	got := toolNames(aggr.resolvedToolset())
	if len(got) != 2 || !slices.Contains(got, "ask") || !slices.Contains(got, "slack.send-msg") {
		t.Fatalf("resolved toolset = %v, want the two built-ins", got)
	}
	// Close after the (empty) manager opened must also succeed.
	if err := aggr.Close(); err != nil {
		t.Fatalf("Close after use: %v", err)
	}
}
