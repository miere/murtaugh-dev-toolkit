package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/voocel/litellm"
	"github.com/voocel/litellm/providers"
)

// httpDoer is the HTTP transport interface litellm injects into a provider
// (aliased from litellm's providers package — litellm does not re-export it at
// the top level). nil selects litellm's resilient default; tests pass a fake.
// It is the single seam that makes Stream testable without network access.
type httpDoer = providers.HTTPDoer

// litellmProvider is the litellm-backed implementation of Provider. One
// instance is bound to a single family/model/credential (see New); Stream is
// safe to call concurrently since litellm.Client holds no per-call state.
type litellmProvider struct {
	client *litellm.Client
	family Family
	model  string
}

// newLiteLLMProvider builds the wrapper. httpClient is nil in production (the
// provider's resilient default is used) and set to a fake transport in tests —
// ProviderConfig.HTTPClient survives litellm's config normalization because it
// only fills the field when nil.
func newLiteLLMProvider(family Family, model, baseURL, apiKey string, httpClient httpDoer) (*litellmProvider, error) {
	cfg := litellm.ProviderConfig{
		APIKey:     apiKey,
		BaseURL:    strings.TrimSpace(baseURL),
		HTTPClient: httpClient,
	}
	client, err := litellm.NewWithProvider(family.providerName(), cfg)
	if err != nil {
		return nil, fmt.Errorf("llm: build %s provider: %w", family, err)
	}
	return &litellmProvider{client: client, family: family, model: model}, nil
}

// Stream maps req into a litellm request, opens the streaming reader, and fans
// the deltas out on a channel as StreamEvent. The channel is always closed; the
// terminal event carries Done=true with StopReason and Usage (or Err on
// failure). The reader is drained in a goroutine and respects ctx cancellation.
func (p *litellmProvider) Stream(ctx context.Context, req Request) (<-chan StreamEvent, error) {
	lreq, err := p.buildRequest(req)
	if err != nil {
		return nil, err
	}

	reader, err := p.client.Stream(ctx, lreq)
	if err != nil {
		return nil, fmt.Errorf("llm: %s stream: %w", p.family, err)
	}

	out := make(chan StreamEvent)
	go p.pump(ctx, reader, out)
	return out, nil
}

// buildRequest translates the provider-agnostic Request into litellm's request
// type. System goes through WithSystemPrompt so each provider places it in the
// right slot (Gemini systemInstruction, Anthropic top-level system, OpenAI
// system message). Per-provider message normalization (consecutive-role merge,
// tool-result batching, ID sanitization) is litellm's job — we only map shapes.
func (p *litellmProvider) buildRequest(req Request) (*litellm.Request, error) {
	msgs := make([]litellm.Message, 0, len(req.Messages))
	for i, m := range req.Messages {
		lm, err := toLiteLLMMessage(m)
		if err != nil {
			return nil, fmt.Errorf("llm: messages[%d]: %w", i, err)
		}
		msgs = append(msgs, lm)
	}

	opts := []litellm.RequestOption{}
	if s := strings.TrimSpace(req.System); s != "" {
		opts = append(opts, litellm.WithSystemPrompt(req.System))
	}
	if req.MaxTokens > 0 {
		opts = append(opts, litellm.WithMaxTokens(req.MaxTokens))
	}
	if req.Temperature > 0 {
		opts = append(opts, litellm.WithTemperature(req.Temperature))
	}
	if len(req.Tools) > 0 {
		tools, err := toLiteLLMTools(req.Tools)
		if err != nil {
			return nil, err
		}
		opts = append(opts, litellm.WithTools(tools...))
	}

	return litellm.NewRequestWithMessages(p.model, msgs, opts...), nil
}

// toLiteLLMMessage maps one canonical Message. The three shapes that must
// round-trip for multi-turn tool calling:
//   - assistant with ToolCalls → litellm assistant message carrying tool_calls
//     (id + function name + raw-JSON arguments) so the next turn echoes them;
//   - tool result → role "tool" with ToolCallID + Content correlating back;
//   - plain user/assistant/system text → Role + Content.
func toLiteLLMMessage(m Message) (litellm.Message, error) {
	lm := litellm.Message{
		Role:    string(m.Role),
		Content: m.Text,
	}

	switch m.Role {
	case RoleTool:
		// litellm correlates the result to the assistant tool_call by ID across
		// every family (Gemini functionResponse, Anthropic tool_result,
		// OpenAI tool message). The id must survive the round-trip.
		lm.ToolCallID = m.ToolCallID
	case RoleAssistant:
		for _, tc := range m.ToolCalls {
			args := string(tc.Arguments)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			lm.ToolCalls = append(lm.ToolCalls, litellm.ToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: litellm.FunctionCall{
					Name:      tc.Name,
					Arguments: args,
				},
			})
		}
	}

	return lm, nil
}

