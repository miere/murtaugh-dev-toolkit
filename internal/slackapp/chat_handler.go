package slackapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/acp"
	"github.com/slack-go/slack"
)

// defaultStatusRefreshInterval controls how often Handle re-asserts the
// "is thinking..." assistant status. Slack auto-clears the status as soon as
// the first stream chunk lands, so without periodic refresh the indicator
// disappears the moment streaming starts while the response is still being
// assembled. Two seconds is a compromise between responsiveness and API churn.
const defaultStatusRefreshInterval = 2 * time.Second

// ChatSessionManager is the narrow surface the slackapp uses to talk to
// the ACP layer. Prompt drives the streaming response, while Lookup and
// Cancel back the interrupt-handling path (App.startChat's previous
// in-flight chat cancellation and the /stop slash command). Lookup is
// side-effect-free: callers must treat (_, false) as "no live session,
// skip the ACP cancel call" rather than implicitly opening one.
type ChatSessionManager interface {
	Prompt(context.Context, acp.ConversationKey, acp.SessionMetadata, acp.PromptRequest) (<-chan acp.Event, error)
	Lookup(acp.ConversationKey) (string, bool)
	Cancel(context.Context, string) error
}

type ChatSessionWarmer interface {
	Warm(context.Context) error
}

type ChatHandler struct {
	api                   StreamAPI
	sessions              map[string]ChatSessionManager
	resolver              func(ChatRequest) string
	interval              time.Duration
	minChars              int
	logger                *slog.Logger
	statusRefreshInterval time.Duration
}

type ChatRequest struct {
	TeamID    string
	ChannelID string
	UserID    string
	ThreadTS  string
	MessageTS string
	Text      string
	DM        bool
	Source    string
}

func NewChatHandler(api StreamAPI, sessions map[string]ChatSessionManager, resolver func(ChatRequest) string, interval time.Duration, minChars int, logger *slog.Logger) *ChatHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &ChatHandler{api: api, sessions: sessions, resolver: resolver, interval: interval, minChars: minChars, logger: logger, statusRefreshInterval: defaultStatusRefreshInterval}
}

func (h *ChatHandler) Warm(ctx context.Context) error {
	for name, manager := range h.sessions {
		warmer, ok := manager.(ChatSessionWarmer)
		if !ok {
			continue
		}
		if err := warmer.Warm(ctx); err != nil {
			h.logger.Warn("failed to warm agent", "agent", name, "error", err)
		}
	}
	return nil
}

