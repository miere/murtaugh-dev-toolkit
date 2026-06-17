package native

import (
	"strings"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
)

// SystemContext carries the dynamic, per-turn context that Goose mistakenly put
// in a standalone user message (its MOIM <info-msg>). In the native loop this
// context lives ONLY in the system prompt, which BuildSystemPrompt rebuilds every
// turn and the loop passes via llm.Request.System. Keeping it out of the message
// array is the structural fix for the consecutive-user empty-reply bug.
type SystemContext struct {
	// Now is the current time; rendered to minute precision like Goose's MOIM.
	// Zero value means "omit the time line".
	Now time.Time

	// Cwd is the agent's working directory. Empty means "omit".
	Cwd string

	// SkillsIndex is an optional, pre-rendered index of available skills. Empty
	// means "omit".
	SkillsIndex string

	// Channel and Thread identify the Slack conversation this turn belongs to,
	// sourced from agent.PromptRequest. Empty fields are omitted.
	Channel string
	Thread  string
}

// SystemContextFromRequest seeds a SystemContext with the Slack location from an
// agent.PromptRequest. Time, cwd, and skills are filled in by the caller.
func SystemContextFromRequest(req agent.PromptRequest, now time.Time, cwd, skillsIndex string) SystemContext {
	return SystemContext{
		Now:         now,
		Cwd:         cwd,
		SkillsIndex: skillsIndex,
		Channel:     req.Channel,
		Thread:      req.Thread,
	}
}

// BuildSystemPrompt returns the full system prompt for a turn: the agent's static
// base prompt followed by a delimited, dynamic context block. It is pure (no I/O,
// no clock reads) so it is fully unit-testable — the caller supplies the clock
// via ctx.Now. When no dynamic context is present the base prompt is returned
// unchanged.
func BuildSystemPrompt(base string, ctx SystemContext) string {
	block := renderContextBlock(ctx)
	base = strings.TrimRight(base, "\n")
	if block == "" {
		return base
	}
	if base == "" {
		return block
	}
	return base + "\n\n" + block
}

// renderContextBlock renders the dynamic context as a single delimited block, or
// "" when there is nothing to render.
func renderContextBlock(ctx SystemContext) string {
	var lines []string
	if !ctx.Now.IsZero() {
		lines = append(lines, "It is currently "+ctx.Now.Format("2006-01-02 15:04 MST"))
	}
	if ctx.Cwd != "" {
		lines = append(lines, "Working directory: "+ctx.Cwd)
	}
	if ctx.Channel != "" {
		loc := "Slack channel: " + ctx.Channel
		if ctx.Thread != "" {
			loc += " (thread " + ctx.Thread + ")"
		}
		lines = append(lines, loc)
	}
	if s := strings.TrimSpace(ctx.SkillsIndex); s != "" {
		lines = append(lines, "Available skills:\n"+s)
	}
	if len(lines) == 0 {
		return ""
	}
	return "<context>\n" + strings.Join(lines, "\n") + "\n</context>"
}
