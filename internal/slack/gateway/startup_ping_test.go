package gateway

import (
	"context"
	"log/slog"
	"testing"

	"github.com/slack-go/slack"
)

type fakeSlackAPI struct {
	users       []slack.User
	openedUsers []string
	postChannel string
	postOptions int
}

func (f *fakeSlackAPI) GetUsersContext(context.Context, ...slack.GetUsersOption) ([]slack.User, error) {
	return f.users, nil
}

func (f *fakeSlackAPI) OpenConversationContext(_ context.Context, params *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error) {
	f.openedUsers = append(f.openedUsers, params.Users...)
	return &slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "DADMIN"}}}, false, false, nil
}

func (f *fakeSlackAPI) PostMessageContext(_ context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	f.postChannel = channelID
	f.postOptions = len(options)
	return channelID, "1717450123.000100", nil
}

func TestSlackStartupNotifierSendsPingToAdminHandle(t *testing.T) {
	api := &fakeSlackAPI{users: []slack.User{{ID: "UADMIN", Name: "admin"}}}
	notifier, err := NewSlackStartupNotifier(api, "@admin", slog.Default())
	if err != nil {
		t.Fatalf("NewSlackStartupNotifier returned error: %v", err)
	}
	if err := notifier.NotifyStartup(context.Background()); err != nil {
		t.Fatalf("NotifyStartup returned error: %v", err)
	}
	if len(api.openedUsers) != 1 || api.openedUsers[0] != "UADMIN" {
		t.Fatalf("expected DM to UADMIN, got %#v", api.openedUsers)
	}
	if api.postChannel != "DADMIN" || api.postOptions == 0 {
		t.Fatalf("expected startup ping in DADMIN with message options, got channel=%q options=%d", api.postChannel, api.postOptions)
	}
}
