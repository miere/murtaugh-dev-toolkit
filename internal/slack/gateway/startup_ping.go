package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/miere/murtaugh-dev-toolkit/internal/slack/pingcard"
	"github.com/slack-go/slack"
)

type StartupNotifier interface {
	NotifyStartup(context.Context) error
}

type SlackAPI interface {
	GetUsersContext(context.Context, ...slack.GetUsersOption) ([]slack.User, error)
	OpenConversationContext(context.Context, *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error)
	PostMessageContext(context.Context, string, ...slack.MsgOption) (string, string, error)
}

type SlackStartupNotifier struct {
	api       SlackAPI
	adminUser string
	blocks    []slack.Block
	logger    *slog.Logger
}

func NewSlackStartupNotifier(api SlackAPI, adminUser string, logger *slog.Logger) (StartupNotifier, error) {
	if logger == nil {
		logger = slog.Default()
	}
	adminUser = strings.TrimSpace(adminUser)
	if adminUser == "" {
		logger.Warn("startup Slack ping disabled: configuration.admin_user is not set")
		return nil, nil
	}
	// The card is built in Go (internal/slack/pingcard) rather than read from a
	// template asset, so the startup ping and its button stay byte-stable and
	// untouchable by config/template edits.
	return &SlackStartupNotifier{api: api, adminUser: adminUser, blocks: pingcard.BuildStartup(), logger: logger}, nil
}

func (n *SlackStartupNotifier) NotifyStartup(ctx context.Context) error {
	userID, err := n.resolveAdminUserID(ctx)
	if err != nil {
		return err
	}
	channel, _, _, err := n.api.OpenConversationContext(ctx, &slack.OpenConversationParameters{Users: []string{userID}, ReturnIM: true})
	if err != nil {
		return fmt.Errorf("open admin DM: %w", err)
	}
	if channel == nil || channel.ID == "" {
		return fmt.Errorf("open admin DM: Slack returned no channel")
	}
	_, ts, err := n.api.PostMessageContext(ctx, channel.ID, slack.MsgOptionText(pingcard.StartupText, false), slack.MsgOptionBlocks(n.blocks...))
	if err != nil {
		return fmt.Errorf("post startup ping: %w", err)
	}
	n.logger.Info("sent Slack startup ping", "admin_user", n.adminUser, "channel", channel.ID, "ts", ts)
	return nil
}

func (n *SlackStartupNotifier) resolveAdminUserID(ctx context.Context) (string, error) {
	ids, err := resolveUserIDs(ctx, n.api, []string{n.adminUser})
	if err != nil {
		return "", fmt.Errorf("resolve configuration.admin_user: %w", err)
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("resolve configuration.admin_user %q: unexpected resolution result", n.adminUser)
	}
	return ids[0], nil
}

func looksLikeUserID(value string) bool {
	return len(value) > 3 && (strings.HasPrefix(value, "U") || strings.HasPrefix(value, "W"))
}

func slackUserMatchesHandle(user slack.User, handle string) bool {
	return strings.EqualFold(user.Name, handle) ||
		strings.EqualFold(user.Username, handle) ||
		strings.EqualFold(user.Profile.DisplayName, handle) ||
		strings.EqualFold(user.Profile.DisplayNameNormalized, handle)
}
