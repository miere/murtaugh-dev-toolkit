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

type recordingWorkflow struct {
	calls    int
	lastUser string
}

func (r *recordingWorkflow) Execute(_ context.Context, interaction slack.InteractionCallback) error {
	r.calls++
	r.lastUser = interaction.User.ID
	return nil
}

func TestHandleInteractiveIgnoresUnauthorizedUser(t *testing.T) {
	wf := &recordingWorkflow{}
	app := &App{
		workflow: wf,
		socket:   nil, // a.ack is a no-op when socket is nil (see app.go)
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:      config.ConfigurationConfig{AllowedUsers: []string{"UALICE00"}},
	}
	app.handleInteractive(socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: slack.InteractionCallback{
			User:    slack.User{ID: "UEVIL000"},
			Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C1"}}},
		},
	})
	time.Sleep(50 * time.Millisecond)
	if wf.calls != 0 {
		t.Fatalf("expected unauthorized interactive callback to bypass workflow, got %d calls", wf.calls)
	}
}

func TestHandleInteractiveAllowsAllowlistedUser(t *testing.T) {
	wf := &recordingWorkflow{}
	app := &App{
		workflow: wf,
		socket:   nil,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:      config.ConfigurationConfig{AllowedUsers: []string{"UALICE00"}},
	}
	app.handleInteractive(socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: slack.InteractionCallback{
			User:    slack.User{ID: "UALICE00"},
			Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C1"}}},
		},
	})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if wf.calls > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if wf.calls != 1 || wf.lastUser != "UALICE00" {
		t.Fatalf("expected allowlisted interactive callback to reach workflow, got calls=%d user=%q", wf.calls, wf.lastUser)
	}
}

type recordingRestart struct {
	calls       int
	lastSource  string
	lastUser    string
	lastChannel string
	lastReason  string
	accept      bool
}

func (r *recordingRestart) trigger(source, userID, channel, reason string) bool {
	r.calls++
	r.lastSource = source
	r.lastUser = userID
	r.lastChannel = channel
	r.lastReason = reason
	return r.accept
}

func TestHandleSlashCommandRestartDeniesNonAdmin(t *testing.T) {
	restart := &recordingRestart{accept: true}
	app := &App{
		handler: &recordingSlashHandler{},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     config.ConfigurationConfig{AdminUser: "UADMIN00", AllowedUsers: []string{"UALICE00"}},
		restart: restart.trigger,
	}
	app.handleSlashCommand(context.Background(), socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: slack.SlashCommand{Command: "/murtaugh", UserID: "UALICE00", ChannelID: "C1", Text: "restart"},
	})
	if restart.calls != 0 {
		t.Fatalf("expected non-admin restart request to bypass coordinator, got %d calls", restart.calls)
	}
}

func TestHandleSlashCommandRestartFiresCoordinatorForAdmin(t *testing.T) {
	restart := &recordingRestart{accept: true}
	app := &App{
		handler: &recordingSlashHandler{},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     config.ConfigurationConfig{AdminUser: "UADMIN00"},
		restart: restart.trigger,
	}
	app.handleSlashCommand(context.Background(), socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: slack.SlashCommand{Command: "/murtaugh", UserID: "UADMIN00", ChannelID: "C1", Text: "restart"},
	})
	if restart.calls != 1 {
		t.Fatalf("expected admin restart to fire coordinator once, got %d calls", restart.calls)
	}
	if restart.lastSource != "slash" || restart.lastUser != "UADMIN00" || restart.lastChannel != "C1" {
		t.Fatalf("unexpected restart payload: source=%q user=%q channel=%q reason=%q",
			restart.lastSource, restart.lastUser, restart.lastChannel, restart.lastReason)
	}
	if restart.lastReason == "" {
		t.Fatal("expected restart reason to be populated for audit log")
	}
}

func TestHandleSlashCommandRestartUnavailableWhenTriggerMissing(t *testing.T) {
	handler := &recordingSlashHandler{}
	app := &App{
		handler: handler,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     config.ConfigurationConfig{AdminUser: "UADMIN00"},
	}
	app.handleSlashCommand(context.Background(), socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: slack.SlashCommand{Command: "/murtaugh", UserID: "UADMIN00", ChannelID: "C1", Text: "restart"},
	})
	// The default handler still runs (it produces the "I do not know restart"
	// fallback), but the restart branch intercepts before the ack and must
	// not panic when the trigger is nil.
	if handler.calls != 1 {
		t.Fatalf("expected default handler to be invoked once, got %d", handler.calls)
	}
}

func TestHandleSlashCommandRestartSurfacesCooldown(t *testing.T) {
	restart := &recordingRestart{accept: false}
	app := &App{
		handler: &recordingSlashHandler{},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     config.ConfigurationConfig{AdminUser: "UADMIN00"},
		restart: restart.trigger,
	}
	app.handleSlashCommand(context.Background(), socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: slack.SlashCommand{Command: "/murtaugh", UserID: "UADMIN00", ChannelID: "C1", Text: "restart"},
	})
	if restart.calls != 1 {
		t.Fatalf("expected coordinator to be consulted exactly once, got %d", restart.calls)
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
