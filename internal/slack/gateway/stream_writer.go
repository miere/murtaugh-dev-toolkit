package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

type StreamWriter struct {
	api       StreamAPI
	channelID string
	threadTS  string
	teamID    string
	userID    string
	interval  time.Duration
	minChars  int
	logger    *slog.Logger

	streamChannel string
	streamTS      string
	pending       string
	lastFlush     time.Time
	flushes       int
	bytesFlushed  int
	started       bool
	stopped       bool
}

func NewStreamWriter(api StreamAPI, channelID string, opts StreamWriterOptions) *StreamWriter {
	if opts.Interval <= 0 {
		opts.Interval = 250 * time.Millisecond
	}
	if opts.MinChars <= 0 {
		opts.MinChars = 24
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &StreamWriter{api: api, channelID: channelID, threadTS: opts.ThreadTS, teamID: opts.TeamID, userID: opts.UserID, interval: opts.Interval, minChars: opts.MinChars, logger: logger}
}

type StreamWriterOptions struct {
	ThreadTS string
	TeamID   string
	UserID   string
	Interval time.Duration
	MinChars int
	Logger   *slog.Logger
}

func (w *StreamWriter) Start(ctx context.Context) error {
	return w.StartWithOptions(ctx)
}

func (w *StreamWriter) StartWithOptions(ctx context.Context, extraOptions ...slack.MsgOption) error {
	if w.started {
		return nil
	}
	// Plan mode groups every task_update chunk under a single Plan block
	// (with a shared title) instead of stacking them as a flat timeline of
	// separate cards. TaskCardWriter opens the plan with a plan_update chunk
	// on the first task. See https://docs.slack.dev/reference/block-kit/blocks/plan-block/
	options := []slack.MsgOption{slack.MsgOptionTaskDisplayMode(slack.TaskDisplayModePlan)}
	options = append(options, extraOptions...)
	if w.threadTS != "" {
		options = append(options, slack.MsgOptionTS(w.threadTS))
	}
	if w.teamID != "" {
		options = append(options, slack.MsgOptionRecipientTeamID(w.teamID))
	}
	if w.userID != "" {
		options = append(options, slack.MsgOptionRecipientUserID(w.userID))
	}
	channel, ts, err := w.api.StartStreamContext(ctx, w.channelID, options...)
	if err != nil {
		return fmt.Errorf("start Slack stream: %w", err)
	}
	w.streamChannel = channel
	w.streamTS = ts
	w.lastFlush = time.Now()
	w.started = true
	return nil
}

func (w *StreamWriter) StreamChannel() string { return w.streamChannel }
func (w *StreamWriter) StreamTS() string      { return w.streamTS }
func (w *StreamWriter) Started() bool         { return w.started }
func (w *StreamWriter) Stopped() bool         { return w.stopped }

// Append buffers a reply delta and paints it on a *coherent boundary*, never
// mid-thought. Coherence here is a paint concern only — the segmenter has
// already sealed this run against any tool activity, so buffering can never
// reorder content, only pace how a text run grows on screen:
//
//   - the first delta paints eagerly, so the reply appears promptly;
//   - thereafter we flush through the last newline, so complete lines (lists,
//     code, paragraphs) land whole;
//   - a long or stale unbroken line still streams (the latency/size cap), but is
//     trimmed to the last space so prose never repaints mid-word.
//
// Slack's streaming API concatenates appends into one growing message, so a
// retained trailing fragment is simply prepended to the next flush — splitting
// on a boundary changes *when* the paint happens, never the final text.
func (w *StreamWriter) Append(ctx context.Context, text string) error {
	if text == "" || w.stopped {
		return nil
	}
	if err := w.Start(ctx); err != nil {
		return err
	}
	w.pending += text
	if w.flushes == 0 {
		return w.Flush(ctx)
	}
	if i := strings.LastIndexByte(w.pending, '\n'); i >= 0 {
		return w.emit(ctx, i+1)
	}
	if len(w.pending) >= w.minChars || time.Since(w.lastFlush) >= w.interval {
		if i := strings.LastIndexByte(w.pending, ' '); i >= 0 {
			return w.emit(ctx, i+1)
		}
		return w.Flush(ctx)
	}
	return nil
}

// Flush paints all buffered text. Used on seal (Stop) and interjection.
func (w *StreamWriter) Flush(ctx context.Context) error {
	return w.emit(ctx, len(w.pending))
}

// emit paints the first n bytes of the buffer to Slack and retains the rest for
// the next flush. n == 0 (or an unstarted/stopped stream) is a no-op.
func (w *StreamWriter) emit(ctx context.Context, n int) error {
	if !w.started || w.stopped || n == 0 {
		return nil
	}
	text := w.pending[:n]
	w.pending = w.pending[n:]
	startedAt := time.Now()
	_, _, err := w.api.AppendStreamContext(ctx, w.streamChannel, w.streamTS, slack.MsgOptionChunks(slack.NewMarkdownTextChunk(text)))
	if err != nil {
		return fmt.Errorf("append Slack stream: %w", err)
	}
	w.flushes++
	w.bytesFlushed += len(text)
	w.logger.Info("appended Slack stream chunk", "channel", w.streamChannel, "bytes", len(text), "duration", time.Since(startedAt), "flushes", w.flushes)
	w.lastFlush = time.Now()
	return nil
}

func (w *StreamWriter) Fail(ctx context.Context, err error) error {
	message := "\n\n:warning: Murtaugh hit an error while talking to the ACP agent."
	if err != nil {
		message += "\n`" + sanitizeSlackInline(err.Error()) + "`"
	}
	if appendErr := w.Append(ctx, message); appendErr != nil {
		return appendErr
	}
	return w.Stop(ctx)
}

func (w *StreamWriter) Stop(ctx context.Context) error {
	if err := w.Flush(ctx); err != nil {
		return err
	}
	if !w.started || w.stopped {
		return nil
	}
	startedAt := time.Now()
	_, _, err := w.api.StopStreamContext(ctx, w.streamChannel, w.streamTS)
	if err != nil {
		return fmt.Errorf("stop Slack stream: %w", err)
	}
	w.stopped = true
	w.logger.Info("stopped Slack stream", "channel", w.streamChannel, "flushes", w.flushes, "bytes", w.bytesFlushed, "duration", time.Since(startedAt))
	return nil
}

func sanitizeSlackInline(text string) string {
	text = strings.ReplaceAll(text, "`", "'")
	if len(text) > 300 {
		return text[:300] + "…"
	}
	return text
}
