package agent

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ProcessOptions struct {
	Command string
	Args    []string
	WorkDir string
	// Env are extra KEY=VALUE entries layered on top of the inherited
	// environment when the agent process is started. Empty leaves the process
	// with Murtaugh's own environment unchanged.
	Env    []string
	Logger *slog.Logger

	// PermissionPolicy governs how agent-initiated session/request_permission
	// requests are answered: "ask" (route to PermissionAsker), "auto-allow", or
	// "auto-deny". Empty is treated as "ask".
	PermissionPolicy string
	// PermissionAsker resolves "ask" permission requests via a human (Slack
	// buttons). nil on headless/CLI paths, where "ask" falls back to deny.
	PermissionAsker PermissionAsker
	// Aggregator, when set, registers each session with Murtaugh's per-agent MCP
	// aggregator and supplies the stdio bridge server advertised in session/new,
	// so the agent can reach Murtaugh's own tools. nil leaves mcpServers empty.
	Aggregator Aggregator
}

type ProcessClient struct {
	opts ProcessOptions
	log  *slog.Logger
	// now sources the current time for the per-turn <context> block. Injectable
	// so tests can assert a fixed timestamp; defaults to time.Now.
	now func() time.Time

	mu          sync.Mutex
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	started     bool
	closed      bool
	nextID      atomic.Int64
	pending     map[int64]chan rpcResponse
	subscribers map[string]chan Event
	// dests records, per active session, the Slack conversation and the prompt's
	// context so an agent-initiated session/request_permission can be routed to a
	// human in the right thread and cancelled when that turn is interrupted.
	dests map[string]promptScope
	// caps records what the agent advertised in its initialize response. Set once
	// by Initialize before any prompt runs, then read-only; guarded by mu.
	caps AgentCapabilities
	// releases holds each registered session's aggregator cleanup, run on Close.
	releases []func()
}

// AgentCapabilities captures the parts of an ACP agent's initialize response
// that govern how Murtaugh may talk to it — chiefly which MCP server transports
// it accepts in session/new. Stdio is always available (mandatory in ACP); HTTP
// and SSE are only honoured when the agent advertises them. Note: an advertised
// transport is necessary but not sufficient — at least one shipping agent
// advertises http while silently dropping http servers, so any future HTTP path
// must verify a connection actually formed rather than trust this flag.
type AgentCapabilities struct {
	ProtocolVersion int
	MCP             MCPCapabilities
	LoadSession     bool
}

// MCPCapabilities reports which url-based MCP server transports the agent
// accepts in session/new (beyond the mandatory stdio).
type MCPCapabilities struct {
	HTTP bool
	SSE  bool
}

// Capabilities returns what the agent advertised at initialize. Zero value until
// Initialize completes.
func (c *ProcessClient) Capabilities() AgentCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps
}

// promptScope is the in-flight context for a session's current prompt: where it
// is talking (loc) and the context that is cancelled when the turn ends.
type promptScope struct {
	loc TurnLocation
	ctx context.Context
}

// rpcOutgoingResponse is a JSON-RPC response Murtaugh writes back to the agent
// when it serves an agent-initiated request (e.g. session/request_permission).
// ID is echoed verbatim as raw JSON so a string- or number-typed id round-trips.
type rpcOutgoingResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
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
	return &ProcessClient{opts: opts, log: logger, now: time.Now, pending: make(map[int64]chan rpcResponse), subscribers: make(map[string]chan Event), dests: make(map[string]promptScope)}
}

func (c *ProcessClient) Initialize(ctx context.Context) error {
	startedAt := time.Now()
	if err := c.start(ctx); err != nil {
		return err
	}
	result, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientInfo": map[string]any{
			"name":    "murtaugh",
			"title":   "Murtaugh Slack ACP Client",
			"version": "0.1.0",
		},
		"clientCapabilities": map[string]any{},
	})
	if err != nil {
		return err
	}
	caps := parseAgentCapabilities(result)
	c.mu.Lock()
	c.caps = caps
	c.mu.Unlock()
	c.log.Info("initialized ACP client",
		"duration", time.Since(startedAt),
		"protocol_version", caps.ProtocolVersion,
		"mcp_http", caps.MCP.HTTP,
		"mcp_sse", caps.MCP.SSE,
		"load_session", caps.LoadSession,
	)
	return nil
}

