package native

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/llm"
)

// buildGroups appends n realistic turn-groups to a conversation: a user message,
// an assistant tool-call, its tool result, and an assistant closing line. Each
// user message is tagged so tests can see which groups survived.
func buildGroups(conv *Conversation, n int, pad string) {
	for i := 0; i < n; i++ {
		tag := string(rune('A' + i))
		conv.AppendUser("user-" + tag + " " + pad)
		conv.AppendAssistant("", []llm.ToolCall{{ID: "c" + tag, Name: "tool", Arguments: []byte(`{}`)}})
		conv.AppendToolResult("c"+tag, "tool", "result-"+tag+" "+pad)
		conv.AppendAssistant("answer-"+tag, nil)
	}
}

func collectEvents() (func(agent.Event), *[]agent.Event) {
	var evs []agent.Event
	return func(ev agent.Event) { evs = append(evs, ev) }, &evs
}

func TestTruncateToBudget_DropsOldestKeepsRecentAndInvariant(t *testing.T) {
	conv := NewConversation()
	pad := strings.Repeat("x", 400) // ~100 tokens per message
	buildGroups(conv, 5, pad)
	full := len(conv.messages)

	// Target small enough to force dropping, but allow at least the last group.
	out := truncateToBudget(conv.messages, 0, 150)

	if len(out) >= full {
		t.Fatalf("expected truncation to drop messages, kept %d of %d", len(out), full)
	}
	if out[0].Role != llm.RoleUser {
		t.Errorf("truncated array must start with a user message, got role %q", out[0].Role)
	}
	if err := assertNoConsecutiveUserAfterTool(out); err != nil {
		t.Errorf("truncated array violates the invariant: %v", err)
	}
	// The most recent group (user-E) must survive.
	if !strings.Contains(out[0].Text, "user-") {
		t.Errorf("unexpected first message: %q", out[0].Text)
	}
	last := out[len(out)-1]
	if last.Text != "answer-E" {
		t.Errorf("most recent turn should be retained, got last %q", last.Text)
	}
}

func TestTruncateToBudget_SingleGroupUnchanged(t *testing.T) {
	conv := NewConversation()
	buildGroups(conv, 1, strings.Repeat("x", 4000))
	out := truncateToBudget(conv.messages, 0, 1) // absurdly small target
	if len(out) != len(conv.messages) {
		t.Fatalf("a single turn-group must not be reduced; got %d want %d", len(out), len(conv.messages))
	}
}

func TestCompact_NoBudgetIsNoop(t *testing.T) {
	l := NewLoop(&fakeProvider{}, "m", nil, 5) // no WithCompaction ⇒ contextLimit 0
	conv := NewConversation()
	buildGroups(conv, 5, strings.Repeat("x", 400))
	before := len(conv.messages)
	emit, evs := collectEvents()
	l.compact(context.Background(), conv, "", emit)
	if len(conv.messages) != before {
		t.Errorf("compaction with no budget must be a no-op, changed %d → %d", before, len(conv.messages))
	}
	if len(*evs) != 0 {
		t.Errorf("expected no events, got %d", len(*evs))
	}
}

func TestCompact_TruncatesWhenOverBudget(t *testing.T) {
	l := NewLoop(&fakeProvider{}, "m", nil, 5).WithCompaction(100, CompactTruncate)
	conv := NewConversation()
	buildGroups(conv, 5, strings.Repeat("x", 400)) // ~500 tokens, well over high(75)
	before := len(conv.messages)
	emit, evs := collectEvents()
	l.compact(context.Background(), conv, "", emit)
	if len(conv.messages) >= before {
		t.Fatalf("expected compaction, %d → %d", before, len(conv.messages))
	}
	if err := assertNoConsecutiveUserAfterTool(conv.messages); err != nil {
		t.Errorf("post-compaction invariant violated: %v", err)
	}
	if conv.lastInputTokens != 0 {
		t.Errorf("lastInputTokens should reset after compaction, got %d", conv.lastInputTokens)
	}
	if len(*evs) == 0 {
		t.Error("expected a status event for the compaction")
	}
}

// TestCompact_RealTokenCountTriggers proves the authoritative provider token
// count drives compaction even when the char estimate is small.
func TestCompact_RealTokenCountTriggers(t *testing.T) {
	l := NewLoop(&fakeProvider{}, "m", nil, 5).WithCompaction(100, CompactTruncate)
	conv := NewConversation()
	buildGroups(conv, 4, "")   // tiny by char estimate
	conv.recordInputTokens(90) // but the provider says we're near the 100 limit (high=75)
	before := len(conv.messages)
	emit, _ := collectEvents()
	l.compact(context.Background(), conv, "", emit)
	if len(conv.messages) >= before {
		t.Errorf("real token count should have triggered compaction, %d → %d", before, len(conv.messages))
	}
}

func TestCompact_SummarizeReplacesOldest(t *testing.T) {
	prov := &fakeProvider{turns: []scriptedTurn{{text: "SUMMARY: earlier groups discussed X and Y", stopReason: "end_turn"}}}
	l := NewLoop(prov, "m", nil, 5).WithCompaction(100, CompactSummarize)
	conv := NewConversation()
	buildGroups(conv, 5, strings.Repeat("x", 400))
	before := len(conv.messages)
	emit, _ := collectEvents()
	l.compact(context.Background(), conv, "", emit)

	if len(conv.messages) >= before {
		t.Fatalf("expected summarize to reduce the array, %d → %d", before, len(conv.messages))
	}
	first := conv.messages[0]
	if first.Role != llm.RoleUser || !strings.Contains(first.Text, "<conversation-summary>") {
		t.Fatalf("expected a summary user message first, got role %q text %q", first.Role, first.Text)
	}
	if !strings.Contains(first.Text, "SUMMARY: earlier groups") {
		t.Errorf("summary text not embedded: %q", first.Text)
	}
	if err := assertNoConsecutiveUserAfterTool(conv.messages); err != nil {
		t.Errorf("post-summarize invariant violated: %v", err)
	}
	if len(prov.requests) != 1 {
		t.Errorf("expected exactly one summarize provider call, got %d", len(prov.requests))
	}
}

// TestCompact_SummarizeFallsBackToTruncate ensures a failing summary call still
// keeps the conversation within budget via truncation.
func TestCompact_SummarizeFallsBackToTruncate(t *testing.T) {
	prov := &fakeProvider{streamErr: context.DeadlineExceeded}
	l := NewLoop(prov, "m", nil, 5).WithCompaction(100, CompactSummarize)
	conv := NewConversation()
	buildGroups(conv, 5, strings.Repeat("x", 400))
	before := len(conv.messages)
	emit, _ := collectEvents()
	l.compact(context.Background(), conv, "", emit)
	if len(conv.messages) >= before {
		t.Fatalf("summary failed; truncation fallback should still reduce, %d → %d", before, len(conv.messages))
	}
	for _, m := range conv.messages {
		if strings.Contains(m.Text, "<conversation-summary>") {
			t.Error("no summary message should be present when the summary call failed")
		}
	}
}
