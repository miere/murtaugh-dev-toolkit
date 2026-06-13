package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/acp"
	"github.com/miere/murtaugh-dev-toolkit/internal/agentdelegate"
	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/miere/murtaugh-dev-toolkit/internal/journal"
	"github.com/miere/murtaugh-dev-toolkit/internal/unfurl"
	"github.com/miere/murtaugh-dev-toolkit/internal/workflow"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// workflowDispatcher is the minimal surface needed to dispatch an interactive
// callback to a workflow engine. *workflow.Engine satisfies it.
type workflowDispatcher interface {
	Execute(ctx context.Context, interaction slack.InteractionCallback) error
}

// RestartTrigger is the function the Gateway calls to request a graceful
// restart. The arguments mirror internal/app.RestartRequest field-by-field
// but stay stringly-typed so gateway does not need to import the
// composition root (which would create a cycle). Returns true when the
// shutdown sequence has begun, false when the request was declined
// (already firing, within cool-down, or no coordinator is wired).
type RestartTrigger func(source, userID, channel, reason string) bool

type Gateway struct {
	api             userDirectoryAPI
	socket          *socketmode.Client
	handler         SlashCommandHandler
	workflow        workflowDispatcher
	chat            *ChatHandler
	chatSessions    map[string]ChatSessionManager
	chatWarmTimeout time.Duration
	// cancelGrace is how long the interrupt path waits after asking the
	// ACP agent to cancel its in-flight prompt before hard-cancelling the
	// chat goroutine's context. Short enough that the user does not stare
	// at a stalled "_interrupted_" marker, long enough that trailing
	// chunks already on the wire can flush. Defaults to 2s via
	// ACPConfig.EffectiveCancelGracePeriod.
	cancelGrace time.Duration
	inFlight    *InFlightRegistry
	// recentEvents suppresses duplicate Slack event deliveries so a
	// redelivered message does not spawn a second chat that interrupts the
	// first. nil disables de-duplication (CLI/MCP and most tests).
	recentEvents    *eventDedup
	unfurl          *LinkUnfurlHandler
	unfurlTimeout   time.Duration
	startupNotifier StartupNotifier
	startupPingSent bool
	logger          *slog.Logger
	// cfg holds the configuration values consulted at runtime. Authz entries
	// (admin_user, allowed_users) start out as configured (IDs or handles) and
	// are mutated in place by resolveAllowSet at the start of Run so the rest
	// of the Gateway can rely on ID-only comparisons via cfg.IsAllowedUser.
	cfg config.ConfigurationConfig
	// restart is the optional graceful-restart trigger. nil in CLI/MCP and
	// in tests that do not need to exercise the restart path; the slash
	// handler reports "not available" when nil.
	restart RestartTrigger
	// resumeStore persists the restart marker between processes. nil
	// disables the "restarting…" / "back online" Slack confirmation flow
	// — the restart still happens, just silently.
	resumeStore ResumeMarkerStore
	// messaging is the Slack surface used by the resume helpers. Set to
	// the same *slack.Client as api in New; kept as a separate field so
	// tests can substitute a narrow fake without re-implementing the
	// full Slack client.
	messaging slackMessagingAPI
	// resumeConsumed flips to true the first time consumeResumeMarker
	// runs after a successful socket connect. Slack may emit multiple
	// EventTypeConnected events across the daemon's life (re-connects,
	// flaky links); we only want to act on the marker once per process.
	resumeConsumed bool
	// configWatchPaths lists files whose mtime, when it advances,
	// triggers a restart suggestion to the admin. Empty (the default)
	// disables the watcher entirely. The composition root populates
	// this from the loaded config's sibling files (slack.yaml,
	// agents.yaml, jobs.yaml).
	configWatchPaths []string
	// scheduledJobs is the job set captured from the loaded config at
	// construction. The scheduler registers the entries whose ScheduleKind
	// is cron/every; manual jobs are ignored. Empty disables scheduling.
	scheduledJobs map[string]config.JobProfile
	// runJob executes a job by name to completion. Injected by the
	// composition root (WithScheduledRunner) as a closure over the jobs.run
	// tool. nil disables the scheduler, so CLI/MCP and tests never pay for
	// it.
	runJob ScheduledRunner
	// recorder receives gateway-stream journal events for inbound interactions
	// (slash commands, interactive callbacks) and is threaded into the workflow
	// engine and unfurl handler. Never nil after New: a nil argument becomes a
	// no-op recorder so call sites never branch.
	recorder journal.Recorder
}

