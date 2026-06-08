package slackapp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/acp"
	"github.com/miere/murtaugh-dev-toolkit/internal/config"
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

// RestartTrigger is the function the App calls to request a graceful
// restart. The arguments mirror internal/app.RestartRequest field-by-field
// but stay stringly-typed so slackapp does not need to import the
// composition root (which would create a cycle). Returns true when the
// shutdown sequence has begun, false when the request was declined
// (already firing, within cool-down, or no coordinator is wired).
type RestartTrigger func(source, userID, channel, reason string) bool

type App struct {
	api             userDirectoryAPI
	socket          *socketmode.Client
	handler         SlashCommandHandler
	workflow        workflowDispatcher
	chat            *ChatHandler
	chatTimeout     time.Duration
	chatWarmTimeout time.Duration
	unfurl          *LinkUnfurlHandler
	unfurlTimeout   time.Duration
	startupNotifier StartupNotifier
	startupPingSent bool
	logger          *slog.Logger
	// cfg holds the configuration values consulted at runtime. Authz entries
	// (admin_user, allowed_users) start out as configured (IDs or handles) and
	// are mutated in place by resolveAllowSet at the start of Run so the rest
	// of the App can rely on ID-only comparisons via cfg.IsAllowedUser.
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
}

func New(cfg config.Config, logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.Default()
	}
	api := slack.New(cfg.OAuth.BotToken, slack.OptionAppLevelToken(cfg.OAuth.AppToken))
	socket := socketmode.New(api, socketmode.OptionDebug(cfg.Configuration.Debug))
	startupNotifier, err := NewSlackStartupNotifier(api, cfg.Configuration.AdminUser, logger)
	if err != nil {
		logger.Error("startup Slack ping disabled", "error", err)
	}
	var chat *ChatHandler
	if !cfg.ACP.Enabled {
		logger.Warn("ACP chat disabled: set acp.enabled: true in agents.yaml to enable DM and app_mention replies")
	}
	if cfg.ACP.Enabled {
		sessions := make(map[string]ChatSessionManager)
		for name, profile := range cfg.Agents {
			client := acp.NewProcessClient(acp.ProcessOptions{
				Command: profile.Command,
				Args:    profile.Args,
				WorkDir: profile.WorkDir,
				Logger:  logger,
			})
			sessions[name] = acp.NewSessionManager(
				client,
				cfg.ACP.EffectiveSessionIdleTimeout(),
				cfg.ACP.EffectiveMaxSessions(),
			).WithLogger(logger)
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
		)
	}
	var unfurlHandler *LinkUnfurlHandler
	if len(cfg.UnfurlRules) > 0 {
		matcher, err := unfurl.NewMatcher(cfg.UnfurlRules)
		if err != nil {
			logger.Error("custom link unfurling disabled", "error", err)
		} else {
			renderer := unfurl.NewRenderer(cfg.BaseDir, nil)
			unfurlHandler = NewLinkUnfurlHandler(matcher, renderer, nil, api, logger)
		}
	}
	return &App{
		api:             api,
		socket:          socket,
		handler:         NewDefaultSlashCommandHandler(cfg.Commands),
		workflow:        workflow.NewEngine(cfg, workflow.Options{Logger: logger}),
		chat:            chat,
		chatTimeout:     cfg.ACP.EffectiveRequestTimeout(),
		chatWarmTimeout: cfg.ACP.EffectiveStartupTimeout(),
		unfurl:          unfurlHandler,
		unfurlTimeout:   2 * time.Minute,
		startupNotifier: startupNotifier,
		logger:          logger,
		cfg:             cfg.Configuration,
		messaging:       api,
	}
}

// WithResumeMarkerStore attaches the persistent store used to bridge
// restart notices across process restarts. When nil (the default) the
// restart flow runs silently — the "restarting…" / "back online"
// confirmation messages are skipped.
func (a *App) WithResumeMarkerStore(store ResumeMarkerStore) *App {
	a.resumeStore = store
	return a
}

// WithRestartTrigger attaches the graceful-restart trigger and returns the
// receiver for fluent wiring at the composition root. When nil (the
// default) the restart slash verb reports the feature as unavailable.
func (a *App) WithRestartTrigger(trigger RestartTrigger) *App {
	a.restart = trigger
	return a
}

func (a *App) Run(ctx context.Context) error {
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

func (a *App) warmChat(ctx context.Context) {
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

func (a *App) handleEvent(ctx context.Context, event socketmode.Event) {
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
// empty the App is effectively locked down and direct interactions will be
// denied; a warning is logged in that case.
func (a *App) resolveAllowSet(ctx context.Context) error {
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

func (a *App) notifyStartup(ctx context.Context) {
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
func (a *App) notifyResume(ctx context.Context) {
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

func (a *App) handleInteractive(event socketmode.Event) {
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
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := a.workflow.Execute(ctx, interaction); err != nil {
			a.logger.Error("interactive workflow failed", "error", err)
		}
	}()
}

func (a *App) handleSlashCommand(ctx context.Context, event socketmode.Event) {
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

	response, err := a.handler.HandleSlashCommand(ctx, command)
	if isRestartSlashCommand(command.Text) {
		a.handleRestartSlashCommand(ctx, event, command)
		return
	}
	if isChatSlashCommand(command.Text) {
		a.handleChatSlashCommand(ctx, event, command)
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
func (a *App) handleRestartSlashCommand(ctx context.Context, event socketmode.Event, command slack.SlashCommand) {
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

func (a *App) handleChatSlashCommand(ctx context.Context, event socketmode.Event, command slack.SlashCommand) {
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

func (a *App) handleEventsAPI(event socketmode.Event) {
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
		a.startChat(context.Background(), ChatRequest{TeamID: eventsAPI.TeamID, ChannelID: inner.Channel, UserID: inner.User, ThreadTS: inner.ThreadTimeStamp, MessageTS: inner.TimeStamp, Text: inner.Text, DM: true, Source: "dm"})
	default:
		a.logger.Debug("ignored Events API event", "inner_type", eventsAPI.InnerEvent.Type)
	}
}

func (a *App) handleLinkShared(teamID string, inner *slackevents.LinkSharedEvent) {
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
		if err := a.unfurl.Handle(ctx, req); err != nil {
			a.logger.Error("link unfurl failed", "channel", inner.Channel, "error", err)
		}
	}()
}

func (a *App) startChat(parent context.Context, req ChatRequest) {
	go func() {
		ctx, cancel := context.WithTimeout(parent, a.chatTimeout)
		defer cancel()
		if err := a.chat.Handle(ctx, req); err != nil {
			a.logger.Error("ACP chat failed", "source", req.Source, "channel", req.ChannelID, "error", err)
		}
	}()
}

func isChatSlashCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "chat")
}

// restartSourceSlash mirrors internal/app.RestartSourceSlash. It is
// duplicated here to keep slackapp independent of the composition root
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

func (a *App) ack(event socketmode.Event, response ...any) {
	if event.Request == nil {
		a.logger.Warn("cannot acknowledge event without request", "type", event.Type)
		return
	}
	if err := a.socket.Ack(*event.Request, response...); err != nil {
		a.logger.Error("failed to acknowledge Slack request", "error", err)
	}
}
