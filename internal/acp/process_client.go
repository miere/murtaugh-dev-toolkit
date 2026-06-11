package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ProcessOptions struct {
	Command string
	Args    []string
	WorkDir string
	Logger  *slog.Logger
}

type ProcessClient struct {
	opts ProcessOptions
	log  *slog.Logger

	mu          sync.Mutex
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	started     bool
	closed      bool
	nextID      atomic.Int64
	pending     map[int64]chan rpcResponse
	subscribers map[string]chan Event
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// jsonRPCMethodNotFound is the standard JSON-RPC code an agent returns when it
// has no handler registered for a method — e.g. an ACP agent that does not
// implement session/cancel. It is a method-level rejection, raised before the
// params are even validated, which makes it a reliable capability signal.
const jsonRPCMethodNotFound = -32601

// cancelProbeSessionID is the synthetic session id used to probe session/cancel
// support. A non-interruptible agent rejects the method itself (-32601) before
// looking at the session; an interruptible one accepts the call or reports an
// unknown session, neither of which is method-not-found.
const cancelProbeSessionID = "murtaugh-cancel-probe"

// RPCError is a structured ACP/JSON-RPC error. It preserves the numeric code so
// callers can branch on it (e.g. IsMethodNotFound) instead of matching strings.
type RPCError struct {
	Method  string
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("ACP %s error %d: %s", e.Method, e.Code, e.Message)
}

// IsMethodNotFound reports whether err is an RPCError carrying the JSON-RPC
// "method not found" code, i.e. the agent does not implement the method.
func IsMethodNotFound(err error) bool {
	var rpcErr *RPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == jsonRPCMethodNotFound
}

type rpcNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func NewProcessClient(opts ProcessOptions) *ProcessClient {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &ProcessClient{opts: opts, log: logger, pending: make(map[int64]chan rpcResponse), subscribers: make(map[string]chan Event)}
}

func (c *ProcessClient) Initialize(ctx context.Context) error {
	startedAt := time.Now()
	if err := c.start(ctx); err != nil {
		return err
	}
	_, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientInfo": map[string]any{
			"name":    "murtaugh",
			"title":   "Murtaugh Slack ACP Client",
			"version": "0.1.0",
		},
		"clientCapabilities": map[string]any{},
	})
	if err == nil {
		c.log.Info("initialized ACP client", "duration", time.Since(startedAt))
	}
	return err
}

func (c *ProcessClient) NewSession(ctx context.Context, _ SessionMetadata) (Session, error) {
	result, err := c.call(ctx, "session/new", map[string]any{
		"cwd":        c.sessionCWD(),
		"mcpServers": []any{},
	})
	if err != nil {
		return Session{}, err
	}
	var decoded struct {
		SessionID string `json:"sessionId"`
		ID        string `json:"id"`
	}
	if len(result) > 0 {
		if err := json.Unmarshal(result, &decoded); err != nil {
			return Session{}, fmt.Errorf("decode session/new response: %w", err)
		}
	}
	id := decoded.SessionID
	if id == "" {
		id = decoded.ID
	}
	if id == "" {
		return Session{}, errors.New("session/new response did not include sessionId")
	}
	return Session{ID: id}, nil
}

func (c *ProcessClient) sessionCWD() string {
	if strings.TrimSpace(c.opts.WorkDir) != "" {
		return c.opts.WorkDir
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func (c *ProcessClient) Prompt(ctx context.Context, sessionID string, request PromptRequest) (<-chan Event, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("session id is required")
	}
	events := make(chan Event, 32)
	c.mu.Lock()
	c.subscribers[sessionID] = events
	c.mu.Unlock()

	go func() {
		defer func() {
			c.unsubscribe(sessionID, events)
			close(events)
		}()
		result, err := c.call(ctx, "session/prompt", map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]string{{"type": "text", "text": request.Text}},
		})
		if err != nil {
			events <- Event{Type: EventError, Error: err}
			return
		}
		if text := extractText(result); text != "" {
			events <- Event{Type: EventText, Text: text}
		}
		events <- Event{Type: EventComplete}
	}()
	return events, nil
}

