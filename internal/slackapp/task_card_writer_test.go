package slackapp

import (
	"context"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh-dev-toolkit/internal/acp"
)

func TestTaskCardWriterSendsTaskUpdateChunk(t *testing.T) {
	api := &fakeStreamAPI{}
	streamer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 5})
	writer := NewTaskCardWriter(api, streamer, time.Millisecond, nil)

	if err := writer.Update(context.Background(), "task-1", "Searching", slack.TaskCardStatusInProgress); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if api.appends != 0 || len(api.startOptions) != 1 {
		t.Fatalf("expected task update on stream start, got appends=%d starts=%d", api.appends, len(api.startOptions))
	}
	chunks, err := extractChunksFromOptions(api.startOptions[0]...)
	if err != nil {
		t.Fatalf("extract chunks: %v", err)
	}
	// The first task opens the Plan block: a plan_update chunk precedes the
	// task_update chunk so the cards render grouped under a shared title.
	if len(chunks) != 2 {
		t.Fatalf("expected plan + task chunk, got %d", len(chunks))
	}
	plans := planChunks(chunks)
	if len(plans) != 1 || plans[0].Title != defaultPlanTitle {
		t.Fatalf("expected one plan_update chunk titled %q, got %+v", defaultPlanTitle, plans)
	}
	tasks := taskChunks(chunks)
	if len(tasks) != 1 {
		t.Fatalf("expected one task_update chunk, got %d", len(tasks))
	}
	if tasks[0].ID != "task-1" || tasks[0].Title != "Searching" || tasks[0].Status != slack.TaskCardStatusInProgress {
		t.Fatalf("unexpected chunk: %+v", tasks[0])
	}
}

func TestTaskCardWriterOpensPlanOnlyOnce(t *testing.T) {
	api := &fakeStreamAPI{}
	streamer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 5})
	writer := NewTaskCardWriter(api, streamer, time.Hour, nil)
	writer.SetPlanTitle("Build the thing")
	ctx := context.Background()

	if err := writer.Update(ctx, "task-1", "Step 1", slack.TaskCardStatusInProgress); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if err := writer.Update(ctx, "task-2", "Step 2", slack.TaskCardStatusInProgress); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	// The plan_update chunk is emitted exactly once, on stream start, with the
	// overridden title. The second task must not re-open the plan.
	startChunks, _ := extractChunksFromOptions(api.startOptions[0]...)
	plans := planChunks(startChunks)
	if len(plans) != 1 || plans[0].Title != "Build the thing" {
		t.Fatalf("expected one plan_update titled %q on start, got %+v", "Build the thing", plans)
	}
	appendChunks, _ := extractChunksFromOptions(api.appendOptions[0]...)
	if got := planChunks(appendChunks); len(got) != 0 {
		t.Fatalf("expected no plan_update chunk on subsequent task, got %+v", got)
	}
}

func TestTaskCardWriterRateLimitsSameTask(t *testing.T) {
	api := &fakeStreamAPI{}
	streamer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 5})
	writer := NewTaskCardWriter(api, streamer, 100*time.Millisecond, nil)
	ctx := context.Background()

	if err := writer.Update(ctx, "task-1", "Step 1", slack.TaskCardStatusInProgress); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if api.appends != 0 || len(api.startOptions) != 1 {
		t.Fatalf("expected first update on stream start, got appends=%d starts=%d", api.appends, len(api.startOptions))
	}
	// Immediate re-send should be suppressed.
	if err := writer.Update(ctx, "task-1", "Step 1", slack.TaskCardStatusInProgress); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if api.appends != 0 {
		t.Fatalf("expected no append after suppressed update, got %d", api.appends)
	}
	// After the interval elapses, the update should go through.
	time.Sleep(120 * time.Millisecond)
	if err := writer.Update(ctx, "task-1", "Step 1", slack.TaskCardStatusComplete); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if api.appends != 1 {
		t.Fatalf("expected 1 append after interval, got %d", api.appends)
	}
}

