package native

import (
	"context"
	"fmt"
	"strings"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/llm"
)

// CompactionMode selects how the conversation is kept within the context budget.
type CompactionMode int

const (
	// CompactTruncate drops the oldest whole turn-groups. The always-on safety
	// net; preserves tool pairings and the no-consecutive-user invariant.
	CompactTruncate CompactionMode = iota
	// CompactSummarize LLM-compresses the oldest groups into a summary message,
	// falling back to truncation if the summary call fails.
	CompactSummarize
)

// parseCompaction maps a config string to a mode; unknown/empty ⇒ truncate.
func parseCompaction(s string) CompactionMode {
	if strings.EqualFold(strings.TrimSpace(s), "summarize") {
		return CompactSummarize
	}
	return CompactTruncate
}

// charsPerToken is the crude estimator Murtaugh uses when no provider-reported
// token count is available. Deliberately low (≈4 chars/token) so the estimate
// errs toward over-counting and compacts a little early rather than overshooting
// the real window.
const charsPerToken = 4

// compaction water marks as fractions of the budget: compact when the estimated
// prompt exceeds highWater, down to at most lowWater. The gap leaves headroom
// for the model's response and for tool results appended within a single turn.
const (
	highWaterNum, highWaterDen = 3, 4 // 0.75
	lowWaterNum, lowWaterDen   = 1, 2 // 0.50
)

func estimateString(s string) int { return len(s) / charsPerToken }

// estimateMessage approximates the token cost of one message: its text, any
// tool-call names/arguments, the tool correlation fields, plus a small
// per-message overhead for role/framing.
func estimateMessage(m llm.Message) int {
	chars := len(m.Text) + len(m.ToolCallID) + len(m.ToolName)
	for _, tc := range m.ToolCalls {
		chars += len(tc.Name) + len(tc.Arguments) + len(tc.ID)
	}
	return chars/charsPerToken + 4
}

func estimateMessages(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += estimateMessage(m)
	}
	return total
}

// userStarts returns the indices at which a turn-group begins (each user
// message). Truncation and summarization only ever cut on these boundaries, so a
// tool result is never separated from the assistant tool-call that produced it
// and the kept array always starts with a user message.
func userStarts(msgs []llm.Message) []int {
	var starts []int
	for i, m := range msgs {
		if m.Role == llm.RoleUser {
			starts = append(starts, i)
		}
	}
	return starts
}

// truncateToBudget drops the oldest whole turn-groups until the remaining
// messages plus the system prompt fit within target tokens, always keeping at
// least the most recent group. It returns the (possibly shortened) slice. Cutting
// only on user-message boundaries keeps tool pairings intact and guarantees the
// result still starts with a user message.
func truncateToBudget(msgs []llm.Message, systemTokens, target int) []llm.Message {
	starts := userStarts(msgs)
	if len(starts) <= 1 {
		return msgs // a single group (or none) cannot be reduced further
	}
	for i := 0; i < len(starts)-1; i++ {
		if systemTokens+estimateMessages(msgs[starts[i]:]) <= target {
			return msgs[starts[i]:]
		}
	}
	// Even the last group alone exceeds target: keep it (best effort).
	return msgs[starts[len(starts)-1]:]
}

// compact checks the conversation against the loop's context budget and, when
// over the high-water mark, compacts it down toward the low-water mark using the
// configured strategy. A no-op when no budget is set (contextLimit ≤ 0) or the
// conversation is already within budget. It emits a status event when it changes
// the array so the activity is visible. Called before each provider turn.
func (l *Loop) compact(ctx context.Context, conv *Conversation, system string, emit func(agent.Event)) {
	if l.contextLimit <= 0 {
		return
	}
	high := l.contextLimit * highWaterNum / highWaterDen
	sysTok := estimateString(system)
	estimated := sysTok + estimateMessages(conv.messages)
	// Use the provider-reported count when it is larger — it is authoritative
	// and accounts for tokenization our char estimate misses.
	current := estimated
	if conv.lastInputTokens > current {
		current = conv.lastInputTokens
	}
	if current <= high {
		return
	}

	target := l.contextLimit * lowWaterNum / lowWaterDen
	before := len(conv.messages)

	if l.compaction == CompactSummarize {
		if l.summarizeOldest(ctx, conv, sysTok, target) {
			l.afterCompact(conv, before, "summarized", emit)
			return
		}
		// Summary failed or could not reduce — fall through to truncation.
	}
	conv.messages = truncateToBudget(conv.messages, sysTok, target)
	l.afterCompact(conv, before, "truncated", emit)
}

func (l *Loop) afterCompact(conv *Conversation, before int, how string, emit func(agent.Event)) {
	if len(conv.messages) >= before {
		return
	}
	// The compacted array's new prompt size is unknown until the next turn
	// reports it; clear the stale authoritative count so the estimate governs
	// until then.
	conv.lastInputTokens = 0
	if emit != nil {
		emit(eventStatus(fmt.Sprintf("Compacted context (%s): %d → %d messages", how, before, len(conv.messages))))
	}
}

// summarizeOldest replaces the oldest turn-groups with a single summary message,
// keeping the most recent groups that fit within target. It returns false (so
// the caller falls back to truncation) when there is nothing to summarize or the
// summary call fails. The kept array still begins with a user message: the
// summary is emitted as a user message ahead of the retained groups.
func (l *Loop) summarizeOldest(ctx context.Context, conv *Conversation, systemTokens, target int) bool {
	starts := userStarts(conv.messages)
	if len(starts) <= 1 {
		return false
	}
	// Find the earliest kept group such that the retained tail fits target.
	keep := len(starts) - 1
	for keep > 0 && systemTokens+estimateMessages(conv.messages[starts[keep-1]:]) <= target {
		keep--
	}
	if keep == 0 {
		return false // the whole tail already fits; nothing to summarize
	}
	older := conv.messages[:starts[keep]]
	kept := append([]llm.Message(nil), conv.messages[starts[keep]:]...)

	summary, err := l.summarize(ctx, older)
	if err != nil || strings.TrimSpace(summary) == "" {
		return false
	}
	conv.messages = append([]llm.Message{{
		Role: llm.RoleUser,
		Text: "<conversation-summary>\n" + strings.TrimSpace(summary) + "\n</conversation-summary>",
	}}, kept...)
	return true
}

// summarize asks the provider to compress a slice of prior messages into a terse
// note. The older messages are rendered to a plain transcript and sent as a
// single user message with no tools, so the call carries no tool-pairing
// constraints of its own.
func (l *Loop) summarize(ctx context.Context, msgs []llm.Message) (string, error) {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(string(effectiveRole(m)))
		b.WriteString(": ")
		if m.Text != "" {
			b.WriteString(m.Text)
		}
		for _, tc := range m.ToolCalls {
			b.WriteString(fmt.Sprintf(" [called %s(%s)]", tc.Name, string(tc.Arguments)))
		}
		b.WriteByte('\n')
	}
	req := llm.Request{
		Model:  l.model,
		System: "You compress conversation history for an AI agent. Produce a terse summary that preserves key facts, decisions, file paths, identifiers, and unfinished tasks. No preamble, no commentary — just the summary.",
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			Text: "Summarize the following conversation so far:\n\n" + b.String(),
		}},
	}
	stream, err := l.provider.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for ev := range stream {
		if ev.Err != nil {
			return "", ev.Err
		}
		out.WriteString(ev.TextDelta)
	}
	return out.String(), nil
}
