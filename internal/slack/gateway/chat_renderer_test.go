package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
)

// newTestSectionRenderer wires a sectionRenderer to the test fakes: every text
// section is a StreamWriter over api, every tool block a StatusLineWriter over
// msgr. A huge throttle interval keeps the status line's in-place updates from
// firing, so post/start counts reflect sections, not refreshes.
func newTestSectionRenderer(api *fakeStreamAPI, msgr *fakeStatusMessenger) *sectionRenderer {
	return newSectionRenderer(
		func() *StreamWriter {
			return NewStreamWriter(api, "C1", StreamWriterOptions{ThreadTS: "100.0", Interval: time.Hour, MinChars: 1, Logger: discardLogger()})
		},
		func() *StatusLineWriter {
			return NewStatusLineWriter(msgr, "C1", "100.0", time.Hour, discardLogger())
		},
		discardLogger(),
	)
}

// TestSectionRenderer_AlternatesBlocksAndMessages is the core UX guarantee: tool
// activity and reply text are rendered as a SEPARATE, ordered sequence of Slack
// messages — a tool block per contiguous tool run, a streamed message per
// contiguous text run — never mixed, regardless of model interleaving. Mirrors
// the canonical "run read/skill/write → talk → run a tool → wrap up" flow, which
// must produce exactly: block, message, block, message.
func TestSectionRenderer_AlternatesBlocksAndMessages(t *testing.T) {
	api := &fakeStreamAPI{}
	msgr := &fakeStatusMessenger{}
	r := newTestSectionRenderer(api, msgr)
	ctx := context.Background()

	// Block 1: three contiguous tools coalesce into one block.
	_ = r.Task(ctx, &agent.TaskEvent{ID: "1", Title: "read", Status: agent.TaskStatusInProgress})
	_ = r.Task(ctx, &agent.TaskEvent{ID: "2", Title: "skill", Status: agent.TaskStatusInProgress})
	_ = r.Task(ctx, &agent.TaskEvent{ID: "3", Title: "write", Status: agent.TaskStatusInProgress})
	// Message 1.
	_ = r.Text(ctx, "here is what I found")
	// Block 2.
	_ = r.Task(ctx, &agent.TaskEvent{ID: "4", Title: "fetch", Status: agent.TaskStatusInProgress})
	// Message 2 (the wrap-up).
	_ = r.Text(ctx, "all done")
	if err := r.Finish(ctx, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if msgr.posts != 2 {
		t.Errorf("expected 2 tool-block messages, got %d", msgr.posts)
	}
	if len(api.startOptions) != 2 {
		t.Errorf("expected 2 text messages, got %d", len(api.startOptions))
	}
	if api.stops != 2 {
		t.Errorf("expected both text messages to be stopped, got %d", api.stops)
	}
}

// TestSectionRenderer_BlockSummarizesItsTools verifies a finalized tool block
// resolves to a compact summary of the tools it ran, not a single "Done
// thinking".
func TestSectionRenderer_BlockSummarizesItsTools(t *testing.T) {
	api := &fakeStreamAPI{}
	msgr := &fakeStatusMessenger{}
	r := newTestSectionRenderer(api, msgr)
	ctx := context.Background()

	_ = r.Task(ctx, &agent.TaskEvent{ID: "1", Title: "read", Status: agent.TaskStatusInProgress})
	_ = r.Task(ctx, &agent.TaskEvent{ID: "2", Title: "skill", Status: agent.TaskStatusInProgress})
	_ = r.Task(ctx, &agent.TaskEvent{ID: "3", Title: "write", Status: agent.TaskStatusInProgress})
	if err := r.Finish(ctx, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	summary := optionValue(msgr.updateOptions, "text")
	if !strings.Contains(summary, "read · skill · write") {
		t.Errorf("block summary = %q, want it to list the tools that ran", summary)
	}
}

// TestSectionRenderer_TextOnlyIsASingleMessage confirms a pure reply (no tools)
// stays one streamed message with no tool block — the common chat case.
func TestSectionRenderer_TextOnlyIsASingleMessage(t *testing.T) {
	api := &fakeStreamAPI{}
	msgr := &fakeStatusMessenger{}
	r := newTestSectionRenderer(api, msgr)
	ctx := context.Background()

	_ = r.Text(ctx, "just an answer")
	if err := r.Finish(ctx, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if msgr.posts != 0 {
		t.Errorf("a tool-less reply must post no tool block, got %d", msgr.posts)
	}
	if len(api.startOptions) != 1 || api.stops != 1 {
		t.Errorf("expected one streamed message, got starts=%d stops=%d", len(api.startOptions), api.stops)
	}
}
