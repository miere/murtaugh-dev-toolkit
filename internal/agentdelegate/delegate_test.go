package agentdelegate

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/config"
)

// fakeClient is a scripted agent.Client. It replays a fixed sequence of events on
// Prompt, or — when block is set — returns a channel that stays open until the
// prompt context is cancelled (to exercise the idle watchdog).
type fakeClient struct {
	initErr    error
	newSessErr error
	promptErr  error
	events     []agent.Event
	block      bool

	initialized bool
	closed      bool
}

func (f *fakeClient) Initialize(context.Context) error { f.initialized = true; return f.initErr }

func (f *fakeClient) NewSession(context.Context, agent.SessionMetadata) (agent.Session, error) {
	return agent.Session{ID: "session-1"}, f.newSessErr
}

func (f *fakeClient) Prompt(ctx context.Context, _ string, _ agent.PromptRequest) (<-chan agent.Event, error) {
	if f.promptErr != nil {
		return nil, f.promptErr
	}
	ch := make(chan agent.Event)
	if f.block {
		go func() {
			<-ctx.Done()
			close(ch)
		}()
		return ch, nil
	}
	go func() {
		for _, e := range f.events {
			ch <- e
		}
		close(ch)
	}()
	return ch, nil
}

func (f *fakeClient) Cancel(context.Context, string) error { return nil }

func (f *fakeClient) Close() error { f.closed = true; return nil }

func newTestRunner(t *testing.T, client *fakeClient, requestTimeout string) *Runner {
	t.Helper()
	agents := map[string]config.AgentProfile{"default": {ACP: &config.ACPProfile{Command: "/bin/true"}}}
	r := NewRunner(agents, config.ACPConfig{RequestTimeout: requestTimeout}, "", slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	r.WithClientFactory(func(config.AgentProfile, *slog.Logger) agent.Client { return client })
	return r
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func textEvent(s string) agent.Event { return agent.Event{Type: agent.EventText, Text: s} }

func TestRunAccumulatesTextUntilComplete(t *testing.T) {
	client := &fakeClient{events: []agent.Event{
		textEvent("Hello, "),
		textEvent("world"),
		{Type: agent.EventComplete},
	}}
	r := newTestRunner(t, client, "1m")

	out, err := r.Run(context.Background(), "default", "hi")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out != "Hello, world" {
		t.Fatalf("got %q, want %q", out, "Hello, world")
	}
	if !client.closed {
		t.Fatal("client was not closed")
	}
}

func TestRunReturnsOnChannelClose(t *testing.T) {
	client := &fakeClient{events: []agent.Event{textEvent("partial")}}
	r := newTestRunner(t, client, "1m")

	out, err := r.Run(context.Background(), "default", "hi")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out != "partial" {
		t.Fatalf("got %q, want %q", out, "partial")
	}
}

func TestRunSurfacesAgentError(t *testing.T) {
	boom := errors.New("boom")
	client := &fakeClient{events: []agent.Event{
		textEvent("starting"),
		{Type: agent.EventError, Error: boom},
	}}
	r := newTestRunner(t, client, "1m")

	_, err := r.Run(context.Background(), "default", "hi")
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped boom error, got %v", err)
	}
}

func TestRunUnknownAgent(t *testing.T) {
	r := newTestRunner(t, &fakeClient{}, "1m")
	_, err := r.Run(context.Background(), "missing", "hi")
	if err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("expected unknown agent error, got %v", err)
	}
}

func TestRunIdleTimeout(t *testing.T) {
	client := &fakeClient{block: true}
	r := newTestRunner(t, client, "20ms")

	_, err := r.Run(context.Background(), "default", "hi")
	if err == nil || !strings.Contains(err.Error(), "went idle") {
		t.Fatalf("expected idle timeout error, got %v", err)
	}
	if !client.closed {
		t.Fatal("client was not closed after idle timeout")
	}
}

// heartbeatClient emits a steady stream of text events spaced by interval and
// then completes. It models a long but *productive* turn: the total wall-clock
// far exceeds the idle window, yet no single gap between events ever does.
type heartbeatClient struct {
	interval time.Duration
	beats    int
	closed   bool
}

