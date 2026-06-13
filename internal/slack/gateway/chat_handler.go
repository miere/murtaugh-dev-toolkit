package gateway

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

// defaultIdleTimeout bounds a chat turn by inactivity rather than total
// wall-clock time. The timer resets on every event the agent emits (text or a
// task update), so a long turn that keeps making progress never trips it; only
// an agent that goes completely silent for this long is treated as stalled.
const defaultIdleTimeout = 10 * time.Minute

// idleTimeoutNotice is appended to the partial response when a turn is abandoned
// for inactivity. It is deliberately honest — the agent was asked to stop, it
// did not fail — and invites the user to continue rather than leaving a silent,
// dead message behind.
const idleTimeoutNotice = "\n\n:hourglass_flowing_sand: _The agent went quiet for %s, so I asked it to stop. It may have stalled — send another message to pick things back up._"

// ChatSessionManager is the narrow surface the gateway uses to talk to
// the ACP layer. Prompt drives the streaming response, while Lookup and
// Cancel back the interrupt-handling path (Gateway.startChat's previous
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
	idleTimeout           time.Duration
	// sessionLog records each turn to the journal's acp_session stream. nil
	// (the default) records nothing; the gateway wires it only when that stream
	// is enabled.
	sessionLog *sessionLogger
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

// WithIdleTimeout sets how long a turn may go without any agent activity before
// it is treated as stalled. Non-positive values are ignored so the default
// stands. Returns the handler for chaining.
func (h *ChatHandler) WithIdleTimeout(d time.Duration) *ChatHandler {
	if d > 0 {
		h.idleTimeout = d
	}
	return h
}

func (h *ChatHandler) effectiveIdleTimeout() time.Duration {
	if h.idleTimeout > 0 {
		return h.idleTimeout
	}
	return defaultIdleTimeout
}

// WithSessionLogger attaches the acp_session turn recorder and returns the
// handler for chaining. nil leaves session logging off.
func (h *ChatHandler) WithSessionLogger(sl *sessionLogger) *ChatHandler {
	h.sessionLog = sl
	return h
}

