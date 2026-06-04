package slackapp

import (
	"context"

	"github.com/slack-go/slack"
)

type StreamAPI interface {
	StartStreamContext(context.Context, string, ...slack.MsgOption) (string, string, error)
	AppendStreamContext(context.Context, string, string, ...slack.MsgOption) (string, string, error)
	StopStreamContext(context.Context, string, string, ...slack.MsgOption) (string, string, error)
}
