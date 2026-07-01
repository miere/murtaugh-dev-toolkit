package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

type fakeStreamAPI struct {
	startedChannel string
	appends        int
	stops          int
	startOptions   [][]slack.MsgOption
	appendOptions  [][]slack.MsgOption

	// rejectCanceledStatus makes SetAssistantThreadsStatusContext fail when
	// invoked with an already-cancelled context, mirroring Slack rejecting a
	// request made on a dead context. Used to prove the final status clear runs
	// on a fresh context.
	rejectCanceledStatus bool

	statusMu     sync.Mutex
	statusCalls  int
	statusParams []slack.AssistantThreadsSetStatusParameters
}

func (f *fakeStreamAPI) StartStreamContext(_ context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	f.startedChannel = channelID
	f.startOptions = append(f.startOptions, options)
	return channelID, "stream-ts", nil
}

func (f *fakeStreamAPI) AppendStreamContext(_ context.Context, _ string, _ string, options ...slack.MsgOption) (string, string, error) {
	f.appends++
	f.appendOptions = append(f.appendOptions, options)
	return "C1", "stream-ts", nil
}

func (f *fakeStreamAPI) StopStreamContext(_ context.Context, _ string, _ string, _ ...slack.MsgOption) (string, string, error) {
	f.stops++
	return "C1", "stream-ts", nil
}

func (f *fakeStreamAPI) SetAssistantThreadsStatusContext(ctx context.Context, params slack.AssistantThreadsSetStatusParameters) error {
	if f.rejectCanceledStatus && ctx.Err() != nil {
		return ctx.Err()
	}
	f.statusMu.Lock()
	defer f.statusMu.Unlock()
	f.statusCalls++
	f.statusParams = append(f.statusParams, params)
	return nil
}

func (f *fakeStreamAPI) statusSnapshot() (int, []slack.AssistantThreadsSetStatusParameters) {
	f.statusMu.Lock()
	defer f.statusMu.Unlock()
	calls := f.statusCalls
	params := make([]slack.AssistantThreadsSetStatusParameters, len(f.statusParams))
	copy(params, f.statusParams)
	return calls, params
}

func extractChunksFromOptions(options ...slack.MsgOption) ([]slack.StreamChunk, error) {
	_, values, err := slack.UnsafeApplyMsgOptions("xoxb-test", "C1", "https://slack.com/api", options...)
	if err != nil {
		return nil, err
	}
	chunksJSON := values.Get("chunks")
	if chunksJSON == "" {
		return nil, nil
	}
	var rawChunks []json.RawMessage
	if err := json.Unmarshal([]byte(chunksJSON), &rawChunks); err != nil {
		return nil, err
	}
	var chunks []slack.StreamChunk
	for _, raw := range rawChunks {
		var typeCheck struct {
			Type slack.StreamChunkType `json:"type"`
		}
		if err := json.Unmarshal(raw, &typeCheck); err != nil {
			return nil, err
		}
		switch typeCheck.Type {
		case slack.StreamChunkTaskUpdate:
			var chunk slack.TaskUpdateChunk
			if err := json.Unmarshal(raw, &chunk); err != nil {
				return nil, err
			}
			chunks = append(chunks, chunk)
		case slack.StreamChunkMarkdownText:
			var chunk slack.MarkdownTextChunk
			if err := json.Unmarshal(raw, &chunk); err != nil {
				return nil, err
			}
			chunks = append(chunks, chunk)
		case slack.StreamChunkPlanUpdate:
			var chunk slack.PlanUpdateChunk
			if err := json.Unmarshal(raw, &chunk); err != nil {
				return nil, err
			}
			chunks = append(chunks, chunk)
		default:
			return nil, fmt.Errorf("unexpected chunk type %q", typeCheck.Type)
		}
	}
	return chunks, nil
}