// parseAgentCapabilities decodes the subset of an ACP initialize response that
// Murtaugh acts on. Missing fields decode to their zero value (stdio-only), the
// safe default. An unparseable result yields zero capabilities rather than an
// error: the handshake already succeeded, and stdio — all Murtaugh needs today —
// is always available.
func parseAgentCapabilities(result json.RawMessage) AgentCapabilities {
	var decoded struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession     bool `json:"loadSession"`
			MCPCapabilities struct {
				HTTP bool `json:"http"`
				SSE  bool `json:"sse"`
			} `json:"mcpCapabilities"`
		} `json:"agentCapabilities"`
	}
	if len(result) > 0 {
		_ = json.Unmarshal(result, &decoded)
	}
	return AgentCapabilities{
		ProtocolVersion: decoded.ProtocolVersion,
		MCP: MCPCapabilities{
			HTTP: decoded.AgentCapabilities.MCPCapabilities.HTTP,
			SSE:  decoded.AgentCapabilities.MCPCapabilities.SSE,
		},
		LoadSession: decoded.AgentCapabilities.LoadSession,
	}
}

func (c *ProcessClient) NewSession(ctx context.Context, meta SessionMetadata) (Session, error) {
	mcpServers, release := c.aggregatorServers(meta)
	result, err := c.call(ctx, "session/new", map[string]any{
		"cwd":        c.sessionCWD(),
		"mcpServers": mcpServers,
	})
	if err != nil {
		if release != nil {
			release()
		}
		return Session{}, err
	}
	var decoded struct {
		SessionID string `json:"sessionId"`
		ID        string `json:"id"`
	}
	if len(result) > 0 {
		if err := json.Unmarshal(result, &decoded); err != nil {
			if release != nil {
				release()
			}
			return Session{}, fmt.Errorf("decode session/new response: %w", err)
		}
	}
	id := decoded.SessionID
	if id == "" {
		id = decoded.ID
	}
	if id == "" {
		if release != nil {
			release()
		}
		return Session{}, errors.New("session/new response did not include sessionId")
	}
	if release != nil {
		c.mu.Lock()
		c.releases = append(c.releases, release)
		c.mu.Unlock()
	}
	return Session{ID: id}, nil
}

// aggregatorServers asks the aggregator (if any) to register this session and
// returns the mcpServers value for session/new plus a release to run if the
// session fails to open. An empty list (and nil release) when no aggregator is
// configured or registration fails — the agent then simply gets no Murtaugh
// tools, which is logged loudly rather than failing the session.
func (c *ProcessClient) aggregatorServers(meta SessionMetadata) ([]any, func()) {
	if c.opts.Aggregator == nil {
		return []any{}, nil
	}
	spec, release, err := c.opts.Aggregator.RegisterSession(meta)
	if err != nil {
		c.log.Warn("aggregator registration failed; ACP agent will have no Murtaugh tools", "error", err)
		return []any{}, nil
	}
	// ACP's env shape is an array of {name,value}; emit in stable key order.
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, map[string]string{"name": k, "value": spec.Env[k]})
	}
	server := map[string]any{
		"name":    spec.Name,
		"command": spec.Command,
		"args":    spec.Args,
		"env":     env,
	}
	return []any{server}, release
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
	// Stash where this turn is talking and its context so a permission request
	// raised mid-turn can be asked in the same thread and cancelled with the turn.
	c.dests[sessionID] = promptScope{loc: TurnLocation{ChannelID: request.Channel, ThreadTS: request.Thread}, ctx: ctx}
	c.mu.Unlock()

	go func() {
		defer func() {
			c.unsubscribe(sessionID, events)
			close(events)
		}()
		result, err := c.call(ctx, "session/prompt", map[string]any{
			"sessionId": sessionID,
			"prompt":    c.promptBlocks(request),
		})
		if err != nil {
			events <- Event{Type: EventError, Error: err}
			return
		}
		text := extractText(result)
		stopReason := extractStopReason(result)
		// stop_reason is logged at INFO because it explains why a turn ended,
		// including the cases that produce no reply (max_tokens, refusal): the
		// single most useful signal when a chat comes back empty.
		c.log.Info("ACP prompt completed", "session_id", sessionID, "stop_reason", stopReason, "response_text", text != "")
		if text != "" {
			events <- Event{Type: EventText, Text: text}
		}
		events <- Event{Type: EventComplete, StopReason: stopReason}
	}()
	return events, nil
}