// toLiteLLMTools maps ToolSpecs to litellm function tools. The JSON Schema is
// rendered to map[string]any so the wire payload is identical and predictable
// across families (Gemini re-marshals Parameters into a map anyway).
func toLiteLLMTools(specs []ToolSpec) ([]litellm.Tool, error) {
	tools := make([]litellm.Tool, 0, len(specs))
	for _, s := range specs {
		params, err := schemaToMap(s.Schema)
		if err != nil {
			return nil, fmt.Errorf("llm: tool %q schema: %w", s.Name, err)
		}
		tools = append(tools, litellm.NewTool(s.Name, s.Description, params))
	}
	return tools, nil
}

// schemaToMap renders a *jsonschema.Schema (which implements json.Marshaler)
// into a generic object. A nil schema yields an empty-object schema so the tool
// still validates (a function with no parameters).
func schemaToMap(schema *jsonschema.Schema) (map[string]any, error) {
	if schema == nil {
		return map[string]any{"type": "object"}, nil
	}
	raw, err := schema.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// pump drains the litellm stream, assembling tool-call deltas (keyed by their
// stream index) and forwarding text/usage/stop, then closes out. It is the only
// goroutine writing to out.
func (p *litellmProvider) pump(ctx context.Context, reader litellm.StreamReader, out chan<- StreamEvent) {
	defer close(out)
	defer reader.Close()

	asm := newToolCallAssembler()
	var (
		stopReason string
		usage      *Usage
	)

	emit := func(ev StreamEvent) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			emit(StreamEvent{Err: err, Done: true})
			return
		}

		chunk, err := reader.Next()
		if err != nil {
			emit(StreamEvent{Err: fmt.Errorf("llm: %s stream: %w", p.family, err), Done: true})
			return
		}
		if chunk == nil {
			continue
		}

		if chunk.Usage != nil {
			usage = &Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
		}
		if chunk.FinishReason != "" {
			stopReason = chunk.FinishReason
		}

		switch chunk.Type {
		case litellm.ChunkTypeContent:
			if chunk.Content != "" {
				if !emit(StreamEvent{TextDelta: chunk.Content}) {
					return
				}
			}
		case litellm.ChunkTypeToolCallDelta:
			asm.add(chunk.ToolCallDelta)
		}

		if chunk.Done {
			// Flush assembled tool calls before the terminal event so the loop
			// sees them as discrete ToolCall events.
			for _, tc := range asm.finish() {
				if !emit(StreamEvent{ToolCall: tc}) {
					return
				}
			}
			emit(StreamEvent{Done: true, StopReason: stopReason, Usage: usage})
			return
		}
	}
}

// toolCallAssembler reassembles streamed tool-call fragments. OpenAI/Anthropic
// send the id + name on the first delta for an index, then argument fragments
// on later deltas at the same index; Gemini sends a single complete delta.
// Keying by Index (the stable per-call slot) handles all three.
type toolCallAssembler struct {
	byIndex map[int]*partialToolCall
	order   []int
}

type partialToolCall struct {
	id   string
	name string
	args strings.Builder
}

func newToolCallAssembler() *toolCallAssembler {
	return &toolCallAssembler{byIndex: make(map[int]*partialToolCall)}
}

func (a *toolCallAssembler) add(d *litellm.ToolCallDelta) {
	if d == nil {
		return
	}
	p, ok := a.byIndex[d.Index]
	if !ok {
		p = &partialToolCall{}
		a.byIndex[d.Index] = p
		a.order = append(a.order, d.Index)
	}
	if d.ID != "" {
		p.id = d.ID
	}
	if d.FunctionName != "" {
		p.name = d.FunctionName
	}
	if d.ArgumentsDelta != "" {
		p.args.WriteString(d.ArgumentsDelta)
	}
}

// finish returns the assembled tool calls in stream order. Calls missing a name
// are dropped (a name is required to dispatch). Empty arguments become "{}".
func (a *toolCallAssembler) finish() []*ToolCall {
	if len(a.order) == 0 {
		return nil
	}
	idx := append([]int(nil), a.order...)
	sort.Ints(idx)
	out := make([]*ToolCall, 0, len(idx))
	for _, i := range idx {
		p := a.byIndex[i]
		if p.name == "" {
			continue
		}
		args := p.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out = append(out, &ToolCall{
			ID:        p.id,
			Name:      p.name,
			Arguments: json.RawMessage(args),
		})
	}
	return out
}
