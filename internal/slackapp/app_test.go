package slackapp

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
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
	app := New(config.Config{Slack: config.SlackConfig{AppToken: "xapp-test", BotToken: "xoxb-test"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if app.startupNotifier != nil {
		t.Fatalf("expected no startup notifier without slack.admin_user, got %#v", app.startupNotifier)
	}
	app.notifyStartup(context.Background())
}

func TestAppMentionEventRoutesToACPChat(t *testing.T) {
	api := &fakeStreamAPI{}
	sessions := &fakeChatSessions{}
	app := &App{chat: NewChatHandler(api, sessions, time.Hour, 1, nil), chatTimeout: time.Second, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	app.handleEventsAPI(socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackevents.EventsAPIEvent{
		TeamID:     "T1",
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: string(slackevents.AppMention), Data: &slackevents.AppMentionEvent{User: "U1", Channel: "C1", Text: "<@UBOT> hello", TimeStamp: "123.4"}},
	}})

	deadline := time.After(time.Second)
	for sessions.prompt == "" {
		select {
		case <-deadline:
			t.Fatal("expected app mention to route to chat")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if sessions.prompt != "hello" {
		t.Fatalf("unexpected prompt: %q", sessions.prompt)
	}
}
