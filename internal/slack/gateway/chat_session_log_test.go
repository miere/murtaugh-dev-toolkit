package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/acp"
	"github.com/miere/murtaugh-dev-toolkit/internal/journal"
)

// scriptedSessions streams a fixed set of events and reports a fixed session id
// from Lookup, so a turn record can assert the session id and transcript.
type scriptedSessions struct {
	id     string
	events []acp.Event
}

func (s scriptedSessions) Prompt(_ context.Context, _ acp.ConversationKey, _ acp.SessionMetadata, _ acp.PromptRequest) (<-chan acp.Event, error) {
	ch := make(chan acp.Event, len(s.events))
	for _, e := range s.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (s scriptedSessions) Lookup(acp.ConversationKey) (string, bool) { return s.id, true }
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
	sessions := scriptedSessions{id: "sess-xyz", events: []acp.Event{
		{Type: acp.EventText, Text: "hello "},
		{Type: acp.EventText, Text: "world"},
		{Type: acp.EventComplete},
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
	sessions := scriptedSessions{id: "sess-err", events: []acp.Event{
		{Type: acp.EventText, Text: "partial"},
		{Type: acp.EventError, Error: errors.New("boom")},
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

func TestChatHandlerNoSessionLogIsNoop(t *testing.T) {
	// Without a session logger, Handle must work and record nothing.
	sessions := scriptedSessions{id: "s", events: []acp.Event{{Type: acp.EventText, Text: "hi"}, {Type: acp.EventComplete}}}
	handler := NewChatHandler(&fakeStreamAPI{}, map[string]ChatSessionManager{"default": sessions},
		func(ChatRequest) string { return "default" }, time.Hour, 1, discardLogger())
	if err := handler.Handle(context.Background(), ChatRequest{ChannelID: "C1", MessageTS: "1.1", Text: "hi", Source: "test"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}
