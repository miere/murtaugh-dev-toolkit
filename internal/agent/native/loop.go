package native

import (
	"context"
	"fmt"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/llm"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

// defaultMaxTurns bounds a single Run when the constructor is given a
// non-positive maxTurns. It is a safety net against runaway tool loops.
const defaultMaxTurns = 40

// defaultToolHeartbeatInterval is how often a still-running tool emits a status
// event so the gateway's idle watchdog (which resets on any event) does not treat
// a turn blocked in a long tool call as stalled. The canonical cases are the
// `ask` tool and the approval gate waiting on a human; it also helps any slow
// tool. Must stay well under the gateway's request_timeout (default 10m). Held on
// the Loop (not a global) so tests can shorten it per-instance without racing.
const defaultToolHeartbeatInterval = 30 * time.Second

// Loop runs the in-process tool-calling turn loop for a native agent. It owns the
// stream→run-tools→repeat cycle against an llm.Provider, executing matching
// tools.Tool instances in-process (no subprocess, no MCP round-trip) and emitting
// agent.Event values for streaming text, status, task progress, completion, and
// errors. It holds no per-session state; the caller supplies the Conversation.
type Loop struct {
	provider llm.Provider
	model    string
	tools    map[string]tools.Tool
	toolList []tools.Tool // registration order, for stable ToolSpec ordering
	maxTurns int
	// contextLimit is the conversation token budget; 0 disables compaction.
	// compaction selects the strategy. Set via WithCompaction.
	contextLimit int
	compaction   CompactionMode
	// cacheRetention enables provider prompt-caching on each turn's request
	// ("5m"/"1h"); empty disables it. Set via WithCache.
	cacheRetention string
	// approver gates side-effecting tool calls behind human approval. nil (the
	// default) disables gating. Set via WithApprover.
	approver Approver
	// heartbeatInterval is how often a running tool emits a keep-alive status
	// event. Defaults to defaultToolHeartbeatInterval; tests shorten it.
	heartbeatInterval time.Duration
}

// Approver gates a side-effecting tool call behind human approval. The loop
// consults it only for tools that classify a call as needing approval
// (tools.ApprovalClassifier); a nil approver disables gating entirely. The
// concrete implementation lives outside this package (it asks over Slack), so
// the loop stays transport-agnostic.
type Approver interface {
	// Approve asks the user to confirm a side-effecting tool call. summary is a
	// short human description (e.g. the shell command). It returns whether to run
	// the tool and, when not, a note to feed back to the model as the tool result.
	Approve(ctx context.Context, toolName, summary string) (allowed bool, note string)
}

// NewLoop constructs a Loop. maxTurns ≤ 0 falls back to defaultMaxTurns.
func NewLoop(provider llm.Provider, model string, ts []tools.Tool, maxTurns int) *Loop {
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}
	byName := make(map[string]tools.Tool, len(ts))
	list := make([]tools.Tool, 0, len(ts))
	for _, t := range ts {
		if t == nil {
			continue
		}
		if _, dup := byName[t.Name()]; dup {
			continue
		}
		byName[t.Name()] = t
		list = append(list, t)
	}
	return &Loop{
		provider:          provider,
		model:             model,
		tools:             byName,
		toolList:          list,
		maxTurns:          maxTurns,
		heartbeatInterval: defaultToolHeartbeatInterval,
	}
}

// WithCompaction sets the conversation token budget and strategy. A limit ≤ 0
// disables compaction (the conversation grows unbounded). Returns the receiver.
func (l *Loop) WithCompaction(contextLimit int, mode CompactionMode) *Loop {
	l.contextLimit = contextLimit
	l.compaction = mode
	return l
}

// WithCache sets the provider prompt-cache retention applied to each turn's
// request ("5m"/"1h"; empty disables). The static system prompt makes the
// cached prefix stable across turns and conversations. Returns the receiver.
func (l *Loop) WithCache(retention string) *Loop {
	l.cacheRetention = retention
	return l
}

// WithApprover sets the approval gate consulted before a side-effecting tool
// runs. nil (the default) disables gating. Returns the receiver.
func (l *Loop) WithApprover(a Approver) *Loop {
	l.approver = a
	return l
}