// unsubscribe retracts a prompt's event subscription, but only if it is still
// the live one. When two prompts race on the same session (e.g. an interrupt
// immediately followed by a follow-up that reuses the session), the second
// prompt overwrites subscribers[sessionID]; an unconditional delete here would
// tear down the live prompt's subscription and silently drop its events.
func (c *ProcessClient) unsubscribe(sessionID string, events chan Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.subscribers[sessionID] == events {
		delete(c.subscribers, sessionID)
	}
}

func (c *ProcessClient) Cancel(ctx context.Context, sessionID string) error {
	_, err := c.call(ctx, "session/cancel", map[string]any{"sessionId": sessionID})
	return err
}

// SupportsCancel probes whether the agent implements session/cancel by issuing
// the call for a synthetic session and inspecting the outcome. A method-not-
// found error means the agent cannot be interrupted; any other result (success
// or an unknown-session error) means the method exists. On a transient/ambient
// failure (process error, cancelled context) it conservatively reports true so
// a flaky probe never silently disables interrupts.
func (c *ProcessClient) SupportsCancel(ctx context.Context) bool {
	err := c.Cancel(ctx, cancelProbeSessionID)
	return !IsMethodNotFound(err)
}

func (c *ProcessClient) Close() error {
	c.mu.Lock()
	c.closed = true
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	c.mu.Unlock()
	c.failAll(errors.New("ACP client closed"))
	return nil
}

func (c *ProcessClient) start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return nil
	}
	if strings.TrimSpace(c.opts.Command) == "" {
		return errors.New("ACP command is required")
	}
	cmd := exec.Command(c.opts.Command, c.opts.Args...)
	cmd.Dir = c.opts.WorkDir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open ACP stdout: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open ACP stdin: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open ACP stderr: %w", err)
	}
	c.log.Info("starting ACP process", "command", c.opts.Command, "workdir", c.opts.WorkDir)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ACP process: %w", err)
	}
	c.cmd = cmd
	c.stdin = stdin
	c.started = true
	go c.readLoop(stdout)
	go c.drainStderr(stderr)
	go func() {
		err := cmd.Wait()
		c.markProcessExited(cmd)
		c.log.Info("ACP process exited", "error", err)
		c.failAll(errors.New("ACP process exited"))
	}()
	return nil
}

func (c *ProcessClient) markProcessExited(cmd *exec.Cmd) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != cmd {
		return
	}
	c.started = false
	c.stdin = nil
	c.cmd = nil
}

func (c *ProcessClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	startedAt := time.Now()
	if err := c.start(ctx); err != nil {
		return nil, err
	}
	id := c.nextID.Add(1)
	responseCh := make(chan rpcResponse, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("ACP client is closed")
	}
	c.pending[id] = responseCh
	encoded, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err == nil {
		_, err = c.stdin.Write(append(encoded, '\n'))
	}
	c.mu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("send ACP request %s: %w", method, err)
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case response, ok := <-responseCh:
		if !ok {
			return nil, errors.New("ACP request failed: process closed")
		}
		if response.Error != nil {
			return nil, &RPCError{Method: method, Code: response.Error.Code, Message: response.Error.Message}
		}
		c.log.Info("completed ACP request", "method", method, "duration", time.Since(startedAt))
		return response.Result, nil
	}
}

func (c *ProcessClient) readLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(line, &envelope); err != nil {
			c.failAll(fmt.Errorf("decode ACP message: %w", err))
			return
		}
		if _, hasID := envelope["id"]; hasID {
			var response rpcResponse
			if err := json.Unmarshal(line, &response); err != nil {
				c.failAll(fmt.Errorf("decode ACP response: %w", err))
				return
			}
			c.deliverResponse(response)
			continue
		}
		var notification rpcNotification
		if err := json.Unmarshal(line, &notification); err != nil {
			c.failAll(fmt.Errorf("decode ACP notification: %w", err))
			return
		}
		c.deliverNotification(notification)
	}
	if err := scanner.Err(); err != nil {
		c.failAll(fmt.Errorf("read ACP stdout: %w", err))
	}
}

func (c *ProcessClient) deliverResponse(response rpcResponse) {
	c.mu.Lock()
	ch := c.pending[response.ID]
	delete(c.pending, response.ID)
	c.mu.Unlock()
	if ch != nil {
		ch <- response
		close(ch)
	}
}

