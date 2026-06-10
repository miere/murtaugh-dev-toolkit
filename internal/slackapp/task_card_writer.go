package slackapp

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh-dev-toolkit/internal/acp"
)

const defaultTaskUpdateInterval = 1 * time.Second

// defaultPlanTitle labels the Plan block that groups an agent's task cards
// when no more specific title (e.g. the user's request) has been supplied.
const defaultPlanTitle = "Tasks"

// defaultTaskTitle is the fallback card title when neither the event nor a
// previously-seen update carried one.
const defaultTaskTitle = "Tool call"

// TaskCardWriter sends task-card updates as Slack stream chunks alongside a
// StreamWriter. Each task update is rate-limited so we do not hammer the Slack
// API while the agent is rapidly iterating. The first task opens a Plan block
// (plan_update chunk) so all subsequent task cards render grouped under a
// single title rather than as separate messages.
type TaskCardWriter struct {
	api       StreamAPI
	streamer  *StreamWriter
	logger    *slog.Logger
	interval  time.Duration
	mu        sync.Mutex
	lastFlush map[string]time.Time
	titles    map[string]string
	planTitle string
	planOpen  bool
}

// NewTaskCardWriter creates a writer that posts task-card updates to the same
// Slack stream as streamer.
func NewTaskCardWriter(api StreamAPI, streamer *StreamWriter, interval time.Duration, logger *slog.Logger) *TaskCardWriter {
	if interval <= 0 {
		interval = defaultTaskUpdateInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &TaskCardWriter{
		api:       api,
		streamer:  streamer,
		logger:    logger,
		interval:  interval,
		lastFlush: make(map[string]time.Time),
		titles:    make(map[string]string),
		planTitle: defaultPlanTitle,
	}
}

// SetPlanTitle overrides the title of the Plan block that groups the task
// cards. It must be called before the first task update is sent; once the
// plan is opened the title is fixed for the stream. An empty title is ignored.
func (w *TaskCardWriter) SetPlanTitle(title string) {
	if title == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.planOpen {
		w.planTitle = title
	}
}

// Update sends a task-card update for a single task. The stream is started if
// needed. Updates are suppressed when the same task was flushed recently,
// unless the status is a terminal state (complete or error) in which case the
// update is always sent.
func (w *TaskCardWriter) Update(ctx context.Context, taskID, title string, status slack.TaskCardStatus) error {
	if !w.shouldFlush(taskID, status) {
		return nil
	}
	chunk := slack.NewTaskUpdateChunk(taskID, title)
	chunk.Status = status
	chunks := append(w.planPrefix(), chunk)
	startedAt := time.Now()
	if !w.streamer.Started() {
		if err := w.streamer.StartWithOptions(ctx, slack.MsgOptionChunks(chunks...)); err != nil {
			return fmt.Errorf("start stream with task update: %w", err)
		}
		w.recordFlush(taskID, startedAt)
		w.logger.Info("started stream with task update", "task_id", taskID, "status", status, "duration", time.Since(startedAt))
		return nil
	}
	_, _, err := w.api.AppendStreamContext(ctx, w.streamer.StreamChannel(), w.streamer.StreamTS(), slack.MsgOptionChunks(chunks...))
	if err != nil {
		return fmt.Errorf("append task update chunk: %w", err)
	}
	w.recordFlush(taskID, startedAt)
	w.logger.Info("sent task update", "task_id", taskID, "status", status, "duration", time.Since(startedAt))
	return nil
}

// planPrefix returns the plan_update chunk that opens the Plan block, but only
// once per stream. Subsequent calls return nil so the plan is established
// exactly once, ahead of the first task card.
func (w *TaskCardWriter) planPrefix() []slack.StreamChunk {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.planOpen {
		return nil
	}
	w.planOpen = true
	return []slack.StreamChunk{slack.NewPlanUpdateChunk(w.planTitle)}
}

// Fail marks a task as failed, reusing the task's previously-seen title when
// title is empty so the terminal card keeps its label.
func (w *TaskCardWriter) Fail(ctx context.Context, taskID, title string) error {
	return w.Update(ctx, taskID, w.resolveTitle(taskID, title), slack.TaskCardStatusError)
}

// Complete marks a task as completed, reusing the task's previously-seen title
// when title is empty.
func (w *TaskCardWriter) Complete(ctx context.Context, taskID, title string) error {
	return w.Update(ctx, taskID, w.resolveTitle(taskID, title), slack.TaskCardStatusComplete)
}

// resolveTitle records a non-empty title and returns the best label for the
// task: the supplied title, else the last-seen title, else a generic fallback.
func (w *TaskCardWriter) resolveTitle(taskID, title string) string {
	if resolved := w.titleFor(taskID, title); resolved != "" {
		return resolved
	}
	return defaultTaskTitle
}

// UpdateFromEvent maps an ACP task event to a Slack task update and sends it.
func (w *TaskCardWriter) UpdateFromEvent(ctx context.Context, event *acp.TaskEvent) error {
	if event == nil {
		return nil
	}
	status := mapTaskStatus(event.Status)
	title := w.titleFor(event.ID, event.Title)
	if title == "" {
		title = defaultTaskTitle
	}
	if err := w.Update(ctx, event.ID, title, status); err != nil {
		return err
	}
	return nil
}

func (w *TaskCardWriter) titleFor(taskID, title string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if title != "" {
		w.titles[taskID] = title
		return title
	}
	return w.titles[taskID]
}

func (w *TaskCardWriter) shouldFlush(taskID string, status slack.TaskCardStatus) bool {
	if status == slack.TaskCardStatusComplete || status == slack.TaskCardStatusError {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	last, ok := w.lastFlush[taskID]
	if !ok {
		return true
	}
	return time.Since(last) >= w.interval
}

func (w *TaskCardWriter) recordFlush(taskID string, t time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastFlush[taskID] = t
}

func mapTaskStatus(status acp.TaskStatus) slack.TaskCardStatus {
	switch status {
	case acp.TaskStatusPending:
		return slack.TaskCardStatusPending
	case acp.TaskStatusInProgress:
		return slack.TaskCardStatusInProgress
	case acp.TaskStatusComplete:
		return slack.TaskCardStatusComplete
	case acp.TaskStatusFailed:
		return slack.TaskCardStatusError
	case acp.TaskStatusCancelled:
		return slack.TaskCardStatusError
	default:
		return slack.TaskCardStatusInProgress
	}
}
