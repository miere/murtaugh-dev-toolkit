package slackapp

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
	buffer        strings.Builder
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
	if w.started {
		return nil
	}
	options := []slack.MsgOption{slack.MsgOptionTaskDisplayMode(slack.TaskDisplayModeTimeline)}
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

func (w *StreamWriter) Append(ctx context.Context, text string) error {
	if text == "" || w.stopped {
		return nil
	}
	if err := w.Start(ctx); err != nil {
		return err
	}
	w.buffer.WriteString(text)
	if w.flushes == 0 {
		return w.Flush(ctx)
	}
	if w.buffer.Len() < w.minChars && time.Since(w.lastFlush) < w.interval {
		return nil
	}
	return w.Flush(ctx)
}

func (w *StreamWriter) Flush(ctx context.Context) error {
	if !w.started || w.stopped || w.buffer.Len() == 0 {
		return nil
	}
	text := w.buffer.String()
	w.buffer.Reset()
	startedAt := time.Now()
	_, _, err := w.api.AppendStreamContext(ctx, w.streamChannel, w.streamTS, slack.MsgOptionMarkdownText(text))
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
