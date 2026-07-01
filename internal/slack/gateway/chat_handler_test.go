package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/config"
)

// tasksProgress forces the full task-card rendering, for the tests that assert
// TaskCardWriter behaviour specifically. The handler default is the simplified
// single-line view.
func tasksProgress(string) config.ProgressDisplay { return config.ProgressDisplayTasks }

// allStreamWrites returns every chunk group written to the stream — the initial
// start options plus every append — so a test can assert what landed regardless
// of which of the (now separate) text/tool streams carried it.
func allStreamWrites(t *testing.T, api *fakeStreamAPI) [][]slack.StreamChunk {
	t.Helper()
	var groups [][]slack.StreamChunk
	for _, opts := range append(append([][]slack.MsgOption{}, api.startOptions...), api.appendOptions...) {
		chunks, err := extractChunksFromOptions(opts...)
		if err != nil {
			t.Fatalf("extract chunks: %v", err)
		}
		groups = append(groups, chunks)
	}
	return groups
}

// assertToolTextSeparation is the coherence guarantee for tasks mode: no single
// stream write ever mixes tool (plan/task) chunks with reply (markdown) chunks.
// Task cards and reply text always land in separate messages, so a card can never
// interleave into an unflushed text run.
func assertToolTextSeparation(t *testing.T, api *fakeStreamAPI) {
	t.Helper()
	for i, chunks := range allStreamWrites(t, api) {
		var toolN, textN int
		for _, c := range chunks {
			switch c.(type) {
			case slack.TaskUpdateChunk, slack.PlanUpdateChunk:
				toolN++
			case slack.MarkdownTextChunk:
				textN++
			}
		}
		if toolN > 0 && textN > 0 {
			t.Fatalf("stream write %d mixes %d tool chunks with %d reply chunks; cards and text must be separate messages", i, toolN, textN)
		}
	}
}

// taskStatusSeen reports whether any stream write set task id to status.
func taskStatusSeen(t *testing.T, api *fakeStreamAPI, id string, status slack.TaskCardStatus) bool {
	t.Helper()
	for _, chunks := range allStreamWrites(t, api) {
		for _, tc := range taskChunks(chunks) {
			if tc.ID == id && tc.Status == status {
				return true
			}
		}
	}
	return false
}

// markdownSeen reports whether any stream write delivered reply text containing want.
func markdownSeen(t *testing.T, api *fakeStreamAPI, want string) bool {
	t.Helper()
	for _, chunks := range allStreamWrites(t, api) {
		for _, c := range chunks {
			if md, ok := c.(slack.MarkdownTextChunk); ok && strings.Contains(md.Text, want) {
				return true
			}
		}
	}
	return false
}

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
func (f *fakeChatSessions) Cancel(context.Context, string) error        { return nil }
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
func (f *fakeChatSessionsRenamedTask) Cancel(context.Context, string) error        { return nil }

// fakePermissionAsker records the request it was asked and returns a fixed
// decision, standing in for the Slack approval gate.
type fakePermissionAsker struct {
	called bool
	loc    agent.TurnLocation
	req    agent.PermissionRequest
	ret    string
}

func (a *fakePermissionAsker) AskPermission(_ context.Context, loc agent.TurnLocation, req agent.PermissionRequest) (string, error) {
	a.called = true
	a.loc = loc
	a.req = req
	return a.ret, nil
}

// fakeChatSessionsWithPermission emits a reply, then an EventPermission, blocks on
// its decision, and continues — the ACP approval shape. It records the decision it
// received so the test can assert it flowed back to the agent.
type fakeChatSessionsWithPermission struct {
	gotDecision chan string
}

func (f *fakeChatSessionsWithPermission) Prompt(_ context.Context, _ agent.ConversationKey, _ agent.SessionMetadata, _ agent.PromptRequest) (<-chan agent.Event, error) {
	ch := make(chan agent.Event)
	go func() {
		defer close(ch)
		ch <- agent.Event{Type: agent.EventText, Text: "I will run a command."}
		dec := make(chan string, 1)
		ch <- agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionPrompt{
			Request: agent.PermissionRequest{
				ToolKind:  "execute",
				ToolTitle: "rm -rf /tmp/x",
				Options:   []agent.PermissionOption{{ID: "a", Name: "Allow", Kind: "allow_once"}},
			},
			Decision: dec,
		}}
		f.gotDecision <- <-dec
		ch <- agent.Event{Type: agent.EventText, Text: "Done."}
		ch <- agent.Event{Type: agent.EventComplete}
	}()
	return ch, nil
}

