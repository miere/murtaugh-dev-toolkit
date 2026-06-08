package slackapp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

const (
	// restartSuggestionBlockID tags the actions block that carries the
	// confirm/dismiss buttons. The router uses it (and the action_id
	// prefix below) to identify suggestion callbacks without depending
	// on free-form text.
	restartSuggestionBlockID = "murtaugh_restart_suggestion"

	restartSuggestionActionPrefix  = "murtaugh_restart_suggestion_"
	restartSuggestionActionConfirm = "murtaugh_restart_suggestion_confirm"
	restartSuggestionActionDismiss = "murtaugh_restart_suggestion_dismiss"

	// restartSourceInteractive mirrors internal/app.RestartSourceInteractive.
	// Duplicated here so slackapp can stay independent of the composition
	// root (importing internal/app would cycle); the values are kept
	// identical by convention.
	restartSourceInteractive = "interactive"

	restartSuggestionHeadline     = ":warning: Murtaugh thinks a restart might help."
	restartSuggestionConfirmedFmt = ":arrows_counterclockwise: Restart confirmed by <@%s>."
	restartSuggestionDismissedFmt = ":no_entry_sign: Restart suggestion dismissed by <@%s>."
	restartSuggestionDenied       = ":lock: Only the configured admin can restart Murtaugh."
	restartSuggestionUnavailable  = ":lock: Restart is not available in this deployment."
	restartSuggestionBusy         = ":hourglass_flowing_sand: A restart is already in progress (or the cool-down has not elapsed)."

	restartSuggestionDefaultReason = "Murtaugh detected a condition that may be resolved by a restart."
)

// BuildRestartSuggestion returns the Block Kit layout used by
// SuggestRestart. The reason is rendered as a context line so the
// operator knows why the bot is suggesting the restart; the two
// buttons carry stable action_ids consumed by the interactive handler.
func BuildRestartSuggestion(reason string) []slack.Block {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = restartSuggestionDefaultReason
	}
	confirm := slack.NewButtonBlockElement(
		restartSuggestionActionConfirm,
		reason,
		slack.NewTextBlockObject(slack.PlainTextType, "Restart now", false, false),
	)
	confirm.Style = slack.StylePrimary
	dismiss := slack.NewButtonBlockElement(
		restartSuggestionActionDismiss,
		reason,
		slack.NewTextBlockObject(slack.PlainTextType, "Dismiss", false, false),
	)
	return []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, restartSuggestionHeadline, false, false), nil, nil),
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, reason, false, false)),
		slack.NewActionBlock(restartSuggestionBlockID, confirm, dismiss),
	}
}

// SuggestRestart posts a restart-suggestion Block Kit message and
// returns the (channel, timestamp) of the posted message. When channel
// is empty, the admin user's DM is opened and used instead. Returns
// an error only when Slack fails; missing surfaces (no messaging
// client, no channel + no admin) are tolerated as silent no-ops so
// the caller can treat the suggestion as best-effort.
func (a *App) SuggestRestart(ctx context.Context, channel, reason string) (string, string, error) {
	if a.messaging == nil {
		a.logger.Debug("restart suggestion skipped: no Slack messaging wired")
		return "", "", nil
	}
	destination, err := a.resolveSuggestionDestination(ctx, channel)
	if err != nil {
		return "", "", err
	}
	if destination == "" {
		a.logger.Debug("restart suggestion skipped: no destination channel or admin DM available")
		return "", "", nil
	}
	blocks := BuildRestartSuggestion(reason)
	postedChannel, ts, err := a.messaging.PostMessageContext(ctx, destination,
		slack.MsgOptionText(restartSuggestionHeadline, false),
		slack.MsgOptionBlocks(blocks...),
	)
	if err != nil {
		return "", "", fmt.Errorf("post restart suggestion: %w", err)
	}
	a.logger.Info("posted restart suggestion", "channel", postedChannel, "ts", ts, "reason", reason)
	return postedChannel, ts, nil
}

// resolveSuggestionDestination returns the channel ID for the
// suggestion post. An explicit channel always wins; otherwise the
// admin user's DM is opened. Returns ("", nil) when neither is
// available — the suggestion is then silently skipped.
func (a *App) resolveSuggestionDestination(ctx context.Context, channel string) (string, error) {
	if channel != "" {
		return channel, nil
	}
	admin := strings.TrimSpace(a.cfg.AdminUser)
	if admin == "" {
		return "", nil
	}
	convo, _, _, err := a.messaging.OpenConversationContext(ctx, &slack.OpenConversationParameters{Users: []string{admin}, ReturnIM: true})
	if err != nil {
		return "", fmt.Errorf("open admin DM for restart suggestion: %w", err)
	}
	if convo == nil || convo.ID == "" {
		return "", fmt.Errorf("open admin DM for restart suggestion: Slack returned no channel")
	}
	return convo.ID, nil
}

