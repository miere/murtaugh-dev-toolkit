package pingcard

import (
	"testing"

	"github.com/slack-go/slack"
)

// pingButton walks the card's single section and returns its accessory button,
// so tests assert on the rendered structure rather than re-deriving it.
func pingButton(t *testing.T, blocks []slack.Block) *slack.ButtonBlockElement {
	t.Helper()
	if len(blocks) != 1 {
		t.Fatalf("expected exactly one block, got %d", len(blocks))
	}
	section, ok := blocks[0].(*slack.SectionBlock)
	if !ok {
		t.Fatalf("expected a section block, got %T", blocks[0])
	}
	if section.BlockID != BlockID {
		t.Fatalf("expected section block_id %q, got %q", BlockID, section.BlockID)
	}
	if section.Accessory == nil || section.Accessory.ButtonElement == nil {
		t.Fatal("expected the section to carry a button accessory")
	}
	return section.Accessory.ButtonElement
}

func TestBuildStartupCarriesPingButton(t *testing.T) {
	blocks := BuildStartup()
	section := blocks[0].(*slack.SectionBlock)
	if section.Text == nil || section.Text.Text != StartupText {
		t.Fatalf("expected startup text %q, got %#v", StartupText, section.Text)
	}
	if btn := pingButton(t, blocks); btn.ActionID != ActionPing {
		t.Fatalf("expected button action_id %q, got %q", ActionPing, btn.ActionID)
	}
}

func TestBuildBackOnlineReusesPingButton(t *testing.T) {
	blocks := BuildBackOnline()
	section := blocks[0].(*slack.SectionBlock)
	if section.Text == nil || section.Text.Text != BackOnlineText {
		t.Fatalf("expected back-online text %q, got %#v", BackOnlineText, section.Text)
	}
	// The back-online card must offer the very same self-test button so the
	// operator's first post-restart action proves the link is healthy.
	if btn := pingButton(t, blocks); btn.ActionID != ActionPing {
		t.Fatalf("expected back-online button action_id %q, got %q", ActionPing, btn.ActionID)
	}
}
