package client

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	slackgo "github.com/slack-go/slack"
)

// PostMessage posts a message via chat.postMessage. When Blocks is non-empty
// it's parsed as a Block Kit payload and forwarded alongside Text (which
// Slack uses as the notification/fallback string). Returns the resolved
// channel ID and the message timestamp.
func (c *SlackClient) PostMessage(ctx context.Context, p PostMessageParams) (PostMessageResult, error) {
	opts := []slackgo.MsgOption{slackgo.MsgOptionText(p.Text, false)}
	if p.ThreadTS != "" {
		opts = append(opts, slackgo.MsgOptionTS(p.ThreadTS))
	}
	if len(p.Blocks) > 0 {
		var blocks slackgo.Blocks
		if err := json.Unmarshal(p.Blocks, &blocks); err != nil {
			return PostMessageResult{}, fmt.Errorf("Error parsing blocks JSON: %s", err.Error())
		}
		opts = append(opts, slackgo.MsgOptionBlocks(blocks.BlockSet...))
	}
	channel, ts, err := c.api.PostMessageContext(ctx, p.ChannelID, opts...)
	if err != nil {
		return PostMessageResult{}, slackError("chat.postMessage", err)
	}
	return PostMessageResult{Channel: channel, TS: ts}, nil
}

// UploadFile uploads a file using slack-go's UploadFileContext, which
// internally uses the external-upload flow Slack now requires
// (files.getUploadURLExternal -> PUT -> files.completeUploadExternal). That
// flow needs the byte count up front, so we stat the file and pass FileSize
// explicitly — otherwise Slack rejects the request with
// "file.upload.v2: file size cannot be 0".
func (c *SlackClient) UploadFile(ctx context.Context, p UploadFileParams) (PostMessageResult, error) {
	info, err := os.Stat(p.FilePath)
	if err != nil {
		return PostMessageResult{}, fmt.Errorf("Error: cannot stat attachment %s: %s", p.FilePath, err.Error())
	}
	if info.Size() == 0 {
		return PostMessageResult{}, fmt.Errorf("Error: attachment %s is empty (0 bytes); Slack rejects empty uploads", p.FilePath)
	}
	params := slackgo.UploadFileParameters{
		File:            p.FilePath,
		FileSize:        int(info.Size()),
		Filename:        p.Filename,
		Title:           p.Title,
		InitialComment:  p.InitialComment,
		Channel:         p.ChannelID,
		ThreadTimestamp: p.ThreadTS,
		SnippetType:     p.SnippetType,
	}
	file, err := c.api.UploadFileContext(ctx, params)
	if err != nil {
		return PostMessageResult{}, slackError("files.upload", err)
	}
	return PostMessageResult{Channel: p.ChannelID, TS: file.ID}, nil
}

// UpdateMessage calls chat.update. Empty Blocks means "text only".
func (c *SlackClient) UpdateMessage(ctx context.Context, p UpdateMessageParams) (PostMessageResult, error) {
	opts := []slackgo.MsgOption{slackgo.MsgOptionText(p.Text, false)}
	if len(p.Blocks) > 0 {
		var blocks slackgo.Blocks
		if err := json.Unmarshal(p.Blocks, &blocks); err != nil {
			return PostMessageResult{}, fmt.Errorf("Error parsing blocks JSON: %s", err.Error())
		}
		opts = append(opts, slackgo.MsgOptionBlocks(blocks.BlockSet...))
	}
	channel, ts, _, err := c.api.UpdateMessageContext(ctx, p.ChannelID, p.TS, opts...)
	if err != nil {
		return PostMessageResult{}, slackError("chat.update", err)
	}
	return PostMessageResult{Channel: channel, TS: ts}, nil
}

// OpenView opens a modal via views.open. The trigger_id must come from a fresh
// user interaction; Slack expires it within a few seconds. The ViewResponse is
// discarded — only the error matters to callers.
func (c *SlackClient) OpenView(ctx context.Context, triggerID string, view slackgo.ModalViewRequest) error {
	if _, err := c.api.OpenViewContext(ctx, triggerID, view); err != nil {
		return slackError("views.open", err)
	}
	return nil
}

// GetHistory fetches up to limit messages newer than oldestTS. The returned
// slice is left in Slack's natural newest-first order; callers reverse it
// before display.
func (c *SlackClient) GetHistory(ctx context.Context, channelID, oldestTS string, limit int) ([]Message, error) {
	resp, err := c.api.GetConversationHistoryContext(ctx, &slackgo.GetConversationHistoryParameters{
		ChannelID: channelID,
		Oldest:    oldestTS,
		Limit:     limit,
	})
	if err != nil {
		return nil, slackError("conversations.history", err)
	}
	return convertMessages(resp.Messages), nil
}

