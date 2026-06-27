package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/slack/restartcard"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// suggestionInteraction synthesises a Slack block_actions callback as
// it would arrive at handleInteractive after the user clicked one of
// the restart-suggestion buttons. The reason mirrors the value the
// button carries in its button payload at post time.
func suggestionInteraction(user, channel, ts, actionID, reason string) slack.InteractionCallback {
	return slack.InteractionCallback{
		Type:    slack.InteractionTypeBlockActions,
		User:    slack.User{ID: user},
		Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: channel}}},
		Message: slack.Message{Msg: slack.Msg{Timestamp: ts}},
		ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{{
			BlockID:  restartcard.BlockID,
			ActionID: actionID,
			Value:    reason,
		}}},
	}
}

func TestSuggestRestartPostsToExplicitChannel(t *testing.T) {
	msg := &recordingMessaging{postReturnedTS: "1700000000.000200"}
	app := &Gateway{logger: newSilentLogger(), messaging: msg}
	channel, ts, err := app.SuggestRestart(context.Background(), "C42", "config drift detected")
	if err != nil {
		t.Fatalf("SuggestRestart returned error: %v", err)
	}
	if channel != "C42" || ts != "1700000000.000200" {
		t.Fatalf("unexpected post target: channel=%q ts=%q", channel, ts)
	}
	if msg.postCalls != 1 || msg.postChannel != "C42" {
		t.Fatalf("expected one post to C42, got calls=%d channel=%q", msg.postCalls, msg.postChannel)
	}
	if msg.openCalls != 0 {
		t.Fatalf("expected no admin DM open when channel is provided, got %d", msg.openCalls)
	}
}

func TestSuggestRestartFallsBackToAdminDM(t *testing.T) {
	msg := &recordingMessaging{postReturnedTS: "1700000000.000300", openChannelID: "DADMIN00"}
	app := &Gateway{
		logger:    newSilentLogger(),
		messaging: msg,
		cfg:       config.AccessConfig{AdminUser: "UADMIN00"},
	}
	channel, _, err := app.SuggestRestart(context.Background(), "", "stuck on boot")
	if err != nil {
		t.Fatalf("SuggestRestart returned error: %v", err)
	}
	if channel != "DADMIN00" {
		t.Fatalf("expected post into admin DM channel, got %q", channel)
	}
	if msg.openCalls != 1 || len(msg.openUsers) != 1 || msg.openUsers[0] != "UADMIN00" {
		t.Fatalf("expected OpenConversation for UADMIN00, got calls=%d users=%#v", msg.openCalls, msg.openUsers)
	}
	if msg.postChannel != "DADMIN00" {
		t.Fatalf("expected post to opened DM, got %q", msg.postChannel)
	}
}

func TestSuggestRestartNoOpWithoutDestination(t *testing.T) {
	msg := &recordingMessaging{}
	app := &Gateway{logger: newSilentLogger(), messaging: msg}
	channel, ts, err := app.SuggestRestart(context.Background(), "", "")
	if err != nil {
		t.Fatalf("SuggestRestart should be a no-op when no destination is available, got err=%v", err)
	}
	if channel != "" || ts != "" {
		t.Fatalf("expected empty results when no destination is available, got channel=%q ts=%q", channel, ts)
	}
	if msg.postCalls != 0 || msg.openCalls != 0 {
		t.Fatalf("expected no Slack traffic when locked down, got post=%d open=%d", msg.postCalls, msg.openCalls)
	}
}

func TestSuggestRestartSurfacesPostError(t *testing.T) {
	msg := &recordingMessaging{postReturnedErr: errors.New("rate_limited")}
	app := &Gateway{logger: newSilentLogger(), messaging: msg}
	_, _, err := app.SuggestRestart(context.Background(), "C1", "test")
	if err == nil {
		t.Fatal("expected SuggestRestart to surface the post error to the caller")
	}
}

func TestIsRestartSuggestionInteraction(t *testing.T) {
	confirm := suggestionInteraction("U1", "C1", "1.0", restartcard.ActionConfirm, "x")
	if !isRestartSuggestionInteraction(confirm) {
		t.Fatal("expected confirm action to be recognised")
	}
	dismiss := suggestionInteraction("U1", "C1", "1.0", restartcard.ActionDismiss, "x")
	if !isRestartSuggestionInteraction(dismiss) {
		t.Fatal("expected dismiss action to be recognised")
	}
	foreign := suggestionInteraction("U1", "C1", "1.0", "github_pull_request_approve", "x")
	foreign.ActionCallback.BlockActions[0].BlockID = "github_pull_request"
	if isRestartSuggestionInteraction(foreign) {
		t.Fatal("did not expect unrelated callback to be recognised")
	}
	if isRestartSuggestionInteraction(slack.InteractionCallback{Type: slack.InteractionTypeShortcut}) {
		t.Fatal("non block_actions callback should never be recognised")
	}
}