func New(cfg config.Config, logger *slog.Logger, recorder journal.Recorder) *Gateway {
	if logger == nil {
		logger = slog.Default()
	}
	if recorder == nil {
		recorder = journal.NopRecorder{}
	}
	api := slack.New(cfg.OAuth.BotToken, slack.OptionAppLevelToken(cfg.OAuth.AppToken))
	socket := socketmode.New(api, socketmode.OptionDebug(cfg.Configuration.Debug))
	startupNotifier, err := NewSlackStartupNotifier(api, cfg.Configuration.AdminUser, logger)
	if err != nil {
		logger.Error("startup Slack ping disabled", "error", err)
	}
	var chat *ChatHandler
	var sessions map[string]ChatSessionManager
	if !cfg.ACP.Enabled {
		logger.Warn("ACP chat disabled: set acp.enabled: true in agents.yaml to enable DM and app_mention replies")
	}
	if cfg.ACP.Enabled {
		sessions = make(map[string]ChatSessionManager)
		for name, profile := range cfg.Agents {
			// Default an agent's working directory to the workspace (the
			// config dir, e.g. ~/.config/murtaugh) when it leaves workdir
			// unset, so agents start where the bundled skills and templates
			// live and can auto-discover them.
			workDir := profile.WorkDir
			if strings.TrimSpace(workDir) == "" {
				workDir = cfg.BaseDir
			}
			client := acp.NewProcessClient(acp.ProcessOptions{
				Command: profile.Command,
				Args:    profile.Args,
				WorkDir: workDir,
				Logger:  logger,
			})
			sessions[name] = acp.NewSessionManager(
				client,
				cfg.ACP.EffectiveSessionIdleTimeout(),
				cfg.ACP.EffectiveMaxSessions(),
			).WithLogger(logger.With("agent", name)).WithCancelOverride(profile.Interruptible)
		}

		resolver := func(req ChatRequest) string {
			if req.DM {
				if cfg.Chat.DMAgent != "" {
					return cfg.Chat.DMAgent
				}
				return cfg.Chat.DefaultAgent
			}
			if agent, ok := cfg.Chat.ChannelAgents[req.ChannelID]; ok {
				return agent
			}
			return cfg.Chat.DefaultAgent
		}

		chat = NewChatHandler(
			api,
			sessions,
			resolver,
			cfg.ACP.EffectiveStreamAppendInterval(),
			cfg.ACP.EffectiveStreamMinChunkChars(),
			logger,
		).WithIdleTimeout(cfg.ACP.EffectiveRequestTimeout())
	}
	// One shared runner backs every delegate-to-agent surface (jobs, workflow
	// triggers, unfurls). Each delegation spins its own isolated agent process,
	// so this is safe to share. Built only when agents are configured; config
	// validation guarantees any delegate-to-agent rule names a known agent.
	var unfurlDelegator UnfurlDelegator
	var workflowDelegator workflow.AgentDelegator
	if len(cfg.Agents) > 0 {
		runner := agentdelegate.NewRunner(cfg.Agents, cfg.ACP, cfg.BaseDir, logger)
		unfurlDelegator = runner
		workflowDelegator = runner
	}
	var unfurlHandler *LinkUnfurlHandler
	if len(cfg.UnfurlRules) > 0 {
		matcher, err := unfurl.NewMatcher(cfg.UnfurlRules)
		if err != nil {
			logger.Error("custom link unfurling disabled", "error", err)
		} else {
			renderer := unfurl.NewRenderer(cfg.BaseDir, nil)
			unfurlHandler = NewLinkUnfurlHandler(matcher, renderer, nil, unfurlDelegator, api, logger).WithRecorder(recorder)
		}
	}
	return &Gateway{
		api:             api,
		socket:          socket,
		handler:         NewDefaultSlashCommandHandler(cfg.Commands),
		workflow:        workflow.NewEngine(cfg, workflow.Options{Logger: logger, Delegator: workflowDelegator, Recorder: recorder}),
		chat:            chat,
		chatSessions:    sessions,
		chatWarmTimeout: cfg.ACP.EffectiveStartupTimeout(),
		cancelGrace:     cfg.ACP.EffectiveCancelGracePeriod(),
		inFlight:        NewInFlightRegistry(),
		recentEvents:    newEventDedup(0),
		unfurl:          unfurlHandler,
		unfurlTimeout:   2 * time.Minute,
		startupNotifier: startupNotifier,
		logger:          logger,
		cfg:             cfg.Configuration,
		messaging:       api,
		scheduledJobs:   cfg.Jobs,
		recorder:        recorder,
	}
}

