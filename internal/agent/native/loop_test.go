package native

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/llm"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

// --- fake provider --------------------------------------------------------

// scriptedTurn describes what the fake provider emits for one Stream call.
type scriptedTurn struct {
	text       string
	toolCalls  []llm.ToolCall
	stopReason string
}

// fakeProvider returns scripted turns in order and records every Request it
// receives so tests can assert over the full sequence of message arrays sent to
// the provider.
type fakeProvider struct {
	turns     []scriptedTurn
	idx       int
	requests  []llm.Request
	streamErr error
}

func (f *fakeProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
	// Deep-copy the messages so later mutations of the Conversation cannot alter
	// what we recorded.
	rec := req
	rec.Messages = append([]llm.Message(nil), req.Messages...)
	f.requests = append(f.requests, rec)

	if f.streamErr != nil {
		return nil, f.streamErr
	}

	var turn scriptedTurn
	if f.idx < len(f.turns) {
		turn = f.turns[f.idx]
	} else {
		// Past the script: emit a clean empty end_turn.
		turn = scriptedTurn{stopReason: "end_turn"}
	}
	f.idx++

	ch := make(chan llm.StreamEvent, len(turn.toolCalls)+2)
	if turn.text != "" {
		ch <- llm.StreamEvent{TextDelta: turn.text}
	}
	for i := range turn.toolCalls {
		tc := turn.toolCalls[i]
		ch <- llm.StreamEvent{ToolCall: &tc}
	}
	stop := turn.stopReason
	if stop == "" {
		stop = "end_turn"
	}
	ch <- llm.StreamEvent{Done: true, StopReason: stop, Usage: &llm.Usage{}}
	close(ch)
	return ch, nil
}

// --- fake tool ------------------------------------------------------------

type fakeTool struct {
	name   string
	result any
	err    error
	calls  int
}

func (t *fakeTool) Name() string                    { return t.name }
func (t *fakeTool) Description() string             { return "fake tool " + t.name }
func (t *fakeTool) InputSchema() *jsonschema.Schema { return &jsonschema.Schema{Type: "object"} }
func (t *fakeTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	t.calls++
	if t.err != nil {
		return nil, t.err
	}
	return t.result, nil
}

// collectEvents drains an emit closure into a slice.
func newCollector() (func(agent.Event), *[]agent.Event) {
	var evs []agent.Event
	return func(e agent.Event) { evs = append(evs, e) }, &evs
}

