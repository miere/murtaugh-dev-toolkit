package slackapp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/acp"
	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/miere/murtaugh-dev-toolkit/internal/workflow"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

type App struct {
	api             *slack.Client
	socket          *socketmode.Client
	handler         SlashCommandHandler
	workflow        *workflow.Engine
	chat            *ChatHandler
	chatTimeout     time.Duration
	chatWarmTimeout time.Duration
	startupNotifier StartupNotifier
	startupPingSent bool
	logger          *slog.Logger
}

func New(cfg config.Config, logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.Default()
	}
	api := slack.New(cfg.Slack.BotToken, slack.OptionAppLevelToken(cfg.Slack.AppToken))
	socket := socketmode.New(api, socketmode.OptionDebug(cfg.Slack.Debug))
	startupNotifier, err := NewSlackStartupNotifier(api, cfg.Slack.AdminUser, logger)
	if err != nil {
		logger.Error("startup Slack ping disabled", "error", err)
	}
	var chat *ChatHandler
	if cfg.ACP.Enabled {
		client := acp.NewProcessClient(acp.ProcessOptions{Command: cfg.ACP.Command, Args: cfg.ACP.Args, WorkDir: cfg.ACP.WorkDir, Logger: logger})
		sessions := acp.NewSessionManager(client, cfg.ACP.EffectiveSessionIdleTimeout(), cfg.ACP.EffectiveMaxSessions()).WithLogger(logger)
		chat = NewChatHandler(api, sessions, cfg.ACP.EffectiveStreamAppendInterval(), cfg.ACP.EffectiveStreamMinChunkChars(), logger)
	}
	return &App{
		api:             api,
		socket:          socket,
		handler:         NewDefaultSlashCommandHandler(cfg.Commands),
		workflow:        workflow.NewEngine(cfg, workflow.Options{Logger: logger}),
		chat:            chat,
		chatTimeout:     cfg.ACP.EffectiveRequestTimeout(),
		chatWarmTimeout: cfg.ACP.EffectiveStartupTimeout(),
		startupNotifier: startupNotifier,
		logger:          logger,
	}
}

func (a *App) Run(ctx context.Context) error {
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

func (a *App) handleInteractive(event socketmode.Event) {
	interaction, ok := event.Data.(slack.InteractionCallback)
	if !ok {
		a.ack(event)
		a.logger.Warn("unexpected interactive payload", "type", fmt.Sprintf("%T", event.Data))
		return
	}

	a.ack(event)
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

	response, err := a.handler.HandleSlashCommand(ctx, command)
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
	if a.chat == nil {
		a.logger.Debug("ignored Events API chat event because ACP chat is disabled", "inner_type", eventsAPI.InnerEvent.Type)
		return
	}
	switch inner := eventsAPI.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		if inner.BotID != "" {
			return
		}
		text := stripSlackMentions(inner.Text)
		a.startChat(context.Background(), ChatRequest{TeamID: eventsAPI.TeamID, ChannelID: inner.Channel, UserID: inner.User, ThreadTS: inner.ThreadTimeStamp, MessageTS: inner.TimeStamp, Text: text, Source: "app_mention"})
	case *slackevents.MessageEvent:
		if inner.BotID != "" || inner.SubType != "" || inner.ChannelType != "im" {
			return
		}
		a.startChat(context.Background(), ChatRequest{TeamID: eventsAPI.TeamID, ChannelID: inner.Channel, UserID: inner.User, ThreadTS: inner.ThreadTimeStamp, MessageTS: inner.TimeStamp, Text: inner.Text, DM: true, Source: "dm"})
	default:
		a.logger.Debug("ignored Events API event", "inner_type", eventsAPI.InnerEvent.Type)
	}
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
