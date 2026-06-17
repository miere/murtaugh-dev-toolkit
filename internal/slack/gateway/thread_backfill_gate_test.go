package gateway

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
)

type stubBackfiller struct {
	called bool
	out    string
	err    error
}

func (s *stubBackfiller) Backfill(_ context.Context, _, _, _ string) (string, error) {
	s.called = true
	return s.out, s.err
}

// lookupSessions is a ChatSessionManager whose only meaningful behaviour is
// Lookup: live reports whether a session already exists for the conversation.
type lookupSessions struct{ live bool }

func (l lookupSessions) Prompt(context.Context, agent.ConversationKey, agent.SessionMetadata, agent.PromptRequest) (<-chan agent.Event, error) {
	return nil, nil
}
func (l lookupSessions) Lookup(agent.ConversationKey) (string, bool) { return "", l.live }
func (l lookupSessions) Cancel(context.Context, string) error        { return nil }

func gateHandler(b threadBackfiller) *ChatHandler {
	return &ChatHandler{logger: slog.Default(), backfiller: b}
}

func TestBackfillHistoryColdThreadedCallsBackfiller(t *testing.T) {
	b := &stubBackfiller{out: "<thread-transcript>...</thread-transcript>"}
	h := gateHandler(b)
	req := ChatRequest{ChannelID: "C1", ThreadTS: "1700000000.000100", MessageTS: "1700000000.000300"}

	got := h.backfillHistory(context.Background(), req, lookupSessions{live: false}, agent.ConversationKey{})
	if !b.called {
		t.Fatal("expected the backfiller to be called for a cold threaded conversation")
	}
	if got != b.out {
		t.Fatalf("expected backfiller output to flow through, got %q", got)
	}
}

func TestBackfillHistorySkipsWarmSession(t *testing.T) {
	b := &stubBackfiller{out: "should not be used"}
	h := gateHandler(b)
	req := ChatRequest{ChannelID: "C1", ThreadTS: "1700000000.000100", MessageTS: "1700000000.000300"}

	got := h.backfillHistory(context.Background(), req, lookupSessions{live: true}, agent.ConversationKey{})
	if b.called {
		t.Fatal("a warm session already holds the history; backfiller must not be called")
	}
	if got != "" {
		t.Fatalf("expected empty history for a warm session, got %q", got)
	}
}

func TestBackfillHistorySkipsTopLevelMessage(t *testing.T) {
	b := &stubBackfiller{out: "should not be used"}
	h := gateHandler(b)
	req := ChatRequest{ChannelID: "C1", ThreadTS: "", MessageTS: "1700000000.000300"}

	got := h.backfillHistory(context.Background(), req, lookupSessions{live: false}, agent.ConversationKey{})
	if b.called {
		t.Fatal("a top-level message has no prior thread; backfiller must not be called")
	}
	if got != "" {
		t.Fatalf("expected empty history for a top-level message, got %q", got)
	}
}

func TestBackfillHistoryNilBackfiller(t *testing.T) {
	h := gateHandler(nil)
	req := ChatRequest{ChannelID: "C1", ThreadTS: "1700000000.000100", MessageTS: "1700000000.000300"}
	if got := h.backfillHistory(context.Background(), req, lookupSessions{live: false}, agent.ConversationKey{}); got != "" {
		t.Fatalf("expected empty history when no backfiller is wired, got %q", got)
	}
}

func TestBackfillHistoryDegradesOnError(t *testing.T) {
	b := &stubBackfiller{err: errors.New("slack down")}
	h := gateHandler(b)
	req := ChatRequest{ChannelID: "C1", ThreadTS: "1700000000.000100", MessageTS: "1700000000.000300"}

	got := h.backfillHistory(context.Background(), req, lookupSessions{live: false}, agent.ConversationKey{})
	if !b.called {
		t.Fatal("expected the backfiller to be attempted")
	}
	if got != "" {
		t.Fatalf("a fetch failure must degrade to empty history, got %q", got)
	}
}
