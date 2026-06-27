// Package fetchreactions implements the `slack.fetch-reactions` tool: fetch
// the messages in a channel that a given user reacted to with a given emoji.
package fetchreactions

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

// HistoryLimit caps the history pull.
const HistoryLimit = 100

// Tool is the `slack.fetch-reactions` capability.
type Tool struct {
	client *slacklib.LazyClient
}

// New constructs a Tool with a SlackClient built lazily from the given bot
// token (sourced from oauth.bot_token in gateway.yaml).
func New(token string) *Tool {
	return &Tool{client: slacklib.NewLazyClient(token)}
}

// NewWith constructs a Tool against the given LazyClient. Used by tests.
func NewWith(client *slacklib.LazyClient) *Tool {
	return &Tool{client: client}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "slack.fetch-reactions" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Fetch messages a specific user reacted to with a given emoji."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"from":    {Type: "string", Description: "User handle (with or without @)."},
			"emoji":   {Type: "string", Description: "Emoji name (with or without colons), e.g. thumbsup or :thumbsup:."},
			"channel": {Type: "string", Description: "Channel name (with or without #) or channel ID."},
			"since":   {Type: "string", Description: "Exclude messages sent before this Sydney datetime (YYYY-MM-DD HH:mm:ss). Default: 24h ago."},
		},
		Required: []string{"from", "emoji", "channel"},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	Channel  string             `json:"channel"`
	Messages []slacklib.Message `json:"messages"`
}

// String renders the filtered messages as oldest-first lines.
func (r Result) String() string { return slacklib.FormatMessages(r.Messages) }

// Invoke runs the tool. It resolves --channel and --from, fetches recent
// history, then filters down to messages that carry a reaction matching
// --emoji (colons stripped) authored in part by the resolved user.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	from, _ := args["from"].(string)
	emoji, _ := args["emoji"].(string)
	channel, _ := args["channel"].(string)
	since, _ := args["since"].(string)

	if from == "" {
		return nil, fmt.Errorf("Error: --from is required")
	}
	if emoji == "" {
		return nil, fmt.Errorf("Error: --emoji is required")
	}
	if channel == "" {
		return nil, fmt.Errorf("Error: --channel is required")
	}
	emojiName := strings.Trim(emoji, ":")

	api, err := t.client.Get()
	if err != nil {
		return nil, err
	}

	channelID, err := slacklib.ResolveChannel(ctx, api, channel)
	if err != nil {
		return nil, err
	}
	userID, err := slacklib.ResolveUser(ctx, api, from)
	if err != nil {
		return nil, err
	}

	sinceTime, err := slacklib.ParseSince(since)
	if err != nil {
		return nil, err
	}
	msgs, err := api.GetHistory(ctx, channelID, slacklib.SlackTS(sinceTime), HistoryLimit)
	if err != nil {
		return nil, err
	}

	filtered := make([]slacklib.Message, 0, len(msgs))
	for _, m := range msgs {
		if reactionMatch(m, emojiName, userID) {
			filtered = append(filtered, m)
		}
	}
	return Result{Channel: channelID, Messages: slacklib.ReverseMessages(filtered)}, nil
}

// reactionMatch reports whether message m carries a reaction named emojiName
// that the user userID is among the reactors of.
func reactionMatch(m slacklib.Message, emojiName, userID string) bool {
	for _, r := range m.Reactions {
		if r.Name != emojiName {
			continue
		}
		for _, u := range r.Users {
			if u == userID {
				return true
			}
		}
	}
	return false
}