func TestHandleRestartSuggestionConfirmAdminFiresTrigger(t *testing.T) {
	store := NewFileResumeMarkerStore(filepathJoin(t, "restart.json"))
	msg := &recordingMessaging{postReturnedTS: "1700000000.000400"}
	restart := &recordingRestart{accept: true}
	app := &Gateway{
		logger:      newSilentLogger(),
		cfg:         config.AccessConfig{AdminUser: "UADMIN00"},
		messaging:   msg,
		resumeStore: store,
		restart:     restart.trigger,
	}
	app.handleRestartSuggestionInteraction(context.Background(),
		suggestionInteraction("UADMIN00", "C1", "1700000000.000100", restartcard.ActionConfirm, "config drift"))
	if restart.calls != 1 {
		t.Fatalf("expected coordinator to fire once, got %d", restart.calls)
	}
	if restart.lastSource != restartSourceInteractive || restart.lastUser != "UADMIN00" || restart.lastChannel != "C1" {
		t.Fatalf("unexpected restart payload: source=%q user=%q channel=%q reason=%q",
			restart.lastSource, restart.lastUser, restart.lastChannel, restart.lastReason)
	}
	if !strings.Contains(restart.lastReason, "config drift") {
		t.Fatalf("expected reason to carry button value, got %q", restart.lastReason)
	}
	if msg.postCalls != 1 {
		t.Fatalf("expected the restart-notice post, got %d", msg.postCalls)
	}
	// The notice must be threaded under the approval card the operator clicked
	// (its message ts), so the restart conversation nests where it was approved.
	if msg.postThreadTS != "1700000000.000100" {
		t.Fatalf("expected restart notice threaded under approval card, got thread_ts=%q", msg.postThreadTS)
	}
	if marker, _ := store.Load(); marker == nil || marker.ThreadTS != "1700000000.000100" {
		t.Fatalf("expected marker to record the approval-card thread, got %#v", marker)
	}
	if msg.updateCalls != 1 || msg.updateChannel != "C1" || msg.updateTS != "1700000000.000100" {
		t.Fatalf("expected suggestion edit for confirm, got calls=%d channel=%q ts=%q", msg.updateCalls, msg.updateChannel, msg.updateTS)
	}
	if !strings.Contains(msg.updateText, "UADMIN00") {
		t.Fatalf("expected suggestion edit to mention the confirming user, got %q", msg.updateText)
	}
	if marker, _ := store.Load(); marker == nil || marker.Source != restartSourceInteractive {
		t.Fatalf("expected resume marker saved with interactive source, got %#v", marker)
	}
}

func TestHandleRestartSuggestionConfirmDeniesNonAdmin(t *testing.T) {
	msg := &recordingMessaging{}
	restart := &recordingRestart{accept: true}
	app := &Gateway{
		logger:    newSilentLogger(),
		cfg:       config.AccessConfig{AdminUser: "UADMIN00", AllowedUsers: []string{"UALICE00"}},
		messaging: msg,
		restart:   restart.trigger,
	}
	app.handleRestartSuggestionInteraction(context.Background(),
		suggestionInteraction("UALICE00", "C1", "1700000000.000100", restartcard.ActionConfirm, "x"))
	if restart.calls != 0 {
		t.Fatalf("expected non-admin confirm to bypass coordinator, got %d", restart.calls)
	}
	if msg.updateCalls != 1 || !strings.Contains(msg.updateText, "admin") {
		t.Fatalf("expected denial edit mentioning admin, got calls=%d text=%q", msg.updateCalls, msg.updateText)
	}
	if msg.postCalls != 0 {
		t.Fatalf("expected no restart-notice post for denied confirm, got %d", msg.postCalls)
	}
}

func TestHandleRestartSuggestionConfirmReportsUnavailableWhenTriggerMissing(t *testing.T) {
	msg := &recordingMessaging{}
	app := &Gateway{
		logger:    newSilentLogger(),
		cfg:       config.AccessConfig{AdminUser: "UADMIN00"},
		messaging: msg,
	}
	app.handleRestartSuggestionInteraction(context.Background(),
		suggestionInteraction("UADMIN00", "C1", "1.0", restartcard.ActionConfirm, "x"))
	if msg.updateCalls != 1 || !strings.Contains(msg.updateText, "not available") {
		t.Fatalf("expected unavailable edit, got calls=%d text=%q", msg.updateCalls, msg.updateText)
	}
	if msg.postCalls != 0 {
		t.Fatalf("expected no notice post when trigger is nil, got %d", msg.postCalls)
	}
}

