package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/config"
)

// tasksProgress forces the full task-card rendering, for the tests that assert
// TaskCardWriter behaviour specifically. The handler default is the simplified
// single-line view.
func tasksProgress(string) config.ProgressDisplay { return config.ProgressDisplayTasks }

type fakeChatSessions struct {
	// mu guards key/prompt: gateway-level tests drive Prompt from the startChat
	// goroutine and poll these fields, so the write and read race without it.
	// Tests that call ChatHandler.Handle synchronously may read the fields
	// directly — the happens-before is established by the sequential call.
	mu     sync.Mutex
	key    agent.ConversationKey
	prompt string
}

func (f *fakeChatSessions) Prompt(_ context.Context, key agent.ConversationKey, _ agent.SessionMetadata, req agent.PromptRequest) (<-chan agent.Event, error) {
	f.mu.Lock()
	f.key = key
	f.prompt = req.Text
	f.mu.Unlock()
	ch := make(chan agent.Event, 2)
	ch <- agent.Event{Type: agent.EventText, Text: "hello from agent"}
	ch <- agent.Event{Type: agent.EventComplete}
	close(ch)
	return ch, nil
}

// promptText returns the last prompt text, safe to read while the startChat
// goroutine may still be invoking Prompt.
func (f *fakeChatSessions) promptText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompt
}

func (f *fakeChatSessions) Lookup(agent.ConversationKey) (string, bool) { return "", false }
func (f *fakeChatSessions) Cancel(context.Context, string) error      { return nil }
func (f *fakeChatSessionsWithTasks) Lookup(agent.ConversationKey) (string, bool) {
	return "", false
}
func (f *fakeChatSessionsWithTasks) Cancel(context.Context, string) error { return nil }
func (f *fakeChatSessionsWithCompletedTaskThenText) Lookup(agent.ConversationKey) (string, bool) {
	return "", false
}
func (f *fakeChatSessionsWithCompletedTaskThenText) Cancel(context.Context, string) error {
	return nil
}

type fakeChatSessionsWithTasks struct {
	key    agent.ConversationKey
	prompt string
}

func (f *fakeChatSessionsWithTasks) Prompt(_ context.Context, key agent.ConversationKey, _ agent.SessionMetadata, req agent.PromptRequest) (<-chan agent.Event, error) {
	f.key = key
	f.prompt = req.Text
	ch := make(chan agent.Event, 4)
	ch <- agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "task-1", Title: "Searching", Status: agent.TaskStatusInProgress}}
	ch <- agent.Event{Type: agent.EventText, Text: "found it"}
	ch <- agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "task-1", Title: "Searching", Status: agent.TaskStatusComplete}}
	ch <- agent.Event{Type: agent.EventComplete}
	close(ch)
	return ch, nil
}

type fakeChatSessionsWithCompletedTaskThenText struct {
	key    agent.ConversationKey
	prompt string
}

func (f *fakeChatSessionsWithCompletedTaskThenText) Prompt(_ context.Context, key agent.ConversationKey, _ agent.SessionMetadata, req agent.PromptRequest) (<-chan agent.Event, error) {
	f.key = key
	f.prompt = req.Text
	ch := make(chan agent.Event, 4)
	ch <- agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "task-1", Title: "Searching", Status: agent.TaskStatusInProgress}}
	ch <- agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "task-1", Title: "Searching", Status: agent.TaskStatusComplete}}
	ch <- agent.Event{Type: agent.EventText, Text: "final answer"}
	ch <- agent.Event{Type: agent.EventComplete}
	close(ch)
	return ch, nil
}

// fakeChatSessionsRenamedTask reproduces goose's tool-call lifecycle: the tool
// starts running with a raw title, the agent then refines the title in a
// follow-up notification that carries no status, and the run finishes without
// an explicit terminal update for the tool. The renamed task must still be
// finalised as complete, not stranded mid-spinner (which Slack renders as a
// warning once the plan closes).
type fakeChatSessionsRenamedTask struct{}