func (f *fakeChatSessionsWithPermission) Lookup(agent.ConversationKey) (string, bool) {
	return "", false
}
func (f *fakeChatSessionsWithPermission) Cancel(context.Context, string) error { return nil }

func TestChatHandlerResolvesACPPermissionInOrder(t *testing.T) {
	api := &fakeStreamAPI{}
	asker := &fakePermissionAsker{ret: "a"}
	f := &fakeChatSessionsWithPermission{gotDecision: make(chan string, 1)}
	sessions := map[string]ChatSessionManager{"default": f}
	handler := NewChatHandler(api, sessions, func(ChatRequest) ChatRoute { return ChatRoute{Agent: "default", ReplyOnThread: true} }, time.Hour, 5, nil).
		WithPermissionAsker(asker)
	if err := handler.Handle(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "123.4", Text: "hi", Source: "test"}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	// The gate was consulted in the turn's own thread, with the request intact.
	if !asker.called {
		t.Fatal("permission asker was never consulted")
	}
	if asker.loc.ChannelID != "C1" || asker.loc.ThreadTS != "123.4" || asker.loc.UserID != "U1" {
		t.Fatalf("asker got wrong location: %+v", asker.loc)
	}
	if asker.req.ToolTitle != "rm -rf /tmp/x" || asker.req.ToolKind != "execute" {
		t.Fatalf("asker got wrong request: %+v", asker.req)
	}
	// The decision flowed back to the agent so it could proceed.
	select {
	case got := <-f.gotDecision:
		if got != "a" {
			t.Fatalf("agent received wrong decision: %q", got)
		}
	default:
		t.Fatal("agent never received a permission decision")
	}
	// The reply was settled into two separate committed messages around the card:
	// the prose before the request closes (BeginInterjection) before the gate is
	// asked, and the prose after opens a fresh message — so the approval card never
	// lands inside an unfinished stream.
	if len(api.startOptions) != 2 {
		t.Fatalf("expected the reply to be split into two messages around the approval, got %d", len(api.startOptions))
	}
	if api.stops != 2 {
		t.Fatalf("expected both reply messages to be committed (stopped), got %d", api.stops)
	}
}

func TestChatHandlerFinalisesRenamedTaskOnSuccess(t *testing.T) {
	api := &fakeStreamAPI{}
	sessions := map[string]ChatSessionManager{"default": &fakeChatSessionsRenamedTask{}}
	resolver := func(req ChatRequest) ChatRoute { return ChatRoute{Agent: "default", ReplyOnThread: true} }
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
	resolver := func(req ChatRequest) ChatRoute { return ChatRoute{Agent: "default", ReplyOnThread: true} }
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
	resolver := func(req ChatRequest) ChatRoute { return ChatRoute{Agent: "default", ReplyOnThread: true} }
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
	// Coherence guarantee: task cards and reply text render in separate messages —
	// no stream write mixes tool chunks with reply markdown.
	assertToolTextSeparation(t, api)
	// The first task opens a Plan block with the task in progress.
	first := allStreamWrites(t, api)[0]
	if plans := planChunks(first); len(plans) != 1 {
		t.Fatalf("expected the first task to open a plan block, got %d plan chunks", len(plans))
	}
	firstTasks := taskChunks(first)
	if len(firstTasks) != 1 || firstTasks[0].ID != "task-1" || firstTasks[0].Title != "Searching" || firstTasks[0].Status != slack.TaskCardStatusInProgress {
		t.Fatalf("unexpected first task chunk: %+v", firstTasks)
	}
	// The task reaches a completed card, and the reply text is delivered.
	if !taskStatusSeen(t, api, "task-1", slack.TaskCardStatusComplete) {
		t.Fatalf("task-1 never reached a completed card")
	}
	if !markdownSeen(t, api, "found it") {
		t.Fatalf("reply text was not delivered")
	}
}