func (c *heartbeatClient) Initialize(context.Context) error { return nil }

func (c *heartbeatClient) NewSession(context.Context, agent.SessionMetadata) (agent.Session, error) {
	return agent.Session{ID: "session-1"}, nil
}

func (c *heartbeatClient) Prompt(ctx context.Context, _ string, _ agent.PromptRequest) (<-chan agent.Event, error) {
	ch := make(chan agent.Event)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for i := 0; i < c.beats; i++ {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			select {
			case ch <- textEvent("."):
			case <-ctx.Done():
				return
			}
		}
		select {
		case ch <- agent.Event{Type: agent.EventComplete}:
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

func (c *heartbeatClient) Cancel(context.Context, string) error { return nil }

func (c *heartbeatClient) Close() error { c.closed = true; return nil }

// TestRunProductiveTurnOutlivesIdleWindow is the delegate-side guard for the
// "5-minute guillotine" bug: the turn must be bounded by *inactivity*, not by
// total wall-clock. The client beats every 20ms for 240ms total — well past the
// 200ms idle window — so a total-time cap would kill it, but an idle-only bound
// lets it run to completion. Regressing Run to a total deadline fails here.
func TestRunProductiveTurnOutlivesIdleWindow(t *testing.T) {
	const beats = 12
	client := &heartbeatClient{interval: 20 * time.Millisecond, beats: beats}
	agents := map[string]config.AgentProfile{"default": {ACP: &config.ACPProfile{Command: "/bin/true"}}}
	r := NewRunner(agents, config.ACPConfig{RequestTimeout: "200ms"}, "", slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	r.WithClientFactory(func(config.AgentProfile, *slog.Logger) agent.Client { return client })

	out, err := r.Run(context.Background(), "default", "hi")
	if err != nil {
		t.Fatalf("a productive long turn must not be killed, got error: %v", err)
	}
	if len(out) != beats {
		t.Fatalf("expected %d heartbeat chars, got %d (%q)", beats, len(out), out)
	}
	if !client.closed {
		t.Fatal("client was not closed after a completed turn")
	}
}

func TestRunForJSONValid(t *testing.T) {
	client := &fakeClient{events: []agent.Event{
		textEvent(`{"text":"hi"}`),
		{Type: agent.EventComplete},
	}}
	r := newTestRunner(t, client, "1m")

	out, err := r.RunForJSON(context.Background(), "default", "hi")
	if err != nil {
		t.Fatalf("RunForJSON returned error: %v", err)
	}
	if string(out) != `{"text":"hi"}` {
		t.Fatalf("got %q", out)
	}
}

func TestRunForJSONNonJSON(t *testing.T) {
	client := &fakeClient{events: []agent.Event{
		textEvent("just some prose, not json"),
		{Type: agent.EventComplete},
	}}
	r := newTestRunner(t, client, "1m")

	_, err := r.RunForJSON(context.Background(), "default", "hi")
	if !errors.Is(err, ErrNonJSONOutput) {
		t.Fatalf("expected ErrNonJSONOutput, got %v", err)
	}
}

func TestRunAndForgetDiscardsOutput(t *testing.T) {
	client := &fakeClient{events: []agent.Event{
		textEvent("side-effects only"),
		{Type: agent.EventComplete},
	}}
	r := newTestRunner(t, client, "1m")

	if err := r.RunAndForget(context.Background(), "default", "hi"); err != nil {
		t.Fatalf("RunAndForget returned error: %v", err)
	}
	if !client.closed {
		t.Fatal("client was not closed")
	}
}

func TestRunInitializeError(t *testing.T) {
	client := &fakeClient{initErr: errors.New("no start")}
	r := newTestRunner(t, client, "1m")

	_, err := r.Run(context.Background(), "default", "hi")
	if err == nil || !strings.Contains(err.Error(), "initialize agent") {
		t.Fatalf("expected initialize error, got %v", err)
	}
	if !client.closed {
		t.Fatal("client should be closed even on init failure")
	}
}
