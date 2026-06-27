package gateway

import (
	"context"
	"fmt"
	"strings"

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

// DefaultSlashCommandHandler renders help and the unknown-verb fallback. The
// real verbs (chat/stop/restart/troubleshoot) are dispatched ahead of this
// handler in gateway.handleSlashCommand by hardcoded predicates, so this type
// owns no config — Slack-side registration lives in the Slack app manifest.
type DefaultSlashCommandHandler struct{}

func NewDefaultSlashCommandHandler() *DefaultSlashCommandHandler {
	return &DefaultSlashCommandHandler{}
}

func (h *DefaultSlashCommandHandler) HandleSlashCommand(_ context.Context, command slack.SlashCommand) (AckResponse, error) {
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
	verbs := fmt.Sprintf("• `%s chat <prompt>` — ask the configured agent\n• `%s stop` — cancel the in-flight response in this thread or DM\n• `%s troubleshoot [symptom]` — collect a diagnostics bundle\n• `%s restart` — admin-only graceful restart\n• `%s help` — show this message", commandName, commandName, commandName, commandName, commandName)
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
