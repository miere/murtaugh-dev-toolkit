package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/agentdelegate"
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
	app := &Gateway{startupNotifier: notifier, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

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
	app := New(config.Config{OAuth: config.OAuthConfig{AppToken: "xapp-test", BotToken: "xoxb-test"}}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
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
	app := &Gateway{
		chat:   NewChatHandler(api, sessions, resolver, time.Hour, 1, nil),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:    config.ConfigurationConfig{AllowedUsers: []string{"U1"}},
	}
	app.handleEventsAPI(socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackevents.EventsAPIEvent{
		TeamID:     "T1",
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: string(slackevents.AppMention), Data: &slackevents.AppMentionEvent{User: "U1", Channel: "C1", Text: "<@UBOT> hello", TimeStamp: "123.4"}},
	}})

	deadline := time.After(time.Second)
	for fakeSessions.promptText() == "" {
		select {
		case <-deadline:
			t.Fatal("expected app mention to route to chat")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if got := fakeSessions.promptText(); got != "hello" {
		t.Fatalf("unexpected prompt: %q", got)
	}
}

// countingChatSessions counts how many times Prompt is invoked so a test can
// assert that a duplicate Slack event does not start a second chat.
type countingChatSessions struct {
	mu      sync.Mutex
	prompts int
}

func (f *countingChatSessions) Prompt(_ context.Context, _ agent.ConversationKey, _ agent.SessionMetadata, _ agent.PromptRequest) (<-chan agent.Event, error) {
	f.mu.Lock()
	f.prompts++
	f.mu.Unlock()
	ch := make(chan agent.Event, 2)
	ch <- agent.Event{Type: agent.EventText, Text: "ok"}
	ch <- agent.Event{Type: agent.EventComplete}
	close(ch)
	return ch, nil
}

func (f *countingChatSessions) Lookup(agent.ConversationKey) (string, bool) { return "", false }
func (f *countingChatSessions) Cancel(context.Context, string) error      { return nil }

func (f *countingChatSessions) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompts
}

func TestDuplicateAppMentionStartsChatOnce(t *testing.T) {
	api := &fakeStreamAPI{}
	fakeSessions := &countingChatSessions{}
	sessions := map[string]ChatSessionManager{"default": fakeSessions}
	resolver := func(req ChatRequest) string { return "default" }
	app := &Gateway{
		chat:         NewChatHandler(api, sessions, resolver, time.Hour, 1, nil),
		inFlight:     NewInFlightRegistry(),
		recentEvents: newEventDedup(time.Minute),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:          config.ConfigurationConfig{AllowedUsers: []string{"U1"}},
	}
	event := socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackevents.EventsAPIEvent{
		TeamID:     "T1",
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: string(slackevents.AppMention), Data: &slackevents.AppMentionEvent{User: "U1", Channel: "C1", Text: "<@UBOT> hello", TimeStamp: "123.4"}},
	}}
	// Same message delivered twice (Slack at-least-once redelivery).
	app.handleEventsAPI(event)
	app.handleEventsAPI(event)

	// Wait for the first chat to run, then confirm the duplicate never started
	// a second one.
	deadline := time.After(time.Second)
	for fakeSessions.count() == 0 {
		select {
		case <-deadline:
			t.Fatal("expected the first app_mention to start a chat")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	time.Sleep(20 * time.Millisecond)
	if got := fakeSessions.count(); got != 1 {
		t.Fatalf("expected duplicate app_mention to be suppressed (1 prompt), got %d", got)
	}
}

// nonInterruptibleSessions is a blocking session manager that reports it does
// not support cancellation, so a follow-up should be dropped while the first
// prompt is still running.
type nonInterruptibleSessions struct {
	mu      sync.Mutex
	prompts int
	release chan struct{}
}

func (f *nonInterruptibleSessions) Prompt(_ context.Context, _ agent.ConversationKey, _ agent.SessionMetadata, _ agent.PromptRequest) (<-chan agent.Event, error) {
	f.mu.Lock()
	f.prompts++
	f.mu.Unlock()
	ch := make(chan agent.Event)
	go func() {
		<-f.release
		ch <- agent.Event{Type: agent.EventText, Text: "done"}
		ch <- agent.Event{Type: agent.EventComplete}
		close(ch)
	}()
	return ch, nil
}

func (f *nonInterruptibleSessions) Lookup(agent.ConversationKey) (string, bool) { return "", false }
func (f *nonInterruptibleSessions) Cancel(context.Context, string) error      { return nil }
func (f *nonInterruptibleSessions) Interruptible() bool                       { return false }
func (f *nonInterruptibleSessions) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompts
}

