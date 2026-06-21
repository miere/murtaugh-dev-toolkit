package gateway

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/journal"
)

func TestSocketStalled(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name        string
		last        time.Time
		now         time.Time
		timeout     time.Duration
		wantStalled bool
	}{
		{"fresh", base, base.Add(5 * time.Second), 10 * time.Minute, false},
		{"exactly at timeout is not stalled", base, base.Add(10 * time.Minute), 10 * time.Minute, false},
		{"past timeout is stalled", base, base.Add(10*time.Minute + time.Second), 10 * time.Minute, true},
		{"long silence", base, base.Add(2 * time.Hour), 10 * time.Minute, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := socketStalled(tt.now, tt.last, tt.timeout)
			if got != tt.wantStalled {
				t.Fatalf("socketStalled = %v, want %v", got, tt.wantStalled)
			}
		})
	}
}

func TestStampActivityRoundTrips(t *testing.T) {
	now := time.Unix(1_700_000_123, 456)
	a := &Gateway{now: func() time.Time { return now }}
	a.stampActivity()
	if got := a.lastActivity(); !got.Equal(now) {
		t.Fatalf("lastActivity = %v, want %v", got, now)
	}
}

func TestHeartbeatOKTrueWithoutWebClient(t *testing.T) {
	a := &Gateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if !a.heartbeatOK(context.Background()) {
		t.Fatal("heartbeatOK should be true (no-op) when no Web API client is wired")
	}
}

func TestRecordConnectionEmitsGatewayConnectionEvent(t *testing.T) {
	spy := &journalSpy{}
	a := &Gateway{recorder: spy, now: time.Now}

	a.recordConnection(context.Background(), journal.LevelWarn, "stalled", "recycling", map[string]any{"silent_ms": int64(42)})

	evs := spy.byKind("connection")
	if len(evs) != 1 {
		t.Fatalf("expected 1 connection event, got %d", len(evs))
	}
	e := evs[0]
	if e.Stream != journal.StreamGateway {
		t.Errorf("stream = %q, want %q", e.Stream, journal.StreamGateway)
	}
	if e.Level != journal.LevelWarn {
		t.Errorf("level = %q, want warn", e.Level)
	}
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", e.Payload)
	}
	if payload["state"] != "stalled" {
		t.Errorf("state = %v, want stalled", payload["state"])
	}
	if payload["silent_ms"] != int64(42) {
		t.Errorf("silent_ms = %v, want 42", payload["silent_ms"])
	}
}

// TestRunConnectionWatchdogRecyclesOnSilence drives the real watchdog goroutine
// with a frozen-but-stale clock so the silence check trips on the first tick and
// forces a reconnect (the cancel func), proving the wiring end to end.
func TestRunConnectionWatchdogRecyclesOnSilence(t *testing.T) {
	spy := &journalSpy{}
	// last activity is far in the past relative to now ⇒ immediately stalled.
	stale := time.Unix(1_000_000, 0)
	a := &Gateway{
		recorder: spy,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:      func() time.Time { return stale.Add(time.Hour) },
	}
	a.lastActivityNano.Store(stale.UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// Fast silence tick; heartbeat far in the future so only the silence
		// path can fire.
		a.runConnectionWatchdog(ctx, cancel, time.Millisecond, time.Hour)
		close(done)
	}()

	select {
	case <-ctx.Done():
		// forced reconnect fired
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("watchdog did not force a reconnect on a stalled socket")
	}

	<-done
	if got := spy.byKind("connection"); len(got) == 0 || got[0].Payload.(map[string]any)["state"] != "stalled" {
		t.Fatalf("expected a 'stalled' connection event, got %#v", spy.byKind("connection"))
	}
}
