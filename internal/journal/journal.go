// Package journal is Murtaugh's agent-facing event store: a structured,
// queryable record of domain events that an AI agent reads back to make
// decisions. It is deliberately distinct from operational logging (slog →
// stderr): slog is for a human tailing the daemon, the journal is for an agent
// issuing filtered queries (Gateway Debug Mode, persistent ACP session logs).
//
// Events are grouped into logical streams (gateway, job, acp_session) within a
// single SQLite database. Writes go through an asynchronous Recorder that never
// blocks the caller; reads and maintenance go through Store. See
// specs-wip/011 for the full design.
package journal

import (
	"context"
	"encoding/json"
	"time"
)

// Stream identifiers. A stream is the logical partition of the events table,
// enabled and retained independently via journal.yaml. These are duplicated as
// string literals in internal/config (which must not import this package).
const (
	StreamGateway    = "gateway"
	StreamJob        = "job"
	StreamACPSession = "acp_session"
)

// Streams lists every known stream in a stable order. Used by validation and
// the stats tool so a stream with no rows still reports zero rather than
// vanishing.
var Streams = []string{StreamGateway, StreamJob, StreamACPSession}

// Level is the severity of an event. Levels are ordered (debug < info < warn <
// error); Query filters by "at least this level".
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// rank maps a level to its ordinal for >= comparisons. Unknown values sort as
// info so a malformed level never hides an event from an error-only query by
// accident.
func (l Level) rank() int {
	switch l {
	case LevelDebug:
		return 0
	case LevelWarn:
		return 2
	case LevelError:
		return 3
	default: // info and anything unrecognised
		return 1
	}
}

// Keys are the correlation columns carried on every event. All are optional and
// populated only when known for that event; they are what make a stream
// queryable ("failed workflow events in this channel", "every event for this
// ACP session"). CorrID (on Event, not here) ties one interaction's events
// together.
type Keys struct {
	TeamID    string `json:"team_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	JobName   string `json:"job_name,omitempty"`
	RuleID    string `json:"rule_id,omitempty"`
}

// Event is what a domain package hands to Recorder.Record. Stream, Kind, Level,
// and Summary are required; the rest are filled when known. Payload is any
// JSON-marshalable value (stream/kind-specific detail) and is capped on write
// (see maxPayloadBytes); BlobRef points at a file holding a large body the row
// only indexes.
type Event struct {
	Stream  string
	Kind    string
	Level   Level
	Summary string
	CorrID  string
	Keys    Keys
	Payload any
	BlobRef string
}

// Record is a stored event read back by Query. It mirrors Event with the
// assigned id and resolved timestamp, and carries Payload as raw JSON so the
// MCP frontend can pass it through untouched.
type Record struct {
	ID      int64           `json:"id"`
	Time    time.Time       `json:"time"`
	Stream  string          `json:"stream"`
	Kind    string          `json:"kind"`
	Level   Level           `json:"level"`
	CorrID  string          `json:"corr_id,omitempty"`
	Keys    Keys            `json:"keys"`
	Summary string          `json:"summary"`
	Payload json.RawMessage `json:"payload,omitempty"`
	BlobRef string          `json:"blob_ref,omitempty"`
}

// Query is the filter set for Store.Query. Every field is optional and ANDed
// together; the zero Query returns the most recent events across all streams up
// to the default limit. Level filters by "at least this severity". Since/Until
// are inclusive time bounds (zero means unbounded).
type Query struct {
	Stream    string
	Kind      string
	Level     Level
	ChannelID string
	UserID    string
	SessionID string
	CorrID    string
	RuleID    string
	Since     time.Time
	Until     time.Time
	Limit     int
}

// StreamStat summarises one stream for the stats tool.
type StreamStat struct {
	Stream string     `json:"stream"`
	Count  int64      `json:"count"`
	Oldest *time.Time `json:"oldest,omitempty"`
	Newest *time.Time `json:"newest,omitempty"`
}

// PruneResult reports what an age-based sweep removed, per stream.
type PruneResult struct {
	Removed map[string]int64 `json:"removed"`
	Total   int64            `json:"total"`
}

// Query result bounds. A query with no explicit limit returns the most recent
// defaultQueryLimit events; an explicit limit is capped at maxQueryLimit so an
// agent cannot pull an unbounded result set.
const (
	defaultQueryLimit = 50
	maxQueryLimit     = 500
)

// effectiveLimit resolves the LIMIT to apply: the default when unset, capped at
// the maximum.
func effectiveLimit(n int) int {
	switch {
	case n <= 0:
		return defaultQueryLimit
	case n > maxQueryLimit:
		return maxQueryLimit
	default:
		return n
	}
}

// Recorder is the write seam every domain package depends on. Implementations
// must be safe for concurrent use and must never block the caller: a disabled
// stream or a full buffer drops the event rather than waiting. Domain code
// calls Record unconditionally — the "is this stream enabled?" decision lives
// here, not at the call site.
type Recorder interface {
	Record(ctx context.Context, e Event)
}

// NopRecorder discards every event. It backs disabled streams and the
// degraded path when the database cannot be opened, so callers need no nil
// checks.
type NopRecorder struct{}

// Record implements Recorder by doing nothing.
func (NopRecorder) Record(context.Context, Event) {}
