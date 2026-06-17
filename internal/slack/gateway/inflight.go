package gateway

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
)

// InFlightRegistry tracks the chat goroutines currently streaming a
// response back to Slack, keyed by ConversationKey (one entry per
// thread / DM). It is the single point of truth used by both
// interrupt-on-new-message (Gateway.startChat cancels the previous entry
// before starting a new one) and the /stop slash command.
//
// The registry holds no Slack or ACP types directly; the cancel
// closure captured at Register time is responsible for whatever
// graceful-then-hard sequence the caller wants to run when the entry
// is cancelled. See app.go::startChat for the production wiring.
type InFlightRegistry struct {
	mu      sync.Mutex
	nextID  atomic.Uint64
	entries map[agent.ConversationKey]*inFlightChat
}

type inFlightChat struct {
	id        uint64
	cancel    context.CancelFunc
	agent     string
	startedAt time.Time
}

// Token is the opaque handle returned by Register. The caller passes
// it back to Unregister so a slow-finishing goroutine cannot clear
// the entry of the follow-up that interrupted it.
type Token uint64

func NewInFlightRegistry() *InFlightRegistry {
	return &InFlightRegistry{entries: make(map[agent.ConversationKey]*inFlightChat)}
}

// Register records a new in-flight chat for the conversation. If a
// previous entry exists it is removed from the registry and its
// cancel func is returned to the caller — the caller is expected to
// invoke it (the interrupt path). Cancellation happens outside the
// mutex so a slow ACP `session/cancel` does not block other callers.
func (r *InFlightRegistry) Register(key agent.ConversationKey, cancel context.CancelFunc, agent string) (Token, context.CancelFunc) {
	if r == nil {
		return 0, nil
	}
	id := Token(r.nextID.Add(1))
	r.mu.Lock()
	previous := r.entries[key]
	r.entries[key] = &inFlightChat{id: uint64(id), cancel: cancel, agent: agent, startedAt: time.Now()}
	r.mu.Unlock()
	if previous == nil {
		return id, nil
	}
	return id, previous.cancel
}

// Unregister drops the entry for the conversation iff the stored
// token matches the supplied one. The token guard prevents a
// slow-finishing goroutine from clearing the entry of a follow-up
// chat that has already replaced it.
func (r *InFlightRegistry) Unregister(key agent.ConversationKey, token Token) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[key]
	if !ok {
		return
	}
	if entry.id == uint64(token) {
		delete(r.entries, key)
	}
}

// Cancel cancels the entry for the conversation, if any. Returns
// true when an entry was found and cancelled, false when there was
// nothing to stop. The cancel closure is invoked outside the mutex.
func (r *InFlightRegistry) Cancel(key agent.ConversationKey) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	entry, ok := r.entries[key]
	if ok {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	entry.cancel()
	return true
}

// Active reports whether a chat is currently in flight for the conversation.
// It races with concurrent Register/Cancel by nature, so callers must treat
// the answer as a best-effort snapshot, not a lock.
func (r *InFlightRegistry) Active(key agent.ConversationKey) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.entries[key]
	return ok
}

// Len returns the current number of tracked in-flight chats. It is
// primarily useful in tests; production code should not branch on
// this value because it races with concurrent Register/Cancel calls.
func (r *InFlightRegistry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}
