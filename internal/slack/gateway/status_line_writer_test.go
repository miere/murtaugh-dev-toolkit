package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
)

// fakeStatusMessenger records the post/update/delete calls StatusLineWriter
// makes so tests can assert the single-message, last-write-wins lifecycle.
type fakeStatusMessenger struct {
	posts   int
	updates int

	postChannel   string
	postThreadTS  string
	postOptions   []slack.MsgOption
	updateTS      string
	updateOptions []slack.MsgOption
}

func (f *fakeStatusMessenger) PostMessageContext(_ context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	f.posts++
	f.postChannel = channelID
	f.postOptions = options
	f.postThreadTS = optionValue(options, "thread_ts")
	return channelID, "status-ts", nil
}

func (f *fakeStatusMessenger) UpdateMessageContext(_ context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error) {
	f.updates++
	f.updateTS = timestamp
	f.updateOptions = options
	return channelID, timestamp, "", nil
}

// optionValue applies the message options and returns one of the url.Values
// they set (e.g. "text", "thread_ts"). Blocks live on a separate config field
// not exposed here, so block structure is asserted via statusContextBlock.
func optionValue(options []slack.MsgOption, key string) string {
	_, values, err := slack.UnsafeApplyMsgOptions("xoxb-test", "C1", "https://slack.com/api", options...)
	if err != nil {
		return ""
	}
	return values.Get(key)
}

func TestStatusContextBlockShape(t *testing.T) {
	block, ok := statusContextBlock("Reading file…").(*slack.ContextBlock)
	if !ok {
		t.Fatalf("expected a *slack.ContextBlock")
	}
	if len(block.ContextElements.Elements) != 1 {
		t.Fatalf("expected one element, got %d", len(block.ContextElements.Elements))
	}
	text, ok := block.ContextElements.Elements[0].(*slack.TextBlockObject)
	if !ok {
		t.Fatalf("expected a plain_text element, got %T", block.ContextElements.Elements[0])
	}
	if text.Type != slack.PlainTextType || text.Text != "Reading file…" {
		t.Fatalf("unexpected text object: %+v", text)
	}
	if text.Emoji == nil || !*text.Emoji {
		t.Fatalf("expected emoji:true on the plain_text element, got %v", text.Emoji)
	}
}

func TestStatusLineWriterPostsThenUpdatesSameMessage(t *testing.T) {
	msg := &fakeStatusMessenger{}
	writer := NewStatusLineWriter(msg, "C1", "thread-1", time.Nanosecond, nil)
	ctx := context.Background()

	if err := writer.UpdateFromEvent(ctx, &agent.TaskEvent{ID: "task-1", Title: "Reading"}); err != nil {
		t.Fatalf("first update: %v", err)
	}
	time.Sleep(time.Millisecond)
	// A second event with a different ACP task id must edit the same message in
	// place — one line, last-write-wins — not post a new one.
	if err := writer.UpdateFromEvent(ctx, &agent.TaskEvent{ID: "task-2", Title: "Writing"}); err != nil {
		t.Fatalf("second update: %v", err)
	}
	if msg.posts != 1 || msg.updates != 1 {
		t.Fatalf("expected one post + one in-place update, got posts=%d updates=%d", msg.posts, msg.updates)
	}
	if msg.postChannel != "C1" || msg.postThreadTS != "thread-1" {
		t.Fatalf("expected post to C1 in thread-1, got channel=%q thread=%q", msg.postChannel, msg.postThreadTS)
	}
	if msg.updateTS != "status-ts" {
		t.Fatalf("expected the update to target the posted ts, got %q", msg.updateTS)
	}
	if got := optionValue(msg.postOptions, "text"); got != "Reading" {
		t.Fatalf("post fallback text = %q, want Reading", got)
	}
	if got := optionValue(msg.updateOptions, "text"); got != "Writing" {
		t.Fatalf("update fallback text = %q, want Writing (last write wins)", got)
	}
}

