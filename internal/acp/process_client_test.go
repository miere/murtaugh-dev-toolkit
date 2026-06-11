package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProcessClientStreamsPromptUpdates(t *testing.T) {
	client := NewProcessClient(ProcessOptions{Command: os.Args[0], Args: []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper"}})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	session, err := client.NewSession(ctx, SessionMetadata{TeamID: "T1"})
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	events, err := client.Prompt(ctx, session.ID, PromptRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("Prompt returned error: %v", err)
	}
	var text strings.Builder
	var taskEvents []*TaskEvent
	for event := range events {
		switch event.Type {
		case EventText:
			text.WriteString(event.Text)
		case EventTask:
			taskEvents = append(taskEvents, event.Task)
		}
	}
	if got := text.String(); got != "Hello from ACP" {
		t.Fatalf("unexpected streamed text %q", got)
	}
	if len(taskEvents) != 2 {
		t.Fatalf("expected two task events, got %d", len(taskEvents))
	}
	if taskEvents[0].ID != "task-1" || taskEvents[1].ID != "task-1" {
		t.Fatalf("unexpected task ids: %+v", taskEvents)
	}
	if taskEvents[0].Status != TaskStatusInProgress || taskEvents[1].Status != TaskStatusComplete {
		t.Fatalf("unexpected task statuses: %+v", taskEvents)
	}
}

func TestProcessClientProcessOutlivesInitializeContext(t *testing.T) {
	client := NewProcessClient(ProcessOptions{Command: os.Args[0], Args: []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper"}})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	cancel()
	session, err := client.NewSession(context.Background(), SessionMetadata{TeamID: "T1"})
	if err != nil {
		t.Fatalf("NewSession returned error after initialize context cancellation: %v", err)
	}
	if session.ID == "" {
		t.Fatal("expected session ID")
	}
}

func TestProcessClientSupportsCancelFalseWhenMethodNotFound(t *testing.T) {
	client := NewProcessClient(ProcessOptions{Command: os.Args[0], Args: []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper"}})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if client.SupportsCancel(ctx) {
		t.Fatal("expected SupportsCancel=false for an agent that returns -32601")
	}
}

func TestProcessClientSupportsCancelTrueWhenMethodExists(t *testing.T) {
	client := NewProcessClient(ProcessOptions{Command: os.Args[0], Args: []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper-cancellable"}})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if !client.SupportsCancel(ctx) {
		t.Fatal("expected SupportsCancel=true when the agent implements session/cancel")
	}
}

func TestIsMethodNotFound(t *testing.T) {
	if !IsMethodNotFound(&RPCError{Method: "session/cancel", Code: -32601, Message: "nope"}) {
		t.Fatal("expected -32601 RPCError to be method-not-found")
	}
	if IsMethodNotFound(&RPCError{Method: "session/cancel", Code: -32602, Message: "bad params"}) {
		t.Fatal("expected -32602 RPCError not to be method-not-found")
	}
	if IsMethodNotFound(errors.New("plain error")) {
		t.Fatal("expected a non-RPC error not to be method-not-found")
	}
	if IsMethodNotFound(nil) {
		t.Fatal("expected nil not to be method-not-found")
	}
}

func TestProcessClientUnsubscribeOnlyRetractsOwnSubscription(t *testing.T) {
	c := NewProcessClient(ProcessOptions{Command: "true"})
	first := make(chan Event, 1)
	second := make(chan Event, 1)

	c.subscribers["s"] = first
	// A second prompt reuses the session and overwrites the subscriber.
	c.subscribers["s"] = second

	// The first prompt tearing down must NOT remove the live (second)
	// subscription.
	c.unsubscribe("s", first)
	if c.subscribers["s"] != second {
		t.Fatalf("stale teardown removed the live subscription: got %v", c.subscribers["s"])
	}

	// The live prompt tearing down removes itself.
	c.unsubscribe("s", second)
	if _, ok := c.subscribers["s"]; ok {
		t.Fatal("live teardown should have removed the subscription")
	}
}

func TestExtractTextFromACPAgentMessageChunkUpdate(t *testing.T) {
	raw := json.RawMessage(`{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"pong"}}}`)
	if got := extractNotificationText(raw); got != "pong" {
		t.Fatalf("unexpected extracted text: %q", got)
	}
}

func TestExtractTextFromACPAgentMessageUpdate(t *testing.T) {
	raw := json.RawMessage(`{"sessionId":"session-1","update":{"sessionUpdate":"agent_message","content":[{"type":"text","text":"final "},{"type":"text","text":"answer"}]}}`)
	if got := extractNotificationText(raw); got != "final answer" {
		t.Fatalf("unexpected extracted text: %q", got)
	}
}

func TestExtractTextIgnoresACPToolCallContent(t *testing.T) {
	raw := json.RawMessage(`{"sessionId":"session-1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"in_progress","content":[{"type":"content","content":{"type":"text","text":"raw tool output"}}]}}`)
	if got := extractNotificationText(raw); got != "" {
		t.Fatalf("expected tool output to be hidden from assistant stream, got %q", got)
	}
}

func TestExtractTaskFromACPNotification(t *testing.T) {
	t.Run("valid task with all fields", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","task":{"id":"task-1","title":"Searching codebase","status":"in_progress","description":"looking for references"}}`)
		task := extractTask(raw)
		if task == nil {
			t.Fatal("expected non-nil task")
		}
		if task.ID != "task-1" {
			t.Fatalf("expected id task-1, got %q", task.ID)
		}
		if task.Title != "Searching codebase" {
			t.Fatalf("expected title 'Searching codebase', got %q", task.Title)
		}
		if task.Status != TaskStatusInProgress {
			t.Fatalf("expected status in_progress, got %q", task.Status)
		}
		if task.Description != "looking for references" {
			t.Fatalf("expected description 'looking for references', got %q", task.Description)
		}
	})

	t.Run("missing id", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","task":{"title":"Foo","status":"pending"}}`)
		if task := extractTask(raw); task != nil {
			t.Fatalf("expected nil for missing id, got %+v", task)
		}
	})

	t.Run("no task field", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","content":{"type":"text","text":"hello"}}`)
		if task := extractTask(raw); task != nil {
			t.Fatalf("expected nil for no task field, got %+v", task)
		}
	})

	t.Run("taskId camelCase alias", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","task":{"taskId":"t-2","title":"Build","status":"complete"}}`)
		task := extractTask(raw)
		if task == nil {
			t.Fatal("expected non-nil task")
		}
		if task.ID != "t-2" {
			t.Fatalf("expected id t-2, got %q", task.ID)
		}
		if task.Status != TaskStatusComplete {
			t.Fatalf("expected status complete, got %q", task.Status)
		}
	})

	t.Run("ACP tool_call update", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"List files","kind":"read","status":"pending"}}`)
		task := extractTask(raw)
		if task == nil {
			t.Fatal("expected non-nil task")
		}
		if task.ID != "call-1" || task.Title != "List files" || task.Status != TaskStatusPending {
			t.Fatalf("unexpected ACP tool task: %+v", task)
		}
		if task.Description != "read" {
			t.Fatalf("expected kind as description, got %q", task.Description)
		}
	})

	t.Run("ACP tool_call_update completed alias", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"done"}}]}}`)
		task := extractTask(raw)
		if task == nil {
			t.Fatal("expected non-nil task")
		}
		if task.ID != "call-1" || task.Status != TaskStatusComplete || task.Output != "done" {
			t.Fatalf("unexpected ACP tool update: %+v", task)
		}
	})
}

