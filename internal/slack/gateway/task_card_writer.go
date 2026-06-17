package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
)

const defaultTaskUpdateInterval = 1 * time.Second

// defaultPlanTitle labels the Plan block that groups an agent's task cards.
// It is a fixed, user-facing label rather than the user's prompt: a prompt can
// be long, multi-line, and is meaningless as a heading once truncated.
const defaultPlanTitle = "Task list"

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
	statuses  map[string]slack.TaskCardStatus
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
		statuses:  make(map[string]slack.TaskCardStatus),
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
	w.recordStatus(taskID, status)
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

// Finish is a no-op: task cards live in the answer stream and are resolved by
// finalizeTasks, so there is no side-channel message to tear down. It exists to
// satisfy progressRenderer alongside StatusLineWriter.
func (w *TaskCardWriter) Finish(context.Context) error { return nil }

// UpdateFromEvent maps an ACP task event to a Slack task update and sends it.
func (w *TaskCardWriter) UpdateFromEvent(ctx context.Context, event *agent.TaskEvent) error {
	if event == nil {
		return nil
	}
	status := w.resolveStatus(event.ID, event.Status)
	title := w.titleFor(event.ID, event.Title)
	if title == "" {
		title = defaultTaskTitle
	}
	if err := w.Update(ctx, event.ID, title, status); err != nil {
		return err
	}
	return nil
}

// resolveStatus maps an ACP task status to its Slack equivalent, but only when
// the agent actually reported a recognised status. ACP agents (e.g. goose)
// emit tool_call_update notifications that refine only the title or content and
// carry no status; mapping those through mapTaskStatus would default them to
// in_progress and silently flip a card that had already completed back to a
// spinner. Instead we reuse the last status seen for the task, falling back to
// in_progress only for the very first update.
func (w *TaskCardWriter) resolveStatus(taskID string, status agent.TaskStatus) slack.TaskCardStatus {
	if mapped, ok := knownTaskStatus(status); ok {
		return mapped
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if last, ok := w.statuses[taskID]; ok {
		return last
	}
	return slack.TaskCardStatusInProgress
}

func (w *TaskCardWriter) recordStatus(taskID string, status slack.TaskCardStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.statuses[taskID] = status
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

func mapTaskStatus(status agent.TaskStatus) slack.TaskCardStatus {
	if mapped, ok := knownTaskStatus(status); ok {
		return mapped
	}
	return slack.TaskCardStatusInProgress
}

// knownTaskStatus maps the ACP task statuses we recognise to their Slack
// equivalents. The boolean distinguishes "the agent reported this status" from
// "fell through to a default" — an empty or unrecognised status returns false
// so callers can preserve a card's previous state instead of overwriting it.
func knownTaskStatus(status agent.TaskStatus) (slack.TaskCardStatus, bool) {
	switch status {
	case agent.TaskStatusPending:
		return slack.TaskCardStatusPending, true
	case agent.TaskStatusInProgress:
		return slack.TaskCardStatusInProgress, true
	case agent.TaskStatusComplete:
		return slack.TaskCardStatusComplete, true
	case agent.TaskStatusFailed:
		return slack.TaskCardStatusError, true
	case agent.TaskStatusCancelled:
		return slack.TaskCardStatusError, true
	default:
		return "", false
	}
}