func (f *fakeChatSessionsRenamedTask) Prompt(_ context.Context, _ agent.ConversationKey, _ agent.SessionMetadata, _ agent.PromptRequest) (<-chan agent.Event, error) {
	ch := make(chan agent.Event, 4)
	ch <- agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "task-1", Title: "edit - /tmp/x.py", Status: agent.TaskStatusInProgress}}
	ch <- agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "task-1", Title: "editing python command"}}
	ch <- agent.Event{Type: agent.EventComplete}
	close(ch)
	return ch, nil
}

func (f *fakeChatSessionsRenamedTask) Lookup(agent.ConversationKey) (string, bool) { return "", false }
func (f *fakeChatSessionsRenamedTask) Cancel(context.Context, string) error      { return nil }

func TestChatHandlerFinalisesRenamedTaskOnSuccess(t *testing.T) {
	api := &fakeStreamAPI{}
	sessions := map[string]ChatSessionManager{"default": &fakeChatSessionsRenamedTask{}}
	resolver := func(req ChatRequest) string { return "default" }
	handler := NewChatHandler(api, sessions, resolver, time.Hour, 5, nil).WithProgressDisplay(tasksProgress)
	if err := handler.Handle(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "123.4", Text: "hi", Source: "test"}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	var statuses []slack.TaskCardStatus
	for _, opts := range append(api.startOptions, api.appendOptions...) {
		chunks, err := extractChunksFromOptions(opts...)
		if err != nil {
			t.Fatalf("extract chunks: %v", err)
		}
		for _, task := range taskChunks(chunks) {
			if task.ID == "task-1" {
				statuses = append(statuses, task.Status)
			}
		}
	}
	if len(statuses) == 0 {
		t.Fatalf("expected task-1 updates, got none")
	}
	if last := statuses[len(statuses)-1]; last != slack.TaskCardStatusComplete {
		t.Fatalf("expected renamed task-1 to end complete, got %q (all: %v)", last, statuses)
	}
	for _, s := range statuses {
		if s == slack.TaskCardStatusError {
			t.Fatalf("renamed task-1 was painted error despite a successful run: %v", statuses)
		}
	}
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
	handler := NewChatHandler(api, sessions, resolver, time.Hour, 5, nil).WithProgressDisplay(tasksProgress)
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
	// Verify the stream was started by opening the Plan block (plan_update)
	// followed by the first task_update chunk.
	chunks, err := extractChunksFromOptions(api.startOptions[0]...)
	if err != nil {
		t.Fatalf("extract chunks from first append: %v", err)
	}
	if plans := planChunks(chunks); len(plans) != 1 {
		t.Fatalf("expected the first task to open a plan block, got %d plan chunks", len(plans))
	}
	tasks := taskChunks(chunks)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task chunk in first append, got %d", len(tasks))
	}
	if tasks[0].ID != "task-1" || tasks[0].Title != "Searching" || tasks[0].Status != slack.TaskCardStatusInProgress {
		t.Fatalf("unexpected first chunk: %+v", tasks[0])
	}
	// Verify the last append is a task completion.
	lastChunks, err := extractChunksFromOptions(api.appendOptions[len(api.appendOptions)-1]...)
	if err != nil {
		t.Fatalf("extract chunks from last append: %v", err)
	}
	lastTasks := taskChunks(lastChunks)
	if len(lastTasks) != 1 {
		t.Fatalf("expected 1 task chunk in last append, got %d", len(lastTasks))
	}
	if lastTasks[0].Status != slack.TaskCardStatusComplete {
		t.Fatalf("expected last chunk status complete, got %q", lastTasks[0].Status)
	}
}