// record emits a gateway-stream journal event, stamping the correlation id
// carried on ctx (minted at interaction ingress). A nil recorder (struct-literal
// Gateways in tests) is a no-op, matching how the gateway treats its other
// optional collaborators.
func (a *Gateway) record(ctx context.Context, kind string, level journal.Level, summary string, keys journal.Keys, payload any) {
	if a.recorder == nil {
		return
	}
	a.recorder.Record(ctx, journal.Event{
		Stream:  journal.StreamGateway,
		Kind:    kind,
		Level:   level,
		Summary: summary,
		CorrID:  journal.CorrIDFromContext(ctx),
		Keys:    keys,
		Payload: payload,
	})
}

// WithResumeMarkerStore attaches the persistent store used to bridge
// restart notices across process restarts. When nil (the default) the
// restart flow runs silently — the "restarting…" / "back online"
// confirmation messages are skipped.
func (a *Gateway) WithResumeMarkerStore(store ResumeMarkerStore) *Gateway {
	a.resumeStore = store
	return a
}

// WithRestartTrigger attaches the graceful-restart trigger and returns the
// receiver for fluent wiring at the composition root. When nil (the
// default) the restart slash verb reports the feature as unavailable.
func (a *Gateway) WithRestartTrigger(trigger RestartTrigger) *Gateway {
	a.restart = trigger
	return a
}

// WithConfigWatchPaths attaches the list of on-disk files whose mtime
// changes should produce a restart suggestion to the admin. Blank
// entries are filtered out; an empty list disables the watcher
// entirely (which is the default, so CLI/MCP modes and tests never
// pay for the polling goroutine).
func (a *Gateway) WithConfigWatchPaths(paths []string) *Gateway {
	cleaned := make([]string, 0, len(paths))
	for _, p := range paths {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	a.configWatchPaths = cleaned
	return a
}

// WithScheduledRunner attaches the executor used to run cron/every-scheduled
// jobs and returns the receiver for fluent wiring. When nil (the default) the
// scheduler is disabled entirely, so CLI/MCP modes and tests never start it.
func (a *Gateway) WithScheduledRunner(runner ScheduledRunner) *Gateway {
	a.runJob = runner
	return a
}

func (a *Gateway) Run(ctx context.Context) error {
	resolveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := a.resolveAllowSet(resolveCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("resolve allowed users: %w", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.socket.RunContext(ctx)
	}()
	a.warmChat(ctx)
	a.startConfigWatcher(ctx)
	stopScheduler := a.startScheduler(ctx)
	defer stopScheduler()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case event := <-a.socket.Events:
			a.handleEvent(ctx, event)
		}
	}
}

func (a *Gateway) warmChat(ctx context.Context) {
	if a.chat == nil {
		return
	}
	go func() {
		warmCtx, cancel := context.WithTimeout(ctx, a.chatWarmTimeout)
		defer cancel()
		if err := a.chat.Warm(warmCtx); err != nil {
			a.logger.Error("ACP warmup failed", "error", err)
			return
		}
		a.logger.Info("ACP warmup completed")
	}()
}

// startConfigWatcher launches the on-disk config file watcher in a
// goroutine that lives for the lifetime of the Run context. No-op
// when no paths are configured so the cost (one ticker, one
// goroutine) is paid only by the Slack daemon path. The watcher's
// callback posts a restart suggestion to the admin DM via
// SuggestRestart, which is itself a best-effort no-op when no
// messaging surface or admin user is available.
func (a *Gateway) startConfigWatcher(ctx context.Context) {
	if len(a.configWatchPaths) == 0 {
		return
	}
	watcher := newConfigWatcher(a.configWatchPaths, defaultConfigWatchInterval, a.onConfigFileChanged, a.logger)
	a.logger.Info("config watcher started", "paths", a.configWatchPaths, "interval", defaultConfigWatchInterval.String())
	go watcher.Run(ctx)
}

// onConfigFileChanged is the watcher's callback. It builds an
// operator-facing reason that names the changed file and its new
// mtime, then asks the bot to surface the restart suggestion via
// the standard Block Kit path. Errors from SuggestRestart are
// logged and never propagated since the watcher's contract is
// best-effort: a missed suggestion is preferable to a noisy stall.
func (a *Gateway) onConfigFileChanged(ctx context.Context, path string, mtime time.Time) {
	reason := fmt.Sprintf("`%s` changed on disk at %s; restart Murtaugh to pick up the new config.",
		filepath.Base(path), mtime.UTC().Format(time.RFC3339))
	a.logger.Info("config file change detected", "path", path, "mtime", mtime)
	if _, _, err := a.SuggestRestart(ctx, "", reason); err != nil {
		a.logger.Error("config watcher: restart suggestion failed", "path", path, "error", err)
	}
}

