// Package stats implements the `journal.stats` tool: a per-stream summary of
// the event journal (row counts and the oldest/newest timestamps), handy for
// confirming a stream is recording and seeing its time span at a glance.
package stats

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh-dev-toolkit/internal/journal"
)

// StoreOpener opens the journal store for one invocation.
type StoreOpener func() (*journal.Store, error)

// Tool is the `journal.stats` capability.
type Tool struct {
	open StoreOpener
}

// New constructs the tool with the given store opener.
func New(open StoreOpener) *Tool { return &Tool{open: open} }

// Name returns the registry key.
func (t *Tool) Name() string { return "journal.stats" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Summarise the event journal: row count and oldest/newest timestamp per stream."
}

// InputSchema returns nil — stats takes no parameters.
func (t *Tool) InputSchema() *jsonschema.Schema { return nil }

// Result is the structured payload returned by Invoke.
type Result struct {
	Streams []journal.StreamStat `json:"streams"`
}

// String renders the per-stream counts for the CLI.
func (r Result) String() string {
	var b strings.Builder
	for _, s := range r.Streams {
		span := "empty"
		if s.Count > 0 && s.Oldest != nil && s.Newest != nil {
			span = fmt.Sprintf("%s → %s", s.Oldest.Format(time.RFC3339), s.Newest.Format(time.RFC3339))
		}
		fmt.Fprintf(&b, "%-12s  %6d  %s\n", s.Stream, s.Count, span)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Invoke opens the store and returns the per-stream summary.
func (t *Tool) Invoke(ctx context.Context, _ map[string]any) (any, error) {
	store, err := t.open()
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	defer store.Close()

	streams, err := store.Stats(ctx)
	if err != nil {
		return nil, err
	}
	return Result{Streams: streams}, nil
}
