// Package query implements the `journal.query` tool: read structured events
// back out of the journal with filters. It is the workhorse of Gateway Debug
// Mode — the gateway chatbot (and any admin) uses it to answer "what happened
// with this interaction?" by filtering on stream, channel, correlation id,
// rule, severity, and time window.
package query

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/journal"
)

// StoreOpener opens the journal store for one invocation. The composition root
// supplies a closure over the configured path; the tool opens per-invoke and
// closes when done, so it never holds a handle and reads run as their own
// connection (WAL lets them proceed while the daemon writes).
type StoreOpener func() (*journal.Store, error)

// Tool is the `journal.query` capability.
type Tool struct {
	open StoreOpener
	now  func() time.Time
}

// New constructs the tool with the given store opener.
func New(open StoreOpener) *Tool {
	return &Tool{open: open, now: time.Now}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "journal.query" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Query the event journal with filters (stream, kind, level, channel, user, session, corr-id, rule, since/until)."
}

// InputSchema documents the filters. All are optional and ANDed together.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"stream":  {Type: "string", Description: "Stream to filter: gateway, job, or acp_session."},
			"kind":    {Type: "string", Description: "Exact event kind, e.g. workflow.trigger, unfurl.render, job.run."},
			"level":   {Type: "string", Description: "Minimum severity (at least): debug, info, warn, error."},
			"channel": {Type: "string", Description: "Slack channel ID to scope to."},
			"user":    {Type: "string", Description: "Slack user ID to scope to."},
			"session": {Type: "string", Description: "ACP session ID to scope to."},
			"corr_id": {Type: "string", Description: "Correlation id — pull every event from one interaction."},
			"rule":    {Type: "string", Description: "Workflow or unfurl rule name."},
			"since":   {Type: "string", Description: "Lower time bound: a Go duration ago (e.g. 2h) or an RFC3339 timestamp."},
			"until":   {Type: "string", Description: "Upper time bound: a Go duration ago (e.g. 5m) or an RFC3339 timestamp."},
			"limit":   {Type: "integer", Description: "Max events to return (default 50, capped at 500), most recent first."},
		},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	Count  int              `json:"count"`
	Events []journal.Record `json:"events"`
}

// String renders the events as compact one-line entries for the CLI.
func (r Result) String() string {
	if r.Count == 0 {
		return "no matching journal events"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d event(s):\n", r.Count)
	for _, e := range r.Events {
		ctx := scopeLabel(e)
		fmt.Fprintf(&b, "%s  %-5s  %s/%s%s  — %s\n",
			e.Time.Format(time.RFC3339), e.Level, e.Stream, e.Kind, ctx, e.Summary)
	}
	return strings.TrimRight(b.String(), "\n")
}

// scopeLabel surfaces the most useful correlation key for a one-line view.
func scopeLabel(e journal.Record) string {
	switch {
	case e.Keys.RuleID != "":
		return " [" + e.Keys.RuleID + "]"
	case e.Keys.JobName != "":
		return " [" + e.Keys.JobName + "]"
	case e.Keys.ChannelID != "":
		return " [" + e.Keys.ChannelID + "]"
	default:
		return ""
	}
}

// Invoke parses the filters, opens the store, and returns the matching events.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	q := journal.Query{
		Stream:    str(args["stream"]),
		Kind:      str(args["kind"]),
		Level:     journal.Level(str(args["level"])),
		ChannelID: str(args["channel"]),
		UserID:    str(args["user"]),
		SessionID: str(args["session"]),
		CorrID:    str(args["corr_id"]),
		RuleID:    str(args["rule"]),
		Limit:     toInt(args["limit"]),
	}
	now := t.now()
	since, err := parseTime(str(args["since"]), now)
	if err != nil {
		return nil, fmt.Errorf("invalid --since: %w", err)
	}
	until, err := parseTime(str(args["until"]), now)
	if err != nil {
		return nil, fmt.Errorf("invalid --until: %w", err)
	}
	q.Since, q.Until = since, until

	store, err := t.open()
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	defer store.Close()

	records, err := store.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	return Result{Count: len(records), Events: records}, nil
}

// parseTime resolves a time bound: empty → zero (unbounded); a Go duration →
// that long before now; otherwise an RFC3339 timestamp.
func parseTime(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither a duration (e.g. 2h) nor an RFC3339 timestamp", s)
	}
	return t, nil
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// toInt coerces a limit arg that may arrive as int (CLI) or float64 (MCP JSON).
func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}
