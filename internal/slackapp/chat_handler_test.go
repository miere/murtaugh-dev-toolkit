package slackapp

import (
	"context"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/acp"
)

type fakeChatSessions struct {
	key    acp.ConversationKey
	prompt string
}

func (f *fakeChatSessions) Prompt(_ context.Context, key acp.ConversationKey, _ acp.SessionMetadata, req acp.PromptRequest) (<-chan acp.Event, error) {
	f.key = key
	f.prompt = req.Text
	ch := make(chan acp.Event, 2)
	ch <- acp.Event{Type: acp.EventText, Text: "hello from agent"}
	ch <- acp.Event{Type: acp.EventComplete}
	close(ch)
	return ch, nil
}

func TestChatHandlerStreamsACPEventsToSlack(t *testing.T) {
	api := &fakeStreamAPI{}
	sessions := &fakeChatSessions{}
	handler := NewChatHandler(api, sessions, time.Hour, 5, nil)
	err := handler.Handle(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "123.4", Text: "hi", Source: "test"})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if sessions.prompt != "hi" || sessions.key.ThreadTS != "123.4" {
		t.Fatalf("unexpected session routing: prompt=%q key=%#v", sessions.prompt, sessions.key)
	}
	if api.appends != 1 || api.stops != 1 {
		t.Fatalf("expected one append and stop, got appends=%d stops=%d", api.appends, api.stops)
	}
}

func TestConversationKeyUsesDMChannelWithoutThread(t *testing.T) {
	key := conversationKey(ChatRequest{TeamID: "T1", ChannelID: "D1", MessageTS: "123.4", DM: true})
	if !key.DM || key.ThreadTS != "" || key.ChannelID != "D1" {
		t.Fatalf("unexpected DM conversation key: %#v", key)
	}
}

func TestStreamThreadTSUsesMessageTimestampForDM(t *testing.T) {
	got := streamThreadTS(ChatRequest{ThreadTS: "", MessageTS: "123.4", DM: true})
	if got != "123.4" {
		t.Fatalf("unexpected stream thread timestamp: %q", got)
	}
}

func TestChatHandlerRequiresSourceMessageTimestampForStreaming(t *testing.T) {
	handler := NewChatHandler(&fakeStreamAPI{}, &fakeChatSessions{}, time.Hour, 5, nil)
	err := handler.Handle(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", Text: "hi", Source: "test"})
	if err == nil || err.Error() != "Slack streaming requires a source message timestamp" {
		t.Fatalf("expected source timestamp error, got: %v", err)
	}
}
