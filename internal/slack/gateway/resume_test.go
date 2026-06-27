package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/slack/pingcard"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// recordingMessaging is the slackMessagingAPI fake used across the resume
// and restart-suggestion tests. Each Slack call is recorded so assertions
// can inspect what the helper actually sent rather than re-deriving it
// from package internals.
type recordingMessaging struct {
	mu              sync.Mutex
	postCalls       int
	postChannel     string
	postTS          string
	postThreadTS    string
	postReturnedTS  string
	postReturnedErr error

	updateCalls      int
	updateChannel    string
	updateTS         string
	updateText       string
	updateOptions    int
	updateReturnsErr error

	openCalls       int
	openUsers       []string
	openChannelID   string
	openReturnedErr error
}

func (m *recordingMessaging) PostMessageContext(_ context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.postCalls++
	m.postChannel = channelID
	if _, values, err := slack.UnsafeApplyMsgOptions("", channelID, "", options...); err == nil {
		m.postThreadTS = values.Get("thread_ts")
	}
	if m.postReturnedErr != nil {
		return "", "", m.postReturnedErr
	}
	if m.postReturnedTS == "" {
		m.postReturnedTS = "1700000000.000100"
	}
	m.postTS = m.postReturnedTS
	return channelID, m.postReturnedTS, nil
}

// recordedPostCalls returns the post count under the lock, for assertions made
// while a handler goroutine may still be running.
func (m *recordingMessaging) recordedPostCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.postCalls
}

func (m *recordingMessaging) UpdateMessageContext(_ context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateCalls++
	m.updateChannel = channelID
	m.updateTS = timestamp
	m.updateOptions = len(options)
	if _, values, err := slack.UnsafeApplyMsgOptions("", channelID, "", options...); err == nil {
		m.updateText = values.Get("text")
	}
	if m.updateReturnsErr != nil {
		return "", "", "", m.updateReturnsErr
	}
	return channelID, timestamp, m.updateText, nil
}

// recordedUpdateCalls returns the update count under the lock, for assertions
// made while a handler goroutine may still be running.
func (m *recordingMessaging) recordedUpdateCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updateCalls
}

func (m *recordingMessaging) OpenConversationContext(_ context.Context, params *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error) {
	m.openCalls++
	if params != nil {
		m.openUsers = append([]string(nil), params.Users...)
	}
	if m.openReturnedErr != nil {
		return nil, false, false, m.openReturnedErr
	}
	if m.openChannelID == "" {
		m.openChannelID = "DADMIN00"
	}
	return &slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: m.openChannelID}}}, false, false, nil
}

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestFileResumeMarkerStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewFileResumeMarkerStore(filepath.Join(dir, "restart.json"))
	marker := ResumeMarker{
		Channel:     "C1",
		ThreadTS:    "1700000000.000050",
		MessageTS:   "1700000000.000100",
		RequestedBy: "UADMIN00",
		RequestedAt: time.Now().UTC().Truncate(time.Second),
		Source:      "slash",
		Reason:      "user requested via /murtaugh restart",
	}
	if err := store.Save(marker); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got == nil {
		t.Fatal("expected Load to return marker, got nil")
	}
	if got.Channel != marker.Channel || got.MessageTS != marker.MessageTS || got.ThreadTS != marker.ThreadTS {
		t.Fatalf("round-trip mismatch: got=%#v want=%#v", got, marker)
	}
	if got.RequestedBy != marker.RequestedBy || got.Source != marker.Source || got.Reason != marker.Reason {
		t.Fatalf("metadata mismatch: got=%#v want=%#v", got, marker)
	}
	if !got.RequestedAt.Equal(marker.RequestedAt) {
		t.Fatalf("RequestedAt round-trip mismatch: got=%v want=%v", got.RequestedAt, marker.RequestedAt)
	}
	// File mode is best-effort on non-POSIX runtimes; only assert on Linux/macOS.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(store.Path())
		if err != nil {
			t.Fatalf("stat marker: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("expected marker perms 0o600, got %o", info.Mode().Perm())
		}
	}
}

func TestFileResumeMarkerStoreLoadMissingReturnsNil(t *testing.T) {
	store := NewFileResumeMarkerStore(filepath.Join(t.TempDir(), "absent.json"))
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load on missing file should not error, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil marker for missing file, got %#v", got)
	}
}

