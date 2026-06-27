package prune

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/journal"
)

func TestPruneRemovesEventsPastRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	retention := map[string]time.Duration{journal.StreamGateway: 24 * time.Hour}

	// Seed one old gateway event (48h ago) and one fresh one. The recorder
	// stamps events at its clock, so an injected past clock backdates them.
	store, err := journal.Open(path, retention)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	old := journal.NewRecorder(store, map[string]bool{journal.StreamGateway: true}, nil,
		journal.WithClock(func() time.Time { return time.Now().Add(-48 * time.Hour) }))
	old.Record(context.Background(), journal.Event{Stream: journal.StreamGateway, Kind: "old", Level: journal.LevelInfo, Summary: "stale"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = old.Close(ctx)
	cancel()

	fresh := journal.NewRecorder(store, map[string]bool{journal.StreamGateway: true}, nil)
	fresh.Record(context.Background(), journal.Event{Stream: journal.StreamGateway, Kind: "new", Level: journal.LevelInfo, Summary: "recent"})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	_ = fresh.Close(ctx2)
	cancel2()
	_ = store.Close()

	tool := New(func() (*journal.Store, error) { return journal.Open(path, retention) })
	res, err := tool.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if r.Total != 1 || r.Removed[journal.StreamGateway] != 1 {
		t.Fatalf("unexpected prune result: %+v", r)
	}

	// The fresh event survives.
	verify, _ := journal.Open(path, retention)
	defer verify.Close()
	got, _ := verify.Query(context.Background(), journal.Query{Stream: journal.StreamGateway})
	if len(got) != 1 || got[0].Summary != "recent" {
		t.Fatalf("expected only the recent event to remain, got %+v", got)
	}
}

func TestPruneNothingToRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	tool := New(func() (*journal.Store, error) {
		return journal.Open(path, map[string]time.Duration{journal.StreamGateway: 24 * time.Hour})
	})
	res, err := tool.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.(Result).Total != 0 {
		t.Fatalf("expected nothing removed, got %+v", res)
	}
}
