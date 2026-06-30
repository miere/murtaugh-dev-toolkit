package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/config"
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
	Prompt(context.Context, agent.ConversationKey, agent.SessionMetadata, agent.PromptRequest) (<-chan agent.Event, error)
	Lookup(agent.ConversationKey) (string, bool)
	Cancel(context.Context, string) error
}

type ChatSessionWarmer interface {
	Warm(context.Context) error
}

type ChatHandler struct {
	api                   StreamAPI
	sessions              map[string]ChatSessionManager
	resolver              func(ChatRequest) ChatRoute
	interval              time.Duration
	minChars              int
	logger                *slog.Logger
	statusRefreshInterval time.Duration
	idleTimeout           time.Duration
	// sessionLog records each turn to the journal's acp_session stream. nil
	// (the default) records nothing; the gateway wires it only when that stream
	// is enabled.
	sessionLog *sessionLogger
	// progressDisplay resolves the per-agent progress rendering. nil defaults
	// every agent to the simplified single-line view.
	progressDisplay func(agent string) config.ProgressDisplay
	// statusMessenger lets the simplified renderer post/edit/delete its own
	// context-block message. nil makes the simplified line a no-op (tests that
	// do not wire Slack); the gateway always supplies it in production.
	statusMessenger statusMessenger
	// backfiller renders an existing Slack thread into a transcript when a brand-
	// new ACP session is opened for it, so the agent starts with the prior
	// conversation as context. nil disables backfill (the prompt is the single
	// triggering message only).
	backfiller threadBackfiller
	// fileFetcher downloads plain-text attachments so their contents can be
	// folded into the prompt. nil disables attachment handling (tests that do
	// not wire Slack); the gateway always supplies it in production.
	fileFetcher fileFetcher
	// uploader delivers agent-produced attachments (EventAttachment) into the
	// turn's thread. nil disables outbound attachments (tests that do not wire
	// Slack); the gateway always supplies it in production.
	uploader attachmentUploader
	// permissionAsker resolves an ACP agent's EventPermission with a human (Slack
	// approval buttons). It is consulted on the turn's own event loop so the
	// approval card is ordered with the reply — the chat handler settles any open
	// reply text first, exactly as the native loop's inline approval is ordered.
	// nil denies every ACP permission request (no human wired), which keeps a
	// headless turn from hanging.
	permissionAsker agent.PermissionAsker
}

// threadBackfiller renders a Slack thread into a transcript block for a cold
// session's first prompt. *ThreadBackfiller satisfies it.
type threadBackfiller interface {
	Backfill(ctx context.Context, channelID, threadTS, excludeTS string) (string, error)
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
	// Files carries any attachments on the triggering Slack message. Plain-text
	// files are fetched and folded into the prompt so the agent can read them.
	Files []slack.File
}

// ChatRoute is the routing decision for a chat request: which agent answers and
// whether the reply is threaded. ReplyOnThread=false makes the bot post directly
// in the channel; see replyThreadTS and conversationKey for how it shapes both
// the reply location and the session binding.
type ChatRoute struct {
	Agent         string
	ReplyOnThread bool
}

