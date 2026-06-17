package native

import (
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
)

func TestBuildSystemPrompt_FoldsDynamicContext(t *testing.T) {
	now := time.Date(2026, 6, 17, 18, 51, 0, 0, time.UTC)
	ctx := SystemContext{
		Now:         now,
		Cwd:         "/work/emily",
		SkillsIndex: "- skill-a\n- skill-b",
		Channel:     "D0B69D0JVUK",
		Thread:      "1700.1",
	}
	out := BuildSystemPrompt("You are Emily.", ctx)

	if !strings.HasPrefix(out, "You are Emily.") {
		t.Fatalf("base prompt missing or not first:\n%s", out)
	}
	for _, want := range []string{
		"It is currently 2026-06-17 18:51 UTC",
		"Working directory: /work/emily",
		"Slack channel: D0B69D0JVUK (thread 1700.1)",
		"Available skills:",
		"skill-a",
		"<context>",
		"</context>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("system prompt missing %q\n---\n%s", want, out)
		}
	}
}

func TestBuildSystemPrompt_NoContextReturnsBaseUnchanged(t *testing.T) {
	base := "You are Emily."
	if got := BuildSystemPrompt(base, SystemContext{}); got != base {
		t.Fatalf("empty context should return base unchanged, got %q", got)
	}
}

func TestBuildSystemPrompt_OmitsThreadWhenChannelOnly(t *testing.T) {
	out := BuildSystemPrompt("base", SystemContext{Channel: "C123"})
	if !strings.Contains(out, "Slack channel: C123") {
		t.Fatalf("missing channel line: %s", out)
	}
	if strings.Contains(out, "thread") {
		t.Fatalf("should not mention thread when none set: %s", out)
	}
}

func TestSystemContextFromRequest_CarriesSlackLocation(t *testing.T) {
	now := time.Now()
	req := agent.PromptRequest{Channel: "C1", Thread: "T1"}
	ctx := SystemContextFromRequest(req, now, "/cwd", "idx")
	if ctx.Channel != "C1" || ctx.Thread != "T1" || ctx.Cwd != "/cwd" || ctx.SkillsIndex != "idx" {
		t.Fatalf("unexpected SystemContext: %#v", ctx)
	}
	if !ctx.Now.Equal(now) {
		t.Fatalf("Now not carried")
	}
}