func (a *Gateway) handleEvent(ctx context.Context, event socketmode.Event) {
	switch event.Type {
	case socketmode.EventTypeConnected:
		a.logger.Debug("socket mode lifecycle event", "type", event.Type)
		a.notifyStartup(ctx)
		a.notifyResume(ctx)
	case socketmode.EventTypeConnecting, socketmode.EventTypeHello:
		a.logger.Debug("socket mode lifecycle event", "type", event.Type)
	case socketmode.EventTypeSlashCommand:
		a.handleSlashCommand(ctx, event)
	case socketmode.EventTypeInteractive:
		a.handleInteractive(event)
	case socketmode.EventTypeEventsAPI:
		a.handleEventsAPI(event)
	default:
		a.logger.Debug("ignored socket mode event", "type", event.Type)
	}
}

// resolveAllowSet resolves configuration.admin_user and configuration.allowed_users
// (each may be a Slack user ID or a handle) into IDs and rewrites a.cfg with
// the resolved values, so subsequent IsAllowedUser checks are ID-only. A
// single users.list call is made when any entry is a handle. Unresolvable
// entries are fatal (fail-closed). When both admin_user and allowed_users are
// empty the Gateway is effectively locked down and direct interactions will be
// denied; a warning is logged in that case.
func (a *Gateway) resolveAllowSet(ctx context.Context) error {
	refs := make([]string, 0, 1+len(a.cfg.AllowedUsers))
	hasAdmin := strings.TrimSpace(a.cfg.AdminUser) != ""
	if hasAdmin {
		refs = append(refs, a.cfg.AdminUser)
	}
	refs = append(refs, a.cfg.AllowedUsers...)
	if len(refs) == 0 {
		a.logger.Warn("authorization locked down: configuration.admin_user and configuration.allowed_users are both empty; direct interactions will be ignored")
		return nil
	}
	ids, err := resolveUserIDs(ctx, a.api, refs)
	if err != nil {
		return err
	}
	if hasAdmin {
		a.cfg.AdminUser = ids[0]
		a.cfg.AllowedUsers = ids[1:]
	} else {
		a.cfg.AllowedUsers = ids
	}
	a.logger.Info("resolved authorized Slack users", "admin_user", a.cfg.AdminUser, "allowed_users", len(a.cfg.AllowedUsers))
	return nil
}

func (a *Gateway) notifyStartup(ctx context.Context) {
	if a.startupNotifier == nil || a.startupPingSent {
		return
	}
	a.startupPingSent = true
	go func() {
		pingCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := a.startupNotifier.NotifyStartup(pingCtx); err != nil {
			a.logger.Error("startup Slack ping failed", "error", err)
		}
	}()
}

// notifyResume runs the once-per-process consumption of the on-disk
// resume marker. Slack may emit several Connected events for one daemon
// (re-connects, network blips); the resumeConsumed flag guards against
// re-editing the same notice on every reconnect.
func (a *Gateway) notifyResume(ctx context.Context) {
	if a.resumeConsumed {
		return
	}
	a.resumeConsumed = true
	go func() {
		resumeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		a.consumeResumeMarker(resumeCtx)
	}()
}

func (a *Gateway) handleInteractive(event socketmode.Event) {
	interaction, ok := event.Data.(slack.InteractionCallback)
	if !ok {
		a.ack(event)
		a.logger.Warn("unexpected interactive payload", "type", fmt.Sprintf("%T", event.Data))
		return
	}

	a.ack(event)
	if !a.cfg.IsAllowedUser(interaction.User.ID) {
		a.logger.Info("denied interactive callback from unauthorized user", "user", interaction.User.ID, "channel", interaction.Channel.ID, "callback_id", interaction.CallbackID)
		return
	}
	if isRestartSuggestionInteraction(interaction) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			a.handleRestartSuggestionInteraction(ctx, interaction)
		}()
		return
	}
	// Mint a correlation id for this interaction and record its arrival. The
	// same id is propagated into the workflow engine via the context so the
	// match/no-match/trigger events all tie back to this one click.
	corrID := journal.NewCorrID("gw")
	a.record(journal.WithCorrID(context.Background(), corrID), "interactive.received", journal.LevelInfo,
		"interactive callback received",
		journal.Keys{TeamID: interaction.Team.ID, ChannelID: interaction.Channel.ID, UserID: interaction.User.ID},
		map[string]any{"interaction_type": string(interaction.Type), "callback_id": interaction.CallbackID})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		ctx = journal.WithCorrID(ctx, corrID)
		if err := a.workflow.Execute(ctx, interaction); err != nil {
			a.logger.Error("interactive workflow failed", "error", err)
		}
	}()
}

