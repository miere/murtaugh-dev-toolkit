package slackapp

import (
	"context"
	"fmt"
	"strings"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/slack-go/slack"
)

type SlashCommandHandler interface {
	HandleSlashCommand(context.Context, slack.SlashCommand) (AckResponse, error)
}

type AckResponse struct {
	ResponseType string        `json:"response_type,omitempty"`
	Text         string        `json:"text,omitempty"`
	Blocks       []slack.Block `json:"blocks,omitempty"`
}

type DefaultSlashCommandHandler struct {
	commands map[string]config.CommandConfig
}

func NewDefaultSlashCommandHandler(commands []config.CommandConfig) *DefaultSlashCommandHandler {
	indexed := make(map[string]config.CommandConfig, len(commands))
	for _, command := range commands {
		indexed[command.Name] = command
	}
	return &DefaultSlashCommandHandler{commands: indexed}
}

func (h *DefaultSlashCommandHandler) HandleSlashCommand(_ context.Context, command slack.SlashCommand) (AckResponse, error) {
	if _, configured := h.commands[command.Command]; len(h.commands) > 0 && !configured {
		return ephemeralText(fmt.Sprintf("Command %s is not configured for Murtaugh.", command.Command)), nil
	}

	fields := strings.Fields(command.Text)
	if len(fields) == 0 {
		return h.help(command.Command), nil
	}

	verb := strings.ToLower(fields[0])
	if verb == "help" {
		return h.help(command.Command), nil
	}
	return ephemeralText(fmt.Sprintf("I do not know how to handle %q yet. Try `%s help`.", verb, command.Command)), nil
}

func (h *DefaultSlashCommandHandler) help(commandName string) AckResponse {
	verbs := fmt.Sprintf("• `%s chat <prompt>` — ask the configured ACP agent\n• `%s stop` — cancel the in-flight response in this thread or DM\n• `%s restart` — admin-only graceful restart\n• `%s help` — show this message", commandName, commandName, commandName, commandName)
	return AckResponse{
		ResponseType: "ephemeral",
		Text:         fmt.Sprintf("%s is connected.", commandName),
		Blocks: []slack.Block{
			slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, fmt.Sprintf("*%s is connected.*", commandName), false, false), nil, nil),
			slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, verbs, false, false), nil, nil),
		},
	}
}

func ephemeralText(text string) AckResponse {
	return AckResponse{ResponseType: "ephemeral", Text: text}
}
