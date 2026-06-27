package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// deadlineCapturingWorkflow records the context handed to Execute so a test can
// assert what bounds, if any, the interactive callback path imposes on a run.
type deadlineCapturingWorkflow struct {
	done        chan struct{}
	hasDeadline bool
	deadline    time.Time
}

func (w *deadlineCapturingWorkflow) Execute(ctx context.Context, _ slack.InteractionCallback, _ []byte) error {
	w.deadline, w.hasDeadline = ctx.Deadline()
	close(w.done)
	return nil
}

// TestInteractiveCallbackImposesNoTotalDeadline pins the fix for the 5-minute
// guillotine. A delegate-to-agent step (e.g. a code review) is legitimately
// long-running and is bounded by the delegate Runner's idle watchdog — not by a
// fixed wall-clock cap on the interactive callback. Goose started, streamed
// tool calls, and was killed at exactly 5:00 because handleInteractive wrapped
// workflow.Execute in context.WithTimeout(ctx, 5*time.Minute).
//
// If anyone reintroduces a WithTimeout/WithDeadline around the workflow run,
// ctx.Deadline() reports ok==true here and the test fails.
func TestInteractiveCallbackImposesNoTotalDeadline(t *testing.T) {
	wf := &deadlineCapturingWorkflow{done: make(chan struct{})}
	app := &Gateway{
		workflow: wf,
		socket:   nil, // a.ack is a no-op when socket is nil
		logger:   discardLogger(),
		cfg:      config.AccessConfig{AllowedUsers: []string{"UALICE00"}},
	}

	app.handleInteractive(socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: slack.InteractionCallback{
			Type:           slack.InteractionTypeBlockActions,
			User:           slack.User{ID: "UALICE00"},
			Channel:        slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C1"}}},
			ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{{ActionID: "run_local_review"}}},
		},
	})

	select {
	case <-wf.done:
	case <-time.After(2 * time.Second):
		t.Fatal("workflow.Execute was never dispatched")
	}
	if wf.hasDeadline {
		t.Fatalf("interactive callback must not impose a total deadline on the workflow; got one %s away", time.Until(wf.deadline))
	}
}
