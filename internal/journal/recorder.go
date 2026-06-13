package journal

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Recorder tuning. These bound memory and write latency; observability must
// never backpressure a Slack turn, so a full buffer drops rather than blocks.
const (
	defaultBufferSize    = 1024
	defaultFlushInterval = time.Second
	defaultBatchSize     = 128
)

// recordedEvent pairs an Event with the wall-clock time it was recorded. The
// timestamp is taken at Record (enqueue) time, not at write time, so a batched
// flush does not skew when the event actually happened.
type recordedEvent struct {
	at    time.Time
	event Event
}

// appender is the write surface the recorder needs from a Store. It exists so
// the recorder can be unit-tested against a fake without a real database.
type appender interface {
	appendBatch(ctx context.Context, batch []recordedEvent) error
}

// AsyncRecorder records events without blocking the caller. Record enqueues
// onto a bounded buffer and returns immediately; a single background goroutine
// drains the buffer and writes batched transactions through the appender (the
// single-writer model SQLite wants). Events for disabled streams are dropped at
// enqueue; a full buffer drops too, counting the loss and surfacing it.
type AsyncRecorder struct {
	app           appender
	enabled       map[string]bool
	ch            chan recordedEvent
	done          chan struct{}
	dropped       atomic.Int64
	logger        *slog.Logger
	now           func() time.Time
	flushInterval time.Duration
	batchSize     int
}

// RecorderOption customises an AsyncRecorder. The defaults suit the gateway
// daemon; tests use these to shrink the buffer and clock.
type RecorderOption func(*AsyncRecorder)

// WithBufferSize sets the enqueue buffer capacity.
func WithBufferSize(n int) RecorderOption {
	return func(r *AsyncRecorder) {
		if n > 0 {
			r.ch = make(chan recordedEvent, n)
		}
	}
}

// WithFlushInterval sets how often a partial batch is flushed.
func WithFlushInterval(d time.Duration) RecorderOption {
	return func(r *AsyncRecorder) {
		if d > 0 {
			r.flushInterval = d
		}
	}
}

// WithClock overrides the timestamp source (for deterministic tests).
func WithClock(now func() time.Time) RecorderOption {
	return func(r *AsyncRecorder) {
		if now != nil {
			r.now = now
		}
	}
}

// WithBatchSize sets how many buffered events a single flush writes. Primarily
// for tests; the default suits production.
func WithBatchSize(n int) RecorderOption {
	return func(r *AsyncRecorder) {
		if n > 0 {
			r.batchSize = n
		}
	}
}

// NewRecorder starts an AsyncRecorder writing to store. enabled is the set of
// streams whose events are persisted; an event on any other stream is dropped
// at Record. The background writer runs until Close. A nil logger is replaced
// with a discard logger.
func NewRecorder(store *Store, enabled map[string]bool, logger *slog.Logger, opts ...RecorderOption) *AsyncRecorder {
	return newRecorder(store, enabled, logger, opts...)
}

// newRecorder builds and starts a recorder against any appender (the real
// Store in production, a fake in tests).
func newRecorder(app appender, enabled map[string]bool, logger *slog.Logger, opts ...RecorderOption) *AsyncRecorder {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noopWriter{}, nil))
	}
	if enabled == nil {
		enabled = map[string]bool{}
	}
	r := &AsyncRecorder{
		app:           app,
		enabled:       enabled,
		ch:            make(chan recordedEvent, defaultBufferSize),
		done:          make(chan struct{}),
		logger:        logger,
		now:           time.Now,
		flushInterval: defaultFlushInterval,
		batchSize:     defaultBatchSize,
	}
	for _, opt := range opts {
		opt(r)
	}
	go r.run()
	return r
}

// Record enqueues e for asynchronous persistence. It never blocks: an event on
// a disabled stream is ignored, and a full buffer drops the event and bumps the
// dropped counter (surfaced by the writer). The caller's hot path is untouched.
func (r *AsyncRecorder) Record(_ context.Context, e Event) {
	if e.Stream == "" || !r.enabled[e.Stream] {
		return
	}
	select {
	case r.ch <- recordedEvent{at: r.now(), event: e}:
	default:
		r.dropped.Add(1)
	}
}

// Close stops the writer, draining any buffered events first, and reports a
// final dropped count. It does not close the Store; the composition root closes
// the store after the recorder has drained. Close returns when the writer has
// finished or ctx is done.
func (r *AsyncRecorder) Close(ctx context.Context) error {
	close(r.ch)
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Dropped reports the number of events dropped because the buffer was full
// since the recorder started. Primarily for tests and diagnostics.
func (r *AsyncRecorder) Dropped() int64 {
	return r.dropped.Load()
}

func (r *AsyncRecorder) run() {
	defer close(r.done)
	ticker := time.NewTicker(r.flushInterval)
	defer ticker.Stop()

	batch := make([]recordedEvent, 0, r.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := r.app.appendBatch(context.Background(), batch); err != nil {
			r.logger.Warn("journal: batch insert failed", "error", err, "events", len(batch))
		}
		batch = batch[:0]
	}
	reportDropped := func() {
		if n := r.dropped.Swap(0); n > 0 {
			r.logger.Warn("journal: dropped events (buffer full)", "count", n)
		}
	}

	for {
		select {
		case e, ok := <-r.ch:
			if !ok {
				flush()
				reportDropped()
				return
			}
			batch = append(batch, e)
			if len(batch) >= r.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
			reportDropped()
		}
	}
}

// noopWriter is an io.Writer that discards everything; backs the fallback
// logger so NewRecorder never panics on a nil logger.
type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }
