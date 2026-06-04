package acp

import (
	"bufio"
	"context"
	"encoding/json"
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
	for event := range events {
		if event.Type == EventText {
			text.WriteString(event.Text)
		}
	}
	if got := text.String(); got != "Hello from ACP" {
		t.Fatalf("unexpected streamed text %q", got)
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

func TestExtractTextFromACPAgentMessageChunkUpdate(t *testing.T) {
	raw := json.RawMessage(`{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"pong"}}}`)
	if got := extractText(raw); got != "pong" {
		t.Fatalf("unexpected extracted text: %q", got)
	}
}

func TestACPHelperProcess(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "acp-helper" {
		return
	}
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
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "content": []map[string]string{{"type": "text", "text": "Hello from ACP"}}}})
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn"}})
		default:
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": fmt.Sprintf("unknown method %s", req.Method)}})
		}
	}
	os.Exit(0)
}