func TestNonInterruptibleAgentDropsFollowUpWhileInFlight(t *testing.T) {
	api := &fakeStreamAPI{}
	msg := &recordingMessaging{}
	fake := &nonInterruptibleSessions{release: make(chan struct{})}
	sessions := map[string]ChatSessionManager{"default": fake}
	resolver := func(req ChatRequest) string { return "default" }
	app := &Gateway{
		chat:         NewChatHandler(api, sessions, resolver, time.Hour, 1, discardLogger()),
		chatSessions: sessions,
		inFlight:     NewInFlightRegistry(),
		recentEvents: newEventDedup(time.Minute),
		messaging:    msg,
		logger:       discardLogger(),
		cfg:          config.ConfigurationConfig{AllowedUsers: []string{"U1"}},
	}
	defer close(fake.release)

	first := ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", ThreadTS: "100.0", MessageTS: "1.1", Text: "first", Source: "test"}
	app.startChat(context.Background(), first)

	// Wait until the first prompt is in flight.
	key := conversationKey(first)
	deadline := time.After(time.Second)
	for !(fake.count() == 1 && app.inFlight.Active(key)) {
		select {
		case <-deadline:
			t.Fatalf("first chat never became active (prompts=%d)", fake.count())
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// A follow-up in the same thread must be dropped, not started.
	app.startChat(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", ThreadTS: "100.0", MessageTS: "2.2", Text: "second", Source: "test"})
	time.Sleep(20 * time.Millisecond)
	if got := fake.count(); got != 1 {
		t.Fatalf("expected follow-up to be dropped (1 prompt), got %d", got)
	}
	// The dropped follow-up gets a thread note so the user is not left guessing.
	if msg.postCalls != 1 {
		t.Fatalf("expected one thread note for the deferred follow-up, got %d", msg.postCalls)
	}
	if msg.postChannel != "C1" {
		t.Fatalf("expected the note in channel C1, got %q", msg.postChannel)
	}
}

func TestAgentInterruptibleGate(t *testing.T) {
	app := &Gateway{chatSessions: map[string]ChatSessionManager{
		"interruptible": &countingChatSessions{},     // no Interruptible() surface -> true
		"not":           &nonInterruptibleSessions{}, // Interruptible() == false
	}}
	if !app.agentInterruptible("interruptible") {
		t.Fatal("an agent without the capability surface must be treated as interruptible")
	}
	if app.agentInterruptible("not") {
		t.Fatal("an agent reporting Interruptible()==false must be non-interruptible")
	}
	if !app.agentInterruptible("missing") {
		t.Fatal("an unknown agent must default to interruptible")
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
			Unfurl: config.UnfurlActionConfig{Template: "templates/unfurl/github-pr.json"},
		},
	})
	if err != nil {
		t.Fatalf("NewMatcher returned error: %v", err)
	}
	return NewLinkUnfurlHandler(matcher, unfurl.NewRenderer(t.TempDir(), nil), nil, nil, api, discardLogger())
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
	handler := NewLinkUnfurlHandler(matcher, unfurl.NewRenderer(t.TempDir(), nil), runner, nil, api, discardLogger())
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
	handler := NewLinkUnfurlHandler(matcher, unfurl.NewRenderer(t.TempDir(), nil), runner, nil, api, discardLogger())
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

type stubUnfurlDelegator struct {
	out    []byte
	err    error
	agent  string
	prompt string
}

func (s *stubUnfurlDelegator) RunForJSON(_ context.Context, agent, prompt string) ([]byte, error) {
	s.agent = agent
	s.prompt = prompt
	return s.out, s.err
}

func TestLinkUnfurlHandlerDelegatePath(t *testing.T) {
	api := &fakeUnfurler{}
	matcher, err := unfurl.NewMatcher(map[string]config.UnfurlRuleConfig{
		"issue": {
			Match:  config.UnfurlMatchConfig{Domain: "github.com", URLPattern: `/issues/(?P<number>\d+)`},
			Unfurl: config.UnfurlActionConfig{DelegateToAgent: &config.DelegateToAgentConfig{Agent: "default", Prompt: "Summarise {{ .URL }} number {{ .Captures.number }}"}},
		},
	})
	if err != nil {
		t.Fatalf("NewMatcher returned error: %v", err)
	}
	del := &stubUnfurlDelegator{out: []byte(`{"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"SUMMARY"}}]}`)}
	handler := NewLinkUnfurlHandler(matcher, unfurl.NewRenderer(t.TempDir(), nil), nil, del, api, discardLogger())
	if err := handler.Handle(context.Background(), LinkSharedRequest{
		ChannelID: "C1",
		MessageTS: "1.1",
		Links:     []slackevents.SharedLinks{{Domain: "github.com", URL: "https://github.com/o/r/issues/12"}},
	}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if api.calls != 1 {
		t.Fatalf("expected 1 unfurl call, got %d", api.calls)
	}
	if del.agent != "default" {
		t.Fatalf("agent = %q, want default", del.agent)
	}
	if !strings.Contains(del.prompt, "issues/12") || !strings.Contains(del.prompt, "number 12") {
		t.Fatalf("prompt was not rendered with URL/captures: %q", del.prompt)
	}
	out, _ := json.Marshal(api.unfurls["https://github.com/o/r/issues/12"])
	if !strings.Contains(string(out), "SUMMARY") {
		t.Fatalf("unexpected delegate attachment: %s", out)
	}
}

func TestLinkUnfurlHandlerSkipsNonJSONDelegate(t *testing.T) {
	api := &fakeUnfurler{}
	matcher, _ := unfurl.NewMatcher(map[string]config.UnfurlRuleConfig{
		"issue": {Match: config.UnfurlMatchConfig{Domain: "github.com"}, Unfurl: config.UnfurlActionConfig{DelegateToAgent: &config.DelegateToAgentConfig{Agent: "default", Prompt: "x"}}},
	})
	del := &stubUnfurlDelegator{err: agentdelegate.ErrNonJSONOutput}
	handler := NewLinkUnfurlHandler(matcher, unfurl.NewRenderer(t.TempDir(), nil), nil, del, api, discardLogger())
	if err := handler.Handle(context.Background(), LinkSharedRequest{
		ChannelID: "C1",
		MessageTS: "1.1",
		Links:     []slackevents.SharedLinks{{Domain: "github.com", URL: "https://github.com/x"}},
	}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if api.calls != 0 {
		t.Fatalf("expected no unfurl when delegate output is non-JSON, got %d", api.calls)
	}
}
