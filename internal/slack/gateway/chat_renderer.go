package gateway

import (
	"context"
	"log/slog"
	"strings"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/agent"
)

// chatRenderer turns an agent's event stream into Slack UI. The ChatHandler
// drives it event-by-event and is otherwise rendering-agnostic. Two
// implementations:
//
//   - wovenRenderer (tasks mode): the legacy behaviour — task cards woven into a
//     single answer-stream message alongside the reply text.
//   - sectionRenderer (simplified mode): an ordered SEQUENCE of separate Slack
//     messages — contiguous tool activity becomes a tool-block message (updated
//     in place), and reply text becomes its own streamed message. A switch
//     between the two closes the open section and opens a new one, so tool
//     execution is always separated from the reply and never mixed into it,
//     regardless of how the model interleaves text and tool calls.
//
// All methods are called from the single ChatHandler event loop (no concurrency).
type chatRenderer interface {
	// Text renders a streamed reply-text delta.
	Text(ctx context.Context, text string) error
	// Task renders a tool/task progress update.
	Task(ctx context.Context, ev *agent.TaskEvent) error
	// Attachment delivers a file the agent produced into the turn's thread, as a
	// separate Slack upload alongside the streamed reply.
	Attachment(ctx context.Context, a *agent.AttachmentEvent) error
	// Note appends a non-reply notice (idle-timeout marker) to the reply surface.
	Note(ctx context.Context, text string) error
	// Finish finalises a successful turn, closing every open section. emptyNote,
	// when non-empty, is posted because the turn produced no reply text.
	Finish(ctx context.Context, emptyNote string) error
	// Fail finalises a turn that errored, surfacing err on the reply surface.
	Fail(ctx context.Context, err error) error
	// Interrupted finalises a caller-cancelled turn: a best-effort "_interrupted_"
	// marker on the reply surface plus closing any open tool block. Never paints a
	// tool red — the agent did not fail.
	Interrupted(ctx context.Context)
	// EnsureStopped is the idempotent safety net run on every exit path.
	EnsureStopped(ctx context.Context)
}

// --- wovenRenderer (tasks mode) -------------------------------------------

// wovenRenderer reproduces the original rendering: one answer-stream message
// carrying both the reply text and the task cards (via TaskCardWriter). It is
// used for the `tasks` progress mode.
type wovenRenderer struct {
	writer    *StreamWriter
	tasks     progressRenderer
	running   map[string]agent.TaskStatus
	uploader  attachmentUploader
	channelID string
	threadTS  string
	logger    *slog.Logger
}

func newWovenRenderer(writer *StreamWriter, tasks progressRenderer, uploader attachmentUploader, channelID, threadTS string, logger *slog.Logger) *wovenRenderer {
	if logger == nil {
		logger = slog.Default()
	}
	return &wovenRenderer{writer: writer, tasks: tasks, running: map[string]agent.TaskStatus{}, uploader: uploader, channelID: channelID, threadTS: threadTS, logger: logger}
}

func (r *wovenRenderer) Text(ctx context.Context, text string) error {
	return r.writer.Append(ctx, text)
}

// Attachment uploads the file as a separate Slack message. The woven reply lives
// in one continuously-edited message, so a file is necessarily a sibling post; it
// lands after the reply message's initial post.
func (r *wovenRenderer) Attachment(ctx context.Context, a *agent.AttachmentEvent) error {
	return deliverAttachment(ctx, r.uploader, r.channelID, r.threadTS, a)
}

func (r *wovenRenderer) Task(ctx context.Context, ev *agent.TaskEvent) error {
	if ev == nil {
		return nil
	}
	if err := r.tasks.UpdateFromEvent(ctx, ev); err != nil {
		r.logger.Warn("failed to send task update", "error", err, "task_id", ev.ID)
	}
	// Only an explicit terminal status retires a task; an update that merely
	// refines the title (no status) keeps it tracked so Finish resolves it.
	if isTerminalTaskStatus(ev.Status) {
		delete(r.running, ev.ID)
	} else {
		r.running[ev.ID] = ev.Status
	}
	return nil
}