// promptBlocks renders a PromptRequest into ACP `session/prompt` content
// blocks. ACP exposes no system role, so leading delimited blocks are the
// closest stand-in for a system note. Order:
//  1. a <context> block carrying the volatile per-turn facts (current time,
//     working directory) — the ACP analogue of native's RenderTurnContext, so
//     an ACP agent knows what day it is and where it is rooted, just like the
//     native loop. Emitted for every caller, chat or CLI.
//  2. a <conversation-context> block (only when the prompt carries a Slack
//     conversation) telling the agent where it is talking so it can hand the
//     same channel/thread to the `restart` tool. Kept as a separate block with
//     machine-readable channel/thread attributes so that parseability is
//     unchanged.
//  3. the thread transcript, when History is set (a freshly opened session
//     backfilling an existing thread).
//  4. the user's text.
func (c *ProcessClient) promptBlocks(request PromptRequest) []map[string]string {
	blocks := make([]map[string]string, 0, 4)
	if ctxText := c.renderTurnContext(); ctxText != "" {
		blocks = append(blocks, map[string]string{"type": "text", "text": ctxText})
	}
	if request.Channel != "" {
		ctxText := fmt.Sprintf(
			"<conversation-context channel=%q thread=%q>You are responding in this Slack conversation. "+
				"If you call the `restart` tool, pass these exact channel and thread values so the approval "+
				"card is asked here.</conversation-context>",
			request.Channel, request.Thread,
		)
		blocks = append(blocks, map[string]string{"type": "text", "text": ctxText})
	}
	if request.History != "" {
		blocks = append(blocks, map[string]string{"type": "text", "text": request.History})
	}
	blocks = append(blocks, map[string]string{"type": "text", "text": request.Text})
	return blocks
}

// renderTurnContext renders the volatile per-turn <context> block (current time
// and working directory) for an ACP prompt, or "" when there is nothing to say.
// It mirrors the native RenderTurnContext format so the two backends present the
// same facts to the model; the Slack location is intentionally left to the
// separate <conversation-context> block above.
func (c *ProcessClient) renderTurnContext() string {
	var lines []string
	if c.now != nil {
		if now := c.now(); !now.IsZero() {
			lines = append(lines, "It is currently "+now.Format("2006-01-02 15:04 MST"))
		}
	}
	if cwd := c.sessionCWD(); cwd != "" && cwd != "." {
		lines = append(lines, "Working directory: "+cwd)
	}
	if len(lines) == 0 {
		return ""
	}
	return "<context>\n" + strings.Join(lines, "\n") + "\n</context>"
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
		delete(c.dests, sessionID)
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
	releases := c.releases
	c.releases = nil
	c.mu.Unlock()
	// Drop every aggregator session this client registered so its tokens can no
	// longer be claimed.
	for _, release := range releases {
		release()
	}
	// Tear down the aggregator's proxied MCP connections, if it holds any.
	if closer, ok := c.opts.Aggregator.(io.Closer); ok {
		_ = closer.Close()
	}
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
	if len(c.opts.Env) > 0 {
		// Inherit Murtaugh's environment, then append the profile's overrides.
		// exec resolves a duplicate key to the last entry, so appending the
		// overrides last makes them win over an inherited var of the same name.
		cmd.Env = append(os.Environ(), c.opts.Env...)
	}
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
		_, hasID := envelope["id"]
		_, hasMethod := envelope["method"]
		switch {
		case hasID && hasMethod:
			// An agent-initiated *request* (it wants a response): permission
			// prompts, fs/terminal calls. Handle off the read loop so a blocking
			// human approval never stalls delivery for every other conversation.
			payload := append([]byte(nil), line...)
			go c.handleAgentRequest(payload)
		case hasID:
			var response rpcResponse
			if err := json.Unmarshal(line, &response); err != nil {
				c.failAll(fmt.Errorf("decode ACP response: %w", err))
				return
			}
			c.deliverResponse(response)
		default:
			var notification rpcNotification
			if err := json.Unmarshal(line, &notification); err != nil {
				c.failAll(fmt.Errorf("decode ACP notification: %w", err))
				return
			}
			c.deliverNotification(notification)
		}
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

// handleAgentRequest serves a request the agent sends to us (the ACP client). The
// only method we implement is session/request_permission; anything else gets a
// method-not-found reply (and a warn) so the agent fails fast instead of blocking
// forever waiting for a response we would otherwise never send.
func (c *ProcessClient) handleAgentRequest(line []byte) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		c.log.Warn("ignoring malformed ACP agent request", "error", err)
		return
	}
	switch req.Method {
	case "session/request_permission":
		c.handlePermissionRequest(req.ID, req.Params)
	default:
		c.log.Warn("unhandled ACP agent request; replying method-not-found", "method", req.Method)
		c.respondError(req.ID, jsonRPCMethodNotFound, "method not implemented by murtaugh ACP client")
	}
}

