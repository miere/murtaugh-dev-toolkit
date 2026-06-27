package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

// recordingUploader captures every UploadAttachment call so tests can assert the
// file and its destination.
type recordingUploader struct {
	files    []*agent.AttachmentEvent
	channels []string
	threads  []string
	err      error
}

func (u *recordingUploader) UploadAttachment(_ context.Context, channelID, threadTS string, a *agent.AttachmentEvent) error {
	u.files = append(u.files, a)
	u.channels = append(u.channels, channelID)
	u.threads = append(u.threads, threadTS)
	return u.err
}

// fakeChatSessionsAttachmentOnly emits a single attachment and then completes
// with no reply text — the "here is your file" turn.
type fakeChatSessionsAttachmentOnly struct{}

func (f *fakeChatSessionsAttachmentOnly) Prompt(_ context.Context, _ agent.ConversationKey, _ agent.SessionMetadata, _ agent.PromptRequest) (<-chan agent.Event, error) {
	ch := make(chan agent.Event, 2)
	ch <- agent.Event{Type: agent.EventAttachment, Attachment: &agent.AttachmentEvent{Filename: "report.pdf", Data: []byte("bytes")}}
	ch <- agent.Event{Type: agent.EventComplete}
	close(ch)
	return ch, nil
}
func (f *fakeChatSessionsAttachmentOnly) Lookup(agent.ConversationKey) (string, bool) {
	return "", false
}
func (f *fakeChatSessionsAttachmentOnly) Cancel(context.Context, string) error { return nil }

func TestChatHandlerDeliversAttachmentAndSuppressesEmptyNote(t *testing.T) {
	api := &fakeStreamAPI{}
	up := &recordingUploader{}
	sessions := map[string]ChatSessionManager{"default": &fakeChatSessionsAttachmentOnly{}}
	resolver := func(ChatRequest) string { return "default" }
	handler := NewChatHandler(api, sessions, resolver, time.Hour, 5, nil).WithUploader(up)

	if err := handler.Handle(context.Background(), ChatRequest{
		TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "123.4", Text: "send it", Source: "test",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(up.files) != 1 || up.files[0].Filename != "report.pdf" {
		t.Fatalf("uploader calls = %+v, want one report.pdf", up.files)
	}
	if up.channels[0] != "C1" || up.threads[0] != "123.4" {
		t.Fatalf("upload location = %s/%s, want C1/123.4", up.channels[0], up.threads[0])
	}
	// An attachment-only turn is NOT empty: no text stream is opened, so the
	// empty-reply note never appears.
	if len(api.startOptions) != 0 || api.appends != 0 {
		t.Fatalf("expected no text stream for an attachment-only turn, got starts=%d appends=%d", len(api.startOptions), api.appends)
	}
}