func eventsOfType(evs []agent.Event, t agent.EventType) []agent.Event {
	var out []agent.Event
	for _, e := range evs {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func rawArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return b
}

// assertAllRequestsClean is the regression test for the Goose MOIM bug: across
// EVERY captured provider Request, the message array must never contain a
// tool-result immediately followed by a standalone user message.
func assertAllRequestsClean(t *testing.T, p *fakeProvider) {
	t.Helper()
	if len(p.requests) == 0 {
		t.Fatal("expected at least one provider request to assert over")
	}
	for i, req := range p.requests {
		if err := assertNoConsecutiveUserAfterTool(req.Messages); err != nil {
			t.Fatalf("request #%d violates no-consecutive-user invariant: %v\nmessages: %#v", i, err, req.Messages)
		}
	}
}

// --- (1) multi-turn tool-calling conversation completes, results round-trip --

func TestRun_MultiTurnToolCalling_RoundTrips(t *testing.T) {
	prov := &fakeProvider{turns: []scriptedTurn{
		{ // turn 1: ask for two tools
			text: "let me check",
			toolCalls: []llm.ToolCall{
				{ID: "c1", Name: "alpha", Arguments: rawArgs(t, map[string]any{"x": 1})},
				{ID: "c2", Name: "beta", Arguments: rawArgs(t, map[string]any{})},
			},
		},
		{ // turn 2: one more tool
			toolCalls: []llm.ToolCall{
				{ID: "c3", Name: "alpha", Arguments: rawArgs(t, map[string]any{"x": 2})},
			},
		},
		{text: "all done", stopReason: "end_turn"},
	}}
	alpha := &fakeTool{name: "alpha", result: map[string]any{"ok": true}}
	beta := &fakeTool{name: "beta", result: "beta-string-result"}

	loop := NewLoop(prov, "test-model", []tools.Tool{alpha, beta}, 10)
	conv := NewConversation()
	conv.AppendUser("hello")
	emit, evs := newCollector()

	stop, err := loop.Run(context.Background(), conv, "SYSTEM", emit)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if stop != "end_turn" {
		t.Fatalf("stop = %q, want end_turn", stop)
	}
	if alpha.calls != 2 || beta.calls != 1 {
		t.Fatalf("tool call counts: alpha=%d beta=%d, want 2/1", alpha.calls, beta.calls)
	}

	// Tool results must round-trip with IDs/names into the conversation.
	msgs := conv.Messages()
	var got []string
	for _, m := range msgs {
		if m.Role == llm.RoleTool {
			got = append(got, m.ToolCallID+":"+m.ToolName+":"+m.Text)
		}
	}
	want := []string{
		`c1:alpha:{"ok":true}`,
		`c2:beta:beta-string-result`,
		`c3:alpha:{"ok":true}`,
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("tool results = %v, want %v", got, want)
	}

	// Completion emitted, all text deltas forwarded.
	if len(eventsOfType(*evs, agent.EventComplete)) != 1 {
		t.Fatalf("expected exactly one EventComplete")
	}
	var text strings.Builder
	for _, e := range eventsOfType(*evs, agent.EventText) {
		text.WriteString(e.Text)
	}
	if text.String() != "let me checkall done" {
		t.Fatalf("forwarded text = %q", text.String())
	}

	assertAllRequestsClean(t, prov)
}

// --- (2) no-consecutive-user regression test (the Goose bug) -----------------

func TestRun_NeverEmitsConsecutiveUserAfterTool(t *testing.T) {
	// A long, tool-heavy conversation with multiple user turns interleaved is
	// exactly the shape that triggered the Goose MOIM bug. The loop must never
	// place a standalone user message right after a tool result, across every
	// provider request.
	prov := &fakeProvider{turns: []scriptedTurn{
		{toolCalls: []llm.ToolCall{{ID: "a", Name: "alpha", Arguments: rawArgs(t, map[string]any{})}}},
		{toolCalls: []llm.ToolCall{{ID: "b", Name: "alpha", Arguments: rawArgs(t, map[string]any{})}}},
		{text: "first answer", stopReason: "end_turn"},
		// second user turn (new Slack message in the same thread)
		{toolCalls: []llm.ToolCall{{ID: "c", Name: "alpha", Arguments: rawArgs(t, map[string]any{})}}},
		{text: "second answer", stopReason: "end_turn"},
	}}
	alpha := &fakeTool{name: "alpha", result: "r"}
	loop := NewLoop(prov, "m", []tools.Tool{alpha}, 20)

	conv := NewConversation()
	conv.AppendUser("first question")
	emit, _ := newCollector()
	if _, err := loop.Run(context.Background(), conv, "SYS", emit); err != nil {
		t.Fatalf("run 1: %v", err)
	}

	// Simulate the next Slack message arriving after tools ran. This is the
	// cascade scenario from the RCA — the conversation tail is an assistant
	// turn, and the new user message must NOT land right after a tool result.
	conv.AppendUser("second question")
	if _, err := loop.Run(context.Background(), conv, "SYS", emit); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	assertAllRequestsClean(t, prov)

	// Also assert the guard catches a deliberately-malformed array, proving it
	// is a real check and not vacuously passing.
	bad := []llm.Message{
		{Role: llm.RoleTool, ToolCallID: "x", Text: "result"},
		{Role: llm.RoleUser, Text: "<info-msg>..."},
	}
	if err := assertNoConsecutiveUserAfterTool(bad); err == nil {
		t.Fatal("guard failed to flag a tool-result followed by standalone user message")
	}
}

// --- (2b) a turn that ends on max_turns must not wedge the next user turn ----

// TestRun_DanglingToolResultThenNextUserTurn reproduces the production
// regression: a turn that exhausts max_turns leaves the conversation tail as a
// tool-result (the loop returns without a closing assistant message). The NEXT
// user message in the same session must NOT land immediately after that
// tool-result — otherwise the no-consecutive-user guard fires and every
// subsequent message in the session errors until the process restarts.
func TestRun_DanglingToolResultThenNextUserTurn(t *testing.T) {
	// Run 1: every turn asks for a tool, so a small maxTurns is exhausted and the
	// conversation tail is a tool-result.
	prov := &fakeProvider{}
	for i := 0; i < 100; i++ {
		prov.turns = append(prov.turns, scriptedTurn{
			toolCalls: []llm.ToolCall{{ID: "z", Name: "alpha", Arguments: rawArgs(t, map[string]any{})}},
		})
	}
	alpha := &fakeTool{name: "alpha", result: "r"}
	loop := NewLoop(prov, "m", []tools.Tool{alpha}, 3)

	conv := NewConversation()
	conv.AppendUser("first question")
	emit, _ := newCollector()
	stop, err := loop.Run(context.Background(), conv, "SYS", emit)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if stop != "max_turns" {
		t.Fatalf("run 1 stop = %q, want max_turns (precondition: dangling tool-result tail)", stop)
	}
	if last := conv.Messages()[conv.Len()-1]; effectiveRole(last) != llm.RoleTool {
		t.Fatalf("precondition not met: tail role = %q, want a dangling tool-result", effectiveRole(last))
	}

	// The next Slack message arrives in the SAME session. Before the fix this
	// produced tool-result → user and the very next Run errored immediately.
	conv.AppendUser("can you tweak max_turns?")
	if err := assertNoConsecutiveUserAfterTool(conv.Messages()); err != nil {
		t.Fatalf("AppendUser left a malformed array after a max_turns turn: %v", err)
	}

	// Let the model now reply cleanly and verify run 2 completes without error.
	prov.turns = append(prov.turns, scriptedTurn{text: "sure", stopReason: "end_turn"})
	if _, err := loop.Run(context.Background(), conv, "SYS", emit); err != nil {
		t.Fatalf("run 2 must not be wedged by the prior max_turns turn: %v", err)
	}
	assertAllRequestsClean(t, prov)
}

// --- (3) empty completion triggers bounded recovery -------------------------

func TestRun_EmptyCompletionRecovers(t *testing.T) {
	prov := &fakeProvider{turns: []scriptedTurn{
		{text: "", stopReason: "end_turn"},           // empty
		{text: "", stopReason: "end_turn"},           // empty retry #1
		{text: "recovered!", stopReason: "end_turn"}, // retry #2 succeeds
	}}
	loop := NewLoop(prov, "m", nil, 5)
	conv := NewConversation()
	conv.AppendUser("hi")
	emit, evs := newCollector()

	stop, err := loop.Run(context.Background(), conv, "SYS", emit)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if stop != "end_turn" {
		t.Fatalf("stop = %q", stop)
	}
	// 1 initial + 2 recovery attempts = 3 provider requests.
	if len(prov.requests) != 3 {
		t.Fatalf("provider requests = %d, want 3 (initial + bounded retries)", len(prov.requests))
	}
	var text strings.Builder
	for _, e := range eventsOfType(*evs, agent.EventText) {
		text.WriteString(e.Text)
	}
	if text.String() != "recovered!" {
		t.Fatalf("recovered text = %q", text.String())
	}
	assertAllRequestsClean(t, prov)
}

func TestRun_EmptyCompletion_BoundedGivesUpCleanly(t *testing.T) {
	prov := &fakeProvider{turns: []scriptedTurn{
		{text: "", stopReason: "end_turn"},
		{text: "", stopReason: "end_turn"},
		{text: "", stopReason: "end_turn"},
		{text: "", stopReason: "end_turn"}, // would loop forever if unbounded
	}}
	loop := NewLoop(prov, "m", nil, 5)
	conv := NewConversation()
	conv.AppendUser("hi")
	emit, evs := newCollector()

	stop, err := loop.Run(context.Background(), conv, "SYS", emit)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if stop != "end_turn" {
		t.Fatalf("stop = %q, want clean end_turn after giving up", stop)
	}
	// initial + maxRecoveryRetries = 1 + 2 = 3, then ends cleanly.
	if len(prov.requests) != 1+maxRecoveryRetries {
		t.Fatalf("provider requests = %d, want %d", len(prov.requests), 1+maxRecoveryRetries)
	}
	if len(eventsOfType(*evs, agent.EventComplete)) != 1 {
		t.Fatal("expected one EventComplete on clean give-up")
	}
}

// --- (4) maxTurns bounds the loop -------------------------------------------

func TestRun_MaxTurnsBoundsLoop(t *testing.T) {
	// Every turn requests a tool, so without a bound the loop never ends.
	prov := &fakeProvider{turns: []scriptedTurn{}}
	// Make the fake always return a tool call by scripting beyond the slice via
	// a generator: reuse a single scripted turn many times.
	for i := 0; i < 100; i++ {
		prov.turns = append(prov.turns, scriptedTurn{
			toolCalls: []llm.ToolCall{{ID: "z", Name: "alpha", Arguments: rawArgs(t, map[string]any{})}},
		})
	}
	alpha := &fakeTool{name: "alpha", result: "r"}
	loop := NewLoop(prov, "m", []tools.Tool{alpha}, 3)
	conv := NewConversation()
	conv.AppendUser("go")
	emit, evs := newCollector()

	stop, err := loop.Run(context.Background(), conv, "SYS", emit)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if stop != "max_turns" {
		t.Fatalf("stop = %q, want max_turns", stop)
	}
	if len(prov.requests) != 3 {
		t.Fatalf("provider requests = %d, want 3 (maxTurns)", len(prov.requests))
	}
	comp := eventsOfType(*evs, agent.EventComplete)
	if len(comp) != 1 || comp[0].StopReason != "max_turns" {
		t.Fatalf("expected EventComplete with max_turns, got %#v", comp)
	}
	assertAllRequestsClean(t, prov)
}

// --- (5) a tool error becomes a tool-result, not a loop abort ---------------

func TestRun_ToolErrorBecomesToolResult(t *testing.T) {
	prov := &fakeProvider{turns: []scriptedTurn{
		{toolCalls: []llm.ToolCall{{ID: "e1", Name: "boom", Arguments: rawArgs(t, map[string]any{})}}},
		{text: "handled the error", stopReason: "end_turn"},
	}}
	boom := &fakeTool{name: "boom", err: errors.New("kaboom")}
	loop := NewLoop(prov, "m", []tools.Tool{boom}, 5)
	conv := NewConversation()
	conv.AppendUser("do it")
	emit, evs := newCollector()

	stop, err := loop.Run(context.Background(), conv, "SYS", emit)
	if err != nil {
		t.Fatalf("Run must NOT abort on tool error, got: %v", err)
	}
	if stop != "end_turn" {
		t.Fatalf("stop = %q", stop)
	}

	// The error must have been fed back as a tool result.
	var toolResult string
	for _, m := range conv.Messages() {
		if m.Role == llm.RoleTool && m.ToolCallID == "e1" {
			toolResult = m.Text
		}
	}
	if !strings.Contains(toolResult, "kaboom") {
		t.Fatalf("tool-result = %q, expected to carry the error", toolResult)
	}
	// And the model got a follow-up turn (proving the loop continued).
	if boom.calls != 1 {
		t.Fatalf("boom.calls = %d, want 1", boom.calls)
	}
	if len(prov.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2 (error fed back, model recovered)", len(prov.requests))
	}
	// A failed task event should have been emitted.
	var sawFailed bool
	for _, e := range eventsOfType(*evs, agent.EventTask) {
		if e.Task != nil && e.Task.Status == agent.TaskStatusFailed {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Fatal("expected a failed task event for the erroring tool")
	}
	assertAllRequestsClean(t, prov)
}

// --- unknown tool also becomes a tool-result --------------------------------

func TestRun_UnknownToolBecomesToolResult(t *testing.T) {
	prov := &fakeProvider{turns: []scriptedTurn{
		{toolCalls: []llm.ToolCall{{ID: "u1", Name: "ghost", Arguments: rawArgs(t, map[string]any{})}}},
		{text: "ok", stopReason: "end_turn"},
	}}
	loop := NewLoop(prov, "m", nil, 5)
	conv := NewConversation()
	conv.AppendUser("call ghost")
	emit, _ := newCollector()

	if _, err := loop.Run(context.Background(), conv, "SYS", emit); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	var res string
	for _, m := range conv.Messages() {
		if m.ToolCallID == "u1" {
			res = m.Text
		}
	}
	if !strings.Contains(res, "unknown tool") {
		t.Fatalf("unknown-tool result = %q", res)
	}
	assertAllRequestsClean(t, prov)
}
