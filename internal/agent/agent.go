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

// Aggregator hands an ACP session the MCP server it should connect to in order
// to reach Murtaugh's own tools (built-ins plus proxied external MCP). The
// concrete implementation lives above this package (it needs the tool registry,
// the MCP client, and the bridge); ProcessClient only knows this seam, so the
// agent package stays free of those dependencies. nil means the agent is told
// about no Murtaugh MCP server (the prior behaviour).
type Aggregator interface {
	// RegisterSession binds a session (its Slack location drives where approval
	// prompts are asked) and returns the MCP server spec to advertise in
	// session/new, plus a release to call when the session ends. An error means
	// no server is advertised; the agent simply gets no Murtaugh tools.
	RegisterSession(meta SessionMetadata) (server MCPServerSpec, release func(), err error)
}

// MCPServerSpec is the stdio MCP server an ACP agent is asked to spawn — the
// `murtaugh mcp-bridge` proxy. It maps onto ACP's stdio McpServer shape in
// session/new (name + command + args + env).
type MCPServerSpec struct {
	Name    string
	Command string
	Args    []string
	// Env are the KEY=VALUE pairs the agent sets on the spawned bridge. Rendered
	// to ACP's [{name,value}] env shape in stable key order.
	Env map[string]string
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
	// Attachment carries a file the agent wants delivered to the user as a
	// first-class part of its reply (EventAttachment), independent of the text
	// stream. Both backends emit it: the native loop when a tool yields a file,
	// the ACP client when an agent message carries a binary content block. The
	// chat handler uploads it into the turn's thread.
	Attachment *AttachmentEvent
}

type EventType string

const (
	EventText       EventType = "text"
	EventStatus     EventType = "status"
	EventComplete   EventType = "complete"
	EventError      EventType = "error"
	EventTask       EventType = "task"
	EventAttachment EventType = "attachment"
)

// AttachmentEvent is a file the agent is sending to the user as part of its
// reply. It is the backend-neutral carrier consumed by the Slack chat handler,
// which uploads it into the turn's thread. The bytes come from exactly one of
// two sources: Path (a file on the daemon host, produced by an in-process
// native tool) or Data (in-memory bytes decoded from an ACP content block).
// When both are set Data wins; when neither is set the attachment is dropped.
type AttachmentEvent struct {
	// Filename is the suggested download name shown to the user. When empty the
	// handler derives one from the path or mimetype.
	Filename string
	// Title is an optional display title; defaults to the filename when empty.
	Title string
	// Comment is an optional message posted alongside the file (the caption).
	Comment string
	// Mimetype is the optional content type, used to derive a filename extension
	// when Filename is empty.
	Mimetype string
	// Path is a file on the daemon host to upload. Used by native tools.
	Path string
	// Data is the in-memory file content to upload. Used by ACP attachments.
	Data []byte
}

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

// TurnLocation is the Slack conversation a turn is running in. The native client
// stashes it on the per-turn context so interactive tools (e.g. `ask`) can post
// their prompt into the same thread — reliably, without depending on the model to
// pass the channel/thread as arguments.
type TurnLocation struct {
	ChannelID string
	ThreadTS  string
}

type turnLocationKey struct{}

// WithTurnLocation returns ctx carrying loc.
func WithTurnLocation(ctx context.Context, loc TurnLocation) context.Context {
	return context.WithValue(ctx, turnLocationKey{}, loc)
}

// TurnLocationFromContext returns the Slack location stashed on ctx, if any. ok
// is false when the turn carries no usable location (e.g. CLI/MCP callers), so
// interactive tools can refuse to run rather than block forever.
func TurnLocationFromContext(ctx context.Context) (TurnLocation, bool) {
	loc, ok := ctx.Value(turnLocationKey{}).(TurnLocation)
	return loc, ok && loc.ChannelID != ""
}

// PermissionOption is one choice an ACP agent offers for a session/request_permission
// request. Kind is the ACP PermissionOptionKind ("allow_once", "allow_always",
// "reject_once", "reject_always"); ID is the optionId echoed back in the response.
type PermissionOption struct {
	ID   string
	Name string
	Kind string
}

// PermissionRequest is an agent-initiated session/request_permission: the agent is
// about to use a tool (ToolName) and wants the client to pick one of Options.
type PermissionRequest struct {
	SessionID string
	ToolName  string
	Options   []PermissionOption
}

// PermissionAsker resolves an ACP permission request by getting a human decision
// (e.g. via Slack buttons). It returns the chosen option's ID, or "" when no option
// was chosen (timeout / dismissal), which the ACP client maps to a "cancelled"
// outcome. Implemented in internal/slack/interaction; nil on headless/CLI paths.
type PermissionAsker interface {
	AskPermission(ctx context.Context, loc TurnLocation, req PermissionRequest) (optionID string, err error)
}