// toolSpecs builds the provider-facing tool advertisement list from the loop's
// tools, in registration order.
func (l *Loop) toolSpecs() []llm.ToolSpec {
	if len(l.toolList) == 0 {
		return nil
	}
	specs := make([]llm.ToolSpec, 0, len(l.toolList))
	for _, t := range l.toolList {
		specs = append(specs, llm.ToolSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Schema:      t.InputSchema(),
		})
	}
	return specs
}

// turnResult captures one provider turn's accumulated output.
type turnResult struct {
	text       string
	toolCalls  []llm.ToolCall
	stopReason string
}

// Run drives the turn loop to completion. It rebuilds the provider Request each
// turn from conv plus the (already-built, per-turn) system prompt, streams the
// completion, forwards text deltas as EventText, accumulates tool calls, and when
// a turn yields tool calls executes each matching tool in-process and appends the
// assistant tool-call message and the tool-result messages to conv before
// looping. It ends when a turn reports end_turn with no pending tool calls, when
// maxTurns is hit, or on a hard error. EventComplete (with stop reason) is
// emitted on success; EventError on failure.
//
// A tool that errors does NOT abort the loop: its error is fed back as a tool
// result so the model can recover. Empty completions are handled by tryRecover
// (recovery.go) so the loop does not spin forever.
func (l *Loop) Run(ctx context.Context, conv *Conversation, system string, emit func(agent.Event)) (stopReason string, err error) {
	for turn := 0; turn < l.maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			emit(eventError(err))
			return "", err
		}

		// Keep the conversation within the context budget before sending it.
		// Runs every turn so growth from tool results (appended within a prompt)
		// is also caught, not just growth across prompts.
		l.compact(ctx, conv, system, emit)

		// Invariant guard: the array we are about to send must never contain a
		// tool-result immediately followed by a standalone user message. This is
		// the structural defense against the Goose MOIM empty-reply bug.
		if gErr := assertNoConsecutiveUserAfterTool(conv.Messages()); gErr != nil {
			emit(eventError(gErr))
			return "", gErr
		}

		res, sErr := l.runTurn(ctx, conv, system, emit)
		if sErr != nil {
			emit(eventError(sErr))
			return "", sErr
		}

		// No tool calls → the model is done speaking this turn.
		if len(res.toolCalls) == 0 {
			// Empty completion (no text, no tool calls): attempt bounded
			// recovery before giving up so a single provider hiccup does not
			// surface as a silent reply.
			if res.text == "" {
				recovered, rRes, rErr := l.tryRecover(ctx, conv, system, emit)
				if rErr != nil {
					emit(eventError(rErr))
					return "", rErr
				}
				if recovered {
					res = rRes
					if len(res.toolCalls) > 0 {
						l.appendAndRunTools(ctx, conv, res, emit)
						continue
					}
				}
			}
			// Record the assistant's (possibly empty) closing text so the
			// conversation tail is a proper assistant turn, then finish.
			conv.AppendAssistant(res.text, nil)
			stop := res.stopReason
			if stop == "" {
				stop = "end_turn"
			}
			emit(eventComplete(stop))
			return stop, nil
		}

		// Tool calls present: append the assistant tool-call turn + results and
		// loop for the model's follow-up.
		l.appendAndRunTools(ctx, conv, res, emit)
	}

	// maxTurns exhausted.
	emit(eventComplete("max_turns"))
	return "max_turns", nil
}

// runTurn performs one provider Stream call, forwarding text deltas and
// accumulating tool calls. It does not mutate conv.
func (l *Loop) runTurn(ctx context.Context, conv *Conversation, system string, emit func(agent.Event)) (turnResult, error) {
	req := llm.Request{
		Model:          l.model,
		System:         system,
		Messages:       conv.Messages(),
		Tools:          l.toolSpecs(),
		CacheRetention: l.cacheRetention,
	}
	stream, err := l.provider.Stream(ctx, req)
	if err != nil {
		return turnResult{}, fmt.Errorf("native: provider stream: %w", err)
	}

	var res turnResult
	for ev := range stream {
		if ev.Err != nil {
			return turnResult{}, fmt.Errorf("native: stream event: %w", ev.Err)
		}
		if ev.TextDelta != "" {
			res.text += ev.TextDelta
			emit(eventText(ev.TextDelta))
		}
		if ev.ToolCall != nil {
			res.toolCalls = append(res.toolCalls, *ev.ToolCall)
		}
		if ev.Done {
			res.stopReason = ev.StopReason
			if ev.Usage != nil {
				conv.recordInputTokens(ev.Usage.InputTokens)
			}
		}
	}
	return res, nil
}