// handlePermissionRequest resolves a session/request_permission per the configured
// policy and writes the ACP RequestPermissionResponse. An empty chosen option (no
// human decision, or no allow/reject option to auto-pick) maps to "cancelled".
func (c *ProcessClient) handlePermissionRequest(id, params json.RawMessage) {
	sessionID, toolName, options := parsePermissionRequest(params)
	optionID := c.decidePermission(sessionID, toolName, options)
	var outcome map[string]any
	if optionID == "" {
		outcome = map[string]any{"outcome": "cancelled"}
	} else {
		outcome = map[string]any{"outcome": "selected", "optionId": optionID}
	}
	c.respondResult(id, map[string]any{"outcome": outcome})
}

// decidePermission returns the optionId to grant for a permission request, or ""
// to cancel. auto-allow/auto-deny pick a matching option without a human; ask
// routes to the PermissionAsker in the session's Slack thread. ask with no asker
// or no known thread denies (returns "") — fail-safe and fast, unlike the hang it
// replaces.
func (c *ProcessClient) decidePermission(sessionID, toolName string, options []PermissionOption) string {
	switch strings.ToLower(strings.TrimSpace(c.opts.PermissionPolicy)) {
	case "auto-allow":
		return pickOptionByKind(options, "allow")
	case "auto-deny":
		return pickOptionByKind(options, "reject")
	default: // ask
		if c.opts.PermissionAsker == nil {
			c.log.Warn("ACP permission request but no human to ask (headless); denying", "tool", toolName, "session_id", sessionID)
			return ""
		}
		c.mu.Lock()
		scope, ok := c.dests[sessionID]
		c.mu.Unlock()
		if !ok || scope.loc.ChannelID == "" {
			c.log.Warn("ACP permission request without a Slack location; denying", "tool", toolName, "session_id", sessionID)
			return ""
		}
		ctx := scope.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		optionID, err := c.opts.PermissionAsker.AskPermission(ctx, scope.loc, PermissionRequest{SessionID: sessionID, ToolName: toolName, Options: options})
		if err != nil {
			c.log.Warn("ACP permission ask failed; denying", "tool", toolName, "error", err)
			return ""
		}
		return optionID
	}
}

// pickOptionByKind returns the optionId of the first option whose kind matches the
// wanted action ("allow" or "reject"), preferring the _once variant over _always,
// then any kind with the wanted prefix. Returns "" when none match.
func pickOptionByKind(options []PermissionOption, want string) string {
	for _, kind := range []string{want + "_once", want + "_always"} {
		for _, o := range options {
			if o.Kind == kind {
				return o.ID
			}
		}
	}
	for _, o := range options {
		if strings.HasPrefix(o.Kind, want) {
			return o.ID
		}
	}
	return ""
}

