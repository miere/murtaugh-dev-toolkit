package gateway

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/config"
	slackclient "github.com/miere/murtaugh/internal/slack/client"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// recordingCountingSessions is a chat session fake that counts how many times
// Prompt is invoked and records the last prompt text, so a no-mention test can
// assert both whether a channel message started a chat and (when it did) that
// the prompt had its mentions stripped.
type recordingCountingSessions struct {
	mu      sync.Mutex
	prompts int
	last    string
}

func (f *recordingCountingSessions) Prompt(_ context.Context, _ agent.ConversationKey, _ agent.SessionMetadata, req agent.PromptRequest) (<-chan agent.Event, error) {
	f.mu.Lock()
	f.prompts++
	f.last = req.Text
	f.mu.Unlock()
	ch := make(chan agent.Event, 2)
	ch <- agent.Event{Type: agent.EventText, Text: "ok"}
	ch <- agent.Event{Type: agent.EventComplete}
	close(ch)
	return ch, nil
}

func (f *recordingCountingSessions) Lookup(agent.ConversationKey) (string, bool) { return "", false }
func (f *recordingCountingSessions) Cancel(context.Context, string) error        { return nil }

func (f *recordingCountingSessions) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompts
}

func (f *recordingCountingSessions) lastPromptText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// primedCache returns a channel-name cache already warmed with the given
// ID→name pairs, so a test resolver can match by NAME glob without any Slack I/O.
func primedCache(t *testing.T, byID map[string]string) *channelNameCache {
	t.Helper()
	channels := make([]slackclient.Channel, 0, len(byID))
	for id, name := range byID {
		channels = append(channels, slackclient.Channel{ID: id, Name: name})
	}
	cache := newChannelNameCache(&fakeChannelDirectory{channels: channels}, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := cache.refresh(context.Background()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	return cache
}

// newNoMentionGateway wires a Gateway with a recording chat session so a test
// can assert whether a channel message started a chat. global is the resolved
// configuration.do_not_require_mention_from set; perChannel is the resolved
// chat.channel_do_not_require_mention map; allowed is allowed_users.
func newNoMentionGateway(global []string, perChannel map[string][]string, allowed []string, cache *channelNameCache) (*Gateway, *recordingCountingSessions) {
	fakeSessions := &recordingCountingSessions{}
	sessions := map[string]ChatSessionManager{"default": fakeSessions}
	resolver := func(req ChatRequest) string { return "default" }
	app := &Gateway{
		chat:                NewChatHandler(&fakeStreamAPI{}, sessions, resolver, time.Hour, 1, nil),
		chatSessions:        sessions,
		inFlight:            NewInFlightRegistry(),
		recentEvents:        newEventDedup(time.Minute),
		channelCache:        cache,
		noMentionPerChannel: perChannel,
		noMentionEverywhere: global,
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: config.AccessConfig{
			AllowedUsers: allowed,
		},
	}
	return app, fakeSessions
}

func channelMessageEvent(team, channel, channelType, user, text, ts string) socketmode.Event {
	return socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackevents.EventsAPIEvent{
		TeamID: team,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.Message),
			Data: &slackevents.MessageEvent{User: user, Channel: channel, ChannelType: channelType, Text: text, TimeStamp: ts},
		},
	}}
}

// waitForPrompts blocks until the recording session has seen want prompts or the
// deadline elapses, then returns the observed count. It gives an async startChat
// goroutine time to run before the assertion.
func waitForPrompts(sessions *recordingCountingSessions, want int) int {
	deadline := time.After(time.Second)
	for sessions.count() < want {
		select {
		case <-deadline:
			return sessions.count()
		default:
			time.Sleep(time.Millisecond)
		}
	}
	// Give a would-be extra prompt a moment to (wrongly) fire before asserting.
	time.Sleep(20 * time.Millisecond)
	return sessions.count()
}

func TestChannelMessageFromGloballyListedUserStartsChatWithoutMention(t *testing.T) {
	cache := primedCache(t, map[string]string{"C1": "general"})
	app, sessions := newNoMentionGateway([]string{"U1"}, nil, []string{"U1"}, cache)

	app.handleEventsAPI(channelMessageEvent("T1", "C1", "channel", "U1", "hello there", "100.1"))

	if got := waitForPrompts(sessions, 1); got != 1 {
		t.Fatalf("expected listed user's channel message to start exactly one chat, got %d", got)
	}
	if got := sessions.lastPromptText(); got != "hello there" {
		t.Fatalf("unexpected prompt text: %q", got)
	}
}

