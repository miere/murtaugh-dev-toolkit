package slackapp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// recordingMessaging is the slackMessagingAPI fake used across the resume
// and restart-suggestion tests. Each Slack call is recorded so assertions
// can inspect what the helper actually sent rather than re-deriving it
// from package internals.
type recordingMessaging struct {
	postCalls       int
	postChannel     string
	postTS          string
	postReturnedTS  string
	postReturnedErr error

	updateCalls      int
	updateChannel    string
	updateTS         string
	updateText       string
	updateReturnsErr error

	openCalls       int
	openUsers       []string
	openChannelID   string
	openReturnedErr error
}

func (m *recordingMessaging) PostMessageContext(_ context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	m.postCalls++
	m.postChannel = channelID
	if m.postReturnedErr != nil {
		return "", "", m.postReturnedErr
	}
	if m.postReturnedTS == "" {
		m.postReturnedTS = "1700000000.000100"
	}
	m.postTS = m.postReturnedTS
	_ = options
	return channelID, m.postReturnedTS, nil
}

func (m *recordingMessaging) UpdateMessageContext(_ context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error) {
	m.updateCalls++
	m.updateChannel = channelID
	m.updateTS = timestamp
	if _, values, err := slack.UnsafeApplyMsgOptions("", channelID, "", options...); err == nil {
		m.updateText = values.Get("text")
	}
	if m.updateReturnsErr != nil {
		return "", "", "", m.updateReturnsErr
	}
	return channelID, timestamp, restartResumedText, nil
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
	app := &App{logger: newSilentLogger(), messaging: msg, resumeStore: store}
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
	app := &App{logger: newSilentLogger(), messaging: msg}
	app.postRestartNoticeAndSaveMarker(context.Background(), "C1", "", "UADMIN00", "slash", "test")
	if msg.postCalls != 0 {
		t.Fatalf("expected no Slack call when store is nil, got %d", msg.postCalls)
	}
}

func TestPostRestartNoticeAndSaveMarkerSkipsWhenChannelEmpty(t *testing.T) {
	store := NewFileResumeMarkerStore(filepath.Join(t.TempDir(), "restart.json"))
	msg := &recordingMessaging{}
	app := &App{logger: newSilentLogger(), messaging: msg, resumeStore: store}
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
	app := &App{logger: newSilentLogger(), messaging: msg, resumeStore: store}
	app.postRestartNoticeAndSaveMarker(context.Background(), "C1", "", "UADMIN00", "slash", "test")
	if marker, _ := store.Load(); marker != nil {
		t.Fatalf("expected no marker when post fails, got %#v", marker)
	}
}

func TestConsumeResumeMarkerNoOpWhenNoMarker(t *testing.T) {
	store := NewFileResumeMarkerStore(filepath.Join(t.TempDir(), "restart.json"))
	msg := &recordingMessaging{}
	app := &App{logger: newSilentLogger(), messaging: msg, resumeStore: store}
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
	app := &App{logger: newSilentLogger(), messaging: msg, resumeStore: store}
	app.consumeResumeMarker(context.Background())
	if msg.updateCalls != 1 {
		t.Fatalf("expected one UpdateMessage call, got %d", msg.updateCalls)
	}
	if msg.updateChannel != "C1" || msg.updateTS != "1700000000.000100" {
		t.Fatalf("unexpected update args: channel=%q ts=%q", msg.updateChannel, msg.updateTS)
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
	app := &App{logger: newSilentLogger(), messaging: msg, resumeStore: store}
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
	app := &App{logger: newSilentLogger(), messaging: msg, resumeStore: store}
	app.consumeResumeMarker(context.Background())
	if got, _ := store.Load(); got != nil {
		t.Fatalf("expected marker cleared even when edit fails, got %#v", got)
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
	app := &App{
		handler:     &recordingSlashHandler{},
		logger:      newSilentLogger(),
		cfg:         config.ConfigurationConfig{AdminUser: "UADMIN00"},
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
