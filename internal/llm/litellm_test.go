package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/voocel/litellm"
)

// fakeTransport is a litellm.HTTPDoer that returns a canned streaming body and
// records the last request body for request-mapping assertions.
type fakeTransport struct {
	body     string
	status   int
	lastBody []byte
	lastURL  string
}

func (f *fakeTransport) Do(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		f.lastBody, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	f.lastURL = req.URL.String()
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
	}, nil
}

func newTestProvider(t *testing.T, family Family, ft *fakeTransport) *litellmProvider {
	t.Helper()
	p, err := newLiteLLMProvider(family, "test-model", "", "test-key", ft)
	if err != nil {
		t.Fatalf("newLiteLLMProvider: %v", err)
	}
	return p
}

func drain(t *testing.T, ch <-chan StreamEvent) []StreamEvent {
	t.Helper()
	var events []StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

// sse formats one OpenAI-style SSE data line.
func sse(payload string) string { return "data: " + payload + "\n\n" }

func TestStream_OpenAI_TextAndToolCall(t *testing.T) {
	// Text delta, then a tool call assembled from name + two argument fragments,
	// finish_reason, then a usage-only terminal chunk.
	body := strings.Join([]string{
		sse(`{"model":"test-model","choices":[{"index":0,"delta":{"content":"Hello"}}]}`),
		sse(`{"model":"test-model","choices":[{"index":0,"delta":{"content":" world"}}]}`),
		sse(`{"model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`),
		sse(`{"model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"NYC\"}"}}]}}]}`),
		sse(`{"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		sse(`{"model":"test-model","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`),
		"data: [DONE]\n\n",
	}, "")

	ft := &fakeTransport{body: body}
	p := newTestProvider(t, FamilyOpenAI, ft)

	ch, err := p.Stream(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drain(t, ch)

	var text strings.Builder
	var toolCalls []*ToolCall
	var final *StreamEvent
	for i := range events {
		ev := events[i]
		switch {
		case ev.Err != nil:
			t.Fatalf("unexpected stream error: %v", ev.Err)
		case ev.TextDelta != "":
			text.WriteString(ev.TextDelta)
		case ev.ToolCall != nil:
			toolCalls = append(toolCalls, ev.ToolCall)
		}
		if ev.Done {
			final = &events[i]
		}
	}

	if got := text.String(); got != "Hello world" {
		t.Errorf("text = %q, want %q", got, "Hello world")
	}
	if len(toolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(toolCalls))
	}
	tc := toolCalls[0]
	if tc.ID != "call_1" || tc.Name != "get_weather" {
		t.Errorf("tool call id/name = %q/%q", tc.ID, tc.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("tool args not valid JSON %q: %v", tc.Arguments, err)
	}
	if args["city"] != "NYC" {
		t.Errorf("tool args city = %v, want NYC", args["city"])
	}
	if final == nil {
		t.Fatal("no terminal Done event")
	}
	if final.StopReason != litellm.FinishReasonToolCall {
		t.Errorf("stop reason = %q, want %q", final.StopReason, litellm.FinishReasonToolCall)
	}
	if final.Usage == nil || final.Usage.InputTokens != 11 || final.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v, want in=11 out=7", final.Usage)
	}
}

func TestStream_OpenAI_RequestMapping(t *testing.T) {
	ft := &fakeTransport{body: sse(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) + "data: [DONE]\n\n"}
	p := newTestProvider(t, FamilyOpenAI, ft)

	schema := &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"city": {Type: "string"}},
		Required:   []string{"city"},
	}

	ch, err := p.Stream(context.Background(), Request{
		System:    "you are helpful",
		MaxTokens: 256,
		Messages: []Message{
			{Role: RoleUser, Text: "weather?"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"NYC"}`)}}},
			{Role: RoleTool, ToolCallID: "call_1", ToolName: "get_weather", Text: `{"temp":72}`},
		},
		Tools: []ToolSpec{{Name: "get_weather", Description: "weather", Schema: schema}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drain(t, ch)

	var sent map[string]any
	if err := json.Unmarshal(ft.lastBody, &sent); err != nil {
		t.Fatalf("request body not JSON: %v\n%s", err, ft.lastBody)
	}

	msgs, _ := sent["messages"].([]any)
	// system + user + assistant(tool_calls) + tool
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4: %s", len(msgs), ft.lastBody)
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" {
		t.Errorf("first message role = %v, want system", sys["role"])
	}
	// litellm may serialize content as a string or a content-block array; accept
	// either as long as the system text is present.
	if !strings.Contains(string(ft.lastBody), "you are helpful") {
		t.Errorf("system text missing from request: %s", ft.lastBody)
	}
	asst := msgs[2].(map[string]any)
	tcs, ok := asst["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("assistant tool_calls missing: %+v", asst)
	}
	tc0 := tcs[0].(map[string]any)
	if tc0["id"] != "call_1" {
		t.Errorf("assistant tool_call id = %v, want call_1", tc0["id"])
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" {
		t.Errorf("tool result message = %+v, want role=tool tool_call_id=call_1", toolMsg)
	}
	if sent["max_tokens"] == nil {
		t.Errorf("max_tokens not sent: %s", ft.lastBody)
	}
	tools, _ := sent["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
}

func TestStream_Gemini_ToolResultRoundTrip(t *testing.T) {
	// One newline-delimited JSON candidate with text + finish, then DONE.
	body := strings.Join([]string{
		sse(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}`),
		"data: [DONE]\n\n",
	}, "")

	ft := &fakeTransport{body: body}
	p := newTestProvider(t, FamilyGemini, ft)

	ch, err := p.Stream(context.Background(), Request{
		System: "sys",
		Messages: []Message{
			{Role: RoleUser, Text: "weather?"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"NYC"}`)}}},
			{Role: RoleTool, ToolCallID: "c1", ToolName: "get_weather", Text: `{"temp":72}`},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drain(t, ch)

	var text strings.Builder
	var final *StreamEvent
	for i := range events {
		if events[i].Err != nil {
			t.Fatalf("stream error: %v", events[i].Err)
		}
		text.WriteString(events[i].TextDelta)
		if events[i].Done {
			final = &events[i]
		}
	}
	if text.String() != "ok" {
		t.Errorf("text = %q, want ok", text.String())
	}
	if final == nil || final.Usage == nil || final.Usage.InputTokens != 5 {
		t.Errorf("final usage = %+v", final)
	}

	// Verify the Gemini request body maps the tool result to a functionResponse
	// part echoing the call name, and the assistant tool call to a functionCall.
	var sent map[string]any
	if err := json.Unmarshal(ft.lastBody, &sent); err != nil {
		t.Fatalf("request body not JSON: %v\n%s", err, ft.lastBody)
	}
	if si, ok := sent["systemInstruction"].(map[string]any); !ok {
		t.Errorf("systemInstruction missing: %s", ft.lastBody)
	} else if _, ok := si["parts"]; !ok {
		t.Errorf("systemInstruction has no parts: %+v", si)
	}
	raw := string(ft.lastBody)
	if !strings.Contains(raw, "functionResponse") {
		t.Errorf("expected functionResponse part in gemini body: %s", raw)
	}
	if !strings.Contains(raw, "functionCall") {
		t.Errorf("expected functionCall part in gemini body: %s", raw)
	}
}

func TestStream_ContextCancellation(t *testing.T) {
	body := strings.Join([]string{
		sse(`{"choices":[{"index":0,"delta":{"content":"a"}}]}`),
		sse(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		"data: [DONE]\n\n",
	}, "")
	ft := &fakeTransport{body: body}
	p := newTestProvider(t, FamilyOpenAI, ft)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before draining

	ch, err := p.Stream(ctx, Request{Messages: []Message{{Role: RoleUser, Text: "hi"}}})
	if err != nil {
		// Stream may already fail at open time on a cancelled ctx; acceptable.
		return
	}
	// Channel must close without blocking forever.
	for range ch {
	}
}

func TestToolCallAssembler(t *testing.T) {
	a := newToolCallAssembler()
	// Two parallel tool calls, out-of-order indexes, fragmented args.
	a.add(&litellm.ToolCallDelta{Index: 1, ID: "b", FunctionName: "second"})
	a.add(&litellm.ToolCallDelta{Index: 0, ID: "a", FunctionName: "first", ArgumentsDelta: `{"x":`})
	a.add(&litellm.ToolCallDelta{Index: 0, ArgumentsDelta: `1}`})
	a.add(&litellm.ToolCallDelta{Index: 1, ArgumentsDelta: ``}) // empty args → "{}"

	got := a.finish()
	if len(got) != 2 {
		t.Fatalf("got %d calls, want 2", len(got))
	}
	if got[0].Name != "first" || string(got[0].Arguments) != `{"x":1}` {
		t.Errorf("call[0] = %+v", got[0])
	}
	if got[1].Name != "second" || string(got[1].Arguments) != "{}" {
		t.Errorf("call[1] = %+v", got[1])
	}
}

func TestSchemaToMap_Nil(t *testing.T) {
	m, err := schemaToMap(nil)
	if err != nil {
		t.Fatalf("schemaToMap(nil): %v", err)
	}
	if m["type"] != "object" {
		t.Errorf("nil schema => %+v, want type=object", m)
	}
}