func TestChannelMessageStripsMentionsFromPrompt(t *testing.T) {
	cache := primedCache(t, map[string]string{"C1": "general"})
	app, sessions := newNoMentionGateway([]string{"U1"}, nil, []string{"U1"}, cache)

	app.handleEventsAPI(channelMessageEvent("T1", "C1", "channel", "U1", "<@UBOT> hello there", "101.1"))

	if got := waitForPrompts(sessions, 1); got != 1 {
		t.Fatalf("expected one chat, got %d", got)
	}
	if got := sessions.lastPromptText(); got != "hello there" {
		t.Fatalf("expected mentions stripped from prompt, got %q", got)
	}
}

func TestChannelMessageFromNonListedUserIsIgnored(t *testing.T) {
	cache := primedCache(t, map[string]string{"C1": "general"})
	// U2 is allowed (authorized) but NOT on any no-mention list.
	app, sessions := newNoMentionGateway([]string{"U1"}, nil, []string{"U1", "U2"}, cache)

	app.handleEventsAPI(channelMessageEvent("T1", "C1", "channel", "U2", "hi", "102.1"))

	if got := waitForPrompts(sessions, 1); got != 0 {
		t.Fatalf("expected non-listed user's channel message to be ignored, got %d prompts", got)
	}
}

func TestChannelMessagePerChannelGlobUnionsCorrectly(t *testing.T) {
	cache := primedCache(t, map[string]string{"C9": "feature-login"})
	// No global list; U7 is waived only via the feature-* per-channel pattern,
	// U8 only via *-prod.
	perChannel := map[string][]string{
		"feature-*": {"U7"},
		"*-prod":    {"U8"},
	}
	app, sessions := newNoMentionGateway(nil, perChannel, []string{"U7", "U8"}, cache)

	// U7 in feature-login (matches feature-*): replies without a mention.
	app.handleEventsAPI(channelMessageEvent("T1", "C9", "channel", "U7", "deploy?", "103.1"))
	if got := waitForPrompts(sessions, 1); got != 1 {
		t.Fatalf("expected feature-* listed user to start a chat, got %d", got)
	}

	// U8 is only waived in *-prod channels, not in feature-login: ignored.
	app.handleEventsAPI(channelMessageEvent("T1", "C9", "channel", "U8", "and me?", "103.2"))
	if got := waitForPrompts(sessions, 2); got != 1 {
		t.Fatalf("expected non-matching-pattern user to be ignored (still 1 prompt), got %d", got)
	}
}

func TestChannelMessageFromUnauthorizedListedUserIsIgnored(t *testing.T) {
	cache := primedCache(t, map[string]string{"C1": "general"})
	// U3 is on the no-mention list but NOT in allowed_users: authz still wins.
	app, sessions := newNoMentionGateway([]string{"U3"}, nil, []string{"U1"}, cache)

	app.handleEventsAPI(channelMessageEvent("T1", "C1", "channel", "U3", "let me in", "104.1"))

	if got := waitForPrompts(sessions, 1); got != 0 {
		t.Fatalf("expected unauthorized (not allowed) listed user to be ignored, got %d prompts", got)
	}
}

func TestChannelMessageDedupWithMentionDoesNotDoubleFire(t *testing.T) {
	cache := primedCache(t, map[string]string{"C1": "general"})
	app, sessions := newNoMentionGateway([]string{"U1"}, nil, []string{"U1"}, cache)

	// Slack delivers BOTH an app_mention and a plain message for the same
	// @mention, sharing one ts. isDuplicateEvent keys on (team, channel, ts), so
	// only the first reaches startChat.
	const ts = "105.1"
	appMention := socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackevents.EventsAPIEvent{
		TeamID: "T1",
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.AppMention),
			Data: &slackevents.AppMentionEvent{User: "U1", Channel: "C1", Text: "<@UBOT> hello", TimeStamp: ts},
		},
	}}
	app.handleEventsAPI(appMention)
	app.handleEventsAPI(channelMessageEvent("T1", "C1", "channel", "U1", "<@UBOT> hello", ts))

	if got := waitForPrompts(sessions, 1); got != 1 {
		t.Fatalf("expected the shared ts to de-dupe app_mention + plain message to a single chat, got %d", got)
	}
}