func (r *wovenRenderer) Note(ctx context.Context, text string) error {
	if !r.writer.Started() {
		if err := r.writer.Start(ctx); err != nil {
			return err
		}
	}
	return r.writer.Append(ctx, text)
}

func (r *wovenRenderer) Finish(ctx context.Context, emptyNote string) error {
	r.finalizeTasks(ctx, slack.TaskCardStatusComplete)
	if !r.writer.Started() {
		if err := r.writer.Start(ctx); err != nil {
			return err
		}
	}
	if emptyNote != "" {
		if err := r.writer.Append(ctx, emptyNote); err != nil {
			return err
		}
	}
	return r.writer.Stop(ctx)
}

func (r *wovenRenderer) Fail(ctx context.Context, err error) error {
	r.finalizeTasks(ctx, slack.TaskCardStatusError)
	return r.writer.Fail(ctx, err)
}

func (r *wovenRenderer) Interrupted(ctx context.Context) {
	if !r.writer.Started() || r.writer.Stopped() {
		return
	}
	if err := r.writer.Append(ctx, "\n\n_interrupted_"); err != nil {
		r.logger.Warn("failed to append interrupted marker", "error", err)
		return
	}
	if err := r.writer.Stop(ctx); err != nil {
		r.logger.Warn("failed to stop stream after interrupt", "error", err)
	}
}

func (r *wovenRenderer) EnsureStopped(ctx context.Context) {
	if r.tasks != nil {
		_ = r.tasks.Finish(ctx)
	}
	if r.writer.Started() && !r.writer.Stopped() {
		if err := r.writer.Stop(ctx); err != nil {
			r.logger.Warn("failed to stop stream on cleanup", "error", err)
		}
	}
}

// finalizeTasks brings every still-running task to a terminal status so a card
// is never stranded mid-spinner. Best-effort: errors are logged, not propagated.
func (r *wovenRenderer) finalizeTasks(ctx context.Context, status slack.TaskCardStatus) {
	for id := range r.running {
		var err error
		if status == slack.TaskCardStatusComplete {
			err = r.tasks.Complete(ctx, id, "")
		} else {
			err = r.tasks.Fail(ctx, id, "")
		}
		if err != nil {
			r.logger.Warn("failed to finalize task", "error", err, "task_id", id, "status", status)
		}
		delete(r.running, id)
	}
}

// --- sectionRenderer (simplified mode) ------------------------------------

type sectionMode int

const (
	sectionNone sectionMode = iota
	sectionText
	sectionTools
)

// sectionRenderer renders the event stream as an ordered sequence of Slack
// messages, alternating between streamed text messages and in-place tool blocks
// as the stream switches between text and tool activity.
type sectionRenderer struct {
	newText   func() *StreamWriter
	newBlock  func() *StatusLineWriter
	uploader  attachmentUploader
	channelID string
	threadTS  string
	logger    *slog.Logger

	mode   sectionMode
	text   *StreamWriter
	block  *StatusLineWriter
	titles []string // distinct tool titles in the current block, for its summary
}

func newSectionRenderer(newText func() *StreamWriter, newBlock func() *StatusLineWriter, uploader attachmentUploader, channelID, threadTS string, logger *slog.Logger) *sectionRenderer {
	if logger == nil {
		logger = slog.Default()
	}
	return &sectionRenderer{newText: newText, newBlock: newBlock, uploader: uploader, channelID: channelID, threadTS: threadTS, logger: logger}
}

// Attachment finalises the open text section first — so prose written before the
// file is committed above it and later text opens a fresh message below it — then
// uploads the file as its own message, preserving the agent's intended order.
func (r *sectionRenderer) Attachment(ctx context.Context, a *agent.AttachmentEvent) error {
	r.closeText(ctx)
	return deliverAttachment(ctx, r.uploader, r.channelID, r.threadTS, a)
}