// isRestartSuggestionInteraction reports whether the supplied
// interaction is a click on one of the restart-suggestion buttons.
// The router uses this to dispatch the callback before the workflow
// engine sees it.
func isRestartSuggestionInteraction(interaction slack.InteractionCallback) bool {
	if interaction.Type != slack.InteractionTypeBlockActions {
		return false
	}
	for _, action := range interaction.ActionCallback.BlockActions {
		if action == nil {
			continue
		}
		if strings.HasPrefix(action.ActionID, restartSuggestionActionPrefix) {
			return true
		}
		if action.BlockID == restartSuggestionBlockID {
			return true
		}
	}
	return false
}

// firstRestartSuggestionAction returns the (action_id, value) of the
// first action belonging to the restart-suggestion namespace. The
// value carries the audit reason that was embedded when the suggestion
// was posted. Returns ("", "") when none is present.
func firstRestartSuggestionAction(interaction slack.InteractionCallback) (string, string) {
	for _, action := range interaction.ActionCallback.BlockActions {
		if action == nil {
			continue
		}
		if strings.HasPrefix(action.ActionID, restartSuggestionActionPrefix) {
			return action.ActionID, action.Value
		}
	}
	return "", ""
}

// handleRestartSuggestionInteraction processes a click on one of the
// restart-suggestion buttons. Two-layer authz: handleInteractive has
// already verified IsAllowedUser; this method additionally requires
// IsAdminUser before triggering the coordinator. The suggestion message
// is edited in place for every terminal outcome so the operator sees a
// final state instead of a stale prompt.
func (a *App) handleRestartSuggestionInteraction(ctx context.Context, interaction slack.InteractionCallback) {
	actionID, reason := firstRestartSuggestionAction(interaction)
	if actionID == "" {
		a.logger.Warn("restart suggestion interaction with no recognized action",
			"user", interaction.User.ID, "channel", interaction.Channel.ID)
		return
	}
	channel := interaction.Channel.ID
	messageTS := interaction.Message.Timestamp
	if channel == "" || messageTS == "" || a.messaging == nil {
		a.logger.Warn("restart suggestion interaction missing context",
			"channel", channel, "ts", messageTS, "messaging_wired", a.messaging != nil)
		return
	}
	switch actionID {
	case restartSuggestionActionDismiss:
		a.editSuggestion(ctx, channel, messageTS, fmt.Sprintf(restartSuggestionDismissedFmt, interaction.User.ID))
		a.logger.Info("restart suggestion dismissed", "user", interaction.User.ID, "channel", channel, "ts", messageTS)
	case restartSuggestionActionConfirm:
		a.handleRestartSuggestionConfirm(ctx, interaction, channel, messageTS, reason)
	default:
		a.logger.Warn("restart suggestion interaction with unexpected action",
			"action_id", actionID, "user", interaction.User.ID)
	}
}

// handleRestartSuggestionConfirm runs the admin-gated confirm branch.
// On accept it mirrors the slash path: post-and-save the resume
// marker first (so it survives the imminent exit), then call the
// coordinator. On any failure the suggestion is edited to reflect the
// outcome (denied / unavailable / busy) and the coordinator is not
// touched.
func (a *App) handleRestartSuggestionConfirm(ctx context.Context, interaction slack.InteractionCallback, channel, messageTS, reason string) {
	user := interaction.User.ID
	if !a.cfg.IsAdminUser(user) {
		a.logger.Info("denied restart suggestion confirm from non-admin", "user", user, "channel", channel)
		a.editSuggestion(ctx, channel, messageTS, restartSuggestionDenied)
		return
	}
	if a.restart == nil {
		a.logger.Warn("restart suggestion confirm received but no coordinator is wired", "user", user, "channel", channel)
		a.editSuggestion(ctx, channel, messageTS, restartSuggestionUnavailable)
		return
	}
	auditReason := strings.TrimSpace(reason)
	if auditReason == "" {
		auditReason = "user confirmed restart suggestion"
	}
	noticeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	a.postRestartNoticeAndSaveMarker(noticeCtx, channel, "", user, restartSourceInteractive, auditReason)
	cancel()
	if !a.restart(restartSourceInteractive, user, channel, auditReason) {
		a.editSuggestion(ctx, channel, messageTS, restartSuggestionBusy)
		return
	}
	a.editSuggestion(ctx, channel, messageTS, fmt.Sprintf(restartSuggestionConfirmedFmt, user))
}

// editSuggestion replaces the original suggestion's text and clears
// its blocks. Best-effort: a failure is logged but not retried since
// the underlying restart decision has already been made.
func (a *App) editSuggestion(ctx context.Context, channel, ts, text string) {
	if a.messaging == nil || channel == "" || ts == "" {
		return
	}
	if _, _, _, err := a.messaging.UpdateMessageContext(ctx, channel, ts,
		slack.MsgOptionText(text, false),
		slack.MsgOptionBlocks(),
	); err != nil {
		a.logger.Error("update restart suggestion failed", "channel", channel, "ts", ts, "error", err)
	}
}
