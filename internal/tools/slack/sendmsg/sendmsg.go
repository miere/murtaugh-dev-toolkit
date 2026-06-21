// Package sendmsg implements the `slack.send-msg` tool: post a message (or
// upload a file) to a Slack channel or DM.
package sendmsg

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/jsonschema-go/jsonschema"

	slacklib "github.com/miere/murtaugh-dev-toolkit/internal/slack/client"
)

// Tool is the `slack.send-msg` capability.
type Tool struct {
	client      *slacklib.LazyClient
	adminClient *slacklib.LazyClient
	warn        io.Writer
}

// New constructs a Tool that builds its Slack clients lazily from the given
// tokens. botToken (oauth.bot_token in slack.yaml) backs the default "as
// bot" sends. userToken (oauth.user_token, the admin's xoxp-… token) backs
// "as admin" sends; when it is empty, requesting as: "admin" returns an
// error. Warnings about unresolvable mentions are written to os.Stderr.
func New(botToken, userToken string) *Tool {
	var adminClient *slacklib.LazyClient
	if userToken != "" {
		adminClient = slacklib.NewLazyClient(userToken)
	}
	return &Tool{client: slacklib.NewLazyClient(botToken), adminClient: adminClient, warn: os.Stderr}
}

// NewWith constructs a Tool against the given LazyClients and warn writer.
// Intended for tests so they can inject fake SlackAPIs and capture
// warnings. A nil adminClient models a missing user token.
func NewWith(client, adminClient *slacklib.LazyClient, warn io.Writer) *Tool {
	return &Tool{client: client, adminClient: adminClient, warn: warn}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "slack.send-msg" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Send a message (optionally with an attachment) to a Slack channel or user."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"body":            {Type: "string", Description: "Message body text. Also used as the notification fallback when blocks are set."},
			"to":              {Type: "string", Description: "Destination: #channel, @user, or C/G/D Slack ID."},
			"attachment":      {Type: "string", Description: "Path to a file to attach. Mutually exclusive with blocks."},
			"thread":          {Type: "string", Description: "Thread timestamp to reply to."},
			"attachment_type": {Type: "string", Enum: []any{"markdown"}, Description: "Snippet type for the attachment."},
			"blocks":          {Type: "string", Description: "Block Kit blocks: either a JSON string (starts with [ or {) or a path to a JSON file. Mutually exclusive with attachment."},
			"as":              {Type: "string", Enum: []any{"bot", "admin"}, Description: "Sender identity: \"bot\" (default) posts as the app; \"admin\" posts as the human admin via their Slack user token (requires oauth.user_token)."},
		},
		Required: []string{"body", "to"},
	}
}

// Result is the structured payload returned by Invoke. The MCP frontend
// JSON-marshals it; the CLI frontend renders it via String().
type Result struct {
	OK      bool   `json:"ok"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	To      string `json:"to"`
}

// String renders the CLI-visible line: `Message sent to <to>.`
func (r Result) String() string { return fmt.Sprintf("Message sent to %s.", r.To) }

// Invoke runs the tool. It resolves --to into a channel ID, expands @handle
// mentions in --body, then either uploads the attachment or posts the
// message; --thread, when present, is forwarded as the parent timestamp.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	body, _ := args["body"].(string)
	to, _ := args["to"].(string)
	attachment, _ := args["attachment"].(string)
	thread, _ := args["thread"].(string)
	attachmentType, _ := args["attachment_type"].(string)
	blocks, _ := args["blocks"].(string)
	as, _ := args["as"].(string)

	if body == "" {
		return nil, fmt.Errorf("Error: --body is required")
	}
	if to == "" {
		return nil, fmt.Errorf("Error: --to is required")
	}
	if attachment != "" && blocks != "" {
		return nil, fmt.Errorf("Error: --attachment and --blocks are mutually exclusive")
	}

	// Pick the sender: "admin" posts via the admin's user token; "bot"
	// (default) and the empty string post via the bot token. We never
	// silently fall back to the bot when admin is requested but unavailable.
	client := t.client
	switch as {
	case "", "bot":
	case "admin":
		if t.adminClient == nil {
			return nil, fmt.Errorf("Error: as=admin requires a Slack user token; set oauth.user_token in slack.yaml")
		}
		client = t.adminClient
	default:
		return nil, fmt.Errorf("Error: --as must be \"bot\" or \"admin\"")
	}

	rawBlocks, err := slacklib.ResolveBlocks(blocks)
	if err != nil {
		return nil, err
	}

	api, err := client.Get()
	if err != nil {
		return nil, err
	}

	channelID, err := slacklib.ResolveTarget(ctx, api, to)
	if err != nil {
		return nil, err
	}

	resolved := slacklib.ResolveMentions(ctx, api, body, t.warn)

	var res slacklib.PostMessageResult
	if attachment != "" {
		if _, err := os.Stat(attachment); err != nil {
			return nil, fmt.Errorf("Error: attachment not found: %s", attachment)
		}
		res, err = api.UploadFile(ctx, slacklib.UploadFileParams{
			ChannelID:      channelID,
			FilePath:       attachment,
			Filename:       filepath.Base(attachment),
			Title:          filepath.Base(attachment),
			InitialComment: resolved,
			SnippetType:    attachmentType,
			ThreadTS:       thread,
		})
		if err != nil {
			return nil, err
		}
	} else {
		res, err = api.PostMessage(ctx, slacklib.PostMessageParams{
			ChannelID: channelID,
			Text:      resolved,
			ThreadTS:  thread,
			Blocks:    rawBlocks,
		})
		if err != nil {
			return nil, err
		}
	}

	return Result{OK: true, Channel: res.Channel, TS: res.TS, To: to}, nil
}
