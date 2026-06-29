package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	slackgo "github.com/slack-go/slack"
)

// ErrTokenMissing is returned when a SlackClient is constructed without a
// bot token. Murtaugh sources the token from oauth.bot_token in gateway.yaml,
// so an empty value means the daemon was never configured.
var ErrTokenMissing = errors.New("Slack bot token is not configured; set oauth.bot_token in gateway.yaml")

// PostMessageParams collects the inputs PostMessage accepts. ThreadTS is
// optional; an empty string means "post to the channel, not a thread".
// Blocks is the raw JSON of Block Kit blocks; nil/empty means text-only.
type PostMessageParams struct {
	ChannelID string
	Text      string
	ThreadTS  string
	Blocks    []byte
}

// UploadFileParams collects the inputs UploadFile accepts. Field semantics
// match Slack's files_upload_v2 call.
type UploadFileParams struct {
	ChannelID      string
	FilePath       string
	Filename       string
	Title          string
	InitialComment string
	SnippetType    string
	ThreadTS       string
}

// PostMessageResult is the subset of chat.postMessage's response the tools
// care about — enough to surface a useful structured result.
type PostMessageResult struct {
	Channel string
	TS      string
}

// UpdateMessageParams collects the inputs UpdateMessage accepts.
type UpdateMessageParams struct {
	ChannelID string
	TS        string
	Text      string
	Blocks    []byte // raw JSON of Block Kit blocks; nil if absent
}

// PostEphemeralParams collects the inputs PostEphemeral accepts. The message is
// visible only to UserID in ChannelID (optionally within ThreadTS). Blocks is the
// raw JSON of Block Kit blocks; nil/empty means text-only.
type PostEphemeralParams struct {
	ChannelID string
	UserID    string
	Text      string
	ThreadTS  string
	Blocks    []byte
}

// WebhookParams collects the inputs RespondURL accepts. It targets a Slack
// interaction response_url rather than a channel; ReplaceOriginal rewrites the
// message the clicked button belonged to (the only way to edit an ephemeral
// prompt). Blocks is the raw JSON of Block Kit blocks; nil/empty means text-only.
type WebhookParams struct {
	Text            string
	Blocks          []byte
	ReplaceOriginal bool
}

// CreateChannelParams collects the inputs CreateChannel accepts. Name is the
// channel name (without a leading "#"). Private selects a private channel.
// Invite is a list of user IDs to add after creation. Topic and Purpose are
// optional; empty values are skipped.
type CreateChannelParams struct {
	Name    string
	Private bool
	Invite  []string
	Topic   string
	Purpose string
}

// CreateChannelResult reports the created channel plus any non-fatal
// follow-up failures. InviteErrors holds one human-readable message per user
// that could not be invited; the channel still exists when it is non-empty.
type CreateChannelResult struct {
	Channel      Channel
	InviteErrors []string
}

// User is the minimal projection of a Slack user the resolver needs.
type User struct {
	ID          string
	Name        string
	DisplayName string
	RealName    string
}

// Channel is the minimal projection of a Slack channel the resolver needs.
type Channel struct {
	ID   string
	Name string
}

// SlackAPI is the seam the tools depend on. SlackClient implements this with
// slack-go calls; tests substitute an in-memory fake.
type SlackAPI interface {
	PostMessage(ctx context.Context, p PostMessageParams) (PostMessageResult, error)
	UploadFile(ctx context.Context, p UploadFileParams) (PostMessageResult, error)
	UpdateMessage(ctx context.Context, p UpdateMessageParams) (PostMessageResult, error)
	// PostEphemeral posts a message visible only to one user via
	// chat.postEphemeral and returns the message timestamp. Ephemeral messages
	// cannot be edited with chat.update; callers rewrite them through the
	// interaction's response_url (RespondURL) instead.
	PostEphemeral(ctx context.Context, p PostEphemeralParams) (string, error)
	// RespondURL POSTs to a Slack interaction response_url (an unauthenticated,
	// short-lived webhook delivered with a button click). It is the only way to
	// edit or replace an ephemeral prompt after the user responds.
	RespondURL(ctx context.Context, responseURL string, p WebhookParams) error
	GetHistory(ctx context.Context, channelID string, oldestTS string, limit int) ([]Message, error)
	GetReplies(ctx context.Context, channelID, threadTS, oldestTS string) ([]Message, error)
	ListChannels(ctx context.Context) ([]Channel, error)
	ListUsers(ctx context.Context) ([]User, error)
	OpenDM(ctx context.Context, userID string) (string, error)
	CreateChannel(ctx context.Context, p CreateChannelParams) (CreateChannelResult, error)
	// OpenView opens a modal view in response to a user interaction. triggerID
	// comes from the block_actions callback that prompted the open; it is
	// short-lived (Slack expires it within seconds), so callers must open
	// promptly. The returned view response is discarded — callers only need to
	// know whether the open succeeded.
	OpenView(ctx context.Context, triggerID string, view slackgo.ModalViewRequest) error
}

// SlackClient is the production SlackAPI implementation. It is constructed
// lazily by NewClient so that registering Slack tools does not require a
// configured token at boot.
type SlackClient struct {
	api *slackgo.Client
}

// NewClient builds a SlackClient from the given bot token. An empty token
// returns ErrTokenMissing so the failure is reported the same way on every
// tool invocation.
func NewClient(token string) (*SlackClient, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrTokenMissing
	}
	return &SlackClient{api: slackgo.New(token)}, nil
}

// LazyClient is a sync.Once-guarded SlackAPI factory. Tools hold one of
// these and call Get on demand; the first call constructs the underlying
// client. Subsequent calls return the cached instance (or the cached error,
// so the failure is reported consistently).
type LazyClient struct {
	once    sync.Once
	client  SlackAPI
	err     error
	factory func() (SlackAPI, error)
}

// NewLazyClient returns a LazyClient that constructs a real SlackClient from
// token on first use. Tests inject a fake factory via NewLazyClientWith.
func NewLazyClient(token string) *LazyClient {
	return &LazyClient{factory: func() (SlackAPI, error) {
		c, err := NewClient(token)
		if err != nil {
			return nil, err
		}
		return c, nil
	}}
}

// NewLazyClientWith returns a LazyClient that calls the given factory on
// first use. Intended for tests and for tools that need a custom seam.
func NewLazyClientWith(factory func() (SlackAPI, error)) *LazyClient {
	return &LazyClient{factory: factory}
}

// Get returns the SlackAPI built by the factory, constructing it on the
// first call. Once a result is cached (success or error) it is returned
// unchanged on subsequent calls.
func (l *LazyClient) Get() (SlackAPI, error) {
	l.once.Do(func() {
		l.client, l.err = l.factory()
	})
	return l.client, l.err
}

// slackError wraps a slack-go error with a diagnostic prefix naming the API
// method, e.g. "Slack error (chat.postMessage): channel_not_found".
func slackError(method string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("Slack error (%s): %s", method, err.Error())
}