func TestHandleRestartSuggestionConfirmSurfacesCooldown(t *testing.T) {
	store := NewFileResumeMarkerStore(filepathJoin(t, "restart.json"))
	msg := &recordingMessaging{postReturnedTS: "1700000000.000500"}
	restart := &recordingRestart{accept: false}
	app := &Gateway{
		logger:      newSilentLogger(),
		cfg:         config.AccessConfig{AdminUser: "UADMIN00"},
		messaging:   msg,
		resumeStore: store,
		restart:     restart.trigger,
	}
	app.handleRestartSuggestionInteraction(context.Background(),
		suggestionInteraction("UADMIN00", "C1", "1.0", restartcard.ActionConfirm, "x"))
	if restart.calls != 1 {
		t.Fatalf("expected coordinator to be consulted once, got %d", restart.calls)
	}
	if msg.updateCalls != 1 || !strings.Contains(msg.updateText, "in progress") {
		t.Fatalf("expected busy edit, got calls=%d text=%q", msg.updateCalls, msg.updateText)
	}
}

func TestHandleRestartSuggestionDismiss(t *testing.T) {
	msg := &recordingMessaging{}
	restart := &recordingRestart{accept: true}
	app := &Gateway{
		logger:    newSilentLogger(),
		cfg:       config.AccessConfig{AdminUser: "UADMIN00"},
		messaging: msg,
		restart:   restart.trigger,
	}
	app.handleRestartSuggestionInteraction(context.Background(),
		suggestionInteraction("UADMIN00", "C1", "1700000000.000100", restartcard.ActionDismiss, "x"))
	if restart.calls != 0 {
		t.Fatalf("expected dismiss to bypass coordinator, got %d", restart.calls)
	}
	if msg.updateCalls != 1 || !strings.Contains(msg.updateText, "dismissed") || !strings.Contains(msg.updateText, "UADMIN00") {
		t.Fatalf("expected dismiss edit mentioning user, got calls=%d text=%q", msg.updateCalls, msg.updateText)
	}
}

func TestHandleRestartSuggestionIgnoresMissingContext(t *testing.T) {
	msg := &recordingMessaging{}
	app := &Gateway{logger: newSilentLogger(), cfg: config.AccessConfig{AdminUser: "UADMIN00"}, messaging: msg}
	app.handleRestartSuggestionInteraction(context.Background(),
		suggestionInteraction("UADMIN00", "", "", restartcard.ActionConfirm, "x"))
	if msg.updateCalls != 0 || msg.postCalls != 0 {
		t.Fatalf("expected interaction with empty context to be ignored, got update=%d post=%d", msg.updateCalls, msg.postCalls)
	}
}

// filepathJoin keeps each test in its own temp dir without dragging in
// "path/filepath" at file scope (which we'd otherwise import only for
// these few sites).
func filepathJoin(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	return dir + "/" + name
}

// TestHandleInteractiveRoutesSuggestionAwayFromWorkflow verifies that
// handleInteractive intercepts restart-suggestion callbacks before
// they reach the workflow engine. The workflow has no business
// interpreting these clicks; if it ever did, an operator clicking
// "Restart now" would also fire some random workflow action.
func TestHandleInteractiveRoutesSuggestionAwayFromWorkflow(t *testing.T) {
	wf := &recordingWorkflow{}
	msg := &recordingMessaging{}
	restart := &recordingRestart{accept: true}
	app := &Gateway{
		workflow:    wf,
		logger:      newSilentLogger(),
		cfg:         config.AccessConfig{AdminUser: "UADMIN00"},
		messaging:   msg,
		resumeStore: NewFileResumeMarkerStore(filepathJoin(t, "restart.json")),
		restart:     restart.trigger,
	}
	app.handleInteractive(socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: suggestionInteraction("UADMIN00", "C1", "1700000000.000100", restartcard.ActionConfirm, "x"),
	})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if restart.callCount() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls, _ := wf.stats(); calls != 0 {
		t.Fatalf("expected workflow engine to be bypassed for restart-suggestion clicks, got %d calls", calls)
	}
	if got := restart.callCount(); got != 1 {
		t.Fatalf("expected restart coordinator to fire once for routed confirm, got %d", got)
	}
}
