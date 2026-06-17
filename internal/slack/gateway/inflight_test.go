package gateway

import (
	"context"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
)

func TestInFlightRegistryRegisterUnregister(t *testing.T) {
	r := NewInFlightRegistry()
	key := agent.ConversationKey{ChannelID: "C1", ThreadTS: "1.0"}
	_, cancel := context.WithCancel(context.Background())
	token, prev := r.Register(key, cancel, "default")
	if prev != nil {
		t.Fatalf("expected no previous entry, got non-nil")
	}
	if r.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", r.Len())
	}
	r.Unregister(key, token)
	if r.Len() != 0 {
		t.Fatalf("expected 0 entries after Unregister, got %d", r.Len())
	}
}

func TestInFlightRegistryActive(t *testing.T) {
	r := NewInFlightRegistry()
	key := agent.ConversationKey{ChannelID: "C1", ThreadTS: "1.0"}
	if r.Active(key) {
		t.Fatal("expected no active chat before Register")
	}
	_, cancel := context.WithCancel(context.Background())
	token, _ := r.Register(key, cancel, "default")
	if !r.Active(key) {
		t.Fatal("expected active chat after Register")
	}
	if r.Active(agent.ConversationKey{ChannelID: "C2"}) {
		t.Fatal("a different conversation must not report active")
	}
	r.Unregister(key, token)
	if r.Active(key) {
		t.Fatal("expected no active chat after Unregister")
	}
	var nilReg *InFlightRegistry
	if nilReg.Active(key) {
		t.Fatal("nil registry must report not active")
	}
}

func TestInFlightRegistryRegisterReturnsPreviousCancel(t *testing.T) {
	r := NewInFlightRegistry()
	key := agent.ConversationKey{ChannelID: "C1", ThreadTS: "1.0"}
	called := false
	first := func() { called = true }
	_, prev := r.Register(key, first, "default")
	if prev != nil {
		t.Fatalf("first Register must return nil previous, got non-nil")
	}
	_, cancel := context.WithCancel(context.Background())
	_, prev = r.Register(key, cancel, "default")
	if prev == nil {
		t.Fatalf("second Register must return the previous cancel func, got nil")
	}
	prev()
	if !called {
		t.Fatalf("expected previous cancel func to be invoked")
	}
}

func TestInFlightRegistryCancelInvokesAndRemoves(t *testing.T) {
	r := NewInFlightRegistry()
	key := agent.ConversationKey{ChannelID: "C1", ThreadTS: "1.0"}
	called := false
	r.Register(key, func() { called = true }, "default")
	if !r.Cancel(key) {
		t.Fatalf("Cancel must return true when an entry exists")
	}
	if !called {
		t.Fatalf("Cancel must invoke the stored cancel func")
	}
	if r.Len() != 0 {
		t.Fatalf("Cancel must remove the entry; got Len=%d", r.Len())
	}
}

func TestInFlightRegistryCancelOnEmptyReturnsFalse(t *testing.T) {
	r := NewInFlightRegistry()
	key := agent.ConversationKey{ChannelID: "C1", ThreadTS: "1.0"}
	if r.Cancel(key) {
		t.Fatalf("Cancel on empty registry must return false")
	}
}

func TestInFlightRegistryUnregisterIgnoresStaleToken(t *testing.T) {
	r := NewInFlightRegistry()
	key := agent.ConversationKey{ChannelID: "C1", ThreadTS: "1.0"}
	staleToken, _ := r.Register(key, func() {}, "default")
	// Second Register replaces the first (without going through
	// Cancel, simulating a follow-up that arrives before the
	// original goroutine has noticed it was interrupted).
	_, prev := r.Register(key, func() {}, "default")
	if prev == nil {
		t.Fatalf("expected previous cancel on second Register")
	}
	// The first goroutine eventually finishes and calls Unregister
	// with its stale token. The entry must remain in the registry
	// so the live chat is still cancellable via /stop.
	r.Unregister(key, staleToken)
	if r.Len() != 1 {
		t.Fatalf("stale Unregister must not remove the live entry; got Len=%d", r.Len())
	}
}

func TestInFlightRegistryNilSafe(t *testing.T) {
	var r *InFlightRegistry
	if r.Cancel(agent.ConversationKey{}) {
		t.Fatalf("nil registry Cancel must return false")
	}
	r.Unregister(agent.ConversationKey{}, 0)
	if r.Len() != 0 {
		t.Fatalf("nil registry Len must return 0")
	}
	token, prev := r.Register(agent.ConversationKey{}, func() {}, "")
	if token != 0 || prev != nil {
		t.Fatalf("nil registry Register must be a no-op")
	}
}
