package slackapp

import (
	"sync"
	"time"
)

// defaultEventDedupTTL bounds how long a message timestamp is remembered for
// duplicate suppression. It only needs to outlive Slack's redelivery window
// (retries plus the occasional post-reconnect replay), and message timestamps
// are unique per message, so a generous TTL is safe and never suppresses a
// genuine new message.
const defaultEventDedupTTL = 15 * time.Minute

// eventDedup suppresses duplicate Slack event deliveries. Slack's Events API
// guarantees at-least-once delivery: the same message event can arrive more
// than once — notably after a socket reconnect — and without de-duplication a
// redelivered app_mention runs startChat again, interrupts the first in-flight
// chat, and surfaces a spurious "_interrupted_" marker with no user-visible
// cause. Keys are message timestamps, which Slack assigns uniquely and
// immutably per message, so a genuine follow-up (new ts) is never suppressed.
type eventDedup struct {
	ttl  time.Duration
	now  func() time.Time
	mu   sync.Mutex
	seen map[string]time.Time
}

func newEventDedup(ttl time.Duration) *eventDedup {
	if ttl <= 0 {
		ttl = defaultEventDedupTTL
	}
	return &eventDedup{ttl: ttl, now: time.Now, seen: make(map[string]time.Time)}
}

// seenBefore records key and reports whether it was already seen within the
// TTL. The first call for a key returns false; subsequent calls within the TTL
// return true. A nil receiver or empty key always returns false so callers can
// stay oblivious to whether de-duplication is wired up. Expired entries are
// pruned opportunistically on each new key.
func (d *eventDedup) seenBefore(key string) bool {
	if d == nil || key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if last, ok := d.seen[key]; ok && now.Sub(last) < d.ttl {
		return true
	}
	d.prune(now)
	d.seen[key] = now
	return false
}

func (d *eventDedup) prune(now time.Time) {
	for k, t := range d.seen {
		if now.Sub(t) >= d.ttl {
			delete(d.seen, k)
		}
	}
}