func TestChatHandlerAppendsFinalTextAfterTaskCompletes(t *testing.T) {
	api := &fakeStreamAPI{}
	fakeSessions := &fakeChatSessionsWithCompletedTaskThenText{}
	sessions := map[string]ChatSessionManager{"default": fakeSessions}
	resolver := func(req ChatRequest) string { return "default" }
	handler := NewChatHandler(api, sessions, resolver, time.Hour, 5, nil).WithProgressDisplay(tasksProgress)
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

// fakeChatSessionsRunningTaskThenComplete emits an in-progress task that never
// receives an explicit terminal update before the agent completes — the
// parallel-task case where the agent finishes without closing every card.
type fakeChatSessionsRunningTaskThenComplete struct{}

func (f *fakeChatSessionsRunningTaskThenComplete) Prompt(_ context.Context, _ agent.ConversationKey, _ agent.SessionMetadata, _ agent.PromptRequest) (<-chan agent.Event, error) {
	ch := make(chan agent.Event, 4)
	ch <- agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "task-1", Title: "Searching", Status: agent.TaskStatusInProgress}}
	ch <- agent.Event{Type: agent.EventText, Text: "done"}
	ch <- agent.Event{Type: agent.EventComplete}
	close(ch)
	return ch, nil
}

func (f *fakeChatSessionsRunningTaskThenComplete) Lookup(agent.ConversationKey) (string, bool) {
	return "", false
}
func (f *fakeChatSessionsRunningTaskThenComplete) Cancel(context.Context, string) error { return nil }

func TestChatHandlerCompletesStillRunningTasksOnSuccess(t *testing.T) {
	api := &fakeStreamAPI{}
	sessions := map[string]ChatSessionManager{"default": &fakeChatSessionsRunningTaskThenComplete{}}
	resolver := func(req ChatRequest) string { return "default" }
	handler := NewChatHandler(api, sessions, resolver, time.Hour, 5, nil).WithProgressDisplay(tasksProgress)
	if err := handler.Handle(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "123.4", Text: "hi", Source: "test"}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	// The task that was still in-progress when the agent completed must be
	// finalised as complete — never left spinning and never painted red.
	var statuses []slack.TaskCardStatus
	for _, opts := range append(api.startOptions, api.appendOptions...) {
		chunks, err := extractChunksFromOptions(opts...)
		if err != nil {
			t.Fatalf("extract chunks: %v", err)
		}
		for _, task := range taskChunks(chunks) {
			if task.ID == "task-1" {
				statuses = append(statuses, task.Status)
			}
		}
	}
	if len(statuses) == 0 {
		t.Fatalf("expected task-1 updates, got none")
	}
	last := statuses[len(statuses)-1]
	if last != slack.TaskCardStatusComplete {
		t.Fatalf("expected task-1 to end complete, got %q (all: %v)", last, statuses)
	}
	for _, s := range statuses {
		if s == slack.TaskCardStatusError {
			t.Fatalf("task-1 was painted error despite a successful run: %v", statuses)
		}
	}
}

// cancellableTaskSessions emits an in-progress task, then — once its context
// is cancelled — surfaces the cancellation as an EventError carrying ctx.Err(),
// exactly as the ACP process client does when a prompt is interrupted.
type cancellableTaskSessions struct {
	taskSent chan struct{}
}

func (f *cancellableTaskSessions) Prompt(ctx context.Context, _ agent.ConversationKey, _ agent.SessionMetadata, _ agent.PromptRequest) (<-chan agent.Event, error) {
	ch := make(chan agent.Event)
	go func() {
		defer close(ch)
		select {
		case ch <- agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "t1", Title: "Working", Status: agent.TaskStatusInProgress}}:
		case <-ctx.Done():
			return
		}
		if f.taskSent != nil {
			close(f.taskSent)
		}
		<-ctx.Done()
		select {
		case ch <- agent.Event{Type: agent.EventError, Error: ctx.Err()}:
		case <-time.After(time.Second):
		}
	}()
	return ch, nil
}

func (f *cancellableTaskSessions) Lookup(agent.ConversationKey) (string, bool) { return "", false }
func (f *cancellableTaskSessions) Cancel(context.Context, string) error      { return nil }

