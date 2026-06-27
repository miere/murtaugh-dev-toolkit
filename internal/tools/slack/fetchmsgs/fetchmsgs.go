// Package fetchmsgs implements the `slack.fetch-msgs` tool: fetch a channel
// or thread's messages, oldest-first, optionally filtered by --since (Sydney
// time, default 24h ago).
package fetchmsgs

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

// HistoryLimit caps the number of messages fetched from conversations.history.
const HistoryLimit = 100

// Tool is the `slack.fetch-msgs` capability.
type Tool struct {
	client *slacklib.LazyClient
}

// New constructs a Tool with a SlackClient built lazily from the given bot
// token (sourced from oauth.bot_token in gateway.yaml).
func New(token string) *Tool {
	return &Tool{client: slacklib.NewLazyClient(token)}
}

// NewWith constructs a Tool against the given LazyClient. Tests use this to
// inject a fake SlackAPI.
func NewWith(client *slacklib.LazyClient) *Tool {
	return &Tool{client: client}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "slack.fetch-msgs" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Fetch messages from a Slack channel or thread, oldest first."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"channel": {Type: "string", Description: "Channel name (with or without #) or channel ID."},
			"thread":  {Type: "string", Description: "Thread timestamp (e.g. 1234567890.123456) to fetch replies from."},
			"since":   {Type: "string", Description: "Exclude messages sent before this Sydney datetime (YYYY-MM-DD HH:mm:ss). Default: 24h ago."},
		},
		Required: []string{"channel"},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	Channel  string             `json:"channel"`
	Messages []slacklib.Message `json:"messages"`
}

// String renders the messages as oldest-first `[HH:MM] @user: text` lines.
func (r Result) String() string { return slacklib.FormatMessages(r.Messages) }

// Invoke runs the tool. It resolves --channel, parses --since, then
// dispatches to GetReplies when --thread is set or GetHistory otherwise.
// Messages are reversed so the result is oldest-first.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	channel, _ := args["channel"].(string)
	thread, _ := args["thread"].(string)
	since, _ := args["since"].(string)

	if channel == "" {
		return nil, fmt.Errorf("Error: --channel is required")
	}

	api, err := t.client.Get()
	if err != nil {
		return nil, err
	}

	channelID, err := slacklib.ResolveChannel(ctx, api, channel)
	if err != nil {
		return nil, err
	}

	sinceTime, err := slacklib.ParseSince(since)
	if err != nil {
		return nil, err
	}
	oldestTS := slacklib.SlackTS(sinceTime)

	var msgs []slacklib.Message
	if thread != "" {
		msgs, err = api.GetReplies(ctx, channelID, thread, oldestTS)
	} else {
		msgs, err = api.GetHistory(ctx, channelID, oldestTS, HistoryLimit)
	}
	if err != nil {
		return nil, err
	}

	return Result{Channel: channelID, Messages: slacklib.ReverseMessages(msgs)}, nil
}