// GetReplies fetches the replies of a thread. oldestTS may be empty.
func (c *SlackClient) GetReplies(ctx context.Context, channelID, threadTS, oldestTS string) ([]Message, error) {
	params := &slackgo.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTS,
	}
	if oldestTS != "" {
		params.Oldest = oldestTS
	}
	msgs, _, _, err := c.api.GetConversationRepliesContext(ctx, params)
	if err != nil {
		return nil, slackError("conversations.replies", err)
	}
	return convertMessages(msgs), nil
}

// ListChannels pages through public and private channels.
func (c *SlackClient) ListChannels(ctx context.Context) ([]Channel, error) {
	var out []Channel
	cursor := ""
	for {
		params := &slackgo.GetConversationsParameters{
			Cursor: cursor,
			Limit:  200,
			Types:  []string{"public_channel", "private_channel"},
		}
		chans, next, err := c.api.GetConversationsContext(ctx, params)
		if err != nil {
			return nil, slackError("conversations.list", err)
		}
		for _, ch := range chans {
			out = append(out, Channel{ID: ch.ID, Name: ch.Name})
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// ListUsers pages through every user in the workspace.
func (c *SlackClient) ListUsers(ctx context.Context) ([]User, error) {
	users, err := c.api.GetUsersContext(ctx)
	if err != nil {
		return nil, slackError("users.list", err)
	}
	out := make([]User, 0, len(users))
	for _, u := range users {
		out = append(out, User{
			ID:          u.ID,
			Name:        u.Name,
			DisplayName: u.Profile.DisplayName,
			RealName:    u.Profile.RealName,
		})
	}
	return out, nil
}

// OpenDM opens (or resumes) a DM channel with the given user and returns its
// channel ID.
func (c *SlackClient) OpenDM(ctx context.Context, userID string) (string, error) {
	ch, _, _, err := c.api.OpenConversationContext(ctx, &slackgo.OpenConversationParameters{
		Users: []string{userID},
	})
	if err != nil {
		return "", slackError("conversations.open", err)
	}
	return ch.ID, nil
}

// CreateChannel creates a public or private channel, then best-effort invites
// any requested users and sets the topic/purpose. Channel creation failure is
// fatal; per-user invite failures are collected into InviteErrors so the
// caller can report them without losing the channel. Topic/purpose failures
// are likewise non-fatal.
func (c *SlackClient) CreateChannel(ctx context.Context, p CreateChannelParams) (CreateChannelResult, error) {
	ch, err := c.api.CreateConversationContext(ctx, slackgo.CreateConversationParams{
		ChannelName: p.Name,
		IsPrivate:   p.Private,
	})
	if err != nil {
		return CreateChannelResult{}, slackError("conversations.create", err)
	}
	res := CreateChannelResult{Channel: Channel{ID: ch.ID, Name: ch.Name}}

	// Invite users one at a time so one bad ID doesn't fail the whole batch.
	for _, user := range p.Invite {
		if _, err := c.api.InviteUsersToConversationContext(ctx, ch.ID, user); err != nil {
			res.InviteErrors = append(res.InviteErrors,
				fmt.Sprintf("%s: %s", user, slackError("conversations.invite", err).Error()))
		}
	}

	if p.Topic != "" {
		if _, err := c.api.SetTopicOfConversationContext(ctx, ch.ID, p.Topic); err != nil {
			res.InviteErrors = append(res.InviteErrors, slackError("conversations.setTopic", err).Error())
		}
	}
	if p.Purpose != "" {
		if _, err := c.api.SetPurposeOfConversationContext(ctx, ch.ID, p.Purpose); err != nil {
			res.InviteErrors = append(res.InviteErrors, slackError("conversations.setPurpose", err).Error())
		}
	}
	return res, nil
}

// convertMessages projects slack-go Message values into the package's public
// Message type so callers never see slack-go types.
func convertMessages(in []slackgo.Message) []Message {
	out := make([]Message, len(in))
	for i, m := range in {
		out[i] = Message{
			TS:       m.Timestamp,
			User:     m.User,
			Text:     m.Text,
			ThreadTS: m.ThreadTimestamp,
		}
		if len(m.Reactions) > 0 {
			out[i].Reactions = make([]Reaction, len(m.Reactions))
			for j, r := range m.Reactions {
				out[i].Reactions[j] = Reaction{Name: r.Name, Users: r.Users, Count: r.Count}
			}
		}
	}
	return out
}