func (r *sectionRenderer) Text(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}
	if r.mode == sectionTools {
		r.closeBlock(ctx)
	}
	if r.mode != sectionText || r.text == nil {
		r.text = r.newText()
		r.mode = sectionText
	}
	return r.text.Append(ctx, text)
}

func (r *sectionRenderer) Task(ctx context.Context, ev *agent.TaskEvent) error {
	if ev == nil {
		return nil
	}
	if r.mode == sectionText {
		r.closeText(ctx)
	}
	if r.mode != sectionTools || r.block == nil {
		r.block = r.newBlock()
		r.mode = sectionTools
	}
	if ev.Title != "" && !containsString(r.titles, ev.Title) {
		r.titles = append(r.titles, ev.Title)
	}
	if err := r.block.UpdateFromEvent(ctx, ev); err != nil {
		r.logger.Warn("failed to send task update", "error", err, "task_id", ev.ID)
	}
	return nil
}

// Note appends a notice to the reply surface — same routing as Text.
func (r *sectionRenderer) Note(ctx context.Context, text string) error {
	return r.Text(ctx, text)
}

func (r *sectionRenderer) Finish(ctx context.Context, emptyNote string) error {
	if emptyNote != "" {
		if err := r.Text(ctx, emptyNote); err != nil {
			return err
		}
	}
	r.closeText(ctx)
	r.closeBlock(ctx)
	return nil
}

func (r *sectionRenderer) Fail(ctx context.Context, err error) error {
	r.closeBlock(ctx)
	if r.mode != sectionText || r.text == nil {
		r.text = r.newText()
		r.mode = sectionText
	}
	return r.text.Fail(ctx, err)
}

func (r *sectionRenderer) Interrupted(ctx context.Context) {
	if r.text != nil && r.text.Started() && !r.text.Stopped() {
		if err := r.text.Append(ctx, "\n\n_interrupted_"); err == nil {
			_ = r.text.Stop(ctx)
		}
	}
	r.closeBlock(ctx)
}

func (r *sectionRenderer) EnsureStopped(ctx context.Context) {
	r.closeText(ctx)
	r.closeBlock(ctx)
}

// closeText finalises the open text section, if any.
func (r *sectionRenderer) closeText(ctx context.Context) {
	if r.text != nil && r.text.Started() && !r.text.Stopped() {
		if err := r.text.Stop(ctx); err != nil {
			r.logger.Debug("failed to stop text section", "error", err)
		}
	}
	r.text = nil
	if r.mode == sectionText {
		r.mode = sectionNone
	}
}

// closeBlock resolves the open tool block to a compact summary of the tools it
// ran (e.g. "✓ read · skill · write"), then clears it.
func (r *sectionRenderer) closeBlock(ctx context.Context) {
	if r.block != nil {
		done := statusLineDoneText
		if len(r.titles) > 0 {
			done = "✓ " + strings.Join(r.titles, " · ")
		}
		if err := r.block.FinishWith(ctx, done); err != nil {
			r.logger.Debug("failed to resolve tool block", "error", err)
		}
	}
	r.block = nil
	r.titles = nil
	if r.mode == sectionTools {
		r.mode = sectionNone
	}
}

// deliverAttachment uploads a as a separate Slack message in the turn's thread.
// A nil uploader (tests, or no upload surface wired) or a nil attachment is a
// silent no-op so the rest of the reply is unaffected.
func deliverAttachment(ctx context.Context, up attachmentUploader, channelID, threadTS string, a *agent.AttachmentEvent) error {
	if up == nil || a == nil {
		return nil
	}
	return up.UploadAttachment(ctx, channelID, threadTS, a)
}

// containsString reports whether want is present in xs.
func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
