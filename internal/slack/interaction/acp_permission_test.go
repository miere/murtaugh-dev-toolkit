package interaction

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

// TestPermissionGate_EphemeralOutcome verifies the ACP permission gate mirrors
// the native approval experience: the prompt is posted ephemerally to the
// triggering user, and the choice rewrites it (via the click's response_url) to a
// concise line keyed on the option kind — allow shows a check, reject is struck
// through, both naming the decider.
func TestPermissionGate_EphemeralOutcome(t *testing.T) {
	cases := []struct {
		name     string
		optionID string
		want     string
	}{
		{"allow", "allow_once", "✓ Tool `terminal` approved by <@U1>"},
		{"reject", "reject_once", "~Tool `terminal` denied by <@U1>~"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker, sig := newSignalingBroker(t)
			gate := NewPermissionGate(broker)
			loc := agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1", UserID: "U1"}
			req := agent.PermissionRequest{
				ToolKind:  "execute", // surfaced to the human as "terminal"
				ToolTitle: "ls -la",
				Options: []agent.PermissionOption{
					{ID: "allow_once", Name: "Allow", Kind: "allow_once"},
					{ID: "reject_once", Name: "Deny", Kind: "reject_once"},
				},
			}

			done := make(chan struct{})
			go func() {
				gate.AskPermission(context.Background(), loc, req)
				close(done)
			}()

			posted := <-sig.posted
			if len(sig.Ephemeral) != 1 || sig.Ephemeral[0].UserID != "U1" {
				t.Fatalf("permission prompt should be ephemeral to the triggering user, got %+v", sig.Ephemeral)
			}
			// The prompt names the tool concisely (execute → terminal) and renders
			// the command in a bash-hinted fenced code block rather than inline.
			if !strings.Contains(string(posted.Blocks), "`terminal`") {
				t.Fatalf("prompt should name the tool concisely, got %s", posted.Blocks)
			}
			if !strings.Contains(string(posted.Blocks), "```bash") || !strings.Contains(string(posted.Blocks), "ls -la") {
				t.Fatalf("prompt should render the command in a bash code block, got %s", posted.Blocks)
			}
			broker.Resolve(corrFromPosted(t, posted), Decision{OptionID: tc.optionID, Label: "x", UserID: "U1", ResponseURL: "https://hooks.slack/x"})
			<-done

			if len(sig.Webhooks) != 1 {
				t.Fatalf("expected the outcome written once via response_url, got %d", len(sig.Webhooks))
			}
			if got := sig.Webhooks[0].Params.Text; got != tc.want {
				t.Fatalf("outcome text = %q, want %q", got, tc.want)
			}
		})
	}
}
