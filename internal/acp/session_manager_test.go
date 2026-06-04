package acp

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClient struct {
	initialized atomic.Int32
	sessions    atomic.Int32
}

func (f *fakeClient) Initialize(context.Context) error {
	f.initialized.Add(1)
	return nil
}

func (f *fakeClient) NewSession(context.Context, SessionMetadata) (Session, error) {
	id := f.sessions.Add(1)
	return Session{ID: fmt.Sprintf("session-%d", id)}, nil
}

func (f *fakeClient) Prompt(context.Context, string, PromptRequest) (<-chan Event, error) {
	ch := make(chan Event, 1)
	ch <- Event{Type: EventComplete}
	close(ch)
	return ch, nil
}

func (f *fakeClient) Cancel(context.Context, string) error { return nil }
func (f *fakeClient) Close() error                         { return nil }

func TestSessionManagerReusesSessionForConversationKey(t *testing.T) {
	client := &fakeClient{}
	manager := NewSessionManager(client, time.Hour, 10)
	key := ConversationKey{TeamID: "T1", ChannelID: "C1", ThreadTS: "123.4"}

	for i := 0; i < 2; i++ {
		ch, err := manager.Prompt(context.Background(), key, SessionMetadata{}, PromptRequest{Text: "hi"})
		if err != nil {
			t.Fatalf("Prompt returned error: %v", err)
		}
		for range ch {
		}
	}
	if client.initialized.Load() != 1 || client.sessions.Load() != 1 {
		t.Fatalf("expected one initialized client/session, got init=%d sessions=%d", client.initialized.Load(), client.sessions.Load())
	}
}

func TestSessionManagerCreatesDistinctSessionsForDistinctThreads(t *testing.T) {
	client := &fakeClient{}
	manager := NewSessionManager(client, time.Hour, 10)
	keys := []ConversationKey{{TeamID: "T1", ChannelID: "C1", ThreadTS: "1"}, {TeamID: "T1", ChannelID: "C1", ThreadTS: "2"}}
	for _, key := range keys {
		ch, err := manager.Prompt(context.Background(), key, SessionMetadata{}, PromptRequest{Text: "hi"})
		if err != nil {
			t.Fatalf("Prompt returned error: %v", err)
		}
		for range ch {
		}
	}
	if client.sessions.Load() != 2 {
		t.Fatalf("expected two sessions, got %d", client.sessions.Load())
	}
}
