package slackapp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/acp"
)

type ChatSessionManager interface {
	Prompt(context.Context, acp.ConversationKey, acp.SessionMetadata, acp.PromptRequest) (<-chan acp.Event, error)
}

type ChatSessionWarmer interface {
	Warm(context.Context) error
}

type ChatHandler struct {
	api      StreamAPI
	sessions ChatSessionManager
	interval time.Duration
	minChars int
	logger   *slog.Logger
}

type ChatRequest struct {
	TeamID    string
	ChannelID string
	UserID    string
	ThreadTS  string
	MessageTS string
	Text      string
	DM        bool
	Source    string
}

func NewChatHandler(api StreamAPI, sessions ChatSessionManager, interval time.Duration, minChars int, logger *slog.Logger) *ChatHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &ChatHandler{api: api, sessions: sessions, interval: interval, minChars: minChars, logger: logger}
}

func (h *ChatHandler) Warm(ctx context.Context) error {
	warmer, ok := h.sessions.(ChatSessionWarmer)
	if !ok {
		return nil
	}
	return warmer.Warm(ctx)
}

func (h *ChatHandler) Handle(ctx context.Context, req ChatRequest) error {
	startedAt := time.Now()
	if h == nil || h.sessions == nil {
		return fmt.Errorf("ACP chat is not enabled")
	}
	prompt := strings.TrimSpace(req.Text)
	if prompt == "" {
		return fmt.Errorf("chat prompt is empty")
	}
	key := conversationKey(req)
	metadata := acp.SessionMetadata{TeamID: req.TeamID, ChannelID: req.ChannelID, ThreadTS: key.ThreadTS, UserID: req.UserID, Source: req.Source}
	streamThreadTS := streamThreadTS(req)
	if streamThreadTS == "" {
		return fmt.Errorf("Slack streaming requires a source message timestamp")
	}
	teamID, userID := req.TeamID, req.UserID
	if req.DM {
		teamID, userID = "", ""
	}
	writer := NewStreamWriter(h.api, req.ChannelID, StreamWriterOptions{ThreadTS: streamThreadTS, TeamID: teamID, UserID: userID, Interval: h.interval, MinChars: h.minChars, Logger: h.logger})
	if err := writer.Start(ctx); err != nil {
		return err
	}
	events, err := h.sessions.Prompt(ctx, key, metadata, acp.PromptRequest{Text: prompt})
	if err != nil {
		return writer.Fail(ctx, err)
	}
	chunks := 0
	bytes := 0
	firstChunkLogged := false
	for event := range events {
		switch event.Type {
		case acp.EventText, acp.EventStatus:
			if event.Text != "" {
				chunks++
				bytes += len(event.Text)
				if !firstChunkLogged {
					firstChunkLogged = true
					h.logger.Info("received first ACP text chunk", "source", req.Source, "channel", req.ChannelID, "duration", time.Since(startedAt), "bytes", len(event.Text))
				}
			}
			if err := writer.Append(ctx, event.Text); err != nil {
				return err
			}
		case acp.EventError:
			return writer.Fail(ctx, event.Error)
		case acp.EventComplete:
			if err := writer.Stop(ctx); err != nil {
				return err
			}
			h.logger.Info("completed ACP chat response", "source", req.Source, "channel", req.ChannelID, "duration", time.Since(startedAt), "chunks", chunks, "bytes", bytes)
			return nil
		}
	}
	if err := writer.Stop(ctx); err != nil {
		return err
	}
	h.logger.Info("completed ACP chat response", "source", req.Source, "channel", req.ChannelID, "duration", time.Since(startedAt), "chunks", chunks, "bytes", bytes)
	return nil
}

func conversationKey(req ChatRequest) acp.ConversationKey {
	if req.DM {
		return acp.ConversationKey{TeamID: req.TeamID, ChannelID: req.ChannelID, DM: true}
	}
	threadTS := req.ThreadTS
	if threadTS == "" {
		threadTS = req.MessageTS
	}
	return acp.ConversationKey{TeamID: req.TeamID, ChannelID: req.ChannelID, ThreadTS: threadTS}
}

func streamThreadTS(req ChatRequest) string {
	if req.ThreadTS != "" {
		return req.ThreadTS
	}
	return req.MessageTS
}
