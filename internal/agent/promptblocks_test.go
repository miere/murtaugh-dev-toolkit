package agent

import (
	"strings"
	"testing"
)

func TestPromptBlocksWithoutContext(t *testing.T) {
	blocks := promptBlocks(PromptRequest{Text: "hello"})
	if len(blocks) != 1 {
		t.Fatalf("expected a single text block when no conversation context is set, got %d", len(blocks))
	}
	if blocks[0]["type"] != "text" || blocks[0]["text"] != "hello" {
		t.Fatalf("unexpected block: %#v", blocks[0])
	}
}

func TestPromptBlocksPrependsConversationContext(t *testing.T) {
	blocks := promptBlocks(PromptRequest{Text: "please restart", Channel: "C123", Thread: "1699999999.000001"})
	if len(blocks) != 2 {
		t.Fatalf("expected a leading context block plus the user text, got %d", len(blocks))
	}
	ctx := blocks[0]["text"]
	if !strings.Contains(ctx, "C123") || !strings.Contains(ctx, "1699999999.000001") {
		t.Fatalf("context block should carry channel and thread, got %q", ctx)
	}
	if !strings.Contains(ctx, "restart") {
		t.Fatalf("context block should hint the restart tool, got %q", ctx)
	}
	// The user's own text must still be the final block, unaltered.
	if blocks[1]["text"] != "please restart" {
		t.Fatalf("expected user text preserved as last block, got %q", blocks[1]["text"])
	}
}

func TestPromptBlocksThreadOptional(t *testing.T) {
	// A channel without a thread (e.g. a channel-root chat) still injects context.
	blocks := promptBlocks(PromptRequest{Text: "hi", Channel: "C123"})
	if len(blocks) != 2 {
		t.Fatalf("expected context block even without a thread, got %d", len(blocks))
	}
}

func TestPromptBlocksInsertsHistoryBetweenContextAndText(t *testing.T) {
	blocks := promptBlocks(PromptRequest{
		Text:    "what's next?",
		Channel: "C123",
		Thread:  "1699999999.000001",
		History: "<thread-transcript>...</thread-transcript>",
	})
	if len(blocks) != 3 {
		t.Fatalf("expected context, history, and user text blocks, got %d", len(blocks))
	}
	if !strings.Contains(blocks[0]["text"], "C123") {
		t.Fatalf("first block should be the conversation context, got %q", blocks[0]["text"])
	}
	if blocks[1]["text"] != "<thread-transcript>...</thread-transcript>" {
		t.Fatalf("history block should be emitted verbatim, got %q", blocks[1]["text"])
	}
	if blocks[2]["text"] != "what's next?" {
		t.Fatalf("user text must remain the final block, got %q", blocks[2]["text"])
	}
}

func TestPromptBlocksHistoryWithoutChannel(t *testing.T) {
	// History without conversation context (defensive: not produced in practice)
	// still emits ahead of the user text rather than being dropped.
	blocks := promptBlocks(PromptRequest{Text: "hi", History: "earlier"})
	if len(blocks) != 2 {
		t.Fatalf("expected history plus user text, got %d", len(blocks))
	}
	if blocks[0]["text"] != "earlier" || blocks[1]["text"] != "hi" {
		t.Fatalf("unexpected ordering: %#v", blocks)
	}
}
