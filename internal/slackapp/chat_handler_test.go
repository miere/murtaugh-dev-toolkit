package slackapp

import (
	"context"
	"testing"
	"time"

	"github.com/slack-go/slack"

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

func (f *fakeChatSessions) Lookup(acp.ConversationKey) (string, bool)  { return "", false }
func (f *fakeChatSessions) Cancel(context.Context, string) error       { return nil }
func (f *fakeChatSessionsWithTasks) Lookup(acp.ConversationKey) (string, bool) {
	return "", false
}
func (f *fakeChatSessionsWithTasks) Cancel(context.Context, string) error { return nil }
func (f *fakeChatSessionsWithCompletedTaskThenText) Lookup(acp.ConversationKey) (string, bool) {
	return "", false
}
func (f *fakeChatSessionsWithCompletedTaskThenText) Cancel(context.Context, string) error {
	return nil
}

type fakeChatSessionsWithTasks struct {
	key    acp.ConversationKey
	prompt string
}

func (f *fakeChatSessionsWithTasks) Prompt(_ context.Context, key acp.ConversationKey, _ acp.SessionMetadata, req acp.PromptRequest) (<-chan acp.Event, error) {
	f.key = key
	f.prompt = req.Text
	ch := make(chan acp.Event, 4)
	ch <- acp.Event{Type: acp.EventTask, Task: &acp.TaskEvent{ID: "task-1", Title: "Searching", Status: acp.TaskStatusInProgress}}
	ch <- acp.Event{Type: acp.EventText, Text: "found it"}
	ch <- acp.Event{Type: acp.EventTask, Task: &acp.TaskEvent{ID: "task-1", Title: "Searching", Status: acp.TaskStatusComplete}}
	ch <- acp.Event{Type: acp.EventComplete}
	close(ch)
	return ch, nil
}

type fakeChatSessionsWithCompletedTaskThenText struct {
	key    acp.ConversationKey
	prompt string
}

func (f *fakeChatSessionsWithCompletedTaskThenText) Prompt(_ context.Context, key acp.ConversationKey, _ acp.SessionMetadata, req acp.PromptRequest) (<-chan acp.Event, error) {
	f.key = key
	f.prompt = req.Text
	ch := make(chan acp.Event, 4)
	ch <- acp.Event{Type: acp.EventTask, Task: &acp.TaskEvent{ID: "task-1", Title: "Searching", Status: acp.TaskStatusInProgress}}
	ch <- acp.Event{Type: acp.EventTask, Task: &acp.TaskEvent{ID: "task-1", Title: "Searching", Status: acp.TaskStatusComplete}}
	ch <- acp.Event{Type: acp.EventText, Text: "final answer"}
	ch <- acp.Event{Type: acp.EventComplete}
	close(ch)
	return ch, nil
}

func TestChatHandlerStreamsACPEventsToSlack(t *testing.T) {
	api := &fakeStreamAPI{}
	fakeSessions := &fakeChatSessions{}
	sessions := map[string]ChatSessionManager{"default": fakeSessions}
	resolver := func(req ChatRequest) string { return "default" }
	handler := NewChatHandler(api, sessions, resolver, time.Hour, 5, nil)
	err := handler.Handle(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "123.4", Text: "hi", Source: "test"})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if fakeSessions.prompt != "hi" || fakeSessions.key.ThreadTS != "123.4" {
		t.Fatalf("unexpected session routing: prompt=%q key=%#v", fakeSessions.prompt, fakeSessions.key)
	}
	// Two status calls: the initial "is thinking..." and the explicit clear on
	// completion. The events complete fast enough that the periodic refresher
	// (defaultStatusRefreshInterval) never fires.
	if api.statusCalls != 2 {
		t.Fatalf("expected two status calls (initial + clear), got %d", api.statusCalls)
	}
	if sp := api.statusParams[0]; sp.ChannelID != "C1" || sp.ThreadTS != "123.4" || sp.Status != "is thinking..." {
		t.Fatalf("unexpected initial status params: %#v", sp)
	}
	if sp := api.statusParams[len(api.statusParams)-1]; sp.ChannelID != "C1" || sp.ThreadTS != "123.4" || sp.Status != "" {
		t.Fatalf("unexpected final status params (expected explicit clear): %#v", sp)
	}
	if api.startedChannel != "C1" {
		t.Fatalf("expected stream started on C1, got %q", api.startedChannel)
	}
	if api.appends != 1 || api.stops != 1 {
		t.Fatalf("expected one append and stop, got appends=%d stops=%d", api.appends, api.stops)
	}
}

