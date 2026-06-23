package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/miere/murtaugh-dev-toolkit/internal/slack/pingcard"
	"github.com/miere/murtaugh-dev-toolkit/internal/slack/restartcard"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// pingClick synthesises the block_actions callback Slack delivers when the
// "Test communication" button is pressed. threadTS is the clicked message's
// own thread_ts (empty for a top-level startup card; set for the back-online
// card that is itself a threaded reply).
func pingClick(user, channel, messageTS, threadTS string) slack.InteractionCallback {
	return slack.InteractionCallback{
		Type:    slack.InteractionTypeBlockActions,
		User:    slack.User{ID: user},
		Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: channel}}},
		Message: slack.Message{Msg: slack.Msg{Timestamp: messageTS, ThreadTimestamp: threadTS}},
		ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{{
			BlockID:  pingcard.BlockID,
			ActionID: pingcard.ActionPing,
		}}},
	}
}

func TestIsPingInteraction(t *testing.T) {
	if !isPingInteraction(pingClick("U1", "C1", "1.0", "")) {
		t.Fatal("expected the Test-communication click to be recognised")
	}
	// action_id alone (no block_id) must still be recognised.
	byAction := pingClick("U1", "C1", "1.0", "")
	byAction.ActionCallback.BlockActions[0].BlockID = ""
	if !isPingInteraction(byAction) {
		t.Fatal("expected recognition by action_id alone")
	}
	foreign := suggestionInteraction("U1", "C1", "1.0", restartcard.ActionConfirm, "x")
	if isPingInteraction(foreign) {
		t.Fatal("did not expect a restart-suggestion click to be treated as a ping")
	}
	if isPingInteraction(slack.InteractionCallback{Type: slack.InteractionTypeShortcut}) {
		t.Fatal("non block_actions callback should never be recognised")
	}
}

func TestHandlePingInteractionThreadsUnderTopLevelCard(t *testing.T) {
	msg := &recordingMessaging{}
	app := &Gateway{logger: newSilentLogger(), messaging: msg}
	// A top-level startup card has no thread_ts, so the pong threads under the
	// card's own ts.
	app.handlePingInteraction(context.Background(), pingClick("U1", "C1", "1700000000.000100", ""))
	if msg.postCalls != 1 || msg.postChannel != "C1" {
		t.Fatalf("expected one pong post to C1, got calls=%d channel=%q", msg.postCalls, msg.postChannel)
	}
	if msg.postThreadTS != "1700000000.000100" {
		t.Fatalf("expected pong threaded under the card ts, got thread_ts=%q", msg.postThreadTS)
	}
}

func TestHandlePingInteractionThreadsUnderExistingThread(t *testing.T) {
	msg := &recordingMessaging{}
	app := &Gateway{logger: newSilentLogger(), messaging: msg}
	// The back-online card is itself a reply (thread_ts set); the pong must join
	// that same thread root, not nest under the reply.
	app.handlePingInteraction(context.Background(), pingClick("U1", "C1", "1700000000.000200", "1700000000.000100"))
	if msg.postThreadTS != "1700000000.000100" {
		t.Fatalf("expected pong threaded under the conversation root, got thread_ts=%q", msg.postThreadTS)
	}
}

// TestHandleInteractiveRoutesPingAwayFromWorkflow verifies the gateway handles
// the ping click itself, before the workflow engine — the whole point of moving
// the self-test into the binary.
func TestHandleInteractiveRoutesPingAwayFromWorkflow(t *testing.T) {
	wf := &recordingWorkflow{}
	msg := &recordingMessaging{}
	app := &Gateway{
		workflow:  wf,
		messaging: msg,
		logger:    newSilentLogger(),
		cfg:       config.ConfigurationConfig{AllowedUsers: []string{"U1"}},
	}
	app.handleInteractive(socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: pingClick("U1", "C1", "1700000000.000100", ""),
	})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if msg.recordedPostCalls() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls, _ := wf.stats(); calls != 0 {
		t.Fatalf("expected workflow engine to be bypassed for ping clicks, got %d calls", calls)
	}
	if got := msg.recordedPostCalls(); got != 1 {
		t.Fatalf("expected exactly one pong post from the gateway, got %d", got)
	}
}
