package journal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// corrIDKey is the unexported context key under which a correlation id is
// carried. A correlation id ties together every journal event emitted while
// handling one inbound interaction (a slash command, a button click, a
// link_shared event), so the gateway debugger can pull the whole story of a
// single interaction with one query.
type corrIDKey struct{}

// NewCorrID mints a fresh correlation id with the given short prefix (e.g.
// "gw" for a gateway interaction). The id is the prefix, an underscore, and 16
// random bytes hex-encoded — unique enough to group an interaction's events
// without coordination. On the vanishingly unlikely event that the system RNG
// fails, it returns the bare prefix so callers never have to handle an error.
func NewCorrID(prefix string) string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return prefix
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}

// WithCorrID returns a context carrying id, so downstream code (the workflow
// engine, the unfurl handler) can stamp the same correlation id on the events
// it records without threading it through every signature.
func WithCorrID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, corrIDKey{}, id)
}

// CorrIDFromContext returns the correlation id carried by ctx, or "" when none
// was set. A blank result is fine: events simply record without a correlation
// id and remain queryable by their other keys.
func CorrIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(corrIDKey{}).(string); ok {
		return id
	}
	return ""
}