func (a *Gateway) handleSlashCommand(ctx context.Context, event socketmode.Event) {
	command, ok := event.Data.(slack.SlashCommand)
	if !ok {
		a.ack(event, ephemeralText("Unsupported slash command payload."))
		a.logger.Warn("unexpected slash command payload", "type", fmt.Sprintf("%T", event.Data))
		return
	}

	if !a.cfg.IsAllowedUser(command.UserID) {
		a.logger.Info("denied slash command from unauthorized user", "command", command.Command, "user", command.UserID, "channel", command.ChannelID)
		a.ack(event, ephemeralText("Sorry, you are not authorized to use this command."))
		return
	}

	ctx = journal.WithCorrID(ctx, journal.NewCorrID("gw"))
	a.record(ctx, "slash.command", journal.LevelInfo, "slash command received",
		journal.Keys{TeamID: command.TeamID, ChannelID: command.ChannelID, UserID: command.UserID},
		map[string]any{"command": command.Command, "text": command.Text})

	response, err := a.handler.HandleSlashCommand(ctx, command)
	if isRestartSlashCommand(command.Text) {
		a.handleRestartSlashCommand(ctx, event, command)
		return
	}
	if isChatSlashCommand(command.Text) {
		a.handleChatSlashCommand(ctx, event, command)
		return
	}
	if isStopSlashCommand(command) {
		a.handleStopSlashCommand(event, command, slashCommandThreadTS(event))
		return
	}
	if err != nil {
		a.logger.Error("slash command failed", "command", command.Command, "error", err)
		response = ephemeralText("Murtaugh hit an error while handling that command.")
	}
	a.ack(event, response)
}

// handleRestartSlashCommand is invoked when an allowed user issues the
// `restart` verb. Authorization is two-layered: the outer
// handleSlashCommand has already checked IsAllowedUser, and this method
// additionally requires IsAdminUser. Non-admin allowed users receive an
// ephemeral deny so the failure mode is discoverable (unlike DMs or
// mentions, where silent ignore is the policy).
//
// On accept, the "restarting…" notice is posted to the originating
// channel and a resume marker is written to disk before the coordinator
// is signalled. The notice + marker are best-effort: any failure is
// logged but never blocks the restart itself (see resume.go).
func (a *Gateway) handleRestartSlashCommand(ctx context.Context, event socketmode.Event, command slack.SlashCommand) {
	if !a.cfg.IsAdminUser(command.UserID) {
		a.logger.Info("denied restart slash command from non-admin user", "command", command.Command, "user", command.UserID, "channel", command.ChannelID)
		a.ack(event, ephemeralText("Sorry, only the configured admin can restart Murtaugh."))
		return
	}
	if a.restart == nil {
		a.logger.Warn("restart slash command received but no coordinator is wired", "user", command.UserID, "channel", command.ChannelID)
		a.ack(event, ephemeralText("Restart is not available in this deployment."))
		return
	}
	reason := fmt.Sprintf("user requested via %s restart", command.Command)
	// Post + persist must happen before the coordinator fires so the
	// marker is durable when the grace timer expires and the process
	// exits. Use a fresh bounded context so a slow Slack API call does
	// not stall the slash ack.
	noticeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	a.postRestartNoticeAndSaveMarker(noticeCtx, command.ChannelID, "", command.UserID, restartSourceSlash, reason)
	cancel()
	if !a.restart(string(restartSourceSlash), command.UserID, command.ChannelID, reason) {
		a.ack(event, ephemeralText("A restart is already in progress (or the cool-down has not elapsed). Try again shortly."))
		return
	}
	a.ack(event, ephemeralText("Restarting Murtaugh now. I'll be back in a moment."))
}