func TestStatusLineWriterThrottles(t *testing.T) {
	msg := &fakeStatusMessenger{}
	writer := NewStatusLineWriter(msg, "C1", "", time.Hour, nil)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := writer.UpdateFromEvent(ctx, &agent.TaskEvent{ID: "t", Title: "tick"}); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	if msg.posts != 1 || msg.updates != 0 {
		t.Fatalf("expected a single throttled post, got posts=%d updates=%d", msg.posts, msg.updates)
	}
}

func TestStatusLineWriterFinishResolvesOnce(t *testing.T) {
	msg := &fakeStatusMessenger{}
	writer := NewStatusLineWriter(msg, "C1", "", time.Hour, nil)
	ctx := context.Background()

	if err := writer.UpdateFromEvent(ctx, &agent.TaskEvent{ID: "t", Title: "Reading"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := writer.Finish(ctx); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := writer.Finish(ctx); err != nil {
		t.Fatalf("second finish: %v", err)
	}
	// Finish edits the posted message once to the done line; the second is a no-op.
	if msg.posts != 1 || msg.updates != 1 || msg.updateTS != "status-ts" {
		t.Fatalf("expected one resolving edit of the posted message, got posts=%d updates=%d ts=%q", msg.posts, msg.updates, msg.updateTS)
	}
	if got := optionValue(msg.updateOptions, "text"); got != statusLineDoneText {
		t.Fatalf("expected the message resolved to %q, got %q", statusLineDoneText, got)
	}
}

func TestStatusLineWriterFinishWithoutPostIsNoop(t *testing.T) {
	msg := &fakeStatusMessenger{}
	writer := NewStatusLineWriter(msg, "C1", "", time.Hour, nil)
	if err := writer.Finish(context.Background()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	// No task events → no message → nothing to resolve.
	if msg.posts != 0 || msg.updates != 0 {
		t.Fatalf("expected no writes when nothing was posted, got posts=%d updates=%d", msg.posts, msg.updates)
	}
}

func TestStatusLineWriterSuppressesUpdatesAfterFinish(t *testing.T) {
	msg := &fakeStatusMessenger{}
	writer := NewStatusLineWriter(msg, "C1", "", time.Nanosecond, nil)
	ctx := context.Background()

	if err := writer.UpdateFromEvent(ctx, &agent.TaskEvent{ID: "t", Title: "Reading"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := writer.Finish(ctx); err != nil {
		t.Fatalf("finish: %v", err)
	}
	time.Sleep(time.Millisecond)
	// A late event after the message has resolved must not overwrite the done line.
	if err := writer.UpdateFromEvent(ctx, &agent.TaskEvent{ID: "t", Title: "Late"}); err != nil {
		t.Fatalf("late update: %v", err)
	}
	if msg.posts != 1 || msg.updates != 1 {
		t.Fatalf("expected only the resolving edit after finish, got posts=%d updates=%d", msg.posts, msg.updates)
	}
	if got := optionValue(msg.updateOptions, "text"); got != statusLineDoneText {
		t.Fatalf("expected the line to stay resolved at %q, got %q", statusLineDoneText, got)
	}
}

func TestStatusLineWriterDefaultsBlankTitle(t *testing.T) {
	msg := &fakeStatusMessenger{}
	writer := NewStatusLineWriter(msg, "C1", "", time.Hour, nil)
	if err := writer.UpdateFromEvent(context.Background(), &agent.TaskEvent{ID: "t"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := optionValue(msg.postOptions, "text"); got != defaultStatusLineTitle {
		t.Fatalf("expected default title %q, got %q", defaultStatusLineTitle, got)
	}
}

func TestStatusLineWriterNilMessengerNoOp(t *testing.T) {
	writer := NewStatusLineWriter(nil, "C1", "", time.Hour, nil)
	ctx := context.Background()
	if err := writer.UpdateFromEvent(ctx, &agent.TaskEvent{ID: "t", Title: "x"}); err != nil {
		t.Fatalf("update with nil messenger should be a no-op, got %v", err)
	}
	if err := writer.Finish(ctx); err != nil {
		t.Fatalf("finish with nil messenger should be a no-op, got %v", err)
	}
}
