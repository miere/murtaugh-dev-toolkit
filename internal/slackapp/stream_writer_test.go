package slackapp

import (
	"context"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

type fakeStreamAPI struct {
	startedChannel string
	appends        int
	stops          int
}

func (f *fakeStreamAPI) StartStreamContext(_ context.Context, channelID string, _ ...slack.MsgOption) (string, string, error) {
	f.startedChannel = channelID
	return channelID, "stream-ts", nil
}

func (f *fakeStreamAPI) AppendStreamContext(_ context.Context, _ string, _ string, _ ...slack.MsgOption) (string, string, error) {
	f.appends++
	return "C1", "stream-ts", nil
}

func (f *fakeStreamAPI) StopStreamContext(_ context.Context, _ string, _ string, _ ...slack.MsgOption) (string, string, error) {
	f.stops++
	return "C1", "stream-ts", nil
}

func TestStreamWriterUsesNativeStreamingMethods(t *testing.T) {
	api := &fakeStreamAPI{}
	writer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 5})
	if err := writer.Append(context.Background(), "hello"); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if err := writer.Stop(context.Background()); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if api.startedChannel != "C1" || api.appends != 1 || api.stops != 1 {
		t.Fatalf("unexpected stream calls: %#v", api)
	}
}