func (a *Gateway) handleChatSlashCommand(ctx context.Context, event socketmode.Event, command slack.SlashCommand) {
	text := slashChatPrompt(command.Text)
	if text == "" {
		a.ack(event, ephemeralText("Usage: `/murtaugh chat <prompt>`"))
		return
	}
	if a.chat == nil {
		a.ack(event, ephemeralText("ACP chat is not enabled. Configure `acp.enabled: true` first."))
		return
	}
	a.ack(event, ephemeralText("Murtaugh is answering in the channel."))
	a.startChat(ctx, ChatRequest{TeamID: command.TeamID, ChannelID: command.ChannelID, UserID: command.UserID, Text: text, Source: "slash_command"})
}

// handleStopSlashCommand cancels the in-flight chat for the
// conversation the command was invoked in. Slack's slash command
// payload carries `thread_ts` when the command was issued from inside
// a thread (the slack-go SlashCommand struct does not surface it, so
// the caller re-parses the raw socketmode payload via
// slashCommandThreadTS and passes it in). For channel-root
// invocations there is no thread context, so we fall back to a
// channel-scoped key — that matches DMs (which have no thread either).
//
// Authorisation: the outer handleSlashCommand has already enforced
// IsAllowedUser, so no extra admin gate is required here.
func (a *Gateway) handleStopSlashCommand(event socketmode.Event, command slack.SlashCommand, threadTS string) {
	key := acp.ConversationKey{TeamID: command.TeamID, ChannelID: command.ChannelID, ThreadTS: threadTS}
	if threadTS == "" && strings.HasPrefix(command.ChannelID, "D") {
		key.DM = true
	}
	if a.inFlight.Cancel(key) {
		a.logger.Info("stop slash command cancelled in-flight chat", "user", command.UserID, "channel", command.ChannelID, "thread_ts", threadTS)
		a.ack(event, ephemeralText("Stopped."))
		return
	}
	a.ack(event, ephemeralText("Nothing to stop."))
}

func (a *Gateway) handleEventsAPI(event socketmode.Event) {
	eventsAPI, ok := event.Data.(slackevents.EventsAPIEvent)
	if !ok {
		a.ack(event)
		a.logger.Warn("unexpected Events API payload", "type", fmt.Sprintf("%T", event.Data))
		return
	}
	a.ack(event)
	switch inner := eventsAPI.InnerEvent.Data.(type) {
	case *slackevents.LinkSharedEvent:
		a.handleLinkShared(eventsAPI.TeamID, inner)
	case *slackevents.AppMentionEvent:
		if a.chat == nil {
			a.logger.Debug("ignored app_mention because ACP chat is disabled")
			return
		}
		if inner.BotID != "" {
			return
		}
		if !a.cfg.IsAllowedUser(inner.User) {
			a.logger.Debug("ignored app_mention from unauthorized user", "user", inner.User, "channel", inner.Channel)
			return
		}
		if a.isDuplicateEvent(eventsAPI.TeamID, inner.Channel, inner.TimeStamp) {
			a.logger.Info("ignored duplicate app_mention", "channel", inner.Channel, "ts", inner.TimeStamp)
			return
		}
		text := stripSlackMentions(inner.Text)
		a.startChat(context.Background(), ChatRequest{TeamID: eventsAPI.TeamID, ChannelID: inner.Channel, UserID: inner.User, ThreadTS: inner.ThreadTimeStamp, MessageTS: inner.TimeStamp, Text: text, Source: "app_mention"})
	case *slackevents.MessageEvent:
		if a.chat == nil {
			a.logger.Debug("ignored message because ACP chat is disabled")
			return
		}
		if inner.BotID != "" || inner.SubType != "" || inner.ChannelType != "im" {
			return
		}
		if !a.cfg.IsAllowedUser(inner.User) {
			a.logger.Debug("ignored DM from unauthorized user", "user", inner.User, "channel", inner.Channel)
			return
		}
		if a.isDuplicateEvent(eventsAPI.TeamID, inner.Channel, inner.TimeStamp) {
			a.logger.Info("ignored duplicate DM", "channel", inner.Channel, "ts", inner.TimeStamp)
			return
		}
		a.startChat(context.Background(), ChatRequest{TeamID: eventsAPI.TeamID, ChannelID: inner.Channel, UserID: inner.User, ThreadTS: inner.ThreadTimeStamp, MessageTS: inner.TimeStamp, Text: inner.Text, DM: true, Source: "dm"})
	default:
		a.logger.Debug("ignored Events API event", "inner_type", eventsAPI.InnerEvent.Type)
	}
}

