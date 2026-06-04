package slackapp

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/slack-go/slack"
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
