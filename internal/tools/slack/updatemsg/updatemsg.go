// Package updatemsg implements the `slack.update-msg` tool: update an
// existing message in a channel, optionally rewriting its Block Kit blocks.
package updatemsg

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

// DefaultBody is the fallback text Slack receives when --body is omitted.
const DefaultBody = "Message updated"

// Tool is the `slack.update-msg` capability.
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
func (t *Tool) Name() string { return "slack.update-msg" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Update an existing message in a Slack channel."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"channel": {Type: "string", Description: "Channel ID, or channel name with leading #."},
			"ts":      {Type: "string", Description: "Timestamp of the message to update."},
			"body":    {Type: "string", Description: "Fallback text for the update. Defaults to 'Message updated'."},
			"blocks":  {Type: "string", Description: "Block Kit blocks: either a JSON string (starts with [ or {) or a path to a JSON file."},
		},
		Required: []string{"channel", "ts"},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	OK              bool   `json:"ok"`
	Channel         string `json:"channel"`
	TS              string `json:"ts"`
	originalChannel string
}

// String renders the CLI-visible line, echoing the *user-supplied* channel
// value (name or ID) rather than the resolved channel ID.
func (r Result) String() string {
	return fmt.Sprintf("Message %s updated in %s.", r.TS, r.originalChannel)
}

// Invoke runs the tool. --channel starting with `#` is resolved through
// conversations.list; any other value is passed through as a Slack channel
// ID.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	channel, _ := args["channel"].(string)
	ts, _ := args["ts"].(string)
	body, _ := args["body"].(string)
	blocks, _ := args["blocks"].(string)

	if channel == "" {
		return nil, fmt.Errorf("Error: --channel is required")
	}
	if ts == "" {
		return nil, fmt.Errorf("Error: --ts is required")
	}
	if body == "" {
		body = DefaultBody
	}

	api, err := t.client.Get()
	if err != nil {
		return nil, err
	}

	var channelID string
	if strings.HasPrefix(channel, "#") {
		channelID, err = slacklib.ResolveChannel(ctx, api, channel)
		if err != nil {
			return nil, err
		}
	} else {
		channelID = channel
	}

	rawBlocks, err := slacklib.ResolveBlocks(blocks)
	if err != nil {
		return nil, err
	}

	res, err := api.UpdateMessage(ctx, slacklib.UpdateMessageParams{
		ChannelID: channelID,
		TS:        ts,
		Text:      body,
		Blocks:    rawBlocks,
	})
	if err != nil {
		return nil, err
	}
	return Result{OK: true, Channel: res.Channel, TS: res.TS, originalChannel: channel}, nil
}
