package mcpclient

import (
	"context"
	"reflect"
	"slices"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

func TestManager_IsolatesConnectFailures(t *testing.T) {
	// Two servers configured to fail in different ways. Open must not panic or
	// return an error; it returns an empty Manager with zero tools.
	cfgs := []ServerConfig{
		{Name: "no-transport"}, // neither command nor url
		{Name: "bad-cmd", Command: "murtaugh-nonexistent-binary"}, // command will fail to start
	}
	m := Open(context.Background(), cfgs, nil)
	defer m.Close()

	if len(m.Tools()) != 0 {
		t.Errorf("expected 0 tools from failing servers, got %d", len(m.Tools()))
	}
	if len(m.clients) != 0 {
		t.Errorf("expected 0 live clients, got %d", len(m.clients))
	}
}

func TestManager_AggregatesAndCloses(t *testing.T) {
	// Stand up two in-memory servers, build their Clients via the test seam, and
	// assemble a Manager by hand to verify Tools() aggregation and Close().
	c1, ss1 := fakeServerNamed(t, "alpha")
	c2, ss2 := fakeServerNamed(t, "beta")
	defer ss1.Close()
	defer ss2.Close()

	m := &Manager{logger: nil}
	for _, c := range []*Client{c1, c2} {
		ts, err := c.ListTools(context.Background())
		if err != nil {
			t.Fatalf("ListTools(%s): %v", c.Name(), err)
		}
		m.clients = append(m.clients, c)
		m.tools = append(m.tools, ts...)
	}
	// fallback logger so Close doesn't nil-deref.
	m.logger = Open(context.Background(), nil, nil).logger

	names := toolNames(m.Tools())
	slices.Sort(names)
	want := []string{"alpha__echo", "beta__echo"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("tool names = %v, want %v", names, want)
	}

	if err := m.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if len(m.clients) != 0 {
		t.Errorf("clients not cleared after Close: %d", len(m.clients))
	}
}

// fakeServerNamed builds an in-memory server exposing a single "echo" tool and
// returns a Client connected to it under the given server name.
func fakeServerNamed(t *testing.T, name string) (*Client, *mcpsdk.ServerSession) {
	t.Helper()
	ctx := context.Background()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: name, Version: "0.0.1"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "echo"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in echoInput) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: in.Message}},
			}, nil, nil
		})
	serverT, clientT := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	c, err := connectWith(ctx, ServerConfig{Name: name}, clientT)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	return c, ss
}

func toolNames(ts []tools.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

func TestServerConfig_TransportSelection(t *testing.T) {
	if _, err := (ServerConfig{Name: "x"}).transport(); err == nil {
		t.Error("expected error when neither command nor url set")
	}
	if _, err := (ServerConfig{Name: "x", Command: "c", URL: "u"}).transport(); err == nil {
		t.Error("expected error when both command and url set")
	}
	st, err := (ServerConfig{Name: "x", Command: "echo"}).transport()
	if err != nil {
		t.Fatalf("stdio transport: %v", err)
	}
	if _, ok := st.(*mcpsdk.CommandTransport); !ok {
		t.Errorf("stdio transport type = %T, want *CommandTransport", st)
	}
	rt, err := (ServerConfig{Name: "x", URL: "https://example/mcp"}).transport()
	if err != nil {
		t.Fatalf("remote transport: %v", err)
	}
	if _, ok := rt.(*mcpsdk.StreamableClientTransport); !ok {
		t.Errorf("remote transport type = %T, want *StreamableClientTransport", rt)
	}
}

func TestMergeEnv_OverrideWins(t *testing.T) {
	base := []string{"FOO=1", "BAR=2"}
	got := mergeEnv(base, map[string]string{"FOO": "9", "BAZ": "3"})
	if !slices.Contains(got, "FOO=9") {
		t.Errorf("override not applied: %v", got)
	}
	if slices.Contains(got, "FOO=1") {
		t.Errorf("base value not removed: %v", got)
	}
	if !slices.Contains(got, "BAR=2") {
		t.Errorf("untouched base var missing: %v", got)
	}
	if !slices.Contains(got, "BAZ=3") {
		t.Errorf("new var missing: %v", got)
	}
}