func TestProcessClientDoesNotDropEventsForSlowConsumer(t *testing.T) {
	const totalChunks = 200
	client := NewProcessClient(ProcessOptions{Command: os.Args[0], Args: []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper"}})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	session, err := client.NewSession(ctx, SessionMetadata{TeamID: "T1"})
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	events, err := client.Prompt(ctx, session.ID, PromptRequest{Text: fmt.Sprintf("burst:%d", totalChunks)})
	if err != nil {
		t.Fatalf("Prompt returned error: %v", err)
	}
	// Consume each event with a small delay so the events channel buffer (32)
	// fills up well before the agent finishes emitting. Without blocking sends
	// in deliverNotification, the chunks beyond the buffer would be silently
	// dropped and the assembled text would be truncated.
	var text strings.Builder
	for event := range events {
		if event.Type == EventText {
			text.WriteString(event.Text)
			time.Sleep(2 * time.Millisecond)
		}
	}
	got := text.String()
	if want := strings.Repeat("x", totalChunks); got != want {
		t.Fatalf("text was truncated by slow consumer: got %d bytes, want %d", len(got), len(want))
	}
}

func TestACPHelperProcess(t *testing.T) {
	mode := ""
	if len(os.Args) > 0 {
		mode = os.Args[len(os.Args)-1]
	}
	if mode != "acp-helper" && mode != "acp-helper-cancellable" {
		return
	}
	supportsCancel := mode == "acp-helper-cancellable"
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			os.Exit(2)
		}
		switch req.Method {
		case "initialize":
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": 1}})
		case "session/new":
			var params struct {
				CWD        string `json:"cwd"`
				MCPServers []any  `json:"mcpServers"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil || params.CWD == "" || params.MCPServers == nil {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "invalid session/new params"}})
				continue
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"sessionId": "session-1"}})
		case "session/prompt":
			var params struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			}
			_ = json.Unmarshal(req.Params, &params)
			promptText := ""
			if len(params.Prompt) > 0 {
				promptText = params.Prompt[0].Text
			}
			if n := parseBurstCount(promptText); n > 0 {
				for i := 0; i < n; i++ {
					_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "x"}}}})
				}
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn"}})
				continue
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "tool_call", "toolCallId": "task-1", "title": "Thinking", "status": "in_progress"}}})
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "task-1", "status": "completed", "content": []map[string]any{{"type": "content", "content": map[string]string{"type": "text", "text": "tool output should not stream"}}}}}})
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "agent_message", "content": []map[string]string{{"type": "text", "text": "Hello from ACP"}}}}})
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn"}})
		case "session/cancel":
			if supportsCancel {
				// An interruptible agent accepts the call (here: reports the
				// probe's synthetic session is unknown — not method-not-found).
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "unknown session"}})
			} else {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "session/cancel not supported"}})
			}
		default:
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": fmt.Sprintf("unknown method %s", req.Method)}})
		}
	}
	os.Exit(0)
}

func parseBurstCount(prompt string) int {
	const prefix = "burst:"
	if !strings.HasPrefix(prompt, prefix) {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(prompt[len(prefix):], "%d", &n); err != nil {
		return 0
	}
	return n
}
