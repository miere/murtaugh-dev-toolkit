package native

import (
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

func TestBuildSystemPrompt_StaticBaseGuidelinesAndSkills(t *testing.T) {
	out := BuildSystemPrompt("You are Emily.", "Always be terse.", "- skill-a\n- skill-b")
	if !strings.HasPrefix(out, "You are Emily.") {
		t.Fatalf("base prompt missing or not first:\n%s", out)
	}
	for _, want := range []string{
		"<project-guidelines>", "Always be terse.", "</project-guidelines>",
		"<skills>", "skill-a", "skill-b", "</skills>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("system prompt missing %q\n---\n%s", want, out)
		}
	}
	// Order: base, then guidelines, then skills.
	if !(strings.Index(out, "You are Emily.") < strings.Index(out, "<project-guidelines>") &&
		strings.Index(out, "<project-guidelines>") < strings.Index(out, "<skills>")) {
		t.Errorf("sections out of order:\n%s", out)
	}
	// The system prompt must carry NOTHING volatile — the point of the caching relocation.
	if strings.Contains(out, "It is currently") || strings.Contains(out, "Working directory") || strings.Contains(out, "Slack channel") {
		t.Fatalf("volatile context leaked into the static system prompt:\n%s", out)
	}
}

func TestBuildSystemPrompt_EmptySectionsOmitted(t *testing.T) {
	base := "You are Emily."
	if got := BuildSystemPrompt(base, "", ""); got != base {
		t.Fatalf("empty guidelines+skills should return base unchanged, got %q", got)
	}
	// Guidelines-only, no base, no skills.
	if got := BuildSystemPrompt("", "Be kind.", ""); !strings.Contains(got, "<project-guidelines>") || strings.Contains(got, "<skills>") {
		t.Fatalf("unexpected render for guidelines-only: %q", got)
	}
}

func TestRenderTurnContext_FoldsVolatileBits(t *testing.T) {
	now := time.Date(2026, 6, 17, 18, 51, 0, 0, time.UTC)
	out := RenderTurnContext(VolatileContext{
		Now:     now,
		Cwd:     "/work/emily",
		Channel: "D0B69D0JVUK",
		Thread:  "1700.1",
	})
	for _, want := range []string{
		"<context>",
		"It is currently 2026-06-17 18:51 UTC",
		"Working directory: /work/emily",
		"Slack channel: D0B69D0JVUK (thread 1700.1)",
		"</context>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("turn context missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderTurnContext_EmptyWhenNothingSet(t *testing.T) {
	if got := RenderTurnContext(VolatileContext{}); got != "" {
		t.Fatalf("expected empty render for empty context, got %q", got)
	}
}

func TestRenderTurnContext_OmitsThreadWhenChannelOnly(t *testing.T) {
	out := RenderTurnContext(VolatileContext{Channel: "C123"})
	if !strings.Contains(out, "Slack channel: C123") {
		t.Fatalf("missing channel line: %s", out)
	}
	if strings.Contains(out, "thread") {
		t.Fatalf("should not mention thread when none set: %s", out)
	}
}

func TestVolatileContextFromRequest_CarriesSlackLocation(t *testing.T) {
	now := time.Now()
	req := agent.PromptRequest{Channel: "C1", Thread: "T1"}
	vc := VolatileContextFromRequest(req, now, "/cwd")
	if vc.Channel != "C1" || vc.Thread != "T1" || vc.Cwd != "/cwd" {
		t.Fatalf("unexpected VolatileContext: %#v", vc)
	}
	if !vc.Now.Equal(now) {
		t.Fatalf("Now not carried")
	}
}
