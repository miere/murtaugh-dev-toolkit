package slackapp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/miere/murtaugh-dev-toolkit/internal/unfurl"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

type recordingStartupNotifier struct {
	calls chan struct{}
}

func (n recordingStartupNotifier) NotifyStartup(context.Context) error {
	n.calls <- struct{}{}
	return nil
}

func TestAppNotifiesStartupOnceWhenSocketConnects(t *testing.T) {
	notifier := recordingStartupNotifier{calls: make(chan struct{}, 2)}
	app := &App{startupNotifier: notifier, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	app.handleEvent(context.Background(), socketmode.Event{Type: socketmode.EventTypeConnected})
	app.handleEvent(context.Background(), socketmode.Event{Type: socketmode.EventTypeConnected})

	select {
	case <-notifier.calls:
	case <-time.After(time.Second):
		t.Fatal("expected startup notification")
	}
	select {
	case <-notifier.calls:
		t.Fatal("expected only one startup notification")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestNewWithoutAdminUserDoesNotInstallTypedNilStartupNotifier(t *testing.T) {
	app := New(config.Config{OAuth: config.OAuthConfig{AppToken: "xapp-test", BotToken: "xoxb-test"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if app.startupNotifier != nil {
		t.Fatalf("expected no startup notifier without configuration.admin_user, got %#v", app.startupNotifier)
	}
	app.notifyStartup(context.Background())
}

func TestAppMentionEventRoutesToACPChat(t *testing.T) {
	api := &fakeStreamAPI{}
	fakeSessions := &fakeChatSessions{}
	sessions := map[string]ChatSessionManager{"default": fakeSessions}
	resolver := func(req ChatRequest) string { return "default" }
	app := &App{
		chat:        NewChatHandler(api, sessions, resolver, time.Hour, 1, nil),
		chatTimeout: time.Second,
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:         config.ConfigurationConfig{AllowedUsers: []string{"U1"}},
	}
	app.handleEventsAPI(socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackevents.EventsAPIEvent{
		TeamID:     "T1",
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: string(slackevents.AppMention), Data: &slackevents.AppMentionEvent{User: "U1", Channel: "C1", Text: "<@UBOT> hello", TimeStamp: "123.4"}},
	}})

	deadline := time.After(time.Second)
	for fakeSessions.prompt == "" {
		select {
		case <-deadline:
			t.Fatal("expected app mention to route to chat")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if fakeSessions.prompt != "hello" {
		t.Fatalf("unexpected prompt: %q", fakeSessions.prompt)
	}
}

type fakeUnfurler struct {
	calls     int
	channelID string
	timestamp string
	unfurls   map[string]slack.Attachment
	err       error
}

func (f *fakeUnfurler) UnfurlMessageContext(_ context.Context, channelID, timestamp string, unfurls map[string]slack.Attachment, _ ...slack.MsgOption) (string, string, string, error) {
	f.calls++
	f.channelID = channelID
	f.timestamp = timestamp
	f.unfurls = unfurls
	return "", "", "", f.err
}

type stubRunner struct {
	output []byte
	err    error
	input  []byte
}

func (s *stubRunner) Run(_ context.Context, _ config.RunTriggerConfig, input []byte) ([]byte, error) {
	s.input = append([]byte(nil), input...)
	return s.output, s.err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTemplateUnfurlHandler(t *testing.T, api Unfurler) *LinkUnfurlHandler {
	t.Helper()
	matcher, err := unfurl.NewMatcher(map[string]config.UnfurlRuleConfig{
		"github-pr": {
			Match:  config.UnfurlMatchConfig{Domain: "github.com", URLPattern: `^https://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/(?P<number>\d+)`},
			Unfurl: config.UnfurlActionConfig{Template: "unfurl/github-pr.json"},
		},
	})
	if err != nil {
		t.Fatalf("NewMatcher returned error: %v", err)
	}
	return NewLinkUnfurlHandler(matcher, unfurl.NewRenderer(t.TempDir(), nil), nil, api, discardLogger())
}

func TestLinkUnfurlHandlerTemplate(t *testing.T) {
	api := &fakeUnfurler{}
	handler := newTemplateUnfurlHandler(t, api)
	err := handler.Handle(context.Background(), LinkSharedRequest{
		ChannelID: "C1",
		MessageTS: "1700000000.000100",
		Links:     []slackevents.SharedLinks{{Domain: "github.com", URL: "https://github.com/acme/widgets/pull/42"}},
	})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if api.calls != 1 {
		t.Fatalf("expected 1 unfurl call, got %d", api.calls)
	}
	att, ok := api.unfurls["https://github.com/acme/widgets/pull/42"]
	if !ok {
		t.Fatalf("expected unfurl keyed by URL, got %#v", api.unfurls)
	}
	out, _ := json.Marshal(att)
	if !strings.Contains(string(out), "Pull Request #42") {
		t.Fatalf("unexpected attachment: %s", out)
	}
}

func TestLinkUnfurlHandlerSkipsComposerTimestamp(t *testing.T) {
	api := &fakeUnfurler{}
	handler := newTemplateUnfurlHandler(t, api)
	if err := handler.Handle(context.Background(), LinkSharedRequest{
		ChannelID: "C1",
		MessageTS: "Af012-not-a-ts",
		Links:     []slackevents.SharedLinks{{Domain: "github.com", URL: "https://github.com/acme/widgets/pull/42"}},
	}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if api.calls != 0 {
		t.Fatalf("expected no unfurl for composer timestamp, got %d", api.calls)
	}
}

func TestLinkUnfurlHandlerSkipsNonMatchingAndDeduplicates(t *testing.T) {
	api := &fakeUnfurler{}
	handler := newTemplateUnfurlHandler(t, api)
	pr := slackevents.SharedLinks{Domain: "github.com", URL: "https://github.com/acme/widgets/pull/9"}
	issue := slackevents.SharedLinks{Domain: "github.com", URL: "https://github.com/acme/widgets/issues/9"}
	if err := handler.Handle(context.Background(), LinkSharedRequest{
		ChannelID: "C1",
		MessageTS: "1.1",
		Links:     []slackevents.SharedLinks{pr, pr, issue},
	}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if api.calls != 1 {
		t.Fatalf("expected 1 unfurl call, got %d", api.calls)
	}
	if len(api.unfurls) != 1 {
		t.Fatalf("expected 1 unfurl entry after dedupe/non-match, got %d", len(api.unfurls))
	}
}

func TestLinkUnfurlHandlerRunPath(t *testing.T) {
	api := &fakeUnfurler{}
	matcher, err := unfurl.NewMatcher(map[string]config.UnfurlRuleConfig{
		"jira": {
			Match:  config.UnfurlMatchConfig{Domain: "example.com", URLPattern: `/browse/(?P<key>[A-Z]+-\d+)`},
			Unfurl: config.UnfurlActionConfig{Run: &config.RunTriggerConfig{Cmd: "echo"}},
		},
	})
	if err != nil {
		t.Fatalf("NewMatcher returned error: %v", err)
	}
	runner := &stubRunner{output: []byte(`{"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"JIRA"}}]}`)}
	handler := NewLinkUnfurlHandler(matcher, unfurl.NewRenderer(t.TempDir(), nil), runner, api, discardLogger())
	if err := handler.Handle(context.Background(), LinkSharedRequest{
		ChannelID: "C1",
		MessageTS: "1.1",
		Links:     []slackevents.SharedLinks{{Domain: "example.com", URL: "https://example.com/browse/ABC-7"}},
	}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if api.calls != 1 {
		t.Fatalf("expected 1 unfurl call, got %d", api.calls)
	}
	if !strings.Contains(string(runner.input), `"ABC-7"`) {
		t.Fatalf("expected captures on command stdin, got %s", runner.input)
	}
	out, _ := json.Marshal(api.unfurls["https://example.com/browse/ABC-7"])
	if !strings.Contains(string(out), "JIRA") {
		t.Fatalf("unexpected run attachment: %s", out)
	}
}

func TestLinkUnfurlHandlerSkipsInvalidRunOutput(t *testing.T) {
	api := &fakeUnfurler{}
	matcher, _ := unfurl.NewMatcher(map[string]config.UnfurlRuleConfig{
		"jira": {Match: config.UnfurlMatchConfig{Domain: "example.com"}, Unfurl: config.UnfurlActionConfig{Run: &config.RunTriggerConfig{Cmd: "echo"}}},
	})
	runner := &stubRunner{output: []byte("not json")}
	handler := NewLinkUnfurlHandler(matcher, unfurl.NewRenderer(t.TempDir(), nil), runner, api, discardLogger())
	if err := handler.Handle(context.Background(), LinkSharedRequest{
		ChannelID: "C1",
		MessageTS: "1.1",
		Links:     []slackevents.SharedLinks{{Domain: "example.com", URL: "https://example.com/x"}},
	}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if api.calls != 0 {
		t.Fatalf("expected no unfurl when run output is invalid, got %d", api.calls)
	}
}
