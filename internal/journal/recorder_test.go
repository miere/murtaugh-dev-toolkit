package journal

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeAppender records the batches handed to it and can optionally block until
// released, to exercise the drop-on-full path deterministically.
type fakeAppender struct {
	mu      sync.Mutex
	events  []Event
	block   chan struct{} // when non-nil, appendBatch waits on it before returning
	blocked chan struct{} // closed the first time appendBatch is entered
}

func (f *fakeAppender) appendBatch(_ context.Context, batch []recordedEvent) error {
	f.mu.Lock()
	if f.blocked != nil {
		select {
		case <-f.blocked:
		default:
			close(f.blocked)
		}
	}
	for _, re := range batch {
		f.events = append(f.events, re.event)
	}
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return nil
}

func (f *fakeAppender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

func TestRecorderDrainsOnClose(t *testing.T) {
	app := &fakeAppender{}
	rec := newRecorder(app, map[string]bool{StreamGateway: true}, nil)

	for i := 0; i < 10; i++ {
		rec.Record(context.Background(), Event{Stream: StreamGateway, Kind: "k", Level: LevelInfo, Summary: "e"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rec.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if app.count() != 10 {
		t.Fatalf("appended %d, want 10 (all drained on close)", app.count())
	}
}

func TestRecorderDropsDisabledStream(t *testing.T) {
	app := &fakeAppender{}
	rec := newRecorder(app, map[string]bool{StreamGateway: true}, nil)

	rec.Record(context.Background(), Event{Stream: StreamGateway, Kind: "k", Level: LevelInfo, Summary: "kept"})
	rec.Record(context.Background(), Event{Stream: StreamJob, Kind: "k", Level: LevelInfo, Summary: "dropped"})
	rec.Record(context.Background(), Event{Stream: "", Kind: "k", Level: LevelInfo, Summary: "no stream"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rec.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if app.count() != 1 {
		t.Fatalf("appended %d, want 1 (disabled/blank streams ignored)", app.count())
	}
	if app.events[0].Summary != "kept" {
		t.Fatalf("kept the wrong event: %q", app.events[0].Summary)
	}
}

func TestRecorderDropsWhenBufferFull(t *testing.T) {
	app := &fakeAppender{
		block:   make(chan struct{}),
		blocked: make(chan struct{}),
	}
	// Tiny buffer + batchSize 1 so the first event flushes immediately and the
	// writer parks inside the blocked appendBatch; further records overflow.
	rec := newRecorder(app, map[string]bool{StreamGateway: true}, nil,
		WithBufferSize(1), WithBatchSize(1))

	// Wait until the writer is parked in appendBatch so the buffer state is
	// deterministic before we flood it.
	for i := 0; i < 200; i++ {
		rec.Record(context.Background(), Event{Stream: StreamGateway, Kind: "k", Level: LevelInfo, Summary: "e"})
	}
	<-app.blocked

	// Flood far beyond what the buffer + in-flight event can hold.
	for i := 0; i < 500; i++ {
		rec.Record(context.Background(), Event{Stream: StreamGateway, Kind: "k", Level: LevelInfo, Summary: "e"})
	}
	if rec.Dropped() == 0 {
		t.Fatalf("expected some events dropped under a full buffer, got 0")
	}

	close(app.block) // unblock the writer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rec.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNopRecorder(t *testing.T) {
	// Compile-time and runtime check that NopRecorder satisfies Recorder and
	// never panics.
	var r Recorder = NopRecorder{}
	r.Record(context.Background(), Event{Stream: StreamGateway, Summary: "ignored"})
}
