package query

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/journal"
)

// seedStore writes the given events to a fresh journal at a temp path and
// returns an opener for it.
func seedStore(t *testing.T, events ...journal.Event) StoreOpener {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	store, err := journal.Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	enabled := map[string]bool{journal.StreamGateway: true, journal.StreamJob: true, journal.StreamACPSession: true}
	rec := journal.NewRecorder(store, enabled, nil)
	for _, e := range events {
		rec.Record(context.Background(), e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rec.Close(ctx); err != nil {
		t.Fatalf("recorder close: %v", err)
	}
	_ = store.Close()
	return func() (*journal.Store, error) { return journal.Open(path, nil) }
}

func TestQueryFilters(t *testing.T) {
	open := seedStore(t,
		journal.Event{Stream: journal.StreamGateway, Kind: "workflow.matched", Level: journal.LevelInfo, Summary: "matched", CorrID: "gw_1", Keys: journal.Keys{ChannelID: "C1", RuleID: "r"}},
		journal.Event{Stream: journal.StreamGateway, Kind: "workflow.trigger", Level: journal.LevelError, Summary: "boom", CorrID: "gw_1", Keys: journal.Keys{ChannelID: "C1", RuleID: "r"}},
		journal.Event{Stream: journal.StreamJob, Kind: "job.run", Level: journal.LevelInfo, Summary: "ran", Keys: journal.Keys{JobName: "nightly"}},
	)
	tool := New(open)

	tests := []struct {
		name string
		args map[string]any
		want int
	}{
		{"all", map[string]any{}, 3},
		{"stream gateway", map[string]any{"stream": journal.StreamGateway}, 2},
		{"level error", map[string]any{"level": "error"}, 1},
		{"corr id", map[string]any{"corr_id": "gw_1"}, 2},
		{"limit", map[string]any{"limit": 1}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tool.Invoke(context.Background(), tc.args)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			r := res.(Result)
			if r.Count != tc.want {
				t.Fatalf("count = %d, want %d", r.Count, tc.want)
			}
		})
	}
}

func TestQueryStringRendersEvents(t *testing.T) {
	open := seedStore(t, journal.Event{Stream: journal.StreamGateway, Kind: "workflow.trigger", Level: journal.LevelError, Summary: "render failed", Keys: journal.Keys{RuleID: "code-review"}})
	res, err := New(open).Invoke(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := res.(Result).String()
	for _, want := range []string{"gateway/workflow.trigger", "[code-review]", "render failed", "error"} {
		if !strings.Contains(out, want) {
			t.Fatalf("String() = %q, missing %q", out, want)
		}
	}
}

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	if got, _ := parseTime("", now); !got.IsZero() {
		t.Fatalf("empty should be zero, got %v", got)
	}
	if got, err := parseTime("2h", now); err != nil || !got.Equal(now.Add(-2*time.Hour)) {
		t.Fatalf("duration parse = %v err=%v", got, err)
	}
	if got, err := parseTime("2026-06-13T10:00:00Z", now); err != nil || !got.Equal(time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("rfc3339 parse = %v err=%v", got, err)
	}
	if _, err := parseTime("garbage", now); err == nil {
		t.Fatalf("expected error for garbage time")
	}
}
