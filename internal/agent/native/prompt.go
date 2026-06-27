package native

import (
	"strings"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

// VolatileContext is the per-turn context that changes turn-to-turn (the current
// time) or conversation-to-conversation (the Slack location). It is rendered onto
// the *current user turn* — never the system prompt — so the system stays
// byte-identical across turns and conversations and the provider can cache it.
//
// It still never becomes a standalone message: client.Prompt folds the rendered
// block into the user's own message text, preserving the MOIM-safety invariant
// that fixed the Goose empty-reply bug. See the native-context-caching decision:
// keeping the system static is the simplest way to ride provider prompt-caching.
type VolatileContext struct {
	// Now is the current time; rendered to minute precision. Zero value omits it.
	Now time.Time
	// Cwd is the agent's working directory. Empty omits it.
	Cwd string
	// Channel and Thread identify the Slack conversation. Empty fields are omitted.
	Channel string
	Thread  string
}

// VolatileContextFromRequest seeds the per-turn context from a prompt request and
// the caller-supplied clock and working directory.
func VolatileContextFromRequest(req agent.PromptRequest, now time.Time, cwd string) VolatileContext {
	return VolatileContext{Now: now, Cwd: cwd, Channel: req.Channel, Thread: req.Thread}
}

// RenderTurnContext renders the volatile context as a single delimited <context>
// block to prepend to the user's message, or "" when there is nothing to render.
func RenderTurnContext(vc VolatileContext) string {
	var lines []string
	if !vc.Now.IsZero() {
		lines = append(lines, "It is currently "+vc.Now.Format("2006-01-02 15:04 MST"))
	}
	if vc.Cwd != "" {
		lines = append(lines, "Working directory: "+vc.Cwd)
	}
	if vc.Channel != "" {
		loc := "Slack channel: " + vc.Channel
		if vc.Thread != "" {
			loc += " (thread " + vc.Thread + ")"
		}
		lines = append(lines, loc)
	}
	if len(lines) == 0 {
		return ""
	}
	return "<context>\n" + strings.Join(lines, "\n") + "\n</context>"
}

// BuildSystemPrompt returns the STATIC system prompt, assembled from up to three
// stable sections: the agent's base prompt, the auto-loaded AGENTS.md project
// guidelines, and the skills index. Nothing volatile lives here, so the result
// is identical across turns and conversations — the cacheable prefix. Empty
// sections are omitted; sections are joined with a blank line.
func BuildSystemPrompt(base, agentsDoc, skillsIndex string) string {
	var b strings.Builder
	add := func(s string) {
		s = strings.TrimRight(s, "\n")
		if s == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(s)
	}
	add(base)
	if a := strings.TrimSpace(agentsDoc); a != "" {
		add("<project-guidelines>\n" + a + "\n</project-guidelines>")
	}
	if idx := strings.TrimSpace(skillsIndex); idx != "" {
		add("<skills>\n" + idx + "\n</skills>")
	}
	return b.String()
}
