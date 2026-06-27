// Package mcp implements the MCP (stdio) frontend. It exposes every tool in
// the shared registry as an MCP tool, and runs the JSON-RPC server over
// stdin/stdout. Stdout is reserved for protocol messages; no logs or raw
// text are written to it.
//
// Output convention: a tool's result is JSON-marshalled and wrapped in a
// single TextContent block. A plain string result is passed through as-is so
// trivial tools (e.g. ping) don't need a struct.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/murtaugh/internal/tools"
)

// ServerName is advertised to MCP clients.
const ServerName = "murtaugh-mcp"

// ServerVersion is advertised to MCP clients.
const ServerVersion = "0.1.0"

// Approver gates a tool call before it runs. It mirrors the native loop's
// Approver (internal/agent/native), so the same *interaction.GateApprover
// satisfies both — the aggregator reuses the exact Slack approval gate the
// native loop uses, rather than reinventing one. A nil Approver means no
// gating. The returned note is surfaced to the agent as the tool's result on
// denial, and may carry an explanation on approval.
type Approver interface {
	Approve(ctx context.Context, toolName, summary string) (allowed bool, note string)
}

// Frontend is the MCP adapter. It serves a fixed set of tools — the shared
// registry (via New) for the standalone `murtaugh mcp` frontend, or a resolved
// per-agent toolset (via NewFromTools) for the ACP aggregator.
type Frontend struct {
	tools    []tools.Tool
	approver Approver
}

// New constructs an MCP Frontend backed by the given registry, ungated. This is
// the standalone stdio frontend exposing every built-in tool.
func New(reg *tools.Registry) *Frontend {
	return &Frontend{tools: reg.All()}
}

// NewFromTools constructs an MCP Frontend serving an explicit resolved toolset,
// optionally gated by approver. This is the per-agent aggregator surface: the
// toolset is whatever toolset.Resolve produced for the agent (built-ins plus
// proxied external MCP tools), and approver applies the same human-in-the-loop
// gate the native loop applies.
func NewFromTools(ts []tools.Tool, approver Approver) *Frontend {
	return &Frontend{tools: ts, approver: approver}
}

// Server builds an *mcpsdk.Server with every tool wired in. It is exposed so
// tests can inspect the resulting server without touching stdio.
func (f *Frontend) Server() *mcpsdk.Server {
	s := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    ServerName,
		Version: ServerVersion,
	}, nil)
	// Guard against two tool names collapsing onto the same published name
	// (e.g. "a.b" and "a-b" would both sanitise to "a_b"). The MCP SDK silently
	// shadows a duplicate rather than erroring, so we fail loudly at startup —
	// a collision is a programming error (registry keys, or a proxied MCP server
	// whose tools collide with a built-in), not a runtime condition.
	seen := make(map[string]string, len(f.tools))
	for _, t := range f.tools {
		published := mcpToolName(t.Name())
		if prior, dup := seen[published]; dup {
			panic(fmt.Sprintf("mcp: tool name collision: %q and %q both publish as %q", prior, t.Name(), published))
		}
		seen[published] = t.Name()
		registerTool(s, t, f.approver)
	}
	return s
}

// invalidMCPNameChar matches any rune disallowed in an LLM-facing tool name.
// The MCP Go SDK tolerates '.', but stricter providers reject it — Gemini's
// function-name regex is exactly [a-zA-Z0-9_-]+, so a dotted name like
// "jobs.define" (after Goose namespacing, "murtaugh__jobs.define") is refused
// with a -32600 "invalid characters" error and the tool call never runs.
var invalidMCPNameChar = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// mcpToolName normalises a registry tool name into an identifier safe to
// expose over MCP: every character outside [A-Za-z0-9_-] becomes '_', so
// "jobs.define" is published as "jobs_define". This is done here, at the single
// boundary where the dotted registry key becomes the LLM-facing id, rather than
// renaming the tools themselves — the dotted keys stay load-bearing for the CLI
// ("murtaugh jobs define") and help. Dispatch is unaffected: AddTool keys the
// handler on the published name, so an inbound CallTool carrying "jobs_define"
// matches and invokes the captured tool directly.
func mcpToolName(name string) string {
	return invalidMCPNameChar.ReplaceAllString(name, "_")
}

// Serve runs the MCP server over a stdio transport. It blocks until the
// connected client disconnects or ctx is cancelled.
func (f *Frontend) Serve(ctx context.Context) error {
	return f.Server().Run(ctx, &mcpsdk.StdioTransport{})
}

// registerTool wires a single tools.Tool into the MCP server using the
// low-level Server.AddTool, so we can publish the tool's own InputSchema and
// dispatch dynamic map[string]any arguments. When approver is non-nil and the
// tool opts into approval (implements ApprovalClassifier and requires it for
// this call), the human gate runs before Invoke — the same ordering as the
// native loop (internal/agent/native/loop.go).
func registerTool(s *mcpsdk.Server, t tools.Tool, approver Approver) {
	schema := t.InputSchema()
	if schema == nil {
		schema = emptyObjectSchema()
	}
	handler := func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		args, err := decodeArgs(req.Params.Arguments)
		if err != nil {
			return errorResult(err), nil
		}
		if denied, note := gate(ctx, t, args, approver); denied {
			// Mirror the native loop: a denial is not an error abort. The note
			// is fed back as the tool's result so the agent can react and pick
			// another path, rather than the whole call failing.
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: note}},
			}, nil
		}
		result, err := t.Invoke(ctx, args)
		if err != nil {
			return errorResult(err), nil
		}
		text, err := renderJSON(result)
		if err != nil {
			return errorResult(err), nil
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
		}, nil
	}
	s.AddTool(&mcpsdk.Tool{
		Name:        mcpToolName(t.Name()),
		Description: t.Description(),
		InputSchema: schema,
	}, handler)
}

// gate runs the approval check for a tool call. It returns (true, note) when the
// call is denied and must be skipped; (false, "") when it may proceed (no
// approver, the tool does not require approval for these args, or the human
// allowed it). This is the aggregator's port of the native loop's pre-Invoke
// gate, using the same optional ApprovalClassifier/ApprovalSummarizer tool
// interfaces.
func gate(ctx context.Context, t tools.Tool, args map[string]any, approver Approver) (denied bool, note string) {
	if approver == nil {
		return false, ""
	}
	classifier, ok := t.(tools.ApprovalClassifier)
	if !ok || !classifier.RequiresApproval(args) {
		return false, ""
	}
	summary := mcpToolName(t.Name())
	if summarizer, ok := t.(tools.ApprovalSummarizer); ok {
		summary = summarizer.ApprovalSummary(args)
	}
	allowed, n := approver.Approve(ctx, t.Name(), summary)
	if !allowed {
		return true, n
	}
	return false, ""
}

// emptyObjectSchema returns the canonical {"type":"object"} schema the SDK
// requires for tools that take no parameters.
func emptyObjectSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object"}
}

// decodeArgs unmarshals raw JSON arguments into a map. An empty payload
// yields a nil map so tools don't need to nil-check.
func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	return m, nil
}

// renderJSON encodes a tool result for transport. Strings are passed through
// to keep trivial tools' output uncluttered; everything else is JSON.
func renderJSON(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// errorResult builds the CallToolResult shape used to surface tool errors,
// per the MCP convention of returning IsError=true with a TextContent
// payload.
func errorResult(err error) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: err.Error()}},
	}
}
