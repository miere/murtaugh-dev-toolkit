package slackapp

import (
	"testing"
	"time"
)

func TestEventDedupSuppressesRepeatWithinTTL(t *testing.T) {
	d := newEventDedup(time.Minute)
	if d.seenBefore("k1") {
		t.Fatal("first sighting must not be a duplicate")
	}
	if !d.seenBefore("k1") {
		t.Fatal("second sighting within TTL must be a duplicate")
	}
	if d.seenBefore("k2") {
		t.Fatal("a different key must not be a duplicate")
	}
}

func TestEventDedupExpiresAfterTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := newEventDedup(time.Minute)
	d.now = func() time.Time { return now }

	if d.seenBefore("k1") {
		t.Fatal("first sighting must not be a duplicate")
	}
	now = now.Add(90 * time.Second) // past the TTL
	if d.seenBefore("k1") {
		t.Fatal("sighting after the TTL must not be treated as a duplicate")
	}
	// The expired entry should have been pruned and re-recorded, so an
	// immediate repeat is once again a duplicate.
	if !d.seenBefore("k1") {
		t.Fatal("repeat immediately after re-recording must be a duplicate")
	}
}

func TestEventDedupNilAndEmptyKeyAreNeverDuplicates(t *testing.T) {
	var d *eventDedup
	if d.seenBefore("anything") {
		t.Fatal("nil dedup must never report a duplicate")
	}
	d = newEventDedup(time.Minute)
	if d.seenBefore("") || d.seenBefore("") {
		t.Fatal("empty key must never report a duplicate")
	}
}
