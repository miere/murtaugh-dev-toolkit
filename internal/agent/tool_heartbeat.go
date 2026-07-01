package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// defaultACPToolHeartbeatInterval mirrors the native loop's tool heartbeat: how
	// often a still-running ACP tool call emits a keep-alive status event so the
	// gateway's idle watchdog (which resets on any event) does not mistake a long,
	// output-silent tool for a stalled turn. Must stay well under request_timeout.
	defaultACPToolHeartbeatInterval = 30 * time.Second
	// defaultACPToolCeiling bounds how long a single tool call may hold a turn. The
	// heartbeat suppresses the idle watchdog while a tool runs, so without a ceiling
	// a genuinely wedged tool (waiting on stdin, deadlocked) would hold the turn
	// indefinitely. Past the ceiling the turn is failed with a specific message. A
	// negative ProcessOptions.ToolCeiling disables the ceiling; zero takes this.
	defaultACPToolCeiling = time.Hour
)

// ErrToolCeiling marks a turn aborted because one tool ran past its execution
// ceiling without producing a result. The gateway renders it as a turn failure and
// drops the session binding — the ACP agent may still be running the tool and,
// lacking session/cancel, cannot be told to stop.
var ErrToolCeiling = errors.New("tool exceeded its execution ceiling")

// toolWatcher tracks which of a session's tool calls are currently in flight, so a
// heartbeat can keep a long tool's turn alive and a ceiling can fail a wedged one.
// An ACP tool call announces itself with a non-terminal task status and is retired
// by a terminal one; between the two it produces no events of its own.
type toolWatcher struct {
	mu      sync.Mutex
	running map[string]runningTool
	now     func() time.Time
}

type runningTool struct {
	started time.Time
	title   string
}

func newToolWatcher(now func() time.Time) *toolWatcher {
	if now == nil {
		now = time.Now
	}
	return &toolWatcher{running: make(map[string]runningTool), now: now}
}

// observe folds one task update into the in-flight set: a terminal status retires
// the tool, any other status (in_progress, pending, or a title-only refinement)
// starts or keeps tracking it. The start time is stamped once, on first sighting,
// so the ceiling measures a tool's true age across its later refinements.
func (w *toolWatcher) observe(id, title string, status TaskStatus) {
	if id == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if isTerminalToolStatus(status) {
		delete(w.running, id)
		return
	}
	r, ok := w.running[id]
	if !ok {
		r.started = w.now()
	}
	if title != "" {
		r.title = title
	}
	w.running[id] = r
}

// snapshot reports how many tools are in flight and, for the longest-running one,
// its title and age — the inputs the heartbeat needs to choose between a keep-alive
// and the ceiling.
func (w *toolWatcher) snapshot() (active int, oldestTitle string, oldest time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	active = len(w.running)
	var earliest time.Time
	for _, r := range w.running {
		if earliest.IsZero() || r.started.Before(earliest) {
			earliest = r.started
			oldestTitle = r.title
		}
	}
	if !earliest.IsZero() {
		oldest = w.now().Sub(earliest)
	}
	return active, oldestTitle, oldest
}

func isTerminalToolStatus(status TaskStatus) bool {
	switch status {
	case TaskStatusComplete, TaskStatusFailed, TaskStatusCancelled:
		return true
	default:
		return false
	}
}

// heartbeat keeps a turn alive while a tool legitimately runs, and fails it when a
// tool runs too long. It ticks on interval; on each tick, if a tool is in flight it
// either emits a meta status event (resetting the gateway's idle watchdog, rendered
// as nothing) or, once the longest-running tool passes the ceiling, cancels the
// turn with ErrToolCeiling. When no tool is in flight it emits nothing, so a
// genuinely idle turn (e.g. a wedged provider call with no tool running) still
// trips the idle watchdog exactly as before — the ceiling only governs tools.
func (c *ProcessClient) heartbeat(ctx context.Context, w *toolWatcher, events chan<- Event, cancel context.CancelCauseFunc, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	interval := c.opts.ToolHeartbeatInterval
	if interval <= 0 {
		interval = defaultACPToolHeartbeatInterval
	}
	ceiling := c.opts.ToolCeiling
	if ceiling == 0 {
		ceiling = defaultACPToolCeiling
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			active, title, oldest := w.snapshot()
			if active == 0 {
				continue
			}
			if ceiling > 0 && oldest >= ceiling {
				if title == "" {
					title = "a tool"
				}
				c.log.Warn("ACP tool exceeded its ceiling; aborting turn", "tool", title, "elapsed", oldest.Round(time.Second), "ceiling", ceiling)
				cancel(fmt.Errorf("%w: %q ran for %s with no result", ErrToolCeiling, title, oldest.Round(time.Second)))
				return
			}
			select {
			case events <- Event{Type: EventStatus, Text: "still working…"}:
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}
}
