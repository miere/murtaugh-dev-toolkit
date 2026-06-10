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
	// Safety net: guarantee the Slack stream is finalised on every exit path.
	// Declared first so it runs last (after the interrupt handler below). The
	// happy path and interrupt path stop the stream themselves, making this a
	// no-op; it only bites when the handler returns early on a writer error,
	// which would otherwise leave the message stuck in the "generating" state.
	defer h.ensureStreamStopped(writer)
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
	taskWriter.SetPlanTitle(planTitle(prompt))
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
		// keep "is thinking..." displayed for up to two minutes). Use a fresh
		// background context: on the interrupt path the request ctx is already
		// cancelled, and reusing it here would make Slack reject the clear and
		// leave "is thinking..." stuck on screen — the exact symptom we are
		// fixing.
		clearCtx, cancelClear := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClear()
		if err := h.api.SetAssistantThreadsStatusContext(clearCtx, slack.AssistantThreadsSetStatusParameters{
			ChannelID: req.ChannelID,
			ThreadTS:  streamThreadTS,
			Status:    "",
		}); err != nil {
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
			// A caller interrupt (new message / /stop) surfaces here as a
			// context cancellation, not an agent failure. Never paint a
			// still-running task red for being cut short — leave the cards in
			// their last real state and let the deferred interrupt handler
			// render the "_interrupted_" marker. Real agent errors (and
			// deadline-exceeded) still fail the in-flight tasks below.
			if errors.Is(event.Error, context.Canceled) || errors.Is(context.Cause(ctx), context.Canceled) {
				return event.Error
			}
			// Only the still-running tasks were genuinely cut short by the
			// error; tasks that already reported a terminal status have been
			// removed from runningTasks and keep their real outcome.
			h.finalizeTasks(ctx, taskWriter, runningTasks, slack.TaskCardStatusError)
			return writer.Fail(ctx, event.Error)
		case acp.EventComplete:
			// The agent finished successfully: any task still marked running
			// never received an explicit terminal update (common with parallel
			// tool calls). Complete them rather than abandoning them mid-spinner
			// or — worse — painting them red.
			h.finalizeTasks(ctx, taskWriter, runningTasks, slack.TaskCardStatusComplete)
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
	// Channel closed without an explicit EventComplete/EventError. No error
	// surfaced, so treat any leftover running tasks as completed rather than
	// failed.
	h.finalizeTasks(ctx, taskWriter, runningTasks, slack.TaskCardStatusComplete)
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

// finalizeTasks brings every still-running task to a terminal status and
// removes it from running. It is the single place that resolves the outcome of
// tasks the agent left open, so a card is never stranded in a spinner or
// painted with a status that contradicts what actually happened. Errors are
// logged, not propagated: finalisation is best-effort cleanup.
func (h *ChatHandler) finalizeTasks(ctx context.Context, taskWriter *TaskCardWriter, running map[string]acp.TaskStatus, status slack.TaskCardStatus) {
	for id := range running {
		var err error
		switch status {
		case slack.TaskCardStatusComplete:
			err = taskWriter.Complete(ctx, id, "")
		default:
			err = taskWriter.Fail(ctx, id, "")
		}
		if err != nil {
			h.logger.Warn("failed to finalize task", "error", err, "task_id", id, "status", status)
		}
		delete(running, id)
	}
}

// ensureStreamStopped finalises the Slack stream if it was started but never
// stopped — the early-return-on-writer-error paths would otherwise leave the
// message stuck showing the "generating" indicator. It runs on a fresh
// background context because the request ctx may already be cancelled.
func (h *ChatHandler) ensureStreamStopped(writer *StreamWriter) {
	if writer == nil || !writer.Started() || writer.Stopped() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := writer.Stop(ctx); err != nil {
		h.logger.Warn("failed to stop stream on cleanup", "error", err)
	}
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

// planTitle derives a short, single-line title for the Plan block that groups
// the agent's task cards, from the user's prompt. Long or multi-line prompts
// are collapsed and truncated so the plan header stays compact.
func planTitle(prompt string) string {
	title := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(prompt, "\r", " "), "\n", " "))
	for strings.Contains(title, "  ") {
		title = strings.ReplaceAll(title, "  ", " ")
	}
	// Truncate on rune boundaries so a multibyte character is never split.
	const maxRunes = 80
	if runes := []rune(title); len(runes) > maxRunes {
		title = strings.TrimSpace(string(runes[:maxRunes])) + "…"
	}
	return title
}

func streamThreadTS(req ChatRequest) string {
	if req.ThreadTS != "" {
		return req.ThreadTS
	}
	return req.MessageTS
}