func TestChatHandlerAppendsFinalTextAfterTaskCompletes(t *testing.T) {
	api := &fakeStreamAPI{}
	fakeSessions := &fakeChatSessionsWithCompletedTaskThenText{}
	sessions := map[string]ChatSessionManager{"default": fakeSessions}
	resolver := func(req ChatRequest) ChatRoute { return ChatRoute{Agent: "default", ReplyOnThread: true} }
	handler := NewChatHandler(api, sessions, resolver, time.Hour, 5, nil).WithProgressDisplay(tasksProgress)
	err := handler.Handle(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "123.4", Text: "hi", Source: "test"})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	// Separating cards into their own message structurally resolves the old
	// constraint that a chunks-only stream could not later switch to markdown: the
	// reply text is a fresh stream, never mixed with the task chunks.
	assertToolTextSeparation(t, api)
	// The task reaches a completed card and the reply text lands after it.
	if !taskStatusSeen(t, api, "task-1", slack.TaskCardStatusComplete) {
		t.Fatalf("task-1 never reached a completed card")
	}
	if !markdownSeen(t, api, "final answer") {
		t.Fatalf("final reply text was not delivered")
	}
	// The reply text is delivered as a markdown chunk in its own stream write,
	// distinct from any task chunk.
	var replyGroups int
	for _, chunks := range allStreamWrites(t, api) {
		for _, c := range chunks {
			if md, ok := c.(slack.MarkdownTextChunk); ok && md.Text == "final answer" {
				replyGroups++
			}
		}
	}
	if replyGroups != 1 {
		t.Fatalf("expected the reply text in exactly one stream write, got %d", replyGroups)
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
	resolver := func(req ChatRequest) ChatRoute { return ChatRoute{Agent: "default", ReplyOnThread: true} }
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
func (f *cancellableTaskSessions) Cancel(context.Context, string) error        { return nil }

func TestChatHandlerDoesNotFailTasksOnInterrupt(t *testing.T) {
	api := &fakeStreamAPI{}
	taskSent := make(chan struct{})
	sessions := map[string]ChatSessionManager{"default": &cancellableTaskSessions{taskSent: taskSent}}
	resolver := func(req ChatRequest) ChatRoute { return ChatRoute{Agent: "default", ReplyOnThread: true} }
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
	resolver := func(req ChatRequest) ChatRoute { return ChatRoute{Agent: "default", ReplyOnThread: true} }
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

func TestConversationKeyBindsDMToThread(t *testing.T) {
	// DMs are always threaded (replyOnThread=true).
	// A top-level DM message roots its own thread, keyed by its MessageTS.
	root := conversationKey(ChatRequest{TeamID: "T1", ChannelID: "D1", MessageTS: "123.4", DM: true}, true)
	if !root.DM || root.ThreadTS != "123.4" || root.ChannelID != "D1" {
		t.Fatalf("top-level DM key should bind to its own MessageTS: %#v", root)
	}

	// A reply inside a DM thread is keyed by that thread's root.
	reply := conversationKey(ChatRequest{TeamID: "T1", ChannelID: "D1", ThreadTS: "123.4", MessageTS: "999.9", DM: true}, true)
	if reply != root {
		t.Fatalf("a DM reply must map to the same key as its thread root: %#v vs %#v", reply, root)
	}

	// Two distinct DM threads must NOT share a session.
	other := conversationKey(ChatRequest{TeamID: "T1", ChannelID: "D1", MessageTS: "555.5", DM: true}, true)
	if other == root {
		t.Fatalf("distinct DM threads must have distinct keys: %#v", other)
	}
}

func TestConversationKeyChannelReplyMode(t *testing.T) {
	// In channel-reply mode (replyOnThread=false) two top-level channel messages
	// with DIFFERENT MessageTS bind to the SAME channel-wide session: a single
	// rolling conversation, not a fresh session per message.
	a := conversationKey(ChatRequest{TeamID: "T1", ChannelID: "C1", MessageTS: "100.1"}, false)
	b := conversationKey(ChatRequest{TeamID: "T1", ChannelID: "C1", MessageTS: "200.2"}, false)
	if a != b {
		t.Fatalf("channel-reply mode must share one key across messages: %#v vs %#v", a, b)
	}
	if a.ThreadTS != "" {
		t.Fatalf("channel-reply key should have an empty ThreadTS, got %q", a.ThreadTS)
	}

	// In threaded mode the same two messages root DISTINCT per-thread sessions.
	ta := conversationKey(ChatRequest{TeamID: "T1", ChannelID: "C1", MessageTS: "100.1"}, true)
	tb := conversationKey(ChatRequest{TeamID: "T1", ChannelID: "C1", MessageTS: "200.2"}, true)
	if ta == tb {
		t.Fatalf("threaded mode must give distinct per-thread keys: %#v", ta)
	}

	// A message already inside a thread keys to its ThreadTS regardless of mode.
	inThread := ChatRequest{TeamID: "T1", ChannelID: "C1", ThreadTS: "100.1", MessageTS: "300.3"}
	if conversationKey(inThread, false) != conversationKey(inThread, true) {
		t.Fatalf("an in-thread message must key to its thread regardless of reply mode")
	}
}

func TestReplyThreadTS(t *testing.T) {
	cases := []struct {
		name          string
		req           ChatRequest
		replyOnThread bool
		want          string
	}{
		{"in-thread always threaded (off)", ChatRequest{ThreadTS: "111.1", MessageTS: "222.2"}, false, "111.1"},
		{"in-thread always threaded (on)", ChatRequest{ThreadTS: "111.1", MessageTS: "222.2"}, true, "111.1"},
		{"top-level threaded roots at message", ChatRequest{MessageTS: "222.2"}, true, "222.2"},
		{"top-level channel-reply is rootless", ChatRequest{MessageTS: "222.2"}, false, ""},
		{"DM uses MessageTS", ChatRequest{MessageTS: "123.4", DM: true}, true, "123.4"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := replyThreadTS(tt.req, tt.replyOnThread); got != tt.want {
				t.Fatalf("replyThreadTS = %q, want %q", got, tt.want)
			}
		})
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
func (f *blockingChatSessions) Cancel(context.Context, string) error        { return nil }

func TestChatHandlerRefreshesAssistantStatusWhileEventsPending(t *testing.T) {
	api := &fakeStreamAPI{}
	release := make(chan struct{})
	fakeSessions := &blockingChatSessions{release: release}
	sessions := map[string]ChatSessionManager{"default": fakeSessions}
	resolver := func(req ChatRequest) ChatRoute { return ChatRoute{Agent: "default", ReplyOnThread: true} }
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
func (f *cancellableChatSessions) Cancel(context.Context, string) error        { return nil }

func TestChatHandlerInterruptedByCancelReturnsNil(t *testing.T) {
	api := &fakeStreamAPI{}
	firstChunkSent := make(chan struct{})
	fakeSessions := &cancellableChatSessions{firstChunkSent: firstChunkSent}
	sessions := map[string]ChatSessionManager{"default": fakeSessions}
	resolver := func(req ChatRequest) ChatRoute { return ChatRoute{Agent: "default", ReplyOnThread: true} }
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
	resolver := func(req ChatRequest) ChatRoute { return ChatRoute{Agent: "default", ReplyOnThread: true} }
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
	resolver := func(req ChatRequest) ChatRoute { return ChatRoute{Agent: "default", ReplyOnThread: true} }
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
func (f *steadyChatSessions) Cancel(context.Context, string) error        { f.cancelled++; return nil }

func TestChatHandlerIdleTimerResetsOnActivity(t *testing.T) {
	api := &fakeStreamAPI{}
	// 10 events, 10ms apart (~100ms of steady activity) under a 60ms idle window:
	// no single gap approaches the window, so the turn must finish, not time out.
	fake := &steadyChatSessions{gap: 10 * time.Millisecond, events: 10}
	sessions := map[string]ChatSessionManager{"default": fake}
	resolver := func(req ChatRequest) ChatRoute { return ChatRoute{Agent: "default", ReplyOnThread: true} }
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