func TestChatHandlerRoutesTaskEventsToTaskCardWriter(t *testing.T) {
	api := &fakeStreamAPI{}
	fakeSessions := &fakeChatSessionsWithTasks{}
	sessions := map[string]ChatSessionManager{"default": fakeSessions}
	resolver := func(req ChatRequest) string { return "default" }
	handler := NewChatHandler(api, sessions, resolver, time.Hour, 5, nil)
	err := handler.Handle(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "123.4", Text: "hi", Source: "test"})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if fakeSessions.prompt != "hi" || fakeSessions.key.ThreadTS != "123.4" {
		t.Fatalf("unexpected session routing: prompt=%q key=%#v", fakeSessions.prompt, fakeSessions.key)
	}
	if api.startedChannel != "C1" {
		t.Fatalf("expected stream started on C1, got %q", api.startedChannel)
	}
	// Expect 2 appends: text + task complete. The first task update starts the stream.
	if api.appends != 2 || len(api.startOptions) != 1 {
		t.Fatalf("expected task start on stream start plus 2 appends, got starts=%d appends=%d", len(api.startOptions), api.appends)
	}
	// Verify the stream was started with a task update chunk.
	chunks, err := extractChunksFromOptions(api.startOptions[0]...)
	if err != nil {
		t.Fatalf("extract chunks from first append: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk in first append, got %d", len(chunks))
	}
	chunk, ok := chunks[0].(slack.TaskUpdateChunk)
	if !ok {
		t.Fatalf("expected TaskUpdateChunk, got %T", chunks[0])
	}
	if chunk.ID != "task-1" || chunk.Title != "Searching" || chunk.Status != slack.TaskCardStatusInProgress {
		t.Fatalf("unexpected first chunk: %+v", chunk)
	}
	// Verify the last append is a task completion.
	lastChunks, err := extractChunksFromOptions(api.appendOptions[len(api.appendOptions)-1]...)
	if err != nil {
		t.Fatalf("extract chunks from last append: %v", err)
	}
	if len(lastChunks) != 1 {
		t.Fatalf("expected 1 chunk in last append, got %d", len(lastChunks))
	}
	lastChunk, ok := lastChunks[0].(slack.TaskUpdateChunk)
	if !ok {
		t.Fatalf("expected TaskUpdateChunk in last append, got %T", lastChunks[0])
	}
	if lastChunk.Status != slack.TaskCardStatusComplete {
		t.Fatalf("expected last chunk status complete, got %q", lastChunk.Status)
	}
}

func TestChatHandlerAppendsFinalTextAfterTaskCompletes(t *testing.T) {
	api := &fakeStreamAPI{}
	fakeSessions := &fakeChatSessionsWithCompletedTaskThenText{}
	sessions := map[string]ChatSessionManager{"default": fakeSessions}
	resolver := func(req ChatRequest) string { return "default" }
	handler := NewChatHandler(api, sessions, resolver, time.Hour, 5, nil)
	err := handler.Handle(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "123.4", Text: "hi", Source: "test"})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if api.appends != 2 || len(api.startOptions) != 1 || api.stops != 1 {
		t.Fatalf("expected task start plus task completion and final text appends, got starts=%d appends=%d stops=%d", len(api.startOptions), api.appends, api.stops)
	}
	chunks, err := extractChunksFromOptions(api.appendOptions[0]...)
	if err != nil {
		t.Fatalf("extract chunks from task completion append: %v", err)
	}
	if len(chunks) != 1 || chunks[0].(slack.TaskUpdateChunk).Status != slack.TaskCardStatusComplete {
		t.Fatalf("expected first append to complete the task, got %+v", chunks)
	}
	text, err := extractMarkdownTextFromOptions(api.appendOptions[1]...)
	if err != nil {
		t.Fatalf("extract markdown text from final append: %v", err)
	}
	if text != "final answer" {
		t.Fatalf("expected final text append, got %q", text)
	}
	// Slack's chat.appendStream rejects a chunks-only stream that subsequently
	// switches to the markdown_text form parameter; once we have sent task
	// chunks the rest of the stream must keep going through the chunks API.
	textChunks, err := extractChunksFromOptions(api.appendOptions[1]...)
	if err != nil {
		t.Fatalf("extract chunks from final append: %v", err)
	}
	if len(textChunks) != 1 {
		t.Fatalf("expected final text to be sent as a single chunk, got %d chunks", len(textChunks))
	}
	if md, ok := textChunks[0].(slack.MarkdownTextChunk); !ok || md.Text != "final answer" {
		t.Fatalf("expected final text to be a markdown_text chunk, got %+v", textChunks[0])
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

type blockingChatSessions struct {
	release chan struct{}
}

func (f *blockingChatSessions) Prompt(_ context.Context, _ acp.ConversationKey, _ acp.SessionMetadata, _ acp.PromptRequest) (<-chan acp.Event, error) {
	ch := make(chan acp.Event, 2)
	go func() {
		<-f.release
		ch <- acp.Event{Type: acp.EventText, Text: "hi"}
		ch <- acp.Event{Type: acp.EventComplete}
		close(ch)
	}()
	return ch, nil
}

func (f *blockingChatSessions) Lookup(acp.ConversationKey) (string, bool) { return "", false }
func (f *blockingChatSessions) Cancel(context.Context, string) error      { return nil }

func TestChatHandlerRefreshesAssistantStatusWhileEventsPending(t *testing.T) {
	api := &fakeStreamAPI{}
	release := make(chan struct{})
	fakeSessions := &blockingChatSessions{release: release}
	sessions := map[string]ChatSessionManager{"default": fakeSessions}
	resolver := func(req ChatRequest) string { return "default" }
	handler := NewChatHandler(api, sessions, resolver, time.Hour, 5, nil)
	handler.statusRefreshInterval = 5 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		done <- handler.Handle(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "123.4", Text: "hi", Source: "test"})
	}()
	// Give the periodic refresher time to fire several times before any events flow.
	deadline := time.Now().Add(time.Second)
	for {
		calls, _ := api.statusSnapshot()
		if calls >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status refresher never fired enough times, got %d", calls)
		}
		time.Sleep(2 * time.Millisecond)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	calls, params := api.statusSnapshot()
	if calls < 4 {
		t.Fatalf("expected at least 4 status calls (initial + refreshes + clear), got %d", calls)
	}
	if params[0].Status != "is thinking..." {
		t.Fatalf("expected initial status to be \"is thinking...\", got %q", params[0].Status)
	}
	if last := params[len(params)-1]; last.Status != "" {
		t.Fatalf("expected last status call to clear the status, got %q", last.Status)
	}
	for i := 1; i < len(params)-1; i++ {
		if params[i].Status != "is thinking..." {
			t.Fatalf("expected intermediate refresh %d to re-assert status, got %q", i, params[i].Status)
		}
	}
}

