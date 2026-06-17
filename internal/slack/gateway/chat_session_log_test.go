package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/journal"
)

// scriptedSessions streams a fixed set of events and reports a fixed session id
// from Lookup, so a turn record can assert the session id and transcript.
type scriptedSessions struct {
	id     string
	events []agent.Event
}

func (s scriptedSessions) Prompt(_ context.Context, _ agent.ConversationKey, _ agent.SessionMetadata, _ agent.PromptRequest) (<-chan agent.Event, error) {
	ch := make(chan agent.Event, len(s.events))
	for _, e := range s.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (s scriptedSessions) Lookup(agent.ConversationKey) (string, bool) { return s.id, true }
func (s scriptedSessions) Cancel(context.Context, string) error      { return nil }

func newLoggingHandler(t *testing.T, rec journal.Recorder, blobDir string, sessions ChatSessionManager) *ChatHandler {
	t.Helper()
	sl := newSessionLogger(rec, blobDir, discardLogger())
	return NewChatHandler(&fakeStreamAPI{}, map[string]ChatSessionManager{"default": sessions},
		func(ChatRequest) string { return "default" }, time.Hour, 1, discardLogger()).WithSessionLogger(sl)
}

func TestChatHandlerRecordsCompletedTurn(t *testing.T) {
	blobDir := t.TempDir()
	rec := &journalSpy{}
	sessions := scriptedSessions{id: "sess-xyz", events: []agent.Event{
		{Type: agent.EventText, Text: "hello "},
		{Type: agent.EventText, Text: "world"},
		{Type: agent.EventComplete},
	}}
	handler := newLoggingHandler(t, rec, blobDir, sessions)

	if err := handler.Handle(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "1.1", Text: "hi there", Source: "test"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	turns := rec.byKind("session.turn")
	if len(turns) != 1 {
		t.Fatalf("expected one session.turn, got %d", len(turns))
	}
	e := turns[0]
	if e.Stream != journal.StreamACPSession || e.Level != journal.LevelInfo {
		t.Fatalf("unexpected envelope: %+v", e)
	}
	if e.Keys.SessionID != "sess-xyz" || e.Keys.ChannelID != "C1" || e.Keys.UserID != "U1" {
		t.Fatalf("unexpected keys: %+v", e.Keys)
	}
	if e.BlobRef == "" {
		t.Fatalf("expected a transcript blob ref")
	}
	payload := e.Payload.(map[string]any)
	if payload["outcome"] != turnCompleted {
		t.Fatalf("outcome = %v, want completed", payload["outcome"])
	}

	// The transcript blob holds the full prompt + response.
	data, err := os.ReadFile(filepath.Join(blobDir, e.BlobRef))
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	for _, want := range []string{"hi there", "hello world", "completed"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("transcript %q missing %q", data, want)
		}
	}
}

func TestChatHandlerRecordsErroredTurn(t *testing.T) {
	blobDir := t.TempDir()
	rec := &journalSpy{}
	sessions := scriptedSessions{id: "sess-err", events: []agent.Event{
		{Type: agent.EventText, Text: "partial"},
		{Type: agent.EventError, Error: errors.New("boom")},
	}}
	handler := newLoggingHandler(t, rec, blobDir, sessions)

	// An agent error is rendered onto the Slack stream and Handle returns nil;
	// the turn is still recorded at error level via the explicit turnErr path.
	_ = handler.Handle(context.Background(), ChatRequest{ChannelID: "C1", MessageTS: "1.1", Text: "go", Source: "test"})
	turns := rec.byKind("session.turn")
	if len(turns) != 1 || turns[0].Level != journal.LevelError {
		t.Fatalf("expected one error-level session.turn, got %+v", turns)
	}
	if turns[0].Payload.(map[string]any)["outcome"] != turnErrored {
		t.Fatalf("outcome = %v, want errored", turns[0].Payload.(map[string]any)["outcome"])
	}
}

func TestChatHandlerSurfacesEmptyReplyWithStopReason(t *testing.T) {
	blobDir := t.TempDir()
	rec := &journalSpy{}
	// A turn that streams no text and completes with a non-end_turn stop reason
	// (the goose "investigated but produced no reply" case).
	sessions := scriptedSessions{id: "sess-empty", events: []agent.Event{
		{Type: agent.EventComplete, StopReason: "max_tokens"},
	}}
	api := &fakeStreamAPI{}
	handler := NewChatHandler(api, map[string]ChatSessionManager{"default": sessions},
		func(ChatRequest) string { return "default" }, time.Hour, 1, discardLogger()).
		WithSessionLogger(newSessionLogger(rec, blobDir, discardLogger()))

	if err := handler.Handle(context.Background(), ChatRequest{ChannelID: "C1", MessageTS: "1.1", Text: "why is it broken?", Source: "dm"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// The user must see a note rather than silence, naming the stop reason.
	if api.appends == 0 {
		t.Fatalf("expected a fallback note to be appended for an empty reply")
	}
	note, err := extractMarkdownTextFromOptions(api.appendOptions[len(api.appendOptions)-1]...)
	if err != nil {
		t.Fatalf("extract note: %v", err)
	}
	if !strings.Contains(note, "max_tokens") {
		t.Fatalf("fallback note should name the stop reason, got %q", note)
	}

	// The journal turn records the empty outcome + stop reason.
	turns := rec.byKind("session.turn")
	if len(turns) != 1 {
		t.Fatalf("expected one session.turn, got %d", len(turns))
	}
	payload := turns[0].Payload.(map[string]any)
	if payload["bytes"] != 0 || payload["stop_reason"] != "max_tokens" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func TestChatHandlerNoSessionLogIsNoop(t *testing.T) {
	// Without a session logger, Handle must work and record nothing.
	sessions := scriptedSessions{id: "s", events: []agent.Event{{Type: agent.EventText, Text: "hi"}, {Type: agent.EventComplete}}}
	handler := NewChatHandler(&fakeStreamAPI{}, map[string]ChatSessionManager{"default": sessions},
		func(ChatRequest) string { return "default" }, time.Hour, 1, discardLogger())
	if err := handler.Handle(context.Background(), ChatRequest{ChannelID: "C1", MessageTS: "1.1", Text: "hi", Source: "test"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}