func TestTaskCardWriterDifferentTasksNotRateLimited(t *testing.T) {
	api := &fakeStreamAPI{}
	streamer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 5})
	writer := NewTaskCardWriter(api, streamer, time.Hour, nil)
	ctx := context.Background()

	if err := writer.Update(ctx, "task-1", "Step 1", slack.TaskCardStatusInProgress); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if err := writer.Update(ctx, "task-2", "Step 2", slack.TaskCardStatusInProgress); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if api.appends != 1 || len(api.startOptions) != 1 {
		t.Fatalf("expected first task on start and second on append, got appends=%d starts=%d", api.appends, len(api.startOptions))
	}
}

func TestTaskCardWriterMapStatus(t *testing.T) {
	tests := []struct {
		acpStatus acp.TaskStatus
		wantSlack slack.TaskCardStatus
	}{
		{acp.TaskStatusPending, slack.TaskCardStatusPending},
		{acp.TaskStatusInProgress, slack.TaskCardStatusInProgress},
		{acp.TaskStatusComplete, slack.TaskCardStatusComplete},
		{acp.TaskStatusFailed, slack.TaskCardStatusError},
		{acp.TaskStatusCancelled, slack.TaskCardStatusError},
		{acp.TaskStatus("unknown"), slack.TaskCardStatusInProgress},
	}
	for _, tc := range tests {
		t.Run(string(tc.acpStatus), func(t *testing.T) {
			got := mapTaskStatus(tc.acpStatus)
			if got != tc.wantSlack {
				t.Fatalf("mapTaskStatus(%q) = %q, want %q", tc.acpStatus, got, tc.wantSlack)
			}
		})
	}
}

func TestTaskCardWriterFailAndComplete(t *testing.T) {
	api := &fakeStreamAPI{}
	streamer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 5})
	writer := NewTaskCardWriter(api, streamer, time.Millisecond, nil)
	ctx := context.Background()

	if err := writer.Fail(ctx, "t1", "Build failed"); err != nil {
		t.Fatalf("Fail returned error: %v", err)
	}
	if err := writer.Complete(ctx, "t2", "Build done"); err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if api.appends != 1 || len(api.startOptions) != 1 {
		t.Fatalf("expected first terminal update on start and second on append, got appends=%d starts=%d", api.appends, len(api.startOptions))
	}
	chunks0, _ := extractChunksFromOptions(api.startOptions[0]...)
	chunks1, _ := extractChunksFromOptions(api.appendOptions[0]...)
	tasks0 := taskChunks(chunks0)
	tasks1 := taskChunks(chunks1)
	if len(tasks0) != 1 || tasks0[0].Status != slack.TaskCardStatusError {
		t.Fatalf("expected error status for fail, got %+v", tasks0)
	}
	if len(tasks1) != 1 || tasks1[0].Status != slack.TaskCardStatusComplete {
		t.Fatalf("expected complete status for complete, got %+v", tasks1)
	}
}

func TestTaskCardWriterReusesTitleForCompletionUpdate(t *testing.T) {
	api := &fakeStreamAPI{}
	streamer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 5})
	writer := NewTaskCardWriter(api, streamer, time.Millisecond, nil)
	ctx := context.Background()

	if err := writer.UpdateFromEvent(ctx, &acp.TaskEvent{ID: "call-1", Title: "List files", Status: acp.TaskStatusInProgress}); err != nil {
		t.Fatalf("UpdateFromEvent returned error: %v", err)
	}
	if err := writer.UpdateFromEvent(ctx, &acp.TaskEvent{ID: "call-1", Status: acp.TaskStatusComplete}); err != nil {
		t.Fatalf("UpdateFromEvent returned error: %v", err)
	}
	chunks, err := extractChunksFromOptions(api.appendOptions[0]...)
	if err != nil {
		t.Fatalf("extract chunks: %v", err)
	}
	chunk := chunks[0].(slack.TaskUpdateChunk)
	if chunk.Title != "List files" || chunk.Status != slack.TaskCardStatusComplete {
		t.Fatalf("expected completion to reuse title, got %+v", chunk)
	}
}
