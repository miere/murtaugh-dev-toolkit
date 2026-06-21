// Package createchannel implements the `slack.create-channel` tool: create a
// public or private Slack channel, optionally inviting users and setting a
// topic/purpose.
package createchannel

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	slacklib "github.com/miere/murtaugh-dev-toolkit/internal/slack/client"
)

// Tool is the `slack.create-channel` capability.
type Tool struct {
	client *slacklib.LazyClient
	warn   io.Writer
}

// New constructs a Tool that builds its Slack client lazily from the given
// bot token (sourced from oauth.bot_token in slack.yaml). Warnings about
// unresolvable invitees are written to os.Stderr.
func New(token string) *Tool {
	return &Tool{client: slacklib.NewLazyClient(token), warn: os.Stderr}
}

// NewWith constructs a Tool against the given LazyClient and warn writer.
// Intended for tests so they can inject a fake SlackAPI and capture warnings.
func NewWith(client *slacklib.LazyClient, warn io.Writer) *Tool {
	return &Tool{client: client, warn: warn}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "slack.create-channel" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Create a public or private Slack channel, optionally inviting users and setting a topic/purpose."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name":    {Type: "string", Description: "Channel name (without a leading #). Slack lowercases it and replaces spaces with hyphens."},
			"private": {Type: "boolean", Description: "Create a private channel instead of a public one."},
			"invite": {
				Type:        "array",
				Items:       &jsonschema.Schema{Type: "string"},
				Description: "Users to invite: @handle mentions or raw U.../W... Slack user IDs.",
			},
			"topic":   {Type: "string", Description: "Channel topic to set after creation."},
			"purpose": {Type: "string", Description: "Channel purpose/description to set after creation."},
		},
		Required: []string{"name"},
	}
}

// Result is the structured payload returned by Invoke. The MCP frontend
// JSON-marshals it; the CLI frontend renders it via String(). InviteErrors is
// populated when one or more invites (or the topic/purpose updates) failed but
// the channel was still created.
type Result struct {
	OK           bool     `json:"ok"`
	Channel      string   `json:"channel"`
	Name         string   `json:"name"`
	Private      bool     `json:"private"`
	InviteErrors []string `json:"invite_errors,omitempty"`
}

// String renders the CLI-visible line, noting any partial failures.
func (r Result) String() string {
	kind := "public"
	if r.Private {
		kind = "private"
	}
	msg := fmt.Sprintf("Created %s channel #%s (%s).", kind, r.Name, r.Channel)
	if len(r.InviteErrors) > 0 {
		msg += fmt.Sprintf(" %d follow-up action(s) failed: %s", len(r.InviteErrors), strings.Join(r.InviteErrors, "; "))
	}
	return msg
}

// Invoke runs the tool. It resolves any @handle invitees to user IDs, creates
// the channel, then surfaces a structured result including any non-fatal
// invite/topic/purpose failures.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	name, _ := args["name"].(string)
	private, _ := args["private"].(bool)
	topic, _ := args["topic"].(string)
	purpose, _ := args["purpose"].(string)

	name = strings.TrimPrefix(strings.TrimSpace(name), "#")
	if name == "" {
		return nil, fmt.Errorf("Error: --name is required")
	}

	api, err := t.client.Get()
	if err != nil {
		return nil, err
	}

	invite, err := t.resolveInvitees(ctx, api, args["invite"])
	if err != nil {
		return nil, err
	}

	res, err := api.CreateChannel(ctx, slacklib.CreateChannelParams{
		Name:    name,
		Private: private,
		Invite:  invite,
		Topic:   topic,
		Purpose: purpose,
	})
	if err != nil {
		return nil, err
	}

	return Result{
		OK:           true,
		Channel:      res.Channel.ID,
		Name:         res.Channel.Name,
		Private:      private,
		InviteErrors: res.InviteErrors,
	}, nil
}

// resolveInvitees turns the raw `invite` argument (a JSON array) into Slack
// user IDs. @handle entries are resolved via the shared resolver; raw Slack
// IDs (U.../W...) pass through untouched. Unresolvable handles are dropped
// with a warning so the channel is still created.
func (t *Tool) resolveInvitees(ctx context.Context, api slacklib.SlackAPI, raw any) ([]string, error) {
	items, ok := raw.([]any)
	if !ok {
		if raw == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("Error: --invite must be an array of @handles or user IDs")
	}
	var ids []string
	for _, it := range items {
		entry, _ := it.(string)
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.HasPrefix(entry, "@") {
			userID, err := slacklib.ResolveUser(ctx, api, entry)
			if err != nil {
				fmt.Fprintf(t.warn, "Warning: invitee '%s' not found, skipping.\n", entry)
				continue
			}
			ids = append(ids, userID)
			continue
		}
		ids = append(ids, entry)
	}
	return ids, nil
}
