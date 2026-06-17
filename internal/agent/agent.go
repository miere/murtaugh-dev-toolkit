package agent

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

	// Channel and Thread carry the Slack conversation the prompt originates
	// from. ACP has no system/instructions field, so when these are set the
	// ProcessClient prepends a delimited context block to the prompt — the
	// closest equivalent — telling the agent where it is. The agent passes
	// them on to the `restart` tool so the approval card is asked in this
	// conversation rather than the admin DM. Empty for non-chat callers.
	Channel string `json:"channel,omitempty"`
	Thread  string `json:"thread,omitempty"`

	// History carries a pre-rendered transcript of the Slack thread that
	// preceded this prompt. ACP's session/prompt is a single user turn with no
	// way to replay prior turns, so when a brand-new session is opened for an
	// existing thread the gateway flattens the backstory into this opaque text
	// block (already framed and author-labelled) and the ProcessClient emits it
	// as its own content block ahead of the user's message. Empty when the
	// session is warm (the agent already holds the history) or the thread is
	// new.
	History string `json:"history,omitempty"`
}

type Event struct {
	Type EventType
	Text string
	// StopReason is the agent's reported reason for ending a turn, carried on
	// EventComplete (e.g. "end_turn", "max_tokens", "refusal"). Empty when the
	// agent did not report one. The chat handler surfaces a non-"end_turn"
	// reason to the user so a turn that ends without a reply is not silent.
	StopReason string
	Error      error
	Task       *TaskEvent
}

type EventType string

const (
	EventText     EventType = "text"
	EventStatus   EventType = "status"
	EventComplete EventType = "complete"
	EventError    EventType = "error"
	EventTask     EventType = "task"
)

type TaskStatus string

const (
	TaskStatusPending    TaskStatus = "pending"
	TaskStatusInProgress TaskStatus = "in_progress"
	TaskStatusComplete   TaskStatus = "complete"
	TaskStatusFailed     TaskStatus = "failed"
	TaskStatusCancelled  TaskStatus = "cancelled"
)

type TaskEvent struct {
	ID          string
	Title       string
	Status      TaskStatus
	Description string
	Output      string
}

type ConversationKey struct {
	TeamID    string
	ChannelID string
	ThreadTS  string
	DM        bool
}
