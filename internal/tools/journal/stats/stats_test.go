package stats

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/journal"
)

func TestStatsReportsPerStreamCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	store, err := journal.Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rec := journal.NewRecorder(store, map[string]bool{journal.StreamGateway: true}, nil)
	rec.Record(context.Background(), journal.Event{Stream: journal.StreamGateway, Kind: "a", Level: journal.LevelInfo, Summary: "1"})
	rec.Record(context.Background(), journal.Event{Stream: journal.StreamGateway, Kind: "b", Level: journal.LevelInfo, Summary: "2"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = rec.Close(ctx)
	_ = store.Close()

	tool := New(func() (*journal.Store, error) { return journal.Open(path, nil) })
	res, err := tool.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	// Every known stream is reported, even empty ones.
	if len(r.Streams) != len(journal.Streams) {
		t.Fatalf("got %d streams, want %d", len(r.Streams), len(journal.Streams))
	}
	byStream := map[string]journal.StreamStat{}
	for _, s := range r.Streams {
		byStream[s.Stream] = s
	}
	if byStream[journal.StreamGateway].Count != 2 {
		t.Fatalf("gateway count = %d, want 2", byStream[journal.StreamGateway].Count)
	}
	if byStream[journal.StreamJob].Count != 0 {
		t.Fatalf("job count = %d, want 0", byStream[journal.StreamJob].Count)
	}
}
