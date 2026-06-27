// Package restart implements the `restart` tool: a request-only entry point
// that asks an admin to approve a graceful restart of Murtaugh.
//
// The tool never restarts anything. It posts the same Block Kit approval card
// the gateway already understands (internal/slack/restartcard); the actual
// restart happens only when the configured admin clicks Confirm in the running
// gateway daemon (or via the admin-only `/murtaugh restart` slash command).
// This keeps the restart behind explicit human consent even though the agent,
// MCP clients, and the CLI can all *request* one.
//
// Destination: when the caller passes a `channel` (the ACP agent, told its
// conversation via the prompt context), the card is asked there; otherwise it
// falls back to the configured admin's DM — the right behaviour for MCP, the
// CLI, and background jobs, which have no chat to ask in.
package restart

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	slackgo "github.com/slack-go/slack"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/restartcard"
)

// Tool is the `restart` capability.
type Tool struct {
	client    *slacklib.LazyClient
	adminUser string
}

// New constructs a Tool that posts the approval card with the given bot token
// (oauth.bot_token in gateway.yaml). adminUser is the configured admin
// (configuration.admin_user) used as the fallback destination when no channel
// is supplied.
func New(token, adminUser string) *Tool {
	return &Tool{client: slacklib.NewLazyClient(token), adminUser: strings.TrimSpace(adminUser)}
}

// NewWith constructs a Tool against the given LazyClient. Intended for tests so
// they can inject a fake SlackAPI.
func NewWith(client *slacklib.LazyClient, adminUser string) *Tool {
	return &Tool{client: client, adminUser: strings.TrimSpace(adminUser)}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "restart" }

// Description returns the human-facing summary used by MCP/CLI clients and the
// agent. It is deliberately explicit that this only *requests* a restart.
func (t *Tool) Description() string {
	return "Request a graceful restart of Murtaugh. This posts an approval card to Slack and " +
		"an admin must click Confirm — it does NOT restart anything by itself. When you are " +
		"running inside a Slack conversation, pass the current `channel` (and `thread`) so the " +
		"approval is asked right there; otherwise the request goes to the admin's DM."
}

// InputSchema returns the JSON Schema for the tool's arguments. Every field is
// optional: with no arguments the card goes to the admin DM with a default reason.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"reason":  {Type: "string", Description: "Why a restart is being requested. Shown on the approval card."},
			"channel": {Type: "string", Description: "Where to ask: #channel, @user, or C/G/D Slack ID. Defaults to the admin's DM."},
			"thread":  {Type: "string", Description: "Thread timestamp to post the card in, when asking inside a thread."},
		},
	}
}

// Result is the structured payload returned by Invoke. The MCP frontend
// JSON-marshals it; the CLI frontend renders it via String().
type Result struct {
	OK      bool   `json:"ok"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// String renders the CLI-visible line.
func (r Result) String() string {
	return "Restart requested; awaiting admin approval in Slack."
}

// Invoke posts the approval card. It resolves the destination (explicit
// channel, else the admin DM), renders the shared restart card, and posts it.
// It never touches a restart coordinator — there is none in the MCP/CLI process.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	reason, _ := args["reason"].(string)
	channel, _ := args["channel"].(string)
	thread, _ := args["thread"].(string)
	channel = strings.TrimSpace(channel)
	thread = strings.TrimSpace(thread)

	api, err := t.client.Get()
	if err != nil {
		return nil, err
	}

	destination, err := t.resolveDestination(ctx, api, channel)
	if err != nil {
		return nil, err
	}

	blocks, err := json.Marshal(slackgo.Blocks{BlockSet: restartcard.Build(reason)})
	if err != nil {
		return nil, fmt.Errorf("encode restart card: %w", err)
	}

	res, err := api.PostMessage(ctx, slacklib.PostMessageParams{
		ChannelID: destination,
		Text:      restartcard.Headline,
		ThreadTS:  thread,
		Blocks:    blocks,
	})
	if err != nil {
		return nil, fmt.Errorf("post restart request: %w", err)
	}
	return Result{OK: true, Channel: res.Channel, TS: res.TS}, nil
}

// resolveDestination returns the channel ID to post the card to. An explicit
// channel always wins (resolved via the shared #channel/@user/ID resolver).
// Otherwise it opens the admin's DM: ResolveTarget rejects raw U… IDs, so a
// bare user ID is opened directly, while a handle is routed through @-resolution.
func (t *Tool) resolveDestination(ctx context.Context, api slacklib.SlackAPI, channel string) (string, error) {
	if channel != "" {
		return slacklib.ResolveTarget(ctx, api, channel)
	}
	if t.adminUser == "" {
		return "", fmt.Errorf("Error: no channel given and no admin user configured (configuration.admin_user) to ask")
	}
	if isUserID(t.adminUser) {
		return api.OpenDM(ctx, t.adminUser)
	}
	target := t.adminUser
	if !strings.HasPrefix(target, "@") {
		target = "@" + target
	}
	return slacklib.ResolveTarget(ctx, api, target)
}

// isUserID reports whether s looks like a raw Slack user ID (U… / W…), which
// ResolveTarget rejects and which must instead be opened via OpenDM.
func isUserID(s string) bool {
	if len(s) < 2 {
		return false
	}
	switch s[0] {
	case 'U', 'W':
		return strings.ToUpper(s) == s
	default:
		return false
	}
}