func (h *ChatHandler) Handle(ctx context.Context, req ChatRequest) (retErr error) {
	startedAt := time.Now()
	if h == nil || len(h.sessions) == 0 {
		return fmt.Errorf("ACP chat is not enabled")
	}

	agentName := h.resolver(req)
	sessions, ok := h.sessions[agentName]
	if !ok {
		return fmt.Errorf("no agent configured for %q (resolved from request)", agentName)
	}

	prompt := strings.TrimSpace(req.Text)
	if prompt == "" {
		return fmt.Errorf("chat prompt is empty")
	}
	key := conversationKey(req)
	metadata := acp.SessionMetadata{TeamID: req.TeamID, ChannelID: req.ChannelID, ThreadTS: key.ThreadTS, UserID: req.UserID, Source: req.Source}
	streamThreadTS := streamThreadTS(req)
	if streamThreadTS == "" {
		return fmt.Errorf("Slack streaming requires a source message timestamp")
	}
	teamID, userID := req.TeamID, req.UserID
	if req.DM {
		teamID, userID = "", ""
	}
	writer := NewStreamWriter(h.api, req.ChannelID, StreamWriterOptions{ThreadTS: streamThreadTS, TeamID: teamID, UserID: userID, Interval: h.interval, MinChars: h.minChars, Logger: h.logger})
	// Interrupt path: when the caller cancels ctx (slackapp's interrupt
	// closure or the /stop slash command), render a "_interrupted_"
	// marker on the partial stream instead of bubbling the cancellation
	// up as an error. Timeout-driven cancellations (DeadlineExceeded)
	// still surface as errors so operators notice stalled agents.
	defer func() {
		if !errors.Is(context.Cause(ctx), context.Canceled) {
			return
		}
		h.renderInterrupted(writer)
		retErr = nil
	}()
	taskWriter := NewTaskCardWriter(h.api, writer, 0, h.logger)
	if err := h.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
		ChannelID: req.ChannelID,
		ThreadTS:  streamThreadTS,
		Status:    "is thinking...",
	}); err != nil {
		h.logger.Warn("failed to set assistant status", "error", err)
	}
	// Slack auto-clears the assistant status as soon as the first stream chunk
	// is sent (start/append/stop). Re-assert it periodically so the indicator
	// stays visible while the back-pressured stream is still being assembled.
	statusCtx, stopStatus := context.WithCancel(ctx)
	statusDone := make(chan struct{})
	go h.refreshAssistantStatus(statusCtx, req.ChannelID, streamThreadTS, "is thinking...", statusDone)
	defer func() {
		stopStatus()
		<-statusDone
		// Best-effort explicit clear so the indicator does not linger (e.g. when
		// the handler errors out before any chunk is sent, Slack would otherwise
		// keep "is thinking..." displayed for up to two minutes).
		if err := h.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
			ChannelID: req.ChannelID,
			ThreadTS:  streamThreadTS,
			Status:    "",
		}); err != nil && !errors.Is(err, context.Canceled) {
			h.logger.Debug("failed to clear assistant status", "error", err)
		}
	}()
	events, err := sessions.Prompt(ctx, key, metadata, acp.PromptRequest{Text: prompt})
	if err != nil {
		return writer.Fail(ctx, err)
	}
	chunks := 0
	bytes := 0
	firstChunkLogged := false
	streamStarted := false
	runningTasks := make(map[string]acp.TaskStatus)
	for event := range events {
		switch event.Type {
		case acp.EventText, acp.EventStatus:
			if event.Text != "" {
				chunks++
				bytes += len(event.Text)
				if !firstChunkLogged {
					firstChunkLogged = true
					h.logger.Info("received first ACP text chunk", "source", req.Source, "channel", req.ChannelID, "duration", time.Since(startedAt), "bytes", len(event.Text))
				}
			}
			if !streamStarted && event.Text != "" {
				if err := writer.Start(ctx); err != nil {
					return err
				}
				streamStarted = true
			}
			if streamStarted {
				if err := writer.Append(ctx, event.Text); err != nil {
					return err
				}
			}
		case acp.EventTask:
			if event.Task == nil {
				continue
			}
			if err := taskWriter.UpdateFromEvent(ctx, event.Task); err != nil {
				h.logger.Warn("failed to send task update", "error", err, "task_id", event.Task.ID)
			}
			if event.Task.Status == acp.TaskStatusInProgress || event.Task.Status == acp.TaskStatusPending {
				runningTasks[event.Task.ID] = event.Task.Status
			} else {
				delete(runningTasks, event.Task.ID)
			}
		case acp.EventError:
			for id := range runningTasks {
				if err := taskWriter.Fail(ctx, id, ""); err != nil {
					h.logger.Warn("failed to mark task as failed", "error", err, "task_id", id)
				}
			}
			return writer.Fail(ctx, event.Error)
		case acp.EventComplete:
			if !streamStarted {
				if err := writer.Start(ctx); err != nil {
					return err
				}
				streamStarted = true
			}
			if err := writer.Stop(ctx); err != nil {
				return err
			}
			h.logger.Info("completed ACP chat response", "source", req.Source, "channel", req.ChannelID, "duration", time.Since(startedAt), "chunks", chunks, "bytes", bytes)
			return nil
		}
	}
	for id := range runningTasks {
		if err := taskWriter.Fail(ctx, id, ""); err != nil {
			h.logger.Warn("failed to mark task as failed on loop exit", "error", err, "task_id", id)
		}
	}
	if !streamStarted {
		if err := writer.Start(ctx); err != nil {
			return err
		}
		streamStarted = true
	}
	if err := writer.Stop(ctx); err != nil {
		return err
	}
	h.logger.Info("completed ACP chat response", "source", req.Source, "channel", req.ChannelID, "duration", time.Since(startedAt), "chunks", chunks, "bytes", bytes)
	return nil
}

func (h *ChatHandler) refreshAssistantStatus(ctx context.Context, channelID, threadTS, status string, done chan<- struct{}) {
	defer close(done)
	interval := h.statusRefreshInterval
	if interval <= 0 {
		interval = defaultStatusRefreshInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := h.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
				ChannelID: channelID,
				ThreadTS:  threadTS,
				Status:    status,
			}); err != nil && !errors.Is(err, context.Canceled) {
				h.logger.Debug("failed to refresh assistant status", "error", err)
			}
		}
	}
}

// renderInterrupted writes the trailing "_interrupted_" marker onto a
// partial stream and stops it. The chat goroutine's context is by
// definition cancelled at this point, so we operate on a fresh bounded
// background context — Slack still needs to be told the stream is
// finalised, otherwise the assistant status lingers and the message
// stays in the in-progress state for up to two minutes. Errors are
// logged but never propagated; the interrupt path is best-effort.
func (h *ChatHandler) renderInterrupted(writer *StreamWriter) {
	if writer == nil {
		return
	}
	if !writer.Started() || writer.Stopped() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := writer.Append(ctx, "\n\n_interrupted_"); err != nil {
		h.logger.Warn("failed to append interrupted marker", "error", err)
		return
	}
	if err := writer.Stop(ctx); err != nil {
		h.logger.Warn("failed to stop stream after interrupt", "error", err)
	}
}

func conversationKey(req ChatRequest) acp.ConversationKey {
	if req.DM {
		return acp.ConversationKey{TeamID: req.TeamID, ChannelID: req.ChannelID, DM: true}
	}
	threadTS := req.ThreadTS
	if threadTS == "" {
		threadTS = req.MessageTS
	}
	return acp.ConversationKey{TeamID: req.TeamID, ChannelID: req.ChannelID, ThreadTS: threadTS}
}

func streamThreadTS(req ChatRequest) string {
	if req.ThreadTS != "" {
		return req.ThreadTS
	}
	return req.MessageTS
}