// cancellableChatSessions emits one text chunk then blocks waiting for
// ctx cancellation, closing the events channel on cancel. It simulates
// a live ACP agent that is mid-response when the user interrupts.
type cancellableChatSessions struct {
	firstChunkSent chan struct{}
}

func (f *cancellableChatSessions) Prompt(ctx context.Context, _ acp.ConversationKey, _ acp.SessionMetadata, _ acp.PromptRequest) (<-chan acp.Event, error) {
	ch := make(chan acp.Event)
	go func() {
		defer close(ch)
		select {
		case ch <- acp.Event{Type: acp.EventText, Text: "partial reply"}:
		case <-ctx.Done():
			return
		}
		if f.firstChunkSent != nil {
			close(f.firstChunkSent)
		}
		<-ctx.Done()
	}()
	return ch, nil
}

func (f *cancellableChatSessions) Lookup(acp.ConversationKey) (string, bool) { return "", false }
func (f *cancellableChatSessions) Cancel(context.Context, string) error      { return nil }

func TestChatHandlerInterruptedByCancelReturnsNil(t *testing.T) {
	api := &fakeStreamAPI{}
	firstChunkSent := make(chan struct{})
	fakeSessions := &cancellableChatSessions{firstChunkSent: firstChunkSent}
	sessions := map[string]ChatSessionManager{"default": fakeSessions}
	resolver := func(req ChatRequest) string { return "default" }
	handler := NewChatHandler(api, sessions, resolver, time.Millisecond, 1, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- handler.Handle(ctx, ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "123.4", Text: "hi", Source: "test"})
	}()
	select {
	case <-firstChunkSent:
	case <-time.After(2 * time.Second):
		t.Fatalf("first chunk never landed; handler likely stalled")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected Handle to swallow caller-initiated cancel and return nil, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Handle did not return within 2s of cancel")
	}
	if api.stops == 0 {
		t.Fatalf("expected writer.Stop to be invoked on interrupt, got stops=%d", api.stops)
	}
}

// (Distinguishing context.Canceled vs context.DeadlineExceeded is
// enforced at the defer level in ChatHandler.Handle via
// errors.Is(context.Cause(ctx), context.Canceled); a behavioural test
// would require the shared fakeStreamAPI to propagate ctx errors,
// which is out of scope here.)

func TestChatHandlerRequiresSourceMessageTimestampForStreaming(t *testing.T) {
	sessions := map[string]ChatSessionManager{"default": &fakeChatSessions{}}
	resolver := func(req ChatRequest) string { return "default" }
	handler := NewChatHandler(&fakeStreamAPI{}, sessions, resolver, time.Hour, 5, nil)
	err := handler.Handle(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", Text: "hi", Source: "test"})
	if err == nil || err.Error() != "Slack streaming requires a source message timestamp" {
		t.Fatalf("expected source timestamp error, got: %v", err)
	}
}
