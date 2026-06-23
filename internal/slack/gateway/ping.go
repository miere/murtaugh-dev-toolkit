package gateway

import (
	"context"

	"github.com/miere/murtaugh-dev-toolkit/internal/slack/pingcard"
	"github.com/slack-go/slack"
)

// isPingInteraction reports whether the callback is a click on the built-in
// "Test communication" button. handleInteractive dispatches it here — in the
// binary — before the workflow engine, so the ping → pong round-trip can never
// be redirected or broken by a configured workflow rule or an on-disk template.
func isPingInteraction(interaction slack.InteractionCallback) bool {
	if interaction.Type != slack.InteractionTypeBlockActions {
		return false
	}
	for _, action := range interaction.ActionCallback.BlockActions {
		if action == nil {
			continue
		}
		if action.ActionID == pingcard.ActionPing || action.BlockID == pingcard.BlockID {
			return true
		}
	}
	return false
}

// handlePingInteraction posts the pong reply in the clicked card's thread. The
// reply text is a Go constant and is sent directly over the Slack messaging
// surface — no template, no response_url, no workflow engine — so the self-test
// remains functional regardless of configuration state.
//
// The pong is threaded under the conversation root so it reads as a reply to
// the card: a card that is itself a threaded reply (the post-restart back-online
// notice) already carries ThreadTimestamp; a top-level card (the startup ping)
// does not, so its own ts is used.
func (a *Gateway) handlePingInteraction(ctx context.Context, interaction slack.InteractionCallback) {
	if a.messaging == nil {
		a.logger.Debug("ping interaction skipped: no Slack messaging wired")
		return
	}
	channel := interaction.Channel.ID
	if channel == "" {
		a.logger.Warn("ping interaction missing channel", "user", interaction.User.ID)
		return
	}
	threadTS := interaction.Message.ThreadTimestamp
	if threadTS == "" {
		threadTS = interaction.Message.Timestamp
	}
	options := []slack.MsgOption{slack.MsgOptionText(pingcard.PongText, false)}
	if threadTS != "" {
		options = append(options, slack.MsgOptionTS(threadTS))
	}
	if _, _, err := a.messaging.PostMessageContext(ctx, channel, options...); err != nil {
		a.logger.Error("post pong reply failed", "channel", channel, "ts", threadTS, "error", err)
		return
	}
	a.logger.Info("posted pong reply", "channel", channel, "ts", threadTS, "user", interaction.User.ID)
}
