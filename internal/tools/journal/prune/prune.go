// Package prune implements the `journal.prune` tool: delete events older than
// each stream's configured retention, on demand. The gateway runs the same
// sweep automatically; this tool is the manual escape hatch for an admin
// troubleshooting or reclaiming space between sweeps.
package prune

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh-dev-toolkit/internal/journal"
)

// StoreOpener opens the journal store for one invocation. It must open with the
// configured per-stream retention, since Prune deletes by that.
type StoreOpener func() (*journal.Store, error)

// Tool is the `journal.prune` capability.
type Tool struct {
	open StoreOpener
	now  func() time.Time
}

// New constructs the tool with the given store opener.
func New(open StoreOpener) *Tool { return &Tool{open: open, now: time.Now} }

// Name returns the registry key.
func (t *Tool) Name() string { return "journal.prune" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Delete journal events older than each stream's configured retention (a manual run of the gateway's sweep)."
}

// InputSchema returns nil — prune takes no parameters; it uses the configured
// per-stream retention.
func (t *Tool) InputSchema() *jsonschema.Schema { return nil }

// Result is the structured payload returned by Invoke.
type Result struct {
	Removed map[string]int64 `json:"removed"`
	Total   int64            `json:"total"`
}

// String renders the per-stream removal counts for the CLI.
func (r Result) String() string {
	if r.Total == 0 {
		return "journal prune: nothing to remove"
	}
	streams := make([]string, 0, len(r.Removed))
	for s := range r.Removed {
		streams = append(streams, s)
	}
	sort.Strings(streams)
	var b strings.Builder
	fmt.Fprintf(&b, "journal prune: removed %d event(s)\n", r.Total)
	for _, s := range streams {
		fmt.Fprintf(&b, "  %-12s  %d\n", s, r.Removed[s])
	}
	return strings.TrimRight(b.String(), "\n")
}

// Invoke opens the store and runs an age-based sweep.
func (t *Tool) Invoke(ctx context.Context, _ map[string]any) (any, error) {
	store, err := t.open()
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	defer store.Close()

	res, err := store.Prune(ctx, t.now())
	if err != nil {
		return nil, err
	}
	return Result{Removed: res.Removed, Total: res.Total}, nil
}
