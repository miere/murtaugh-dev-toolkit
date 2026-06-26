package agent

import (
	"context"
	"os"
	"testing"
	"time"
)

type fakeAggregator struct {
	spec       MCPServerSpec
	err        error
	registered int
	released   int
}

func (f *fakeAggregator) RegisterSession(SessionMetadata) (MCPServerSpec, func(), error) {
	if f.err != nil {
		return MCPServerSpec{}, nil, f.err
	}
	f.registered++
	return f.spec, func() { f.released++ }, nil
}

func TestAggregatorServersEmitsBridgeServer(t *testing.T) {
	fake := &fakeAggregator{spec: MCPServerSpec{
		Name:    "murtaugh",
		Command: "/bin/murtaugh",
		Args:    []string{"mcp-bridge"},
		Env:     map[string]string{"MURTAUGH_BRIDGE_TOKEN": "tok", "MURTAUGH_BRIDGE_SOCKET": "/run/s"},
	}}
	c := NewProcessClient(ProcessOptions{Aggregator: fake})

	servers, release := c.aggregatorServers(SessionMetadata{})
	if len(servers) != 1 {
		t.Fatalf("expected one mcp server, got %d", len(servers))
	}
	srv, ok := servers[0].(map[string]any)
	if !ok {
		t.Fatalf("server is not a map: %T", servers[0])
	}
	if srv["name"] != "murtaugh" || srv["command"] != "/bin/murtaugh" {
		t.Fatalf("unexpected server name/command: %+v", srv)
	}
	env, ok := srv["env"].([]map[string]string)
	if !ok || len(env) != 2 {
		t.Fatalf("env is not a 2-entry array: %#v", srv["env"])
	}
	// Stable, sorted key order: SOCKET sorts before TOKEN.
	if env[0]["name"] != "MURTAUGH_BRIDGE_SOCKET" || env[0]["value"] != "/run/s" {
		t.Fatalf("env[0] = %+v, want the socket first", env[0])
	}
	if env[1]["name"] != "MURTAUGH_BRIDGE_TOKEN" || env[1]["value"] != "tok" {
		t.Fatalf("env[1] = %+v, want the token second", env[1])
	}

	if release == nil {
		t.Fatal("expected a non-nil release")
	}
	release()
	if fake.released != 1 {
		t.Fatalf("release did not run the aggregator cleanup (released=%d)", fake.released)
	}
}

func TestAggregatorServersEmptyWithoutAggregator(t *testing.T) {
	c := NewProcessClient(ProcessOptions{})
	servers, release := c.aggregatorServers(SessionMetadata{})
	if len(servers) != 0 {
		t.Fatalf("expected no servers without an aggregator, got %d", len(servers))
	}
	if release != nil {
		t.Fatal("expected nil release without an aggregator")
	}
}

func TestAggregatorServersSwallowsRegistrationError(t *testing.T) {
	fake := &fakeAggregator{err: context.DeadlineExceeded}
	c := NewProcessClient(ProcessOptions{Aggregator: fake})
	servers, release := c.aggregatorServers(SessionMetadata{})
	if len(servers) != 0 || release != nil {
		t.Fatal("a registration error must yield no servers and no release (agent just gets no tools)")
	}
}

func TestNewSessionRegistersAndCloseReleases(t *testing.T) {
	fake := &fakeAggregator{spec: MCPServerSpec{
		Name:    "murtaugh",
		Command: os.Args[0],
		Args:    []string{"mcp-bridge"},
		Env:     map[string]string{"MURTAUGH_BRIDGE_SOCKET": "/run/s", "MURTAUGH_BRIDGE_TOKEN": "t"},
	}}
	client := NewProcessClient(ProcessOptions{
		Command:    os.Args[0],
		Args:       []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper"},
		Aggregator: fake,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := client.NewSession(ctx, SessionMetadata{TeamID: "T1"}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if fake.registered != 1 {
		t.Fatalf("expected one registration, got %d", fake.registered)
	}
	if fake.released != 0 {
		t.Fatalf("session should not be released before Close (released=%d)", fake.released)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fake.released != 1 {
		t.Fatalf("Close did not release the registered session (released=%d)", fake.released)
	}
}
