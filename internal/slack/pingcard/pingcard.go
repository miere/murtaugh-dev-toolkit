// Package pingcard is the single source of truth for Murtaugh's built-in
// communication self-test: the "Test communication" card and its pong reply.
//
// It lives in its own package, owned entirely by the binary. The card is built
// in Go and the button click is handled directly by the gateway — never routed
// through the configurable workflow engine or on-disk templates. That keeps the
// ping → pong round-trip stable no matter what agents or operators do to config
// and template files, which is the whole point of moving it here.
//
// It is also the connect-time vocabulary the restart flow reuses: the
// "back online" state of a restart is rendered as this same card, so the
// operator's first affordance after a restart is the very button that proves
// the link is healthy.
package pingcard

import "github.com/slack-go/slack"

const (
	// BlockID tags the section that carries the Test-communication button.
	// The gateway router keys on it (and the action_id below) to recognise
	// the click without depending on free-form text.
	BlockID = "murtaugh_ping"
	// ActionPing fires the pong self-test reply (handled in the gateway).
	// It is namespaced so a user-defined workflow rule can never collide with
	// or shadow the built-in self-test.
	ActionPing = "murtaugh_ping_test"

	// StartupText heads the card posted on a normal startup.
	StartupText = ":zap: The server has started."
	// BackOnlineText heads the card the restart flow renders when Murtaugh
	// returns, replacing the "restarting…" notice in place.
	BackOnlineText = ":white_check_mark: Murtaugh is back online."
	// PongText is the reply posted when the Test-communication button is clicked.
	PongText = ":recycle: The server communication is functional."

	buttonLabel = "Test communication"
)

// BuildStartup returns the card shown on a normal startup: a short notice plus
// the Test-communication button.
func BuildStartup() []slack.Block { return build(StartupText) }

// BuildBackOnline returns the card the restart flow renders when Murtaugh comes
// back online: the same Test-communication affordance, with back-online copy.
func BuildBackOnline() []slack.Block { return build(BackOnlineText) }

// build lays out the single-section card. The button is a section accessory
// (matching the original embedded template), and the section carries BlockID so
// the gateway router can recognise the callback structurally.
func build(text string) []slack.Block {
	button := slack.NewButtonBlockElement(
		ActionPing,
		"",
		slack.NewTextBlockObject(slack.PlainTextType, buttonLabel, true, false),
	)
	section := slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
		nil,
		slack.NewAccessory(button),
	)
	section.BlockID = BlockID
	return []slack.Block{section}
}