func TestChatHandlerDoesNotFailTasksOnInterrupt(t *testing.T) {
	api := &fakeStreamAPI{}
	taskSent := make(chan struct{})
	sessions := map[string]ChatSessionManager{"default": &cancellableTaskSessions{taskSent: taskSent}}
	resolver := func(req ChatRequest) string { return "default" }
	handler := NewChatHandler(api, sessions, resolver, time.Millisecond, 1, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- handler.Handle(ctx, ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "123.4", Text: "hi", Source: "test"})
	}()
	select {
	case <-taskSent:
	case <-time.After(2 * time.Second):
		t.Fatalf("task never landed; handler likely stalled")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil on interrupt, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Handle did not return within 2s of cancel")
	}
	// The in-progress task was cut short by an interrupt, not a failure: no
	// task card may be pushed to the error status.
	for _, opts := range append(api.startOptions, api.appendOptions...) {
		chunks, err := extractChunksFromOptions(opts...)
		if err != nil {
			t.Fatalf("extract chunks: %v", err)
		}
		for _, task := range taskChunks(chunks) {
			if task.Status == slack.TaskCardStatusError {
				t.Fatalf("interrupted task was painted error: %+v", task)
			}
		}
	}
}

func TestChatHandlerClearsStatusOnFreshContextAfterCancel(t *testing.T) {
	api := &fakeStreamAPI{rejectCanceledStatus: true}
	firstChunkSent := make(chan struct{})
	sessions := map[string]ChatSessionManager{"default": &cancellableChatSessions{firstChunkSent: firstChunkSent}}
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
			t.Fatalf("expected nil on interrupt, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Handle did not return within 2s of cancel")
	}
	// Even though the request context was cancelled, the assistant status must
	// be cleared via a fresh context so "is thinking..." does not linger.
	_, params := api.statusSnapshot()
	if len(params) == 0 || params[len(params)-1].Status != "" {
		t.Fatalf("expected the final status call to clear the indicator on a fresh context, got %#v", params)
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

func (f *blockingChatSessions) Prompt(_ context.Context, _ agent.ConversationKey, _ agent.SessionMetadata, _ agent.PromptRequest) (<-chan agent.Event, error) {
	ch := make(chan agent.Event, 2)
	go func() {
		<-f.release
		ch <- agent.Event{Type: agent.EventText, Text: "hi"}
		ch <- agent.Event{Type: agent.EventComplete}
		close(ch)
	}()
	return ch, nil
}

func (f *blockingChatSessions) Lookup(agent.ConversationKey) (string, bool) { return "", false }
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

func (f *cancellableChatSessions) Prompt(ctx context.Context, _ agent.ConversationKey, _ agent.SessionMetadata, _ agent.PromptRequest) (<-chan agent.Event, error) {
	ch := make(chan agent.Event)
	go func() {
		defer close(ch)
		select {
		case ch <- agent.Event{Type: agent.EventText, Text: "partial reply"}:
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

func (f *cancellableChatSessions) Lookup(agent.ConversationKey) (string, bool) { return "", false }
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

// stallingChatSessions emits a task and a text chunk, then goes silent until
// its prompt context is cancelled — a stalled agent that never completes. It
// records session/cancel calls and hands out a session id so the idle-timeout
// path can look one up.
type stallingChatSessions struct {
	sessionID string
	cancelled []string
}

func (f *stallingChatSessions) Prompt(ctx context.Context, _ agent.ConversationKey, _ agent.SessionMetadata, _ agent.PromptRequest) (<-chan agent.Event, error) {
	ch := make(chan agent.Event)
	go func() {
		defer close(ch)
		select {
		case ch <- agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "task-1", Title: "Working", Status: agent.TaskStatusInProgress}}:
		case <-ctx.Done():
			return
		}
		select {
		case ch <- agent.Event{Type: agent.EventText, Text: "partial reply"}:
		case <-ctx.Done():
			return
		}
		<-ctx.Done() // stall until the handler gives up and cancels the prompt
	}()
	return ch, nil
}

func (f *stallingChatSessions) Lookup(agent.ConversationKey) (string, bool) { return f.sessionID, true }
func (f *stallingChatSessions) Cancel(_ context.Context, sessionID string) error {
	f.cancelled = append(f.cancelled, sessionID)
	return nil
}

func TestChatHandlerIdleTimeoutStopsAgentAndPostsNotice(t *testing.T) {
	api := &fakeStreamAPI{}
	fake := &stallingChatSessions{sessionID: "sess-1"}
	sessions := map[string]ChatSessionManager{"default": fake}
	resolver := func(req ChatRequest) string { return "default" }
	handler := NewChatHandler(api, sessions, resolver, time.Hour, 1, nil).WithIdleTimeout(50 * time.Millisecond)

	err := handler.Handle(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "123.4", Text: "hi", Source: "test"})
	if err != nil {
		t.Fatalf("expected Handle to return nil on idle timeout, got: %v", err)
	}
	// The agent was asked to stop so it stops burning undeliverable work.
	if len(fake.cancelled) != 1 || fake.cancelled[0] != "sess-1" {
		t.Fatalf("expected one session/cancel for sess-1, got %v", fake.cancelled)
	}
	// A real, honest notice was posted rather than a silent dead UI.
	var posted strings.Builder
	for _, opts := range append(api.startOptions, api.appendOptions...) {
		if text, err := extractMarkdownTextFromOptions(opts...); err == nil {
			posted.WriteString(text)
		}
	}
	if !strings.Contains(posted.String(), "asked it to stop") {
		t.Fatalf("expected an idle-timeout notice, got markdown: %q", posted.String())
	}
	// The stalled task card is never repainted red: the agent did not fail.
	for _, opts := range append(api.startOptions, api.appendOptions...) {
		chunks, cerr := extractChunksFromOptions(opts...)
		if cerr != nil {
			t.Fatalf("extract chunks: %v", cerr)
		}
		for _, task := range taskChunks(chunks) {
			if task.Status == slack.TaskCardStatusError {
				t.Fatalf("idle timeout falsely painted task %q as error", task.ID)
			}
		}
	}
	if api.stops == 0 {
		t.Fatalf("expected the stream to be stopped on idle timeout")
	}
}

// steadyChatSessions emits several events spaced under the idle window, then
// completes — a long but continuously-progressing turn that must never time out.
type steadyChatSessions struct {
	gap       time.Duration
	events    int
	cancelled int
}

func (f *steadyChatSessions) Prompt(_ context.Context, _ agent.ConversationKey, _ agent.SessionMetadata, _ agent.PromptRequest) (<-chan agent.Event, error) {
	ch := make(chan agent.Event)
	go func() {
		defer close(ch)
		for i := 0; i < f.events; i++ {
			time.Sleep(f.gap)
			ch <- agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "task-1", Title: "Working", Status: agent.TaskStatusInProgress}}
		}
		ch <- agent.Event{Type: agent.EventText, Text: "done"}
		ch <- agent.Event{Type: agent.EventComplete}
	}()
	return ch, nil
}