// resetIdleTimer restarts t for another idle window, draining an already-fired
// timer first so the next select does not observe a stale tick.
func resetIdleTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
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

	// Session-log accumulation. Declared before the defers below so the deferred
	// recorder (registered first → runs last, after the interrupt handler has
	// settled retErr) reads the final outcome, transcript, and session id.
	var (
		respBuf    strings.Builder
		chunkSeen  int
		byteSeen   int
		timedOut   bool
		turnErr    error
		sessionID  string
		stopReason string
	)
	if h.sessionLog != nil {
		defer func() {
			// An agent failure returns through writer.Fail, which may itself
			// return nil, so retErr alone does not reveal it — turnErr captures
			// the agent error explicitly. Interrupt (ctx cancelled) and idle
			// timeout take precedence as they are not failures of the agent.
			outcome := turnCompleted
			switch {
			case errors.Is(context.Cause(ctx), context.Canceled):
				outcome = turnInterrupted
			case timedOut:
				outcome = turnTimedOut
			case turnErr != nil || retErr != nil:
				outcome = turnErrored
			}
			// Fresh context: on the interrupt/timeout paths the request ctx is
			// already cancelled, but the row enqueue + transcript write must run.
			h.sessionLog.record(context.Background(), sessionTurn{
				req: req, agent: agentName, sessionID: sessionID, prompt: prompt,
				response: respBuf.String(), outcome: outcome, stopReason: stopReason,
				duration: time.Since(startedAt), chunks: chunkSeen, bytes: byteSeen,
			})
		}()
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
	// Interrupt path: when the caller cancels ctx (gateway's interrupt
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
	// Drive the prompt under a child context we can cancel ourselves. The idle
	// watchdog uses it to unblock the in-flight ACP request without touching the
	// parent ctx — so on a timeout we can still render our own message on the
	// parent rather than tripping the interrupt path.
	promptCtx, cancelPrompt := context.WithCancel(ctx)
	defer cancelPrompt()
	events, err := sessions.Prompt(promptCtx, key, metadata, acp.PromptRequest{Text: prompt})
	if err != nil {
		turnErr = err
		return writer.Fail(ctx, err)
	}
	// The session now exists; capture its id for the turn record.
	if id, ok := sessions.Lookup(key); ok {
		sessionID = id
	}
	firstChunkLogged := false
	streamStarted := false
	runningTasks := make(map[string]acp.TaskStatus)
	// Idle watchdog: a turn is bounded by inactivity, not total wall-clock. Every
	// event resets the timer; only an agent that goes silent for the whole window
	// trips it. A long turn that keeps emitting tool calls never times out.
	idle := time.NewTimer(h.effectiveIdleTimeout())
	defer idle.Stop()
	for {
		select {
		case <-idle.C:
			// The agent went silent for the whole idle window. Ask it to stop,
			// post an honest notice (the parent ctx is still alive — we never
			// cancelled it), then unblock the in-flight request and drain so the
			// stream tears down cleanly. Task cards keep their last reported
			// status: the agent did not fail, we stopped waiting, so we never
			// repaint them red.
			timedOut = true
			if err := h.handleIdleTimeout(ctx, sessions, key, writer, streamStarted); err != nil {
				h.logger.Warn("failed to finalise stream on idle timeout", "error", err)
			}
			cancelPrompt()
			// Drain until the ACP layer closes the channel. The prompt goroutine
			// and the shared readLoop block on their sends; abandoning the channel
			// here would stall event delivery for every other conversation.
			for range events {
			}
			h.logger.Warn("ACP chat timed out on inactivity", "source", req.Source, "channel", req.ChannelID, "duration", time.Since(startedAt), "idle_timeout", h.effectiveIdleTimeout())
			return nil
		case event, ok := <-events:
			if !ok {
				// Channel closed without an explicit EventComplete/EventError. No
				// error surfaced, so treat leftover running tasks as completed.
				return h.finishStream(ctx, writer, taskWriter, runningTasks, &streamStarted, req, startedAt, chunkSeen, byteSeen, stopReason)
			}
			resetIdleTimer(idle, h.effectiveIdleTimeout())
			switch event.Type {
			case acp.EventText, acp.EventStatus:
				if event.Text != "" {
					chunkSeen++
					byteSeen += len(event.Text)
					respBuf.WriteString(event.Text)
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
				// Only an explicit terminal status retires a task from the running
				// set. An update that merely refines the title or content carries
				// no status (empty/unknown); treating that as "done" used to drop
				// the task from runningTasks, so finalizeTasks never resolved it
				// and the card was stranded mid-spinner — which Slack paints with a
				// warning once the plan closes. Keep tracking it until a real
				// terminal status arrives or the agent completes.
				if isTerminalTaskStatus(event.Task.Status) {
					delete(runningTasks, event.Task.ID)
				} else {
					runningTasks[event.Task.ID] = event.Task.Status
				}
			case acp.EventError:
				// A caller interrupt (new message / /stop) surfaces here as a
				// context cancellation, not an agent failure. Never paint a
				// still-running task red for being cut short — leave the cards in
				// their last real state and let the deferred interrupt handler
				// render the "_interrupted_" marker. Real agent errors still fail
				// the in-flight tasks below.
				if errors.Is(event.Error, context.Canceled) || errors.Is(context.Cause(ctx), context.Canceled) {
					return event.Error
				}
				// Only the still-running tasks were genuinely cut short by the
				// error; tasks that already reported a terminal status have been
				// removed from runningTasks and keep their real outcome.
				turnErr = event.Error
				h.finalizeTasks(ctx, taskWriter, runningTasks, slack.TaskCardStatusError)
				return writer.Fail(ctx, event.Error)
			case acp.EventComplete:
				// The agent finished successfully: any task still marked running
				// never received an explicit terminal update (common with parallel
				// tool calls). Complete them rather than abandoning them
				// mid-spinner or — worse — painting them red.
				stopReason = event.StopReason
				return h.finishStream(ctx, writer, taskWriter, runningTasks, &streamStarted, req, startedAt, chunkSeen, byteSeen, stopReason)
			}
		}
	}
}

// finishStream resolves a successful turn: it completes any task cards the agent
// left open, makes sure a Slack message exists, stops the stream, and logs. It
// is shared by the explicit EventComplete and the channel-closed-cleanly paths.
func (h *ChatHandler) finishStream(ctx context.Context, writer *StreamWriter, taskWriter *TaskCardWriter, runningTasks map[string]acp.TaskStatus, streamStarted *bool, req ChatRequest, startedAt time.Time, chunks, bytes int, stopReason string) error {
	h.finalizeTasks(ctx, taskWriter, runningTasks, slack.TaskCardStatusComplete)
	if !*streamStarted {
		if err := writer.Start(ctx); err != nil {
			return err
		}
		*streamStarted = true
	}
	// A turn that completed but streamed no agent text would otherwise leave the
	// user staring at silence (or just task cards). Surface a short note — with
	// the agent's stop reason when it explains the emptiness — so an empty reply
	// is legible rather than mistaken for a dead bot.
	if bytes == 0 {
		if err := writer.Append(ctx, emptyReplyNote(stopReason)); err != nil {
			return err
		}
		h.logger.Warn("ACP turn completed with no agent text", "source", req.Source, "channel", req.ChannelID, "stop_reason", stopReason)
	}
	if err := writer.Stop(ctx); err != nil {
		return err
	}
	h.logger.Info("completed ACP chat response", "source", req.Source, "channel", req.ChannelID, "duration", time.Since(startedAt), "chunks", chunks, "bytes", bytes, "stop_reason", stopReason)
	return nil
}

// emptyReplyNote builds the message shown when a turn produced no agent text.
// A non-"end_turn" stop reason (max_tokens, refusal, …) is the likely cause and
// is named; otherwise the note nudges the user to retry.
func emptyReplyNote(stopReason string) string {
	if stopReason != "" && stopReason != "end_turn" {
		return fmt.Sprintf(":warning: _The agent ended the turn without a reply (stop reason: `%s`). Try rephrasing or asking again._", stopReason)
	}
	return ":warning: _The agent finished without a text reply — it may have only run tools. Nudge it with another message to continue._"
}

// handleIdleTimeout reacts to a stalled turn: it asks the agent to stop (so it
// does not keep burning work whose output can no longer be delivered) and posts
// an honest "asked it to stop" notice on the still-live parent context, then
// finalises the stream. It deliberately does NOT touch task cards — the agent
// did not fail, so their last reported status stands rather than turning red.
func (h *ChatHandler) handleIdleTimeout(ctx context.Context, sessions ChatSessionManager, key acp.ConversationKey, writer *StreamWriter, streamStarted bool) error {
	idle := h.effectiveIdleTimeout()
	h.logger.Warn("ACP turn idle; asking agent to stop", "idle_timeout", idle)
	// Tell the agent to stop. Best-effort, on a fresh context: the parent ctx is
	// still alive (we never cancelled it) but session/cancel is cleanup that must
	// run regardless of the turn's deadline.
	if sessionID, ok := sessions.Lookup(key); ok {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := sessions.Cancel(cancelCtx, sessionID); err != nil {
			h.logger.Warn("ACP session cancel on idle timeout failed", "error", err, "session_id", sessionID)
		}
		cancel()
	}
	if !streamStarted {
		if err := writer.Start(ctx); err != nil {
			return err
		}
	}
	if err := writer.Append(ctx, fmt.Sprintf(idleTimeoutNotice, idle.Round(time.Second))); err != nil {
		return err
	}
	return writer.Stop(ctx)
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

// isTerminalTaskStatus reports whether an ACP task status is a final outcome.
// A task in any other state — pending, in_progress, or an update that omitted
// its status — is still running and must stay tracked so it is finalised
// rather than abandoned mid-flight.
func isTerminalTaskStatus(status acp.TaskStatus) bool {
	switch status {
	case acp.TaskStatusComplete, acp.TaskStatusFailed, acp.TaskStatusCancelled:
		return true
	default:
		return false
	}
}

func streamThreadTS(req ChatRequest) string {
	if req.ThreadTS != "" {
		return req.ThreadTS
	}
	return req.MessageTS
}
