package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock so tool-age assertions are deterministic
// rather than wall-clock dependent.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestToolWatcherTracksInFlightTools(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	w := newToolWatcher(clk.now)

	if active, _, _ := w.snapshot(); active != 0 {
		t.Fatalf("empty watcher: active = %d, want 0", active)
	}

	w.observe("t1", "go test", TaskStatusInProgress)
	clk.advance(90 * time.Second)
	w.observe("t2", "grep", TaskStatusInProgress)

	active, title, oldest := w.snapshot()
	if active != 2 {
		t.Fatalf("active = %d, want 2", active)
	}
	if title != "go test" || oldest != 90*time.Second {
		t.Fatalf("oldest = (%q, %s), want (go test, 1m30s)", title, oldest)
	}

	// A terminal status retires the tool.
	w.observe("t1", "", TaskStatusComplete)
	if active, title, _ = w.snapshot(); active != 1 || title != "grep" {
		t.Fatalf("after complete: active=%d title=%q, want 1/grep", active, title)
	}

	// The start time is stamped once: a later title-only refinement keeps the age.
	clk.advance(10 * time.Second)
	w.observe("t2", "grep -r", "")
	if _, title, oldest = w.snapshot(); title != "grep -r" || oldest != 10*time.Second {
		t.Fatalf("after refine: (%q, %s), want (grep -r, 10s)", title, oldest)
	}
}

// runHeartbeat starts c.heartbeat and returns its cancel cause func and channels so
// a test can assert its emissions and shut it down.
func runHeartbeat(c *ProcessClient, w *toolWatcher) (chan Event, context.Context, context.CancelCauseFunc, chan struct{}, chan struct{}) {
	events := make(chan Event, 8)
	ctx, cancel := context.WithCancelCause(context.Background())
	stop := make(chan struct{})
	done := make(chan struct{})
	go c.heartbeat(ctx, w, events, cancel, stop, done)
	return events, ctx, cancel, stop, done
}

func TestHeartbeatEmitsKeepAliveWhileToolRuns(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	w := newToolWatcher(clk.now)
	w.observe("t1", "go test", TaskStatusInProgress)

	c := NewProcessClient(ProcessOptions{ToolHeartbeatInterval: time.Millisecond, ToolCeiling: time.Hour})
	events, _, cancel, stop, done := runHeartbeat(c, w)
	defer cancel(nil)

	select {
	case ev := <-events:
		if ev.Type != EventStatus {
			t.Fatalf("event type = %q, want status", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no keep-alive emitted while a tool was running")
	}
	close(stop)
	<-done
}

func TestHeartbeatCeilingCancelsTurn(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	w := newToolWatcher(clk.now)
	w.observe("t1", "go test", TaskStatusInProgress)
	clk.advance(20 * time.Minute) // past the ceiling

	c := NewProcessClient(ProcessOptions{ToolHeartbeatInterval: time.Millisecond, ToolCeiling: 10 * time.Minute})
	events, ctx, cancel, stop, done := runHeartbeat(c, w)
	defer cancel(nil)
	defer close(stop)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ceiling did not fire")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, ErrToolCeiling) {
		t.Fatalf("cancel cause = %v, want ErrToolCeiling", cause)
	}
	// The tool was already past the ceiling on the first tick, so no keep-alive
	// should have gone out — the turn fails rather than being kept alive.
	select {
	case ev := <-events:
		t.Fatalf("unexpected keep-alive before ceiling: %+v", ev)
	default:
	}
}

func TestHeartbeatSilentWhenNoToolRunning(t *testing.T) {
	w := newToolWatcher(nil) // no tools in flight

	c := NewProcessClient(ProcessOptions{ToolHeartbeatInterval: time.Millisecond, ToolCeiling: time.Hour})
	events, ctx, cancel, stop, done := runHeartbeat(c, w)
	defer cancel(nil)

	// Let several ticks pass; with no tool running the heartbeat must stay silent so
	// a genuinely stalled turn still trips the gateway's idle watchdog.
	time.Sleep(20 * time.Millisecond)
	close(stop)
	<-done

	select {
	case ev := <-events:
		t.Fatalf("expected silence with no tool running, got %+v", ev)
	default:
	}
	if cause := context.Cause(ctx); cause != nil {
		t.Fatalf("context cancelled unexpectedly: %v", cause)
	}
}
