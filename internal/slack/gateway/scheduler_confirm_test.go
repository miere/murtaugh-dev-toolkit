package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	slacklib "github.com/miere/murtaugh-dev-toolkit/internal/slack/client"
	"github.com/miere/murtaugh-dev-toolkit/internal/slack/client/slacktest"
	"github.com/miere/murtaugh-dev-toolkit/internal/slack/interaction"
)

// signalingSlackAPI wraps the shared fake to announce each post so the test can
// learn the broker's correlation id and resolve the confirmation prompt.
type signalingSlackAPI struct {
	*slacktest.FakeAPI
	posted chan slacklib.PostMessageParams
}

func (s *signalingSlackAPI) PostMessage(ctx context.Context, p slacklib.PostMessageParams) (slacklib.PostMessageResult, error) {
	res, err := s.FakeAPI.PostMessage(ctx, p)
	s.posted <- p
	return res, err
}

// dmMessaging is a minimal slackMessagingAPI that opens a fixed admin DM.
type dmMessaging struct{ dm string }

func (d *dmMessaging) PostMessageContext(context.Context, string, ...slackgo.MsgOption) (string, string, error) {
	return "", "", nil
}
func (d *dmMessaging) UpdateMessageContext(context.Context, string, string, ...slackgo.MsgOption) (string, string, string, error) {
	return "", "", "", nil
}
func (d *dmMessaging) OpenConversationContext(context.Context, *slackgo.OpenConversationParameters) (*slackgo.Channel, bool, bool, error) {
	ch := &slackgo.Channel{}
	ch.ID = d.dm
	return ch, false, false, nil
}

func newConfirmGateway(t *testing.T) (*Gateway, *signalingSlackAPI, *interaction.Broker) {
	t.Helper()
	sig := &signalingSlackAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "DADMIN", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	broker := interaction.NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }))
	a := &Gateway{
		logger:       discardLogger(),
		interactions: broker,
		messaging:    &dmMessaging{dm: "DADMIN"},
		cfg:          config.AccessConfig{AdminUser: "UADMIN"},
	}
	return a, sig, broker
}

func heldJob() config.JobProfile {
	unconfirmed := false
	return config.JobProfile{Command: "/bin/echo", Args: []string{"hi"}, Every: "1h", Confirmed: &unconfirmed}
}

func TestConfirmHeldJob_ApprovedRunsAndRemembers(t *testing.T) {
	a, sig, broker := newConfirmGateway(t)

	out := make(chan bool, 1)
	go func() { out <- a.confirmHeldJob(context.Background(), "myjob", heldJob()) }()

	posted := <-sig.posted
	if posted.ChannelID != "DADMIN" {
		t.Fatalf("confirmation posted to %q, want the admin DM DADMIN", posted.ChannelID)
	}
	broker.Resolve(corrFromBlocks(t, posted.Blocks), interaction.Decision{OptionID: "approve", UserID: "UADMIN"})

	if !<-out {
		t.Fatal("approval should return true (run the job)")
	}
	if !a.isJobConfirmed("myjob") {
		t.Fatal("approved job should be remembered as confirmed for this session")
	}
}

func TestConfirmHeldJob_DeniedDoesNotRun(t *testing.T) {
	a, sig, broker := newConfirmGateway(t)

	out := make(chan bool, 1)
	go func() { out <- a.confirmHeldJob(context.Background(), "myjob", heldJob()) }()

	posted := <-sig.posted
	broker.Resolve(corrFromBlocks(t, posted.Blocks), interaction.Decision{OptionID: "deny", UserID: "UADMIN"})

	if <-out {
		t.Fatal("denial should return false (do not run)")
	}
	if a.isJobConfirmed("myjob") {
		t.Fatal("denied job must not be marked confirmed")
	}
}

func TestConfirmHeldJob_NoBrokerDoesNotRun(t *testing.T) {
	a := &Gateway{logger: discardLogger(), cfg: config.AccessConfig{AdminUser: "UADMIN"}}
	if a.confirmHeldJob(context.Background(), "myjob", heldJob()) {
		t.Fatal("with no broker wired, a held job must not be confirmed")
	}
}

// corrFromBlocks parses the correlation id out of the posted prompt's first
// button (action_id = "murtaugh_interaction:<corr>:<idx>").
func corrFromBlocks(t *testing.T, raw []byte) string {
	t.Helper()
	var blocks slackgo.Blocks
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("posted blocks are not valid Block Kit JSON: %v", err)
	}
	for _, b := range blocks.BlockSet {
		if action, ok := b.(*slackgo.ActionBlock); ok && action.Elements != nil {
			for _, el := range action.Elements.ElementSet {
				if btn, ok := el.(*slackgo.ButtonBlockElement); ok {
					if parts := strings.Split(btn.ActionID, ":"); len(parts) >= 3 {
						return parts[1]
					}
				}
			}
		}
	}
	t.Fatal("no broker button found in posted blocks")
	return ""
}