func (f *steadyChatSessions) Lookup(agent.ConversationKey) (string, bool) { return "sess-1", true }
func (f *steadyChatSessions) Cancel(context.Context, string) error      { f.cancelled++; return nil }

func TestChatHandlerIdleTimerResetsOnActivity(t *testing.T) {
	api := &fakeStreamAPI{}
	// 10 events, 10ms apart (~100ms of steady activity) under a 60ms idle window:
	// no single gap approaches the window, so the turn must finish, not time out.
	fake := &steadyChatSessions{gap: 10 * time.Millisecond, events: 10}
	sessions := map[string]ChatSessionManager{"default": fake}
	resolver := func(req ChatRequest) string { return "default" }
	handler := NewChatHandler(api, sessions, resolver, time.Hour, 1, nil).WithIdleTimeout(60 * time.Millisecond)

	err := handler.Handle(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "123.4", Text: "hi", Source: "test"})
	if err != nil {
		t.Fatalf("expected Handle to complete a steadily-progressing turn, got: %v", err)
	}
	if fake.cancelled != 0 {
		t.Fatalf("a progressing turn must not be cancelled as idle, got %d cancels", fake.cancelled)
	}
	var posted strings.Builder
	for _, opts := range append(api.startOptions, api.appendOptions...) {
		if text, err := extractMarkdownTextFromOptions(opts...); err == nil {
			posted.WriteString(text)
		}
	}
	if strings.Contains(posted.String(), "asked it to stop") {
		t.Fatalf("a progressing turn must not post an idle-timeout notice, got: %q", posted.String())
	}
}