func TestFileResumeMarkerStoreClearIsIdempotent(t *testing.T) {
	store := NewFileResumeMarkerStore(filepath.Join(t.TempDir(), "absent.json"))
	if err := store.Clear(); err != nil {
		t.Fatalf("Clear on missing file should not error, got %v", err)
	}
	if err := store.Save(ResumeMarker{Channel: "C1", MessageTS: "1.0"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := store.Clear(); err != nil {
		t.Fatalf("Clear after Save returned error: %v", err)
	}
	got, err := store.Load()
	if err != nil || got != nil {
		t.Fatalf("expected marker gone after Clear, got=%#v err=%v", got, err)
	}
}

func TestFileResumeMarkerStoreSaveCreatesParentDirs(t *testing.T) {
	deep := filepath.Join(t.TempDir(), "nested", "subdir", "restart.json")
	store := NewFileResumeMarkerStore(deep)
	if err := store.Save(ResumeMarker{Channel: "C1", MessageTS: "1.0"}); err != nil {
		t.Fatalf("Save into nested path returned error: %v", err)
	}
	if _, err := os.Stat(deep); err != nil {
		t.Fatalf("expected marker at %q, stat err: %v", deep, err)
	}
}

func TestPostRestartNoticeAndSaveMarkerHappyPath(t *testing.T) {
	store := NewFileResumeMarkerStore(filepath.Join(t.TempDir(), "restart.json"))
	msg := &recordingMessaging{postReturnedTS: "1700000123.000456"}
	app := &Gateway{logger: newSilentLogger(), messaging: msg, resumeStore: store}
	app.postRestartNoticeAndSaveMarker(context.Background(), "C1", "", "UADMIN00", "slash", "test reason")
	if msg.postCalls != 1 {
		t.Fatalf("expected one PostMessageContext call, got %d", msg.postCalls)
	}
	if msg.postChannel != "C1" {
		t.Fatalf("expected post to channel C1, got %q", msg.postChannel)
	}
	marker, err := store.Load()
	if err != nil || marker == nil {
		t.Fatalf("expected marker to be saved, got=%#v err=%v", marker, err)
	}
	if marker.Channel != "C1" || marker.MessageTS != "1700000123.000456" {
		t.Fatalf("unexpected marker contents: %#v", marker)
	}
	if marker.RequestedBy != "UADMIN00" || marker.Source != "slash" || marker.Reason != "test reason" {
		t.Fatalf("marker audit fields mismatch: %#v", marker)
	}
	if marker.RequestedAt.IsZero() {
		t.Fatal("expected RequestedAt to be populated")
	}
}

func TestPostRestartNoticeAndSaveMarkerSkipsWhenStoreNil(t *testing.T) {
	msg := &recordingMessaging{}
	app := &Gateway{logger: newSilentLogger(), messaging: msg}
	app.postRestartNoticeAndSaveMarker(context.Background(), "C1", "", "UADMIN00", "slash", "test")
	if msg.postCalls != 0 {
		t.Fatalf("expected no Slack call when store is nil, got %d", msg.postCalls)
	}
}

func TestPostRestartNoticeAndSaveMarkerSkipsWhenChannelEmpty(t *testing.T) {
	store := NewFileResumeMarkerStore(filepath.Join(t.TempDir(), "restart.json"))
	msg := &recordingMessaging{}
	app := &Gateway{logger: newSilentLogger(), messaging: msg, resumeStore: store}
	app.postRestartNoticeAndSaveMarker(context.Background(), "", "", "UADMIN00", "slash", "test")
	if msg.postCalls != 0 {
		t.Fatalf("expected no Slack call when channel is empty, got %d", msg.postCalls)
	}
	if marker, _ := store.Load(); marker != nil {
		t.Fatalf("expected no marker saved when channel is empty, got %#v", marker)
	}
}

func TestPostRestartNoticeAndSaveMarkerSkipsSaveOnPostError(t *testing.T) {
	store := NewFileResumeMarkerStore(filepath.Join(t.TempDir(), "restart.json"))
	msg := &recordingMessaging{postReturnedErr: errors.New("boom")}
	app := &Gateway{logger: newSilentLogger(), messaging: msg, resumeStore: store}
	app.postRestartNoticeAndSaveMarker(context.Background(), "C1", "", "UADMIN00", "slash", "test")
	if marker, _ := store.Load(); marker != nil {
		t.Fatalf("expected no marker when post fails, got %#v", marker)
	}
}

func TestConsumeResumeMarkerNoOpWhenNoMarker(t *testing.T) {
	store := NewFileResumeMarkerStore(filepath.Join(t.TempDir(), "restart.json"))
	msg := &recordingMessaging{}
	app := &Gateway{logger: newSilentLogger(), messaging: msg, resumeStore: store}
	app.consumeResumeMarker(context.Background())
	if msg.updateCalls != 0 {
		t.Fatalf("expected no UpdateMessage call when no marker exists, got %d", msg.updateCalls)
	}
}

func TestConsumeResumeMarkerEditsAndClears(t *testing.T) {
	store := NewFileResumeMarkerStore(filepath.Join(t.TempDir(), "restart.json"))
	if err := store.Save(ResumeMarker{Channel: "C1", MessageTS: "1700000000.000100", RequestedBy: "UADMIN00", RequestedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed Save returned error: %v", err)
	}
	msg := &recordingMessaging{}
	app := &Gateway{logger: newSilentLogger(), messaging: msg, resumeStore: store}
	if !app.consumeResumeMarker(context.Background()) {
		t.Fatal("expected consumeResumeMarker to report it rendered the back-online card")
	}
	if msg.updateCalls != 1 {
		t.Fatalf("expected one UpdateMessage call, got %d", msg.updateCalls)
	}
	if msg.updateChannel != "C1" || msg.updateTS != "1700000000.000100" {
		t.Fatalf("unexpected update args: channel=%q ts=%q", msg.updateChannel, msg.updateTS)
	}
	// The "restarting…" notice must become the ping card: back-online copy plus
	// the Test-communication button. The button rides in a blocks option, so the
	// edit carries text *and* blocks (two options) rather than the old text-only
	// edit. pingcard's own tests pin the button's action_id.
	if !strings.Contains(msg.updateText, "back online") {
		t.Fatalf("expected back-online text on the edit, got %q", msg.updateText)
	}
	if msg.updateOptions < 2 {
		t.Fatalf("expected the edit to carry the ping card blocks (text+blocks), got %d option(s)", msg.updateOptions)
	}
	if pingcard.BackOnlineText == "" || !strings.Contains(pingcard.BackOnlineText, "back online") {
		t.Fatalf("pingcard.BackOnlineText drifted from the asserted copy: %q", pingcard.BackOnlineText)
	}
	if got, _ := store.Load(); got != nil {
		t.Fatalf("expected marker cleared after consume, got %#v", got)
	}
}

func TestConsumeResumeMarkerDropsStaleMarker(t *testing.T) {
	store := NewFileResumeMarkerStore(filepath.Join(t.TempDir(), "restart.json"))
	stale := time.Now().Add(-2 * resumeMarkerMaxAge).UTC()
	if err := store.Save(ResumeMarker{Channel: "C1", MessageTS: "1.0", RequestedAt: stale}); err != nil {
		t.Fatalf("seed Save returned error: %v", err)
	}
	msg := &recordingMessaging{}
	app := &Gateway{logger: newSilentLogger(), messaging: msg, resumeStore: store}
	app.consumeResumeMarker(context.Background())
	if msg.updateCalls != 0 {
		t.Fatalf("expected no UpdateMessage call for stale marker, got %d", msg.updateCalls)
	}
	if got, _ := store.Load(); got != nil {
		t.Fatalf("expected stale marker cleared, got %#v", got)
	}
}

func TestConsumeResumeMarkerClearsEvenWhenEditFails(t *testing.T) {
	store := NewFileResumeMarkerStore(filepath.Join(t.TempDir(), "restart.json"))
	if err := store.Save(ResumeMarker{Channel: "C1", MessageTS: "1.0", RequestedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed Save returned error: %v", err)
	}
	msg := &recordingMessaging{updateReturnsErr: errors.New("channel_not_found")}
	app := &Gateway{logger: newSilentLogger(), messaging: msg, resumeStore: store}
	app.consumeResumeMarker(context.Background())
	if got, _ := store.Load(); got != nil {
		t.Fatalf("expected marker cleared even when edit fails, got %#v", got)
	}
}

// TestNotifyConnectedResumesAndSuppressesStartupPing covers point 2a: when a
// fresh restart marker is waiting, notifyConnected renders the back-online card
// (one edit) and does NOT also fire the standalone startup ping.
func TestNotifyConnectedResumesAndSuppressesStartupPing(t *testing.T) {
	store := NewFileResumeMarkerStore(filepath.Join(t.TempDir(), "restart.json"))
	if err := store.Save(ResumeMarker{Channel: "C1", MessageTS: "1700000000.000100", RequestedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed Save returned error: %v", err)
	}
	msg := &recordingMessaging{}
	notifier := recordingStartupNotifier{calls: make(chan struct{}, 1)}
	app := &Gateway{logger: newSilentLogger(), messaging: msg, resumeStore: store, startupNotifier: notifier}

	app.notifyConnected(context.Background())

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && msg.recordedUpdateCalls() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if msg.recordedUpdateCalls() != 1 {
		t.Fatalf("expected the back-online edit, got %d update calls", msg.recordedUpdateCalls())
	}
	// Give any (erroneous) startup ping a chance to fire, then assert it didn't.
	select {
	case <-notifier.calls:
		t.Fatal("expected the startup ping to be suppressed while resuming from a restart")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestNotifyConnectedSendsStartupPingWithoutMarker is the other branch: a fresh
// boot with no marker greets with the normal startup ping and edits nothing.
func TestNotifyConnectedSendsStartupPingWithoutMarker(t *testing.T) {
	store := NewFileResumeMarkerStore(filepath.Join(t.TempDir(), "absent.json"))
	msg := &recordingMessaging{}
	notifier := recordingStartupNotifier{calls: make(chan struct{}, 1)}
	app := &Gateway{logger: newSilentLogger(), messaging: msg, resumeStore: store, startupNotifier: notifier}

	app.notifyConnected(context.Background())

	select {
	case <-notifier.calls:
	case <-time.After(time.Second):
		t.Fatal("expected the startup ping to fire on a fresh boot")
	}
	if got := msg.recordedUpdateCalls(); got != 0 {
		t.Fatalf("expected no message edit on a fresh boot, got %d", got)
	}
}

// TestNotifyConnectedGreetsOnlyOnce guards the once-per-process flag against
// repeated Connected events (re-connects, flaky links).
func TestNotifyConnectedGreetsOnlyOnce(t *testing.T) {
	msg := &recordingMessaging{}
	notifier := recordingStartupNotifier{calls: make(chan struct{}, 2)}
	app := &Gateway{
		logger:          newSilentLogger(),
		messaging:       msg,
		resumeStore:     NewFileResumeMarkerStore(filepath.Join(t.TempDir(), "absent.json")),
		startupNotifier: notifier,
	}

	app.notifyConnected(context.Background())
	app.notifyConnected(context.Background())

	select {
	case <-notifier.calls:
	case <-time.After(time.Second):
		t.Fatal("expected one startup ping")
	}
	select {
	case <-notifier.calls:
		t.Fatal("expected only one greeting across repeated Connected events")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHandleSlashCommandRestartPostsNoticeBeforeTrigger(t *testing.T) {
	store := NewFileResumeMarkerStore(filepath.Join(t.TempDir(), "restart.json"))
	msg := &recordingMessaging{postReturnedTS: "1700000000.000100"}
	var postedBefore bool
	restart := func(string, string, string, string) bool {
		postedBefore = msg.postCalls == 1
		return true
	}
	app := &Gateway{
		handler:     &recordingSlashHandler{},
		logger:      newSilentLogger(),
		cfg:         config.AccessConfig{AdminUser: "UADMIN00"},
		restart:     restart,
		resumeStore: store,
		messaging:   msg,
	}
	app.handleSlashCommand(context.Background(), socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: slack.SlashCommand{Command: "/murtaugh", UserID: "UADMIN00", ChannelID: "C1", Text: "restart"},
	})
	if msg.postCalls != 1 {
		t.Fatalf("expected restart notice to be posted, got %d post calls", msg.postCalls)
	}
	if !postedBefore {
		t.Fatal("expected notice to be posted BEFORE the coordinator trigger fires")
	}
	marker, err := store.Load()
	if err != nil || marker == nil {
		t.Fatalf("expected marker saved by slash handler, got=%#v err=%v", marker, err)
	}
	if marker.Channel != "C1" || marker.RequestedBy != "UADMIN00" {
		t.Fatalf("unexpected marker contents: %#v", marker)
	}
}
