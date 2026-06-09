package slackapp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/acp"
	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

func TestDefaultSlashCommandHandlerHelp(t *testing.T) {
	handler := NewDefaultSlashCommandHandler([]config.CommandConfig{{Name: "/murtaugh"}})
	response, err := handler.HandleSlashCommand(context.Background(), slack.SlashCommand{Command: "/murtaugh", Text: "help"})
	if err != nil {
		t.Fatalf("HandleSlashCommand returned error: %v", err)
	}
	if response.ResponseType != "ephemeral" {
		t.Fatalf("unexpected response type: %q", response.ResponseType)
	}
	if len(response.Blocks) == 0 || !strings.Contains(response.Text, "connected") {
		t.Fatalf("expected connected help response, got: %#v", response)
	}
}

func TestDefaultSlashCommandHandlerRejectsUnconfiguredCommand(t *testing.T) {
	handler := NewDefaultSlashCommandHandler([]config.CommandConfig{{Name: "/murtaugh"}})
	response, err := handler.HandleSlashCommand(context.Background(), slack.SlashCommand{Command: "/unknown"})
	if err != nil {
		t.Fatalf("HandleSlashCommand returned error: %v", err)
	}
	if !strings.Contains(response.Text, "not configured") {
		t.Fatalf("expected not configured response, got: %#v", response)
	}
}

func TestIsChatSlashCommand(t *testing.T) {
	if !isChatSlashCommand("chat hello") || !isChatSlashCommand("CHAT hello") {
		t.Fatal("expected chat slash command to be recognized")
	}
	if slashChatPrompt("CHAT hello") != "hello" {
		t.Fatalf("unexpected chat prompt: %q", slashChatPrompt("CHAT hello"))
	}
	if isChatSlashCommand("help") {
		t.Fatal("did not expect help to be recognized as chat")
	}
}

func TestIsRestartSlashCommand(t *testing.T) {
	for _, text := range []string{"restart", "RESTART", "Restart now", "  restart  "} {
		if !isRestartSlashCommand(text) {
			t.Errorf("expected %q to be recognized as restart", text)
		}
	}
	for _, text := range []string{"", "help", "chat restart", "restartx"} {
		if isRestartSlashCommand(text) {
			t.Errorf("did not expect %q to be recognized as restart", text)
		}
	}
}

func TestIsStopSlashCommand(t *testing.T) {
	cases := []struct {
		name    string
		command slack.SlashCommand
		want    bool
	}{
		{"standalone /stop", slack.SlashCommand{Command: "/stop"}, true},
		{"standalone /stop case-insensitive", slack.SlashCommand{Command: "/STOP"}, true},
		{"verb form lower", slack.SlashCommand{Command: "/murtaugh", Text: "stop"}, true},
		{"verb form upper", slack.SlashCommand{Command: "/murtaugh", Text: "STOP"}, true},
		{"verb form with trailing args", slack.SlashCommand{Command: "/murtaugh", Text: "stop now"}, true},
		{"chat is not stop", slack.SlashCommand{Command: "/murtaugh", Text: "chat hello"}, false},
		{"help is not stop", slack.SlashCommand{Command: "/murtaugh", Text: "help"}, false},
		{"empty text on non-/stop command is not stop", slack.SlashCommand{Command: "/murtaugh"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStopSlashCommand(tc.command); got != tc.want {
				t.Fatalf("isStopSlashCommand(%+v) = %v, want %v", tc.command, got, tc.want)
			}
		})
	}
}

func TestSlashCommandThreadTSExtractsFromPayload(t *testing.T) {
	payload, err := json.Marshal(map[string]string{"thread_ts": "1700000000.000100"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	event := socketmode.Event{Request: &socketmode.Request{Payload: payload}}
	if got := slashCommandThreadTS(event); got != "1700000000.000100" {
		t.Fatalf("expected thread_ts extracted from payload, got %q", got)
	}
}

func TestSlashCommandThreadTSEmptyWhenAbsent(t *testing.T) {
	payload := []byte(`{"channel_id":"C1"}`)
	event := socketmode.Event{Request: &socketmode.Request{Payload: payload}}
	if got := slashCommandThreadTS(event); got != "" {
		t.Fatalf("expected empty thread_ts when payload omits it, got %q", got)
	}
}

func TestSlashCommandThreadTSEmptyWhenNoRequest(t *testing.T) {
	if got := slashCommandThreadTS(socketmode.Event{}); got != "" {
		t.Fatalf("expected empty thread_ts when event has no request, got %q", got)
	}
}

func TestHandleStopSlashCommandCancelsRegisteredChat(t *testing.T) {
	app := &App{
		inFlight: NewInFlightRegistry(),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	key := acp.ConversationKey{TeamID: "T1", ChannelID: "C1", ThreadTS: "1700000000.000100"}
	cancelled := false
	app.inFlight.Register(key, func() { cancelled = true }, "default")
	// event.Request left nil so a.ack short-circuits (we exercise the
	// registry side of the contract; the ack pipeline is covered by the
	// existing slash command tests).
	app.handleStopSlashCommand(socketmode.Event{}, slack.SlashCommand{TeamID: "T1", ChannelID: "C1", UserID: "U1", Command: "/stop"}, "1700000000.000100")

	if !cancelled {
		t.Fatalf("expected cancel func to be invoked")
	}
	if app.inFlight.Len() != 0 {
		t.Fatalf("expected registry to drop the entry after Cancel, got Len=%d", app.inFlight.Len())
	}
}

func TestHandleStopSlashCommandNoOpWhenNothingRegistered(t *testing.T) {
	app := &App{
		inFlight: NewInFlightRegistry(),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	app.handleStopSlashCommand(socketmode.Event{}, slack.SlashCommand{TeamID: "T1", ChannelID: "C1", UserID: "U1", Command: "/stop"}, "")
	if app.inFlight.Len() != 0 {
		t.Fatalf("registry must remain empty after no-op /stop, got Len=%d", app.inFlight.Len())
	}
}

func TestHandleStopSlashCommandUsesDMKeyForDMChannels(t *testing.T) {
	app := &App{
		inFlight: NewInFlightRegistry(),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	dmKey := acp.ConversationKey{TeamID: "T1", ChannelID: "D1", DM: true}
	cancelled := false
	app.inFlight.Register(dmKey, func() { cancelled = true }, "default")
	// No thread_ts, channel starts with "D" → DM key.
	app.handleStopSlashCommand(socketmode.Event{}, slack.SlashCommand{TeamID: "T1", ChannelID: "D1", UserID: "U1", Command: "/stop"}, "")
	if !cancelled {
		t.Fatalf("expected DM cancel to be invoked")
	}
}

func TestDefaultSlashCommandHandlerHelpMentionsRestart(t *testing.T) {
	handler := NewDefaultSlashCommandHandler([]config.CommandConfig{{Name: "/murtaugh"}})
	response, err := handler.HandleSlashCommand(context.Background(), slack.SlashCommand{Command: "/murtaugh", Text: "help"})
	if err != nil {
		t.Fatalf("HandleSlashCommand returned error: %v", err)
	}
	rendered := response.Text
	for _, block := range response.Blocks {
		if section, ok := block.(*slack.SectionBlock); ok && section.Text != nil {
			rendered += "\n" + section.Text.Text
		}
	}
	if !strings.Contains(rendered, "restart") {
		t.Fatalf("expected help text to mention restart verb, got: %q", rendered)
	}
}