func NewChatHandler(api StreamAPI, sessions map[string]ChatSessionManager, resolver func(ChatRequest) ChatRoute, interval time.Duration, minChars int, logger *slog.Logger) *ChatHandler {
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

// WithProgressDisplay sets the resolver that picks each agent's progress
// rendering. nil (the default) renders every agent in simplified mode. Returns
// the handler for chaining.
func (h *ChatHandler) WithProgressDisplay(resolve func(agent string) config.ProgressDisplay) *ChatHandler {
	h.progressDisplay = resolve
	return h
}

// WithStatusMessenger wires the Slack surface the simplified progress renderer
// uses to manage its own context-block message. Returns the handler for
// chaining. nil leaves the simplified line a no-op.
func (h *ChatHandler) WithStatusMessenger(m statusMessenger) *ChatHandler {
	h.statusMessenger = m
	return h
}

// WithBackfiller wires the thread backfiller that seeds a cold ACP session with
// the existing Slack thread. Returns the handler for chaining. nil (the default)
// disables backfill. A nil *ThreadBackfiller is also tolerated so callers can
// wire it unconditionally and let an unbuilt backfiller stay disabled.
func (h *ChatHandler) WithBackfiller(b *ThreadBackfiller) *ChatHandler {
	if b == nil {
		return h
	}
	h.backfiller = b
	return h
}

// WithFileFetcher wires the downloader used to fold plain-text attachments into
// the prompt. Returns the handler for chaining. nil (the default) disables
// attachment handling.
func (h *ChatHandler) WithFileFetcher(f fileFetcher) *ChatHandler {
	if f == nil {
		return h
	}
	h.fileFetcher = f
	return h
}

// WithUploader wires the surface used to deliver agent-produced attachments into
// the turn's thread. Returns the handler for chaining. nil (the default) disables
// outbound attachments.
func (h *ChatHandler) WithUploader(u attachmentUploader) *ChatHandler {
	if u == nil {
		return h
	}
	h.uploader = u
	return h
}

// WithPermissionAsker wires the gate that resolves an ACP agent's permission
// requests (EventPermission) with a human. Returns the handler for chaining. nil
// (the default) leaves the handler denying every ACP permission request.
func (h *ChatHandler) WithPermissionAsker(a agent.PermissionAsker) *ChatHandler {
	if a == nil {
		return h
	}
	h.permissionAsker = a
	return h
}

// resolveProgressDisplay returns the configured mode for the agent, defaulting
// to simplified when no resolver is wired or it returns an empty value.
func (h *ChatHandler) resolveProgressDisplay(agent string) config.ProgressDisplay {
	if h.progressDisplay == nil {
		return config.ProgressDisplaySimplified
	}
	if mode := h.progressDisplay(agent); mode != "" {
		return mode
	}
	return config.ProgressDisplaySimplified
}

// newChatRenderer builds the per-turn renderer for the resolved progress mode:
// the woven task-card view (tasks mode — cards + reply in one answer stream) or
// the alternating section view (simplified mode, the default — tool blocks and
// reply messages as a separate, ordered sequence).
func (h *ChatHandler) newChatRenderer(mode config.ProgressDisplay, channelID, threadTS string, opts StreamWriterOptions) chatRenderer {
	if mode == config.ProgressDisplayTasks {
		writer := NewStreamWriter(h.api, channelID, opts)
		return newWovenRenderer(writer, NewTaskCardWriter(h.api, writer, 0, h.logger), h.uploader, channelID, threadTS, h.logger)
	}
	return newSectionRenderer(
		func() *StreamWriter { return NewStreamWriter(h.api, channelID, opts) },
		func() *StatusLineWriter {
			return NewStatusLineWriter(h.statusMessenger, channelID, threadTS, 0, h.logger)
		},
		h.uploader,
		channelID,
		threadTS,
		h.logger,
	)
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

// backfillHistory renders the existing Slack thread into a transcript to seed a
// brand-new ACP session, returning "" when no backfill is needed or possible.
// It backfills only a threaded conversation (ThreadTS set) whose session is not
// already live: a warm session already holds the history, and a top-level
// message has no prior thread to read. The triggering message (MessageTS) is
// excluded so it is not duplicated ahead of the user's own prompt text. A fetch
// failure is logged and degraded to "" — the agent proceeds without backstory
// rather than the turn failing.
func (h *ChatHandler) backfillHistory(ctx context.Context, req ChatRequest, sessions ChatSessionManager, key agent.ConversationKey) string {
	if h.backfiller == nil || req.ThreadTS == "" {
		return ""
	}
	if _, live := sessions.Lookup(key); live {
		return ""
	}
	history, err := h.backfiller.Backfill(ctx, req.ChannelID, req.ThreadTS, req.MessageTS)
	if err != nil {
		h.logger.Warn("thread backfill failed; proceeding without history", "channel", req.ChannelID, "thread", req.ThreadTS, "error", err)
		return ""
	}
	if history != "" {
		h.logger.Info("seeding new ACP session with thread history", "channel", req.ChannelID, "thread", req.ThreadTS)
	}
	return history
}

func (h *ChatHandler) Handle(ctx context.Context, req ChatRequest) (retErr error) {
	startedAt := time.Now()
	if h == nil || len(h.sessions) == 0 {
		return fmt.Errorf("ACP chat is not enabled")
	}

	route := h.resolver(req)
	agentName := route.Agent
	sessions, ok := h.sessions[agentName]
	if !ok {
		return fmt.Errorf("no agent configured for %q (resolved from request)", agentName)
	}

	prompt := strings.TrimSpace(req.Text)
	// Fold any plain-text attachments into the message the agent sees. A
	// caption-less upload (no text, only files) is still a valid prompt.
	attachments := h.renderAttachments(ctx, req.Files)
	if prompt == "" && attachments == "" {
		return fmt.Errorf("chat prompt is empty")
	}
	promptForAgent := prompt
	if attachments != "" {
		if promptForAgent == "" {
			promptForAgent = attachments
		} else {
			promptForAgent += "\n\n" + attachments
		}
	}
	key := conversationKey(req, route.ReplyOnThread)
	metadata := agent.SessionMetadata{TeamID: req.TeamID, ChannelID: req.ChannelID, ThreadTS: key.ThreadTS, UserID: req.UserID, Source: req.Source}
	history := h.backfillHistory(ctx, req, sessions, key)
	streamThreadTS := replyThreadTS(req, route.ReplyOnThread)
	// streamThreadTS is empty by design in channel-reply mode (post at the channel
	// root). Posting still needs a triggering timestamp, so guard on that instead.
	if req.ThreadTS == "" && req.MessageTS == "" {
		return fmt.Errorf("Slack streaming requires a source message timestamp")
	}

	// Session-log accumulation. Declared before the defers below so the deferred
	// recorder (registered first → runs last, after the interrupt handler has
	// settled retErr) reads the final outcome, transcript, and session id.
	var (
		respBuf    strings.Builder
		chunkSeen  int
		byteSeen   int
		attachSeen int
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
	progressMode := h.resolveProgressDisplay(agentName)
	streamOpts := StreamWriterOptions{ThreadTS: streamThreadTS, TeamID: teamID, UserID: userID, Interval: h.interval, MinChars: h.minChars, Logger: h.logger}
	renderer := h.newChatRenderer(progressMode, req.ChannelID, streamThreadTS, streamOpts)
	// Safety net: finalise every Slack message (text sections and tool blocks) on
	// any exit path. Declared first so it runs last (after the interrupt handler
	// below); the happy/error/idle paths finalise themselves, making this a no-op
	// except on an early return. Fresh context: the request ctx may already be
	// cancelled.
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		renderer.EnsureStopped(sctx)
	}()
	// Interrupt path: when the caller cancels ctx (gateway's interrupt closure or
	// the /stop slash command), render an "_interrupted_" marker instead of
	// bubbling the cancellation up as an error. Timeout-driven cancellations
	// (DeadlineExceeded) still surface as errors so operators notice stalls. Fresh
	// context, since the request ctx is cancelled on this path.
	defer func() {
		if !errors.Is(context.Cause(ctx), context.Canceled) {
			return
		}
		ictx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		renderer.Interrupted(ictx)
		retErr = nil
	}()
	// The assistant-threads "is thinking..." indicator is a thread-scoped Slack
	// feature, so it only applies when we are replying in a thread. In
	// channel-reply mode (empty streamThreadTS) there is no thread to attach it
	// to — skip it rather than emit a meaningless call that Slack rejects.
	if streamThreadTS != "" {
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
	}
	// Drive the prompt under a child context we can cancel ourselves. The idle
	// watchdog uses it to unblock the in-flight ACP request without touching the
	// parent ctx — so on a timeout we can still render our own message on the
	// parent rather than tripping the interrupt path.
	promptCtx, cancelPrompt := context.WithCancel(ctx)
	defer cancelPrompt()
	events, err := sessions.Prompt(promptCtx, key, metadata, agent.PromptRequest{Text: promptForAgent, History: history})
	if err != nil {
		turnErr = err
		return renderer.Fail(ctx, err)
	}
	// The session now exists; capture its id for the turn record.
	if id, ok := sessions.Lookup(key); ok {
		sessionID = id
	}
	firstChunkLogged := false
	// finish resolves a successful turn: it finalises every open section and, when
	// the turn produced no reply text, surfaces an empty-reply note so silence is
	// legible. Shared by the explicit EventComplete and channel-closed paths.
	finish := func() error {
		emptyNote := ""
		// A turn that delivered a file but no prose is not empty — suppress the
		// note so an attachment-only reply does not look like a failed turn.
		if byteSeen == 0 && attachSeen == 0 {
			emptyNote = emptyReplyNote(stopReason)
			h.logger.Warn("agent turn completed with no agent text", "source", req.Source, "channel", req.ChannelID, "stop_reason", stopReason)
		}
		if err := renderer.Finish(ctx, emptyNote); err != nil {
			return err
		}
		h.logger.Info("completed agent chat response", "source", req.Source, "channel", req.ChannelID, "duration", time.Since(startedAt), "chunks", chunkSeen, "bytes", byteSeen, "attachments", attachSeen, "stop_reason", stopReason)
		return nil
	}
	// Idle watchdog: a turn is bounded by inactivity, not total wall-clock. Every
	// event resets the timer; only an agent that goes silent for the whole window
	// trips it. A long turn that keeps emitting tool calls never times out.
	idle := time.NewTimer(h.effectiveIdleTimeout())
	defer idle.Stop()
	for {
		select {
		case <-idle.C:
			// The agent went silent for the whole idle window. Ask it to stop, post
			// an honest notice (the parent ctx is still alive — we never cancelled
			// it), finalise the open section, then unblock the in-flight request and
			// drain so the stream tears down cleanly. Tool blocks keep their last
			// reported state: the agent did not fail, we stopped waiting.
			timedOut = true
			if sid, ok := sessions.Lookup(key); ok {
				cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if cerr := sessions.Cancel(cancelCtx, sid); cerr != nil {
					h.logger.Warn("agent session cancel on idle timeout failed", "error", cerr, "session_id", sid)
				}
				cancel()
				// Drop the wedged session so the next turn opens a fresh one rather
				// than reusing a session that may still hold an in-flight tool call —
				// agents without session/cancel cannot be told to abandon it. The
				// shared agent process keeps running; only this binding is reset.
				if d, ok := sessions.(interface {
					Discard(agent.ConversationKey)
				}); ok {
					d.Discard(key)
				}
			}
			if nerr := renderer.Note(ctx, fmt.Sprintf(idleTimeoutNotice, h.effectiveIdleTimeout().Round(time.Second))); nerr != nil {
				h.logger.Warn("failed to post idle-timeout notice", "error", nerr)
			}
			renderer.EnsureStopped(ctx)
			cancelPrompt()
			// Drain until the agent layer closes the channel. The prompt goroutine
			// and the shared readLoop block on their sends; abandoning the channel
			// here would stall event delivery for every other conversation.
			for range events {
			}
			h.logger.Warn("agent chat timed out on inactivity", "source", req.Source, "channel", req.ChannelID, "duration", time.Since(startedAt), "idle_timeout", h.effectiveIdleTimeout())
			return nil
		case event, ok := <-events:
			if !ok {
				// Channel closed without an explicit EventComplete/EventError.
				return finish()
			}
			resetIdleTimer(idle, h.effectiveIdleTimeout())
			switch event.Type {
			case agent.EventText:
				if event.Text != "" {
					chunkSeen++
					byteSeen += len(event.Text)
					respBuf.WriteString(event.Text)
					if !firstChunkLogged {
						firstChunkLogged = true
						h.logger.Info("received first agent text chunk", "source", req.Source, "channel", req.ChannelID, "duration", time.Since(startedAt), "bytes", len(event.Text))
					}
				}
				if err := renderer.Text(ctx, event.Text); err != nil {
					return err
				}
			case agent.EventStatus:
				// Progress/meta only (e.g. compaction, empty-reply retry) — never
				// part of the reply. The idle timer was already reset above. (ACP
				// never emits EventStatus; this is native meta.)
				if event.Text != "" {
					h.logger.Debug("agent status", "source", req.Source, "channel", req.ChannelID, "status", event.Text)
				}
			case agent.EventTask:
				if event.Task == nil {
					continue
				}
				if err := renderer.Task(ctx, event.Task); err != nil {
					return err
				}
			case agent.EventAttachment:
				if event.Attachment == nil {
					continue
				}
				// Best-effort: a failed upload is logged but never aborts the turn —
				// the text reply still matters. attachSeen counts only successful
				// deliveries, so a failed attachment-only turn still surfaces the
				// empty-reply note rather than going silent.
				if err := renderer.Attachment(ctx, event.Attachment); err != nil {
					h.logger.Warn("failed to deliver agent attachment", "source", req.Source, "channel", req.ChannelID, "filename", event.Attachment.Filename, "error", err)
				} else {
					attachSeen++
					h.logger.Info("delivered agent attachment", "source", req.Source, "channel", req.ChannelID, "filename", event.Attachment.Filename)
				}
			case agent.EventPermission:
				// An ACP agent is asking the human to approve a tool call. Resolve it
				// on this event loop — so it is ordered after the reply text/tasks that
				// preceded it on the stream — and feed the decision back to the agent.
				// Settling the open reply first means the approval card lands below a
				// committed message instead of an unfinished, streaming one (the "looks
				// truncated" symptom); this mirrors the native loop, whose inline
				// approval is naturally ordered after the tool's task event.
				if event.Permission == nil {
					continue
				}
				decision := h.askPermission(ctx, req, streamThreadTS, renderer, event.Permission.Request)
				if event.Permission.Decision != nil {
					event.Permission.Decision <- decision
				}
			case agent.EventError:
				// A caller interrupt (new message / /stop) surfaces here as a context
				// cancellation, not an agent failure: return it and let the deferred
				// interrupt handler render the "_interrupted_" marker without painting
				// a tool red. Real agent errors are surfaced on the reply surface.
				if errors.Is(event.Error, context.Canceled) || errors.Is(context.Cause(ctx), context.Canceled) {
					return event.Error
				}
				turnErr = event.Error
				return renderer.Fail(ctx, event.Error)
			case agent.EventComplete:
				stopReason = event.StopReason
				return finish()
			}
		}
	}
}

// askPermission resolves an ACP permission request, returning the chosen option's
// ID ("" denies). It first settles any open reply section through the renderer so
// the out-of-band approval card posts below a committed message rather than an
// in-flight stream, then asks the human via the wired gate in the turn's own
// thread. A nil gate (no human wired) or an ask error denies — the turn must not
// hang waiting on an answer that can never come.
func (h *ChatHandler) askPermission(ctx context.Context, req ChatRequest, threadTS string, renderer chatRenderer, pr agent.PermissionRequest) string {
	renderer.BeginInterjection(ctx)
	if h.permissionAsker == nil {
		h.logger.Warn("ACP permission request but no asker wired; denying", "channel", req.ChannelID, "tool_kind", pr.ToolKind)
		return ""
	}
	loc := agent.TurnLocation{ChannelID: req.ChannelID, ThreadTS: threadTS, UserID: req.UserID}
	optionID, err := h.permissionAsker.AskPermission(ctx, loc, pr)
	if err != nil {
		h.logger.Warn("ACP permission ask failed; denying", "channel", req.ChannelID, "error", err)
		return ""
	}
	return optionID
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

// conversationKey identifies the agent session a request belongs to. A threaded
// conversation — channel thread OR DM — is bound to its thread, so a session is
// never shared across threads: the key is the request's ThreadTS, falling back
// to its own MessageTS for a top-level message that roots a new thread
// (mirroring replyThreadTS, so the key matches where replies are posted).
//
// In channel-reply mode (replyOnThread=false on a top-level message) there is no
// thread to bind to, and binding per-MessageTS would spawn a fresh session every
// message — shredding context. Instead ThreadTS stays empty, so every top-level
// message in the channel maps to the SAME key {ChannelID, ThreadTS:""}: one
// long-lived, channel-wide rolling conversation. The key omits UserID, so that
// session is shared by the channel's participants. An explicit reset is /clear,
// not an implicit per-message one.
//
// The DM flag is retained so a DM thread and a same-id channel thread cannot
// collide and so session metadata/logging still distinguishes the two surfaces.
func conversationKey(req ChatRequest, replyOnThread bool) agent.ConversationKey {
	threadTS := req.ThreadTS
	if threadTS == "" && replyOnThread {
		threadTS = req.MessageTS
	}
	return agent.ConversationKey{TeamID: req.TeamID, ChannelID: req.ChannelID, ThreadTS: threadTS, DM: req.DM}
}

// isTerminalTaskStatus reports whether an ACP task status is a final outcome.
// A task in any other state — pending, in_progress, or an update that omitted
// its status — is still running and must stay tracked so it is finalised
// rather than abandoned mid-flight.
func isTerminalTaskStatus(status agent.TaskStatus) bool {
	switch status {
	case agent.TaskStatusComplete, agent.TaskStatusFailed, agent.TaskStatusCancelled:
		return true
	default:
		return false
	}
}

// replyThreadTS decides where the bot's reply is posted. An incoming message
// that is ALREADY in a thread always gets a threaded reply (never yank a
// threaded conversation out to the channel root). For a top-level message,
// replyOnThread selects the strategy: true roots a thread at the message
// (historical behaviour), false posts directly at the channel root (empty
// thread_ts). Mirrors conversationKey so the reply lands where the session lives.
func replyThreadTS(req ChatRequest, replyOnThread bool) string {
	if req.ThreadTS != "" {
		return req.ThreadTS
	}
	if !replyOnThread {
		return ""
	}
	return req.MessageTS
}