func (a *Gateway) handleLinkShared(teamID string, inner *slackevents.LinkSharedEvent) {
	if a.unfurl == nil {
		a.logger.Debug("ignored link_shared because no unfurl-rules are configured")
		return
	}
	req := LinkSharedRequest{
		TeamID:    teamID,
		ChannelID: inner.Channel,
		UserID:    inner.User,
		MessageTS: inner.MessageTimeStamp,
		ThreadTS:  inner.ThreadTimeStamp,
		Links:     inner.Links,
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), a.unfurlTimeout)
		defer cancel()
		ctx = journal.WithCorrID(ctx, journal.NewCorrID("gw"))
		if err := a.unfurl.Handle(ctx, req); err != nil {
			a.logger.Error("link unfurl failed", "channel", inner.Channel, "error", err)
		}
	}()
}

// isDuplicateEvent reports whether a message event (identified by its
// immutable Slack timestamp) has already been handled. Slack may deliver the
// same event more than once; without this guard a redelivery spawns a second
// chat that interrupts the first. An empty timestamp or unconfigured dedup
// cache is never treated as a duplicate.
func (a *Gateway) isDuplicateEvent(teamID, channelID, ts string) bool {
	if ts == "" {
		return false
	}
	return a.recentEvents.seenBefore(teamID + "|" + channelID + "|" + ts)
}

// agentInterruptible reports whether the resolved agent supports interrupting
// an in-flight prompt. Unknown agents, and session managers that do not expose
// the capability, are treated as interruptible so behaviour is unchanged unless
// detection explicitly says otherwise.
func (a *Gateway) agentInterruptible(agent string) bool {
	sessions, ok := a.chatSessions[agent]
	if !ok {
		return true
	}
	checker, ok := sessions.(interface{ Interruptible() bool })
	if !ok {
		return true
	}
	return checker.Interruptible()
}

// followUpDeferredText is the thread note posted when a follow-up is held back
// because the agent cannot be interrupted.
const followUpDeferredText = ":hourglass_flowing_sand: Still working on your previous message — this agent can't be interrupted, so I'll finish that first before picking this up."

// notifyFollowUpDeferred posts a brief, best-effort thread note so a user whose
// follow-up was dropped (non-interruptible agent, response in flight) is not
// left wondering why nothing happened. Failures are logged, never propagated;
// a missing messaging surface (CLI/MCP, some tests) makes this a no-op.
func (a *Gateway) notifyFollowUpDeferred(parent context.Context, req ChatRequest) {
	if a.messaging == nil {
		return
	}
	threadTS := streamThreadTS(req)
	if threadTS == "" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	options := []slack.MsgOption{slack.MsgOptionText(followUpDeferredText, false), slack.MsgOptionTS(threadTS)}
	if _, _, err := a.messaging.PostMessageContext(ctx, req.ChannelID, options...); err != nil {
		a.logger.Warn("failed to post follow-up deferred note", "channel", req.ChannelID, "thread_ts", threadTS, "error", err)
	}
}

// startChat launches a chat goroutine for the request and wires it into
// the in-flight registry so subsequent messages on the same
// conversation interrupt the previous response, and so the /stop slash
// command can cancel it. The cancellation closure stored in the
// registry runs a two-step graceful-then-hard sequence: ask the ACP
// agent to cancel its prompt (best-effort, non-blocking) and, after
// cancelGrace, hard-cancel the chat goroutine's context. The grace
// window lets trailing chunks already on the wire flush as
// "_interrupted_" rather than vanish, which is what ChatHandler.Handle
// renders when it sees context.Canceled (vs DeadlineExceeded).
func (a *Gateway) startChat(parent context.Context, req ChatRequest) {
	key := conversationKey(req)
	agent := ""
	if a.chat != nil && a.chat.resolver != nil {
		agent = a.chat.resolver(req)
	}
	// When the agent cannot be interrupted (session/cancel unsupported), a
	// follow-up must neither cancel the in-flight response (the cancel is a
	// no-op at the agent and only yields a misleading "_interrupted_") nor run
	// concurrently against the same ACP session. Let the first finish; log and
	// drop the follow-up.
	if !a.agentInterruptible(agent) && a.inFlight.Active(key) {
		a.logger.Info("ignoring follow-up while a response is in flight; agent is not interruptible", "channel", req.ChannelID, "thread_ts", key.ThreadTS, "agent", agent)
		a.notifyFollowUpDeferred(parent, req)
		return
	}
	// No total wall-clock deadline here: a turn is bounded by inactivity inside
	// ChatHandler (WithIdleTimeout), so a long-but-progressing response is never
	// killed mid-flight. This context stays cancellable purely for the interrupt
	// and /stop paths.
	ctx, cancelCtx := context.WithCancel(parent)
	cancelFunc := a.buildInterruptCancel(key, agent, cancelCtx)
	_, previous := a.inFlight.Register(key, cancelFunc, agent)
	if previous != nil {
		a.logger.Info("interrupting previous in-flight chat", "channel", req.ChannelID, "thread_ts", key.ThreadTS, "agent", agent)
		previous()
	}
	go func() {
		defer cancelCtx()
		err := a.chat.Handle(ctx, req)
		a.inFlight.Cancel(key) // best-effort self-unregister; no-op if already replaced
		if err != nil {
			a.logger.Error("ACP chat failed", "source", req.Source, "channel", req.ChannelID, "error", err)
		}
	}()
}

