package gateway

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/miere/murtaugh-dev-toolkit/internal/journal"
	"github.com/miere/murtaugh-dev-toolkit/internal/unfurl"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// journalSpy captures the events a handler records.
type journalSpy struct {
	mu     sync.Mutex
	events []journal.Event
}

func (s *journalSpy) Record(_ context.Context, e journal.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *journalSpy) byKind(kind string) []journal.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []journal.Event
	for _, e := range s.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestUnfurlHandlerRecordsRenderAndPost(t *testing.T) {
	api := &fakeUnfurler{}
	rec := &journalSpy{}
	handler := newTemplateUnfurlHandler(t, api).WithRecorder(rec)

	ctx := journal.WithCorrID(context.Background(), "gw_unfurl")
	if err := handler.Handle(ctx, LinkSharedRequest{
		TeamID:    "T1",
		ChannelID: "C1",
		UserID:    "U1",
		MessageTS: "1700000000.000100",
		Links:     []slackevents.SharedLinks{{Domain: "github.com", URL: "https://github.com/acme/widgets/pull/42"}},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	render := rec.byKind("unfurl.render")
	if len(render) != 1 || render[0].Level != journal.LevelInfo {
		t.Fatalf("expected one info unfurl.render, got %+v", render)
	}
	if render[0].Keys.RuleID != "github-pr" || render[0].Keys.ChannelID != "C1" || render[0].CorrID != "gw_unfurl" {
		t.Fatalf("unfurl.render keys/corr wrong: %+v", render[0])
	}
	post := rec.byKind("unfurl.post")
	if len(post) != 1 || post[0].Level != journal.LevelInfo {
		t.Fatalf("expected one info unfurl.post, got %+v", post)
	}
}

func TestUnfurlHandlerRecordsNoMatch(t *testing.T) {
	api := &fakeUnfurler{}
	rec := &journalSpy{}
	handler := newTemplateUnfurlHandler(t, api).WithRecorder(rec)

	if err := handler.Handle(context.Background(), LinkSharedRequest{
		ChannelID: "C1",
		MessageTS: "1.1",
		Links:     []slackevents.SharedLinks{{Domain: "example.com", URL: "https://example.com/not-matching"}},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	noMatch := rec.byKind("unfurl.no_match")
	if len(noMatch) != 1 || noMatch[0].Level != journal.LevelDebug {
		t.Fatalf("expected one debug unfurl.no_match, got %+v", noMatch)
	}
	if len(rec.byKind("unfurl.post")) != 0 {
		t.Fatalf("did not expect an unfurl.post with no matches")
	}
}

func TestJournalSweeperRunsAtStartup(t *testing.T) {
	var calls atomic.Int32
	app := &Gateway{
		logger:            discardLogger(),
		journalSweep:      func(context.Context) error { calls.Add(1); return nil },
		journalSweepEvery: time.Hour, // long, so only the startup sweep fires within the test
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.startJournalSweeper(ctx)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && calls.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatal("expected a startup sweep")
	}
}

func TestJournalSweeperNoopWhenUnset(t *testing.T) {
	// A Gateway with no sweep wired must not start a goroutine or panic.
	app := &Gateway{logger: discardLogger()}
	app.startJournalSweeper(context.Background())
}

func TestHandleInteractiveRecordsIngress(t *testing.T) {
	rec := &journalSpy{}
	app := &Gateway{
		workflow: &recordingWorkflow{},
		socket:   nil, // a.ack is a no-op when socket is nil
		logger:   discardLogger(),
		cfg:      config.ConfigurationConfig{AllowedUsers: []string{"UALICE00"}},
		recorder: rec,
	}
	app.handleInteractive(socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: slack.InteractionCallback{
			Type:    slack.InteractionTypeBlockActions,
			User:    slack.User{ID: "UALICE00"},
			Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C1"}}},
		},
	})

	// interactive.received is recorded synchronously before the workflow
	// goroutine is launched, so it is safe to read immediately.
	got := rec.byKind("interactive.received")
	if len(got) != 1 {
		t.Fatalf("expected one interactive.received event, got %d", len(got))
	}
	e := got[0]
	if e.Stream != journal.StreamGateway || e.Level != journal.LevelInfo {
		t.Fatalf("unexpected envelope: %+v", e)
	}
	if e.Keys.ChannelID != "C1" || e.Keys.UserID != "UALICE00" {
		t.Fatalf("unexpected keys: %+v", e.Keys)
	}
	if !strings.HasPrefix(e.CorrID, "gw_") {
		t.Fatalf("expected a minted corr id, got %q", e.CorrID)
	}
}

func TestHandleInteractiveUnauthorizedRecordsNothing(t *testing.T) {
	rec := &journalSpy{}
	app := &Gateway{
		workflow: &recordingWorkflow{},
		socket:   nil,
		logger:   discardLogger(),
		cfg:      config.ConfigurationConfig{AllowedUsers: []string{"UALICE00"}},
		recorder: rec,
	}
	app.handleInteractive(socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: slack.InteractionCallback{User: slack.User{ID: "UEVIL000"}},
	})
	time.Sleep(20 * time.Millisecond)
	if len(rec.byKind("interactive.received")) != 0 {
		t.Fatalf("unauthorized interactive should record nothing")
	}
}

func TestHandleSlashCommandRecordsIngress(t *testing.T) {
	rec := &journalSpy{}
	app := &Gateway{
		handler:  NewDefaultSlashCommandHandler(nil),
		socket:   nil,
		logger:   discardLogger(),
		cfg:      config.ConfigurationConfig{AllowedUsers: []string{"UALICE00"}},
		recorder: rec,
	}
	app.handleSlashCommand(context.Background(), socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: slack.SlashCommand{Command: "/murtaugh", Text: "hello", UserID: "UALICE00", ChannelID: "C1", TeamID: "T1"},
	})
	got := rec.byKind("slash.command")
	if len(got) != 1 {
		t.Fatalf("expected one slash.command event, got %d", len(got))
	}
	if got[0].Keys.UserID != "UALICE00" || got[0].Keys.ChannelID != "C1" {
		t.Fatalf("unexpected keys: %+v", got[0].Keys)
	}
	if !strings.HasPrefix(got[0].CorrID, "gw_") {
		t.Fatalf("expected a minted corr id, got %q", got[0].CorrID)
	}
}

func TestUnfurlHandlerRecordsBuildFailure(t *testing.T) {
	api := &fakeUnfurler{}
	rec := &journalSpy{}
	matcher, _ := unfurl.NewMatcher(map[string]config.UnfurlRuleConfig{
		"jira": {Match: config.UnfurlMatchConfig{Domain: "example.com"}, Unfurl: config.UnfurlActionConfig{Run: &config.RunTriggerConfig{Cmd: "echo"}}},
	})
	runner := &stubRunner{output: []byte("not json")}
	handler := NewLinkUnfurlHandler(matcher, unfurl.NewRenderer(t.TempDir(), nil), runner, nil, api, discardLogger()).WithRecorder(rec)

	if err := handler.Handle(context.Background(), LinkSharedRequest{
		ChannelID: "C1",
		MessageTS: "1.1",
		Links:     []slackevents.SharedLinks{{Domain: "example.com", URL: "https://example.com/x"}},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	render := rec.byKind("unfurl.render")
	if len(render) != 1 || render[0].Level != journal.LevelError {
		t.Fatalf("expected one error unfurl.render, got %+v", render)
	}
	if len(rec.byKind("unfurl.post")) != 0 {
		t.Fatalf("did not expect an unfurl.post when the only build failed")
	}
}
