package native

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/llm"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

// blockingTool's Invoke parks until release is closed, standing in for the `ask`
// tool waiting on a human.
type blockingTool struct{ release chan struct{} }

func (b *blockingTool) Name() string                    { return "blocker" }
func (b *blockingTool) Description() string             { return "blocks until released" }
func (b *blockingTool) InputSchema() *jsonschema.Schema { return nil }
func (b *blockingTool) Invoke(ctx context.Context, _ map[string]any) (any, error) {
	select {
	case <-b.release:
		return "done", nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestInvokeTool_EmitsHeartbeatsWhileBlocking(t *testing.T) {
	prev := toolHeartbeatInterval
	toolHeartbeatInterval = 5 * time.Millisecond
	defer func() { toolHeartbeatInterval = prev }()

	bt := &blockingTool{release: make(chan struct{})}
	loop := NewLoop(nil, "m", []tools.Tool{bt}, 1)

	var mu sync.Mutex
	statuses := 0
	emit := func(ev agent.Event) {
		mu.Lock()
		if ev.Type == agent.EventStatus {
			statuses++
		}
		mu.Unlock()
	}

	done := make(chan string, 1)
	go func() {
		done <- loop.invokeTool(context.Background(), llm.ToolCall{ID: "1", Name: "blocker"}, emit)
	}()

	// Let several heartbeat intervals elapse while the tool is parked.
	time.Sleep(40 * time.Millisecond)
	close(bt.release)

	if out := <-done; out != "done" {
		t.Fatalf("tool result = %q, want \"done\"", out)
	}
	mu.Lock()
	got := statuses
	mu.Unlock()
	if got == 0 {
		t.Fatal("expected at least one heartbeat status event while the tool blocked")
	}
}
