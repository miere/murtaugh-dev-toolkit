package native

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
)

// newTestClient builds a Client wired to a fake provider, bypassing Build (which
// would need real credentials). Initialize still runs to resolve an (empty)
// toolset and construct the loop.
func newTestClient(prov *fakeProvider) *Client {
	return &Client{
		provider: prov,
		model:    "test-model",
		maxTurns: 10,
		now:      func() time.Time { return time.Date(2026, 6, 17, 18, 51, 0, 0, time.UTC) },
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: make(map[string]*nativeSession),
		cancels:  make(map[string]*inflight),
	}
}

func drain(ch <-chan agent.Event) []agent.Event {
	var evs []agent.Event
	for ev := range ch {
		evs = append(evs, ev)
	}
	return evs
}

func TestClient_PromptStreamsTextAndCompletes(t *testing.T) {
	prov := &fakeProvider{turns: []scriptedTurn{{text: "hello there", stopReason: "end_turn"}}}
	c := newTestClient(prov)
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sess, err := c.NewSession(ctx, agent.SessionMetadata{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{Text: "hi"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	evs := drain(ch)

	var text strings.Builder
	var sawComplete bool
	for _, ev := range evs {
		switch ev.Type {
		case agent.EventText:
			text.WriteString(ev.Text)
		case agent.EventComplete:
			sawComplete = true
		}
	}
	if text.String() != "hello there" {
		t.Errorf("streamed text = %q, want %q", text.String(), "hello there")
	}
	if !sawComplete {
		t.Error("expected an EventComplete")
	}
}

func TestClient_UnknownSession(t *testing.T) {
	c := newTestClient(&fakeProvider{})
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.Prompt(context.Background(), "nope", agent.PromptRequest{Text: "x"}); err == nil {
		t.Fatal("expected error for unknown session")
	}
}

// TestClient_HistoryFoldsIntoSingleUserMessage proves the cold-start backfill
// (req.History) is merged into the SAME user message rather than appended as a
// second one — so the array never gains consecutive user messages.
func TestClient_HistoryFoldsIntoSingleUserMessage(t *testing.T) {
	prov := &fakeProvider{turns: []scriptedTurn{{text: "ok", stopReason: "end_turn"}}}
	c := newTestClient(prov)
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sess, _ := c.NewSession(ctx, agent.SessionMetadata{})
	ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{
		Text:    "current question",
		History: "earlier thread backstory",
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	drain(ch)

	if len(prov.requests) == 0 {
		t.Fatal("provider received no request")
	}
	msgs := prov.requests[0].Messages
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 message (folded), got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" {
		t.Errorf("first message role = %q, want user", msgs[0].Role)
	}
	if !strings.Contains(msgs[0].Text, "earlier thread backstory") || !strings.Contains(msgs[0].Text, "current question") {
		t.Errorf("folded user message missing history or question: %q", msgs[0].Text)
	}
	// The Slack/turn context must NOT be in the message array — it belongs in
	// the system prompt.
	if !strings.Contains(prov.requests[0].System, "2026-06-17") {
		t.Errorf("expected per-turn context in System, got %q", prov.requests[0].System)
	}
}

func TestClient_SupportsCancel(t *testing.T) {
	c := newTestClient(&fakeProvider{})
	if !c.SupportsCancel(context.Background()) {
		t.Error("native client should report SupportsCancel = true")
	}
}
