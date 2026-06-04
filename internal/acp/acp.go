package acp

import "context"

type Client interface {
	Initialize(context.Context) error
	NewSession(context.Context, SessionMetadata) (Session, error)
	Prompt(context.Context, string, PromptRequest) (<-chan Event, error)
	Cancel(context.Context, string) error
	Close() error
}

type Session struct {
	ID string
}

type SessionMetadata struct {
	TeamID    string `json:"teamId,omitempty"`
	ChannelID string `json:"channelId,omitempty"`
	ThreadTS  string `json:"threadTs,omitempty"`
	UserID    string `json:"userId,omitempty"`
	Source    string `json:"source,omitempty"`
}

type PromptRequest struct {
	Text string `json:"text"`
}

type Event struct {
	Type  EventType
	Text  string
	Error error
}

type EventType string

const (
	EventText     EventType = "text"
	EventStatus   EventType = "status"
	EventComplete EventType = "complete"
	EventError    EventType = "error"
)

type ConversationKey struct {
	TeamID    string
	ChannelID string
	ThreadTS  string
	DM        bool
}
