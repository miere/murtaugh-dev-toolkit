package journal

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// openTestStore opens a journal in a temp directory with the given retention.
func openTestStore(t *testing.T, retention map[string]time.Duration) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	store, err := Open(path, retention)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seed inserts events at the given wall-clock time via the write path.
func seed(t *testing.T, s *Store, at time.Time, events ...Event) {
	t.Helper()
	batch := make([]recordedEvent, 0, len(events))
	for _, e := range events {
		batch = append(batch, recordedEvent{at: at, event: e})
	}
	if err := s.appendBatch(context.Background(), batch); err != nil {
		t.Fatalf("appendBatch: %v", err)
	}
}

func TestStoreQueryFilters(t *testing.T) {
	s := openTestStore(t, nil)
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	seed(t, s, base,
		Event{Stream: StreamGateway, Kind: "workflow.matched", Level: LevelInfo, Summary: "matched rule a",
			CorrID: "gw_1", Keys: Keys{ChannelID: "C1", RuleID: "rule-a"}},
		Event{Stream: StreamGateway, Kind: "workflow.render", Level: LevelError, Summary: "render failed",
			CorrID: "gw_1", Keys: Keys{ChannelID: "C1", RuleID: "rule-a"},
			Payload: map[string]any{"error": "missing key"}},
		Event{Stream: StreamGateway, Kind: "unfurl.post", Level: LevelInfo, Summary: "posted",
			Keys: Keys{ChannelID: "C2"}},
		Event{Stream: StreamJob, Kind: "job.run", Level: LevelWarn, Summary: "job slow",
			Keys: Keys{JobName: "nightly"}},
	)

	tests := []struct {
		name  string
		query Query
		want  int
	}{
		{"all", Query{}, 4},
		{"by stream", Query{Stream: StreamGateway}, 3},
		{"by channel", Query{ChannelID: "C1"}, 2},
		{"by corr id", Query{CorrID: "gw_1"}, 2},
		{"by rule", Query{RuleID: "rule-a"}, 2},
		{"by kind", Query{Kind: "job.run"}, 1},
		{"level at least warn", Query{Level: LevelWarn}, 2},   // error + warn
		{"level at least error", Query{Level: LevelError}, 1}, // error only
		{"stream + level error", Query{Stream: StreamGateway, Level: LevelError}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Query(context.Background(), tc.query)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d records, want %d", len(got), tc.want)
			}
		})
	}
}

func TestStoreQueryOrderingAndPayload(t *testing.T) {
	s := openTestStore(t, nil)
	t0 := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	seed(t, s, t0, Event{Stream: StreamGateway, Kind: "a", Level: LevelInfo, Summary: "older"})
	seed(t, s, t0.Add(time.Minute), Event{Stream: StreamGateway, Kind: "b", Level: LevelError, Summary: "newer",
		Payload: map[string]any{"n": 42}})

	got, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	// Most-recent first.
	if got[0].Summary != "newer" || got[1].Summary != "older" {
		t.Fatalf("unexpected ordering: %q then %q", got[0].Summary, got[1].Summary)
	}
	if !got[0].Time.Equal(t0.Add(time.Minute)) {
		t.Fatalf("unexpected time: %v", got[0].Time)
	}
	var payload map[string]any
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
		t.Fatalf("payload not valid JSON: %v", err)
	}
	if payload["n"].(float64) != 42 {
		t.Fatalf("unexpected payload: %v", payload)
	}
}

func TestStoreQueryLimit(t *testing.T) {
	s := openTestStore(t, nil)
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		seed(t, s, base.Add(time.Duration(i)*time.Second),
			Event{Stream: StreamGateway, Kind: "k", Level: LevelInfo, Summary: "e"})
	}
	got, err := s.Query(context.Background(), Query{Limit: 3})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3 (limit honoured)", len(got))
	}
}

func TestStoreStats(t *testing.T) {
	s := openTestStore(t, nil)
	t0 := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	seed(t, s, t0, Event{Stream: StreamGateway, Kind: "a", Level: LevelInfo, Summary: "1"})
	seed(t, s, t0.Add(time.Hour), Event{Stream: StreamGateway, Kind: "b", Level: LevelInfo, Summary: "2"})

	stats, err := s.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	// Every known stream is reported, even empty ones.
	if len(stats) != len(Streams) {
		t.Fatalf("got %d stats, want %d", len(stats), len(Streams))
	}
	byStream := map[string]StreamStat{}
	for _, st := range stats {
		byStream[st.Stream] = st
	}
	gw := byStream[StreamGateway]
	if gw.Count != 2 {
		t.Fatalf("gateway count = %d, want 2", gw.Count)
	}
	if gw.Oldest == nil || !gw.Oldest.Equal(t0) {
		t.Fatalf("gateway oldest = %v, want %v", gw.Oldest, t0)
	}
	if gw.Newest == nil || !gw.Newest.Equal(t0.Add(time.Hour)) {
		t.Fatalf("gateway newest = %v", gw.Newest)
	}
	if job := byStream[StreamJob]; job.Count != 0 || job.Oldest != nil {
		t.Fatalf("empty stream should report zero, got %+v", job)
	}
}

func TestStorePruneByStreamRetention(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	s := openTestStore(t, map[string]time.Duration{
		StreamGateway: 24 * time.Hour,
		// job has no retention entry → never pruned.
	})

	seed(t, s, now.Add(-48*time.Hour), Event{Stream: StreamGateway, Kind: "old", Level: LevelInfo, Summary: "old gw"})
	seed(t, s, now.Add(-1*time.Hour), Event{Stream: StreamGateway, Kind: "new", Level: LevelInfo, Summary: "new gw"})
	seed(t, s, now.Add(-72*time.Hour), Event{Stream: StreamJob, Kind: "old", Level: LevelInfo, Summary: "old job"})

	res, err := s.Prune(context.Background(), now)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.Removed[StreamGateway] != 1 || res.Total != 1 {
		t.Fatalf("unexpected prune result: %+v", res)
	}

	// The old gateway row is gone; the recent gateway row and the unconfigured
	// job row survive.
	gw, _ := s.Query(context.Background(), Query{Stream: StreamGateway})
	if len(gw) != 1 || gw[0].Summary != "new gw" {
		t.Fatalf("gateway survivors = %+v", gw)
	}
	job, _ := s.Query(context.Background(), Query{Stream: StreamJob})
	if len(job) != 1 {
		t.Fatalf("job stream should be untouched, got %d", len(job))
	}
}

func TestStorePayloadTruncation(t *testing.T) {
	s := openTestStore(t, nil)
	big := make([]byte, maxPayloadBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	seed(t, s, time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC),
		Event{Stream: StreamGateway, Kind: "big", Level: LevelInfo, Summary: "huge", Payload: string(big)})

	got, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var marker map[string]any
	if err := json.Unmarshal(got[0].Payload, &marker); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if marker["_truncated"] != true {
		t.Fatalf("expected truncation marker, got %v", marker)
	}
}
