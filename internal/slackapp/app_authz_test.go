package slackapp

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

type recordingSlashHandler struct {
	calls    int
	lastUser string
}

func (r *recordingSlashHandler) HandleSlashCommand(_ context.Context, command slack.SlashCommand) (AckResponse, error) {
	r.calls++
	r.lastUser = command.UserID
	return AckResponse{ResponseType: "ephemeral", Text: "ok"}, nil
}

func TestResolveAllowSetMutatesConfigForHandlesAndIDs(t *testing.T) {
	api := &fakeUserDirectory{users: []slack.User{
		{ID: "UADMIN00", Name: "admin"},
		{ID: "UBOB0000", Profile: slack.UserProfile{DisplayNameNormalized: "bob"}},
	}}
	app := &App{
		api:    api,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: config.ConfigurationConfig{
			AdminUser:    "@admin",
			AllowedUsers: []string{"U0ALICE00", "bob"},
		},
	}
	if err := app.resolveAllowSet(context.Background()); err != nil {
		t.Fatalf("resolveAllowSet returned error: %v", err)
	}
	if app.cfg.AdminUser != "UADMIN00" {
		t.Fatalf("expected admin to be resolved to UADMIN00, got %q", app.cfg.AdminUser)
	}
	if got := app.cfg.AllowedUsers; len(got) != 2 || got[0] != "U0ALICE00" || got[1] != "UBOB0000" {
		t.Fatalf("expected allowed_users [U0ALICE00 UBOB0000], got %#v", got)
	}
	if api.calls != 1 {
		t.Fatalf("expected exactly one users.list call, got %d", api.calls)
	}
	if !app.cfg.IsAllowedUser("UADMIN00") || !app.cfg.IsAllowedUser("UBOB0000") || !app.cfg.IsAllowedUser("U0ALICE00") {
		t.Fatalf("expected admin and allowed users to pass IsAllowedUser: cfg=%#v", app.cfg)
	}
	if app.cfg.IsAllowedUser("UELSE000") {
		t.Fatalf("expected unrelated user to be denied")
	}
}

func TestResolveAllowSetNoOpWhenNothingConfigured(t *testing.T) {
	api := &fakeUserDirectory{}
	app := &App{api: api, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := app.resolveAllowSet(context.Background()); err != nil {
		t.Fatalf("resolveAllowSet returned error: %v", err)
	}
	if api.calls != 0 {
		t.Fatalf("expected no API call when nothing is configured, got %d", api.calls)
	}
	if app.cfg.IsAllowedUser("UANY0000") {
		t.Fatalf("expected lockdown: no user allowed when nothing is configured")
	}
}

func TestResolveAllowSetFailsClosedOnUnresolvable(t *testing.T) {
	api := &fakeUserDirectory{users: []slack.User{{ID: "UADMIN00", Name: "admin"}}}
	app := &App{
		api:    api,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:    config.ConfigurationConfig{AdminUser: "@admin", AllowedUsers: []string{"@ghost"}},
	}
	err := app.resolveAllowSet(context.Background())
	if err == nil {
		t.Fatal("expected error when handle cannot be resolved")
	}
	// Config must not be partially mutated: admin remains as configured.
	if app.cfg.AdminUser != "@admin" {
		t.Fatalf("expected admin to remain unchanged on resolution failure, got %q", app.cfg.AdminUser)
	}
}

func TestHandleSlashCommandDeniesUnauthorizedUser(t *testing.T) {
	handler := &recordingSlashHandler{}
	app := &App{
		handler: handler,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     config.ConfigurationConfig{AdminUser: "UADMIN00"},
	}
	app.handleSlashCommand(context.Background(), socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: slack.SlashCommand{Command: "/murtaugh", UserID: "UEVIL000", Text: "help"},
	})
	if handler.calls != 0 {
		t.Fatalf("expected unauthorized slash command to bypass handler, got %d calls", handler.calls)
	}
}

func TestHandleSlashCommandAllowsAdmin(t *testing.T) {
	handler := &recordingSlashHandler{}
	app := &App{
		handler: handler,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     config.ConfigurationConfig{AdminUser: "UADMIN00"},
	}
	app.handleSlashCommand(context.Background(), socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: slack.SlashCommand{Command: "/murtaugh", UserID: "UADMIN00", Text: "help"},
	})
	if handler.calls != 1 || handler.lastUser != "UADMIN00" {
		t.Fatalf("expected admin slash command to reach handler, got calls=%d user=%q", handler.calls, handler.lastUser)
	}
}

func TestAppMentionEventIgnoresUnauthorizedUser(t *testing.T) {
	sessions := &fakeChatSessions{}
	app := &App{
		chat:        NewChatHandler(&fakeStreamAPI{}, map[string]ChatSessionManager{"default": sessions}, func(ChatRequest) string { return "default" }, time.Hour, 1, nil),
		chatTimeout: time.Second,
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:         config.ConfigurationConfig{AllowedUsers: []string{"UALICE00"}},
	}
	app.handleEventsAPI(socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackevents.EventsAPIEvent{
		TeamID:     "T1",
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: string(slackevents.AppMention), Data: &slackevents.AppMentionEvent{User: "UEVIL000", Channel: "C1", Text: "<@UBOT> hello", TimeStamp: "123.4"}},
	}})
	time.Sleep(50 * time.Millisecond)
	if sessions.prompt != "" {
		t.Fatalf("expected unauthorized mention to be silently ignored, got prompt %q", sessions.prompt)
	}
}

func TestDMEventIgnoresUnauthorizedUser(t *testing.T) {
	sessions := &fakeChatSessions{}
	app := &App{
		chat:        NewChatHandler(&fakeStreamAPI{}, map[string]ChatSessionManager{"default": sessions}, func(ChatRequest) string { return "default" }, time.Hour, 1, nil),
		chatTimeout: time.Second,
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:         config.ConfigurationConfig{AllowedUsers: []string{"UALICE00"}},
	}
	app.handleEventsAPI(socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackevents.EventsAPIEvent{
		TeamID:     "T1",
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: string(slackevents.Message), Data: &slackevents.MessageEvent{User: "UEVIL000", Channel: "D1", ChannelType: "im", Text: "hello", TimeStamp: "123.4"}},
	}})
	time.Sleep(50 * time.Millisecond)
	if sessions.prompt != "" {
		t.Fatalf("expected unauthorized DM to be silently ignored, got prompt %q", sessions.prompt)
	}
}
