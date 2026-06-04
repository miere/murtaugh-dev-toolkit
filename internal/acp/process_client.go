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
			c.mu.Lock()
			delete(c.subscribers, sessionID)
			c.mu.Unlock()
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

func (c *ProcessClient) Cancel(ctx context.Context, sessionID string) error {
	_, err := c.call(ctx, "session/cancel", map[string]any{"sessionId": sessionID})
	return err
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
			return nil, fmt.Errorf("ACP %s error %d: %s", method, response.Error.Code, response.Error.Message)
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
	event := Event{Type: EventText, Text: extractText(notification.Params)}
	if event.Text == "" {
		return
	}
	c.mu.Lock()
	ch := c.subscribers[sessionID]
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- event:
		default:
		}
	}
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
