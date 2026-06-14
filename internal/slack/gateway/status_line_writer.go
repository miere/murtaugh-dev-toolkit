package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh-dev-toolkit/internal/acp"
)

// progressRenderer is the surface ChatHandler uses to turn ACP task events into
// Slack progress UI. Two implementations back it: TaskCardWriter (the full
// multi-card plan, woven into the answer stream) and StatusLineWriter (the
// simplified single context-block line in its own message). The handler is
// written against this interface so the choice is a per-agent config concern,
// not a code path.
//
// UpdateFromEvent reports progress; Complete/Fail resolve an individual task
// for renderers that track them; Finish runs once at the end of the turn,
// unconditionally, so a renderer that owns a side-channel message can tear it
// down regardless of how the turn ended.
type progressRenderer interface {
	UpdateFromEvent(ctx context.Context, event *acp.TaskEvent) error
	Complete(ctx context.Context, taskID, title string) error
	Fail(ctx context.Context, taskID, title string) error
	Finish(ctx context.Context) error
}

// statusMessenger is the narrow Slack surface StatusLineWriter needs to manage
// its own message: post it and edit it in place (including the final resolved
// line). *slack.Client satisfies it; tests substitute a fake.
type statusMessenger interface {
	PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error)
	UpdateMessageContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error)
}

// defaultStatusLineTitle labels the line before any task has reported a title,
// and is the fallback when an update carries none.
const defaultStatusLineTitle = "Working…"

// statusLineDoneText is the final line the message resolves to when the turn
// ends, so the operator sees a settled state rather than the last in-flight
// activity frozen in place.
const statusLineDoneText = "✓ Done thinking"

// StatusLineWriter renders an agent's progress as a single context-block message
// — `{type: context, elements: [{type: plain_text, emoji: true}]}` — that it
// posts once, rewrites in place on every update (last-write-wins), and resolves
// to a "✓ Done thinking" line when the turn ends. Unlike TaskCardWriter it does
// not touch the answer stream: it owns its own non-intrusive message in the same
// thread, so the rendering is a plain context line rather than a task card. It
// keeps no per-task state — there is exactly one line — so Complete/Fail are
// no-ops and teardown is a single resolving update in Finish.
type StatusLineWriter struct {
	messenger statusMessenger
	channelID string
	threadTS  string
	logger    *slog.Logger
	interval  time.Duration

	mu        sync.Mutex
	lastTitle string
	lastFlush time.Time
	flushed   bool
	postChan  string // channel the message was posted to (echoed by Slack)
	postTS    string // timestamp of the posted message; "" until first post
	done      bool   // Finish has run; further writes are suppressed
}

// NewStatusLineWriter creates a simplified progress writer that posts its own
// message to channelID (in threadTS when set). A nil messenger makes every
// method a safe no-op, which keeps tests that do not wire Slack happy.
func NewStatusLineWriter(messenger statusMessenger, channelID, threadTS string, interval time.Duration, logger *slog.Logger) *StatusLineWriter {
	if interval <= 0 {
		interval = defaultTaskUpdateInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &StatusLineWriter{messenger: messenger, channelID: channelID, threadTS: threadTS, logger: logger, interval: interval}
}

// UpdateFromEvent collapses a task event into the single status line. The
// event's title (or the last one seen, or a default) becomes the line's text.
// Updates are throttled so a rapidly-iterating agent does not hammer Slack; the
// latest title is always remembered so the next write reflects it.
func (w *StatusLineWriter) UpdateFromEvent(ctx context.Context, event *acp.TaskEvent) error {
	if event == nil {
		return nil
	}
	title := w.rememberTitle(event.Title)
	if !w.shouldFlush() {
		return nil
	}
	return w.render(ctx, title)
}

// Complete is a no-op: the simplified line carries no per-task status, and the
// message is removed wholesale in Finish.
func (w *StatusLineWriter) Complete(context.Context, string, string) error { return nil }

// Fail is a no-op for the same reason as Complete; an agent failure is surfaced
// on the answer stream, not the progress line.
func (w *StatusLineWriter) Fail(context.Context, string, string) error { return nil }

// Finish resolves the status message to "✓ Done thinking". It runs once; a turn
// that never posted a line (no task events) is a no-op — there is nothing to
// resolve, and an unsolicited "done" line would be noise. Errors are logged,
// not returned, since teardown is best-effort cleanup.
func (w *StatusLineWriter) Finish(ctx context.Context) error {
	w.mu.Lock()
	if w.done || w.messenger == nil || w.postTS == "" {
		w.done = true
		w.mu.Unlock()
		return nil
	}
	w.done = true
	channel, ts := w.postChan, w.postTS
	w.mu.Unlock()
	if _, _, _, err := w.messenger.UpdateMessageContext(ctx, channel, ts, statusMsgOptions(statusLineDoneText)...); err != nil {
		w.logger.Debug("resolve status line failed", "channel", channel, "ts", ts, "error", err)
		return err
	}
	return nil
}

// render posts the status message on the first write and edits it in place
// thereafter. Suppressed once Finish has run so a late event cannot resurrect a
// deleted message.
func (w *StatusLineWriter) render(ctx context.Context, text string) error {
	w.mu.Lock()
	if w.done || w.messenger == nil {
		w.mu.Unlock()
		return nil
	}
	channel, ts := w.postChan, w.postTS
	w.mu.Unlock()

	startedAt := time.Now()
	if ts == "" {
		options := statusMsgOptions(text)
		if w.threadTS != "" {
			options = append(options, slack.MsgOptionTS(w.threadTS))
		}
		postedChannel, postedTS, err := w.messenger.PostMessageContext(ctx, w.channelID, options...)
		if err != nil {
			return fmt.Errorf("post status line: %w", err)
		}
		w.recordPost(postedChannel, postedTS, startedAt)
		return nil
	}
	if _, _, _, err := w.messenger.UpdateMessageContext(ctx, channel, ts, statusMsgOptions(text)...); err != nil {
		return fmt.Errorf("update status line: %w", err)
	}
	w.recordFlush(startedAt)
	return nil
}

// statusMsgOptions renders the status line into the message options shared by
// the post, in-place update, and final resolve: the context block plus a plain
// text fallback for notifications and accessibility.
func statusMsgOptions(text string) []slack.MsgOption {
	return []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionBlocks(statusContextBlock(text)),
	}
}

// statusContextBlock builds the single context block carrying the status text
// as an emoji-enabled plain_text element.
func statusContextBlock(text string) slack.Block {
	return slack.NewContextBlock("", slack.NewTextBlockObject(slack.PlainTextType, text, true, false))
}

// rememberTitle records a non-empty title and returns the best label for the
// line: the supplied title, else the last-seen title, else the default.
func (w *StatusLineWriter) rememberTitle(title string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if title != "" {
		w.lastTitle = title
	}
	if w.lastTitle == "" {
		return defaultStatusLineTitle
	}
	return w.lastTitle
}

// shouldFlush rate-limits updates: the first always flushes, the rest only once
// the interval has elapsed since the last write.
func (w *StatusLineWriter) shouldFlush() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.flushed {
		return true
	}
	return time.Since(w.lastFlush) >= w.interval
}

func (w *StatusLineWriter) recordPost(channel, ts string, t time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.postChan = channel
	w.postTS = ts
	w.flushed = true
	w.lastFlush = t
}

func (w *StatusLineWriter) recordFlush(t time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushed = true
	w.lastFlush = t
}
