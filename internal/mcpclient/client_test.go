package mcpclient

import (
	"context"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/murtaugh/internal/tools"
)

// echoInput is the typed argument of the fake server's "echo" tool. Using a
// typed AddTool makes the SDK infer and publish a real JSON schema, exercising
// the convertSchema round-trip on the client side.
type echoInput struct {
	Message string `json:"message" jsonschema:"the text to echo back"`
}

// newFakeServer builds an in-process MCP server exposing one "echo" tool and a
// "boom" tool that always reports an error, then returns a Client connected to
// it over the SDK's in-memory transport. The returned cleanup closes both ends.
func newFakeServer(t *testing.T) (*Client, func()) {
	t.Helper()
	ctx := context.Background()

	server := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "fake",
		Version: "0.0.1",
	}, nil)

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "echo",
		Description: "Echo the message back.",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, in echoInput) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo: " + in.Message}},
		}, nil, nil
	})

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "boom",
		Description: "Always fails.",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{
			IsError: true,
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "kaboom"}},
		}, nil, nil
	})

	serverT, clientT := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}

	c, err := connectWith(ctx, ServerConfig{Name: "fake"}, clientT)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}

	cleanup := func() {
		_ = c.Close()
		_ = ss.Close()
	}
	return c, cleanup
}

func TestListTools_PrefixesAndSchema(t *testing.T) {
	c, cleanup := newFakeServer(t)
	defer cleanup()

	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}

	byName := map[string]bool{}
	for _, tl := range tools {
		byName[tl.Name()] = true
	}
	if !byName["fake__echo"] {
		t.Errorf("missing prefixed tool fake__echo; got %v", byName)
	}
	if !byName["fake__boom"] {
		t.Errorf("missing prefixed tool fake__boom; got %v", byName)
	}

	// The echo tool's schema should round-trip into a *jsonschema.Schema with
	// the "message" property declared.
	echo := findTool(tools, "fake__echo")
	if echo == nil {
		t.Fatal("fake__echo not found")
	}
	if echo.Description() != "Echo the message back." {
		t.Errorf("description = %q", echo.Description())
	}
	schema := echo.InputSchema()
	if schema == nil {
		t.Fatal("echo schema is nil; expected a converted object schema")
	}
	if _, ok := schema.Properties["message"]; !ok {
		t.Errorf("echo schema missing 'message' property; props=%v", schema.Properties)
	}
}

func TestInvoke_RoundTrip(t *testing.T) {
	c, cleanup := newFakeServer(t)
	defer cleanup()

	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	echo := findTool(tools, "fake__echo")
	if echo == nil {
		t.Fatal("fake__echo not found")
	}

	out, err := echo.Invoke(context.Background(), map[string]any{"message": "hello"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	got, ok := out.(string)
	if !ok {
		t.Fatalf("result type = %T, want string", out)
	}
	if got != "echo: hello" {
		t.Errorf("result = %q, want %q", got, "echo: hello")
	}
}

func TestInvoke_ToolError(t *testing.T) {
	c, cleanup := newFakeServer(t)
	defer cleanup()

	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	boom := findTool(tools, "fake__boom")
	if boom == nil {
		t.Fatal("fake__boom not found")
	}

	_, err = boom.Invoke(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an error from a tool reporting IsError")
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Errorf("error %q does not contain server message 'kaboom'", err)
	}
}

func findTool(ts []tools.Tool, name string) tools.Tool {
	for _, t := range ts {
		if t.Name() == name {
			return t
		}
	}
	return nil
}