// parsePermissionRequest extracts the session id, a human-facing tool name, and the
// offered options from a session/request_permission params object.
func parsePermissionRequest(raw json.RawMessage) (sessionID, toolName string, options []PermissionOption) {
	var p struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			Title string `json:"title"`
			Kind  string `json:"kind"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(raw, &p)
	sessionID = p.SessionID
	toolName = p.ToolCall.Title
	if toolName == "" {
		toolName = p.ToolCall.Kind
	}
	for _, o := range p.Options {
		options = append(options, PermissionOption{ID: o.OptionID, Name: o.Name, Kind: o.Kind})
	}
	return sessionID, toolName, options
}

// respondResult writes a JSON-RPC success response to the agent, echoing id.
func (c *ProcessClient) respondResult(id json.RawMessage, result any) {
	c.writeResponse(rpcOutgoingResponse{JSONRPC: "2.0", ID: id, Result: result})
}

// respondError writes a JSON-RPC error response to the agent, echoing id.
func (c *ProcessClient) respondError(id json.RawMessage, code int, message string) {
	c.writeResponse(rpcOutgoingResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}

func (c *ProcessClient) writeResponse(resp rpcOutgoingResponse) {
	encoded, err := json.Marshal(resp)
	if err != nil {
		c.log.Warn("encode ACP response", "error", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.stdin == nil {
		return
	}
	if _, err := c.stdin.Write(append(encoded, '\n')); err != nil {
		c.log.Warn("write ACP response", "error", err)
	}
}

func (c *ProcessClient) deliverNotification(notification rpcNotification) {
	if notification.Method != "session/update" {
		// Surface any ACP notification we don't implement so a protocol feature we
		// silently ignore is visible in the log rather than invisible (the class of
		// gap that hid the dropped permission request).
		c.log.Warn("ignoring unhandled ACP notification", "method", notification.Method)
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
	kind := sessionUpdateKind(notification.Params)
	c.log.Debug("ACP session/update", "session_id", sessionID, "update", kind)

	if task := extractTask(notification.Params); task != nil {
		// Block on the send: dropping task or text notifications truncates the
		// agent response in the consumer (chat handler). The readLoop is back-
		// pressured by the consumer, which is the intended behaviour.
		ch <- Event{Type: EventTask, Task: task}
		return
	}
	// A single agent message can carry binary content blocks (an image, audio, or
	// an embedded resource blob) alongside its text. Surface each as a first-class
	// attachment the chat handler uploads — emitted ahead of the text so the file
	// lands before the prose that introduces it. Block on the send for the same
	// back-pressure reason as text/task above.
	for _, a := range extractAttachments(notification.Params) {
		ch <- Event{Type: EventAttachment, Attachment: a}
	}
	event := Event{Type: EventText, Text: extractNotificationText(notification.Params)}
	if event.Text == "" {
		// An update we neither rendered as a task nor recognised as agent text.
		// Thought chunks etc. are expected and silent; but if an *unrecognised*
		// kind carries text we'd otherwise drop it, which looks like an empty
		// reply — log it at WARN so protocol drift (e.g. a goose update changing
		// the answer's envelope) is visible rather than silent.
		if kind != "" && !knownSilentUpdate(kind) && carriesText(notification.Params) {
			c.log.Warn("ACP session/update carried text under an unhandled kind; reply may appear empty", "session_id", sessionID, "update", kind)
		}
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

// extractAttachments pulls binary content blocks (image, audio, or an embedded
// resource blob) out of an agent_message_chunk/agent_message session/update and
// decodes them into AttachmentEvents the chat handler can upload. It is
// deliberately limited to agent messages — content on user-message or tool-call
// updates is not the agent replying with a file — and to embedded bytes: a
// resource_link block carries only a URI (no bytes) and is left to the text path
// to mention. Anything malformed (bad base64, missing data) is skipped rather
// than failing the turn.
func extractAttachments(raw json.RawMessage) []*AttachmentEvent {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	update, ok := m["update"].(map[string]any)
	if !ok {
		return nil
	}
	if su, _ := update["sessionUpdate"].(string); su != "agent_message_chunk" && su != "agent_message" {
		return nil
	}
	content, ok := update["content"]
	if !ok {
		return nil
	}
	var out []*AttachmentEvent
	for _, block := range contentBlocks(content) {
		if a := attachmentFromBlock(block); a != nil {
			out = append(out, a)
		}
	}
	return out
}

// contentBlocks normalises an ACP content field — which may be a single block
// object or an array of them — into a slice of block maps.
func contentBlocks(content any) []map[string]any {
	switch c := content.(type) {
	case []any:
		out := make([]map[string]any, 0, len(c))
		for _, item := range c {
			if bm, ok := item.(map[string]any); ok {
				out = append(out, bm)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{c}
	}
	return nil
}

// attachmentFromBlock decodes one content block into an AttachmentEvent, or nil
// when the block is text, a bare link, or otherwise carries no embedded bytes.
func attachmentFromBlock(block map[string]any) *AttachmentEvent {
	switch t, _ := block["type"].(string); t {
	case "image", "audio":
		data, _ := block["data"].(string)
		mimeType, _ := block["mimeType"].(string)
		return decodeAttachment(data, mimeType, "", t)
	case "resource":
		res, ok := block["resource"].(map[string]any)
		if !ok {
			return nil
		}
		blob, _ := res["blob"].(string)
		if blob == "" {
			return nil // a text resource (or bare link) — nothing to upload
		}
		mimeType, _ := res["mimeType"].(string)
		uri, _ := res["uri"].(string)
		return decodeAttachment(blob, mimeType, uri, "resource")
	default:
		return nil
	}
}

// decodeAttachment base64-decodes an embedded blob and builds an AttachmentEvent
// with a best-effort filename derived from the resource URI or the mimetype.
// Returns nil when the payload is empty or not valid base64.
func decodeAttachment(b64, mimeType, uri, kind string) *AttachmentEvent {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(data) == 0 {
		return nil
	}
	return &AttachmentEvent{
		Filename: attachmentFilename(uri, mimeType, kind),
		Mimetype: mimeType,
		Data:     data,
	}
}

// attachmentFilename derives a download name: the URI's base name when present,
// otherwise "<kind><ext>" with the extension inferred from the mimetype.
func attachmentFilename(uri, mimeType, kind string) string {
	if uri != "" {
		if base := path.Base(uri); base != "." && base != "/" && base != "" {
			return base
		}
	}
	name := kind
	if name == "" {
		name = "attachment"
	}
	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return name + exts[0]
	}
	return name
}

// extractStopReason pulls the agent's stop reason out of a session/prompt
// result, tolerating either the camelCase (ACP spec) or snake_case spelling.
// Returns "" when none is present.
func extractStopReason(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, key := range []string{"stopReason", "stop_reason"} {
		if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// sessionUpdateKind returns the update.sessionUpdate discriminator of a
// session/update notification, or "" when absent. Used for diagnostics.
func sessionUpdateKind(raw json.RawMessage) string {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	update, ok := m["update"].(map[string]any)
	if !ok {
		return ""
	}
	kind, _ := update["sessionUpdate"].(string)
	return kind
}

// knownSilentUpdate reports whether an update kind is one we deliberately do
// not turn into a chat reply (reasoning, plans, tool bookkeeping). These are
// expected to carry no agent-message text, so dropping them is not drift.
func knownSilentUpdate(kind string) bool {
	switch kind {
	case "agent_thought_chunk", "tool_call", "tool_call_update",
		"plan", "available_commands_update", "current_mode_update", "user_message_chunk":
		return true
	default:
		return false
	}
}

// carriesText reports whether a session/update's update.content holds any
// non-empty text. Used to detect an unrecognised kind that is smuggling the
// agent's reply (protocol drift) so we can log rather than silently drop it.
func carriesText(raw json.RawMessage) bool {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return false
	}
	m, ok := value.(map[string]any)
	if !ok {
		return false
	}
	update, ok := m["update"].(map[string]any)
	if !ok {
		return false
	}
	content, ok := update["content"]
	if !ok {
		return false
	}
	return strings.TrimSpace(strings.Join(extractStrings(content), "")) != ""
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