// appendAndRunTools records the assistant's tool-call turn, then executes each
// tool in-process and appends a tool-result message for each. A tool error
// becomes an error tool-result (not a loop abort) so the model can recover.
func (l *Loop) appendAndRunTools(ctx context.Context, conv *Conversation, res turnResult, emit func(agent.Event)) {
	conv.AppendAssistant(res.text, res.toolCalls)
	for _, call := range res.toolCalls {
		out := l.invokeTool(ctx, call, emit)
		conv.AppendToolResult(call.ID, call.Name, out)
	}
}

// invokeTool runs one tool call in-process, emitting task events around it, and
// returns the string to feed back as the tool result. Unknown tools and tool
// errors both produce an error result string rather than aborting. Tool activity
// is reported ONLY as task events (the gateway renders them in the thinking/
// status surface, above the answer) — never as EventStatus/EventText, which the
// gateway streams into the answer message itself.
func (l *Loop) invokeTool(ctx context.Context, call llm.ToolCall, emit func(agent.Event)) string {
	emit(eventTask(call.ID, call.Name, agent.TaskStatusInProgress, ""))

	t, ok := l.tools[call.Name]
	if !ok {
		msg := fmt.Sprintf("error: unknown tool %q", call.Name)
		emit(eventTask(call.ID, call.Name, agent.TaskStatusFailed, msg))
		return msg
	}

	args, decErr := decodeToolArgs(call.Arguments)
	if decErr != nil {
		msg := fmt.Sprintf("error: invalid arguments for tool %q: %v", call.Name, decErr)
		emit(eventTask(call.ID, call.Name, agent.TaskStatusFailed, msg))
		return msg
	}

	// Keep the turn alive while we wait on the (possibly slow) approval gate and
	// the tool run: both can block — the gate awaiting a human, a tool awaiting a
	// human or a long job — and would otherwise emit nothing between the
	// in-progress event above and the result, letting the gateway's idle watchdog
	// cancel the turn. The heartbeat emits a meta status event the gateway renders
	// as nothing but resets its timer on. Started before the gate so the approval
	// wait is covered too.
	stopHeartbeat := make(chan struct{})
	go heartbeat(ctx, emit, stopHeartbeat, l.heartbeatInterval)
	defer close(stopHeartbeat)

	// Approval gate: for a tool that classifies this call as side-effecting, ask
	// the user before running it. A denied/timed-out call is skipped with a note
	// fed back to the model — never an abort, so the tool-call/result pairing
	// stays intact.
	if l.approver != nil {
		if classifier, ok := t.(tools.ApprovalClassifier); ok && classifier.RequiresApproval(args) {
			// Prefer the tool's own richer summary (e.g. a job's command +
			// schedule) so the human approves the real thing; fall back to the
			// gate's generic args rendering for tools that don't implement it.
			summary := approvalSummary(args)
			if summarizer, ok := t.(tools.ApprovalSummarizer); ok {
				summary = summarizer.ApprovalSummary(args)
			}
			allowed, note := l.approver.Approve(ctx, call.Name, summary)
			if !allowed {
				emit(eventTask(call.ID, call.Name, agent.TaskStatusFailed, note))
				return note
			}
		}
	}

	result, invErr := t.Invoke(ctx, args)
	if invErr != nil {
		msg := fmt.Sprintf("error: %v", invErr)
		emit(eventTask(call.ID, call.Name, agent.TaskStatusFailed, msg))
		return msg
	}

	out := toolResultString(result)
	emit(eventTask(call.ID, call.Name, agent.TaskStatusComplete, out))
	return out
}

// heartbeat emits a status event every toolHeartbeatInterval until stop is closed
// or ctx is cancelled. A still-running tool produces no events of its own, so
// without this a long/blocking tool call would let the gateway's inactivity
// watchdog trip mid-turn. The status event is meta only — the gateway resets its
// idle timer on it but renders nothing to the reply.
func heartbeat(ctx context.Context, emit func(agent.Event), stop <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			emit(eventStatus("still working…"))
		}
	}
}
