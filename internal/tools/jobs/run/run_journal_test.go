package run

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/miere/murtaugh-dev-toolkit/internal/journal"
)

type fakeRecorder struct {
	mu     sync.Mutex
	events []journal.Event
}

func (f *fakeRecorder) Record(_ context.Context, e journal.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func (f *fakeRecorder) only(t *testing.T) journal.Event {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) != 1 {
		t.Fatalf("expected exactly one event, got %d", len(f.events))
	}
	return f.events[0]
}

func TestInvoke_RecordsCommandSuccess(t *testing.T) {
	rec := &fakeRecorder{}
	tl := NewWith(lookupFrom(map[string]config.JobProfile{
		"hello": {Command: "/bin/echo", Args: []string{"hi"}},
	}), &bytes.Buffer{}, &bytes.Buffer{}).WithRecorder(rec)

	if _, err := tl.Invoke(context.Background(), map[string]any{"name": "hello"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	e := rec.only(t)
	if e.Stream != journal.StreamJob || e.Kind != "job.run" || e.Level != journal.LevelInfo {
		t.Fatalf("unexpected event envelope: %+v", e)
	}
	if e.Keys.JobName != "hello" {
		t.Fatalf("job name key = %q, want hello", e.Keys.JobName)
	}
	payload, ok := e.Payload.(map[string]any)
	if !ok || payload["exit_code"] != 0 {
		t.Fatalf("unexpected payload: %+v", e.Payload)
	}
}

func TestInvoke_RecordsNonZeroExitAsError(t *testing.T) {
	rec := &fakeRecorder{}
	tl := NewWith(lookupFrom(map[string]config.JobProfile{
		"fail": {Command: "/bin/sh", Args: []string{"-c", "exit 3"}},
	}), &bytes.Buffer{}, &bytes.Buffer{}).WithRecorder(rec)

	if _, err := tl.Invoke(context.Background(), map[string]any{"name": "fail"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	e := rec.only(t)
	if e.Level != journal.LevelError {
		t.Fatalf("non-zero exit should record error level, got %v", e.Level)
	}
	if payload, ok := e.Payload.(map[string]any); !ok || payload["exit_code"] != 3 {
		t.Fatalf("unexpected payload: %+v", e.Payload)
	}
}

func TestInvoke_UnknownJobRecordsNothing(t *testing.T) {
	rec := &fakeRecorder{}
	tl := New(lookupFrom(map[string]config.JobProfile{})).WithRecorder(rec)
	if _, err := tl.Invoke(context.Background(), map[string]any{"name": "missing"}); err == nil {
		t.Fatal("expected error for unknown job")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.events) != 0 {
		t.Fatalf("unknown job should not record a job.run event, got %d", len(rec.events))
	}
}
