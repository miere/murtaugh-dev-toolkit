package agentdelegate

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/acp"
	"github.com/miere/murtaugh-dev-toolkit/internal/config"
)

// fakeClient is a scripted acp.Client. It replays a fixed sequence of events on
// Prompt, or — when block is set — returns a channel that stays open until the
// prompt context is cancelled (to exercise the idle watchdog).
type fakeClient struct {
	initErr    error
	newSessErr error
	promptErr  error
	events     []acp.Event
	block      bool

	initialized bool
	closed      bool
}

func (f *fakeClient) Initialize(context.Context) error { f.initialized = true; return f.initErr }

func (f *fakeClient) NewSession(context.Context, acp.SessionMetadata) (acp.Session, error) {
	return acp.Session{ID: "session-1"}, f.newSessErr
}

func (f *fakeClient) Prompt(ctx context.Context, _ string, _ acp.PromptRequest) (<-chan acp.Event, error) {
	if f.promptErr != nil {
		return nil, f.promptErr
	}
	ch := make(chan acp.Event)
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
	agents := map[string]config.AgentProfile{"default": {Command: "/bin/true"}}
	r := NewRunner(agents, config.ACPConfig{RequestTimeout: requestTimeout}, "", slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	r.WithClientFactory(func(config.AgentProfile, *slog.Logger) acp.Client { return client })
	return r
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func textEvent(s string) acp.Event { return acp.Event{Type: acp.EventText, Text: s} }

func TestRunAccumulatesTextUntilComplete(t *testing.T) {
	client := &fakeClient{events: []acp.Event{
		textEvent("Hello, "),
		textEvent("world"),
		{Type: acp.EventComplete},
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
	client := &fakeClient{events: []acp.Event{textEvent("partial")}}
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
	client := &fakeClient{events: []acp.Event{
		textEvent("starting"),
		{Type: acp.EventError, Error: boom},
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

func TestRunForJSONValid(t *testing.T) {
	client := &fakeClient{events: []acp.Event{
		textEvent(`{"text":"hi"}`),
		{Type: acp.EventComplete},
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
	client := &fakeClient{events: []acp.Event{
		textEvent("just some prose, not json"),
		{Type: acp.EventComplete},
	}}
	r := newTestRunner(t, client, "1m")

	_, err := r.RunForJSON(context.Background(), "default", "hi")
	if !errors.Is(err, ErrNonJSONOutput) {
		t.Fatalf("expected ErrNonJSONOutput, got %v", err)
	}
}

func TestRunAndForgetDiscardsOutput(t *testing.T) {
	client := &fakeClient{events: []acp.Event{
		textEvent("side-effects only"),
		{Type: acp.EventComplete},
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
