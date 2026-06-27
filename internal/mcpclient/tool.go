package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/murtaugh/internal/tools"
)

// nameSeparator joins a server name and a remote tool name into the Murtaugh
// tool name. Using "__" (double underscore) mirrors the Goose/MCP convention
// for namespacing tools and stays within the [a-zA-Z0-9_-] character class
// that strict providers (e.g. Gemini) require, so the prefixed name never
// needs further sanitising downstream.
const nameSeparator = "__"

// PrefixedName returns the Murtaugh-facing tool name for a remote tool on the
// named server: "<server>__<tool>". Exported so the wiring/toolset layer can
// reason about provenance without re-deriving the scheme.
func PrefixedName(server, tool string) string {
	return server + nameSeparator + tool
}

// remoteTool adapts one remote MCP tool to the tools.Tool interface. Invoke
// marshals the args map and dispatches CallTool over the shared session,
// returning the concatenated text content of the result.
type remoteTool struct {
	name        string
	description string
	schema      *jsonschema.Schema
	remoteName  string
	session     *mcpsdk.ClientSession
}

// wrapTool builds a tools.Tool from a remote MCP tool descriptor bound to an
// open session. The published name is "<server>__<remote>". The remote tool's
// JSON-Schema input (delivered over the wire as a map[string]any) is converted
// into a *jsonschema.Schema so it round-trips through Murtaugh's frontends
// unchanged; a conversion failure degrades to a nil schema rather than
// dropping the tool.
func wrapTool(server string, session *mcpsdk.ClientSession, t *mcpsdk.Tool) tools.Tool {
	return &remoteTool{
		name:        PrefixedName(server, t.Name),
		description: t.Description,
		schema:      convertSchema(t.InputSchema),
		remoteName:  t.Name,
		session:     session,
	}
}

func (t *remoteTool) Name() string                    { return t.name }
func (t *remoteTool) Description() string             { return t.description }
func (t *remoteTool) InputSchema() *jsonschema.Schema { return t.schema }

// Invoke calls the remote tool with args and returns its text content. A
// tool-level error (IsError on the result) is surfaced as a Go error so the
// agent loop treats it like any other failed tool call.
func (t *remoteTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	res, err := t.session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      t.remoteName,
		Arguments: args,
	})
	if err != nil {
		return nil, fmt.Errorf("mcpclient: call tool %q: %w", t.name, err)
	}
	text := textContent(res)
	if res.IsError {
		if text == "" {
			text = "tool reported an error"
		}
		return nil, fmt.Errorf("mcpclient: tool %q error: %s", t.name, text)
	}
	return text, nil
}

// textContent concatenates the TextContent blocks of a CallToolResult. Non-text
// content (images, embedded resources) is ignored: the native loop consumes
// plain text, matching the server frontend's text-only output convention.
func textContent(res *mcpsdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// convertSchema turns the remote tool's input schema — which the SDK delivers
// to clients as the default JSON marshaling of the server's schema (typically
// a map[string]any) — into a *jsonschema.Schema by round-tripping through JSON.
// nil or an unconvertible value yields a nil schema (a tool that takes no
// declared parameters).
func convertSchema(raw any) *jsonschema.Schema {
	if raw == nil {
		return nil
	}
	if s, ok := raw.(*jsonschema.Schema); ok {
		return s
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil
	}
	return &s
}
