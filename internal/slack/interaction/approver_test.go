package interaction

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

// TestGateApprover_EphemeralOutcome verifies the full approval experience: with a
// triggering user on the turn, the prompt is posted ephemerally to that user, and
// the decision rewrites it (via the click's response_url) to a concise outcome
// line — approved with a check, denied struck through, both naming the decider.
func TestGateApprover_EphemeralOutcome(t *testing.T) {
	cases := []struct {
		name     string
		optionID string
		label    string
		want     string
	}{
		{"approve", "approve", "Approve", "✓ Tool `terminal` approved by <@U1>"},
		{"deny", "deny", "Deny", "~Tool `terminal` denied by <@U1>~"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker, sig := newSignalingBroker(t)
			ctx := agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1", UserID: "U1"})

			done := make(chan struct{})
			go func() {
				NewApprover(broker).Approve(ctx, "terminal", "rm -rf x")
				close(done)
			}()

			posted := <-sig.posted
			if len(sig.Ephemeral) != 1 || sig.Ephemeral[0].UserID != "U1" {
				t.Fatalf("approval prompt should be ephemeral to the triggering user, got %+v", sig.Ephemeral)
			}
			broker.Resolve(corrFromPosted(t, posted), Decision{OptionID: tc.optionID, Label: tc.label, UserID: "U1", ResponseURL: "https://hooks.slack/x"})
			<-done

			if len(sig.Webhooks) != 1 {
				t.Fatalf("expected the outcome written once via response_url, got %d", len(sig.Webhooks))
			}
			if got := sig.Webhooks[0].Params.Text; got != tc.want {
				t.Fatalf("outcome text = %q, want %q", got, tc.want)
			}
			// Blocks carry the same outcome line (modulo JSON's <,> escaping), so
			// the rewritten message is button-less and self-describing.
			if !strings.Contains(string(sig.Webhooks[0].Params.Blocks), "Tool `terminal`") {
				t.Fatalf("outcome blocks should carry the outcome line, got %s", sig.Webhooks[0].Params.Blocks)
			}
		})
	}
}

func TestGateApprover_NoLocationProceeds(t *testing.T) {
	broker, _ := newSignalingBroker(t)
	allowed, note := NewApprover(broker).Approve(context.Background(), "terminal", "rm -rf x")
	if !allowed || note != "" {
		t.Fatalf("headless (no Slack location) should proceed ungated, got allowed=%v note=%q", allowed, note)
	}
}

func TestGateApprover_Approved(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	ctx := agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"})

	type res struct {
		allowed bool
		note    string
	}
	out := make(chan res, 1)
	go func() {
		a, n := NewApprover(broker).Approve(ctx, "terminal", "rm -rf x")
		out <- res{a, n}
	}()

	posted := <-sig.posted
	broker.Resolve(corrFromPosted(t, posted), Decision{OptionID: "approve", Label: "Approve", UserID: "U1"})

	got := <-out
	if !got.allowed || got.note != "" {
		t.Fatalf("approve should allow with no note, got allowed=%v note=%q", got.allowed, got.note)
	}
}

func TestGateApprover_Denied(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	ctx := agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"})

	out := make(chan bool, 1)
	go func() {
		allowed, _ := NewApprover(broker).Approve(ctx, "terminal", "rm -rf x")
		out <- allowed
	}()

	posted := <-sig.posted
	broker.Resolve(corrFromPosted(t, posted), Decision{OptionID: "deny", Label: "Deny", UserID: "U1"})

	if <-out {
		t.Fatal("deny should not allow the call")
	}
}

// TestGateApprover_AlwaysAllow verifies that picking "Approve & always allow"
// approves the call AND remembers the exact summary, so an identical second call
// is approved silently — with no new prompt posted.
func TestGateApprover_AlwaysAllow(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	ctx := agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"})
	approver := NewApprover(broker)

	// First call: the user chooses "always allow".
	out := make(chan bool, 1)
	go func() {
		allowed, _ := approver.Approve(ctx, "terminal", "rm -rf x")
		out <- allowed
	}()

	posted := <-sig.posted
	broker.Resolve(corrFromPosted(t, posted), Decision{OptionID: "approve_always", Label: "Approve & always allow", UserID: "U1"})

	if !<-out {
		t.Fatal("approve_always should allow the call")
	}

	// Second call with the same summary returns immediately (synchronously) and
	// posts no new prompt: it never reaches the broker.
	allowed, note := approver.Approve(ctx, "terminal", "rm -rf x")
	if !allowed || note != "" {
		t.Fatalf("a remembered summary should be allowed silently, got allowed=%v note=%q", allowed, note)
	}
	if len(sig.posted) != 0 {
		t.Fatal("an always-allowed summary should not post a second prompt")
	}
}

// TestGateApprover_AlwaysAllowIsExact verifies the always-allow set is matched
// exactly: a different summary after an always-allow still prompts.
func TestGateApprover_AlwaysAllowIsExact(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	ctx := agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"})
	approver := NewApprover(broker)

	// Remember "rm -rf x".
	out := make(chan bool, 1)
	go func() {
		allowed, _ := approver.Approve(ctx, "terminal", "rm -rf x")
		out <- allowed
	}()
	posted := <-sig.posted
	broker.Resolve(corrFromPosted(t, posted), Decision{OptionID: "approve_always", Label: "Approve & always allow", UserID: "U1"})
	if !<-out {
		t.Fatal("approve_always should allow the call")
	}

	// A DIFFERENT summary is not covered: it must prompt again.
	out2 := make(chan bool, 1)
	go func() {
		allowed, _ := approver.Approve(ctx, "terminal", "rm -rf y")
		out2 <- allowed
	}()
	posted2 := <-sig.posted // would block / fail if no prompt were posted
	broker.Resolve(corrFromPosted(t, posted2), Decision{OptionID: "deny", Label: "Deny", UserID: "U1"})
	if <-out2 {
		t.Fatal("a different summary should still be gated and deniable")
	}
}
