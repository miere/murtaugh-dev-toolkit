package interaction

import (
	"context"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
)

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
