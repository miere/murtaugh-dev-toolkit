package native

import (
	"context"
	"fmt"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/llm"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

// defaultMaxTurns bounds a single Run when the constructor is given a
// non-positive maxTurns. It is a safety net against runaway tool loops.
const defaultMaxTurns = 40

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
		provider: provider,
		model:    model,
		tools:    byName,
		toolList: list,
		maxTurns: maxTurns,
	}
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
		Model:    l.model,
		System:   system,
		Messages: conv.Messages(),
		Tools:    l.toolSpecs(),
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

// invokeTool runs one tool call in-process, emitting status/task events around
// it, and returns the string to feed back as the tool result. Unknown tools and
// tool errors both produce an error result string rather than aborting.
func (l *Loop) invokeTool(ctx context.Context, call llm.ToolCall, emit func(agent.Event)) string {
	emit(eventStatus("Running tool " + call.Name))
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