func (c *ProcessClient) deliverNotification(notification rpcNotification) {
	if notification.Method != "session/update" {
		return
	}
	var params map[string]any
	if err := json.Unmarshal(notification.Params, &params); err != nil {
		return
	}
	sessionID, _ := params["sessionId"].(string)
	if sessionID == "" {
		sessionID, _ = params["session_id"].(string)
	}
	if sessionID == "" {
		return
	}
	c.mu.Lock()
	ch := c.subscribers[sessionID]
	c.mu.Unlock()
	if ch == nil {
		return
	}
	if task := extractTask(notification.Params); task != nil {
		// Block on the send: dropping task or text notifications truncates the
		// agent response in the consumer (chat handler). The readLoop is back-
		// pressured by the consumer, which is the intended behaviour.
		ch <- Event{Type: EventTask, Task: task}
		return
	}
	event := Event{Type: EventText, Text: extractNotificationText(notification.Params)}
	if event.Text == "" {
		return
	}
	ch <- event
}

func (c *ProcessClient) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		ch <- rpcResponse{ID: id, Error: &rpcError{Code: -1, Message: err.Error()}}
		close(ch)
		delete(c.pending, id)
	}
	for _, ch := range c.subscribers {
		select {
		case ch <- Event{Type: EventError, Error: err}:
		default:
		}
	}
}

func (c *ProcessClient) drainStderr(reader io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(reader, 64*1024))
}

func extractText(raw json.RawMessage) string {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.Join(extractStrings(value), "")
}

func extractNotificationText(raw json.RawMessage) string {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	updateMap, ok := m["update"].(map[string]any)
	if !ok {
		return strings.Join(extractStrings(value), "")
	}
	if sessionUpdate, _ := updateMap["sessionUpdate"].(string); sessionUpdate != "agent_message_chunk" && sessionUpdate != "agent_message" {
		return ""
	}
	if content, ok := updateMap["content"]; ok {
		return strings.Join(extractStrings(content), "")
	}
	return strings.Join(extractStrings(updateMap), "")
}

func extractTask(raw json.RawMessage) *TaskEvent {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if taskMap, ok := m["task"].(map[string]any); ok {
		return taskFromMap(taskMap)
	}
	updateMap, ok := m["update"].(map[string]any)
	if !ok {
		return nil
	}
	sessionUpdate, _ := updateMap["sessionUpdate"].(string)
	if sessionUpdate != "tool_call" && sessionUpdate != "tool_call_update" {
		return nil
	}
	return taskFromMap(updateMap)
}

func taskFromMap(taskMap map[string]any) *TaskEvent {
	id, _ := taskMap["id"].(string)
	if id == "" {
		id, _ = taskMap["taskId"].(string)
	}
	if id == "" {
		id, _ = taskMap["toolCallId"].(string)
	}
	if id == "" {
		id, _ = taskMap["tool_call_id"].(string)
	}
	if id == "" {
		return nil
	}
	task := &TaskEvent{ID: id}
	if title, ok := taskMap["title"].(string); ok {
		task.Title = title
	}
	if desc, ok := taskMap["description"].(string); ok {
		task.Description = desc
	}
	if task.Description == "" {
		if kind, ok := taskMap["kind"].(string); ok {
			task.Description = kind
		}
	}
	if status, ok := taskMap["status"].(string); ok {
		task.Status = normalizeTaskStatus(status)
	}
	if content, ok := taskMap["content"]; ok {
		task.Output = strings.Join(extractStrings(content), "")
	}
	return task
}

func normalizeTaskStatus(status string) TaskStatus {
	switch TaskStatus(status) {
	case TaskStatusComplete, "completed":
		return TaskStatusComplete
	case TaskStatusFailed:
		return TaskStatusFailed
	case TaskStatusCancelled:
		return TaskStatusCancelled
	case TaskStatusPending:
		return TaskStatusPending
	case TaskStatusInProgress:
		return TaskStatusInProgress
	default:
		return TaskStatus(status)
	}
}

func extractStrings(value any) []string {
	switch v := value.(type) {
	case map[string]any:
		if text, ok := v["text"].(string); ok {
			return []string{text}
		}
		var out []string
		for _, key := range []string{"update", "content", "delta", "message", "chunks", "updates"} {
			if child, ok := v[key]; ok {
				out = append(out, extractStrings(child)...)
			}
		}
		return out
	case []any:
		var out []string
		for _, child := range v {
			out = append(out, extractStrings(child)...)
		}
		return out
	default:
		return nil
	}
}