// buildInterruptCancel returns the cancellation closure stored in the
// in-flight registry for one chat goroutine. It is invoked either by
// the next message on the same conversation (interrupt path) or by the
// /stop slash command. The sequence:
//
//  1. Look up the live ACP session ID for the conversation. If there is
//     one, fire a non-blocking session/cancel — it tells the agent to
//     stop generating but keeps the session alive for the follow-up.
//  2. Wait cancelGrace, then hard-cancel the chat goroutine's context.
//     The grace timer runs in its own goroutine so the registry call
//     returns immediately; the chat goroutine itself will see the
//     context cancellation and unwind through ChatHandler.Handle's
//     interrupted path.
//
// Resolution of agent name → session manager uses chatSessions, which
// the Gateway captured at construction time. When ACP is disabled the
// closure degenerates to a plain cancelCtx call.
func (a *Gateway) buildInterruptCancel(key acp.ConversationKey, agent string, cancelCtx context.CancelFunc) context.CancelFunc {
	return func() {
		go func() {
			if a.chatSessions != nil {
				if sessions, ok := a.chatSessions[agent]; ok {
					if sessionID, live := sessions.Lookup(key); live {
						cancelReqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						if err := sessions.Cancel(cancelReqCtx, sessionID); err != nil {
							a.logger.Warn("ACP session cancel failed", "agent", agent, "session_id", sessionID, "error", err)
						}
						cancel()
					}
				}
			}
			time.Sleep(a.cancelGrace)
			cancelCtx()
		}()
	}
}

func isChatSlashCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "chat")
}

// isStopSlashCommand recognises both the standalone `/stop` slash
// command (carried in command.Command) and the `<command> stop` verb
// form (carried in command.Text), so operators can wire either shape
// in the Slack app config. Matching is case-insensitive and tolerant
// of leading/trailing whitespace.
func isStopSlashCommand(command slack.SlashCommand) bool {
	if strings.EqualFold(strings.TrimSpace(command.Command), "/stop") {
		return true
	}
	fields := strings.Fields(command.Text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "stop")
}

// slashCommandThreadTS extracts thread_ts from the raw socketmode
// payload. Slack includes the field on slash command invocations made
// from inside a thread, but slack-go's SlashCommand struct does not
// surface it, so we re-parse the JSON. Returns "" when the field is
// absent (channel-root invocations and DMs), which the caller treats
// as a channel-scoped lookup.
func slashCommandThreadTS(event socketmode.Event) string {
	if event.Request == nil {
		return ""
	}
	var payload struct {
		ThreadTS string `json:"thread_ts"`
	}
	if err := json.Unmarshal(event.Request.Payload, &payload); err != nil {
		return ""
	}
	return payload.ThreadTS
}

// restartSourceSlash mirrors internal/app.RestartSourceSlash. It is
// duplicated here to keep gateway independent of the composition root
// (importing internal/app would cycle back). Compatibility is enforced by
// keeping both string values identical.
const restartSourceSlash = "slash"

func isRestartSlashCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "restart")
}

func slashChatPrompt(text string) string {
	fields := strings.Fields(text)
	if len(fields) <= 1 || !strings.EqualFold(fields[0], "chat") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), fields[0]))
}

func stripSlackMentions(text string) string {
	fields := strings.Fields(text)
	kept := fields[:0]
	for _, field := range fields {
		if strings.HasPrefix(field, "<@") && strings.HasSuffix(field, ">") {
			continue
		}
		kept = append(kept, field)
	}
	return strings.Join(kept, " ")
}

func (a *Gateway) ack(event socketmode.Event, response ...any) {
	if event.Request == nil {
		a.logger.Warn("cannot acknowledge event without request", "type", event.Type)
		return
	}
	if err := a.socket.Ack(*event.Request, response...); err != nil {
		a.logger.Error("failed to acknowledge Slack request", "error", err)
	}
}