func extractMarkdownTextFromOptions(options ...slack.MsgOption) (string, error) {
	chunks, err := extractChunksFromOptions(options...)
	if err != nil {
		return "", err
	}
	for _, chunk := range chunks {
		if md, ok := chunk.(slack.MarkdownTextChunk); ok {
			return md.Text, nil
		}
	}
	_, values, err := slack.UnsafeApplyMsgOptions("xoxb-test", "C1", "https://slack.com/api", options...)
	if err != nil {
		return "", err
	}
	return values.Get("markdown_text"), nil
}

// taskChunks returns only the task_update chunks, dropping the leading
// plan_update chunk that opens the Plan block on the first task.
func taskChunks(chunks []slack.StreamChunk) []slack.TaskUpdateChunk {
	var out []slack.TaskUpdateChunk
	for _, chunk := range chunks {
		if task, ok := chunk.(slack.TaskUpdateChunk); ok {
			out = append(out, task)
		}
	}
	return out
}

// planChunks returns only the plan_update chunks.
func planChunks(chunks []slack.StreamChunk) []slack.PlanUpdateChunk {
	var out []slack.PlanUpdateChunk
	for _, chunk := range chunks {
		if plan, ok := chunk.(slack.PlanUpdateChunk); ok {
			out = append(out, plan)
		}
	}
	return out
}

// appendedText returns the markdown text painted on each AppendStreamContext
// call, in order, so a test can assert both the paint cadence (how many flushes)
// and the reconstructed message.
func (f *fakeStreamAPI) appendedText(t *testing.T) []string {
	t.Helper()
	out := make([]string, 0, len(f.appendOptions))
	for _, opts := range f.appendOptions {
		text, err := extractMarkdownTextFromOptions(opts...)
		if err != nil {
			t.Fatalf("extract markdown: %v", err)
		}
		out = append(out, text)
	}
	return out
}

// TestStreamWriterHoldsPartialLine proves a text run is painted on line
// boundaries: the eager first paint, then a held partial line that only lands
// once its newline arrives. The size/time caps are disabled so the newline is
// the sole trigger.
func TestStreamWriterHoldsPartialLine(t *testing.T) {
	api := &fakeStreamAPI{}
	writer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 1000})
	ctx := context.Background()
	for _, delta := range []string{"First line.\n", "partial ", "still partial", " done\n"} {
		if err := writer.Append(ctx, delta); err != nil {
			t.Fatalf("Append(%q): %v", delta, err)
		}
	}
	if err := writer.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got := api.appendedText(t)
	want := []string{"First line.\n", "partial still partial done\n"}
	if len(got) != len(want) {
		t.Fatalf("paint count = %d (%q), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paint %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestStreamWriterTrimsToWordBoundary proves the size cap never repaints
// mid-word: when a long unbroken line trips the cap, it flushes only through the
// last space, retaining the trailing partial word for the next paint.
func TestStreamWriterTrimsToWordBoundary(t *testing.T) {
	api := &fakeStreamAPI{}
	writer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 10})
	ctx := context.Background()
	for _, delta := range []string{"Hello ", "world foo", "bar"} {
		if err := writer.Append(ctx, delta); err != nil {
			t.Fatalf("Append(%q): %v", delta, err)
		}
	}
	if err := writer.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got := api.appendedText(t)
	want := []string{"Hello ", "world ", "foobar"}
	if len(got) != len(want) {
		t.Fatalf("paint count = %d (%q), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paint %d = %q, want %q", i, got[i], want[i])
		}
	}
	joined := got[0] + got[1] + got[2]
	if joined != "Hello world foobar" {
		t.Fatalf("reconstructed = %q, want %q", joined, "Hello world foobar")
	}
}

func TestStreamWriterUsesNativeStreamingMethods(t *testing.T) {
	api := &fakeStreamAPI{}
	writer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 5})
	if err := writer.Append(context.Background(), "hello"); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if err := writer.Stop(context.Background()); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if api.startedChannel != "C1" || api.appends != 1 || api.stops != 1 {
		t.Fatalf("unexpected stream calls: %#v", api)
	}
}
