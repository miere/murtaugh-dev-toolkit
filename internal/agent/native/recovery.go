package native

import (
	"context"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
)

// maxRecoveryRetries bounds the empty-completion recovery loop. Modeled on the
// Hermes recovery path (see ignore/goose_moim_and_hermes_consecutive_msg_merger.md
// §9), which retries a small fixed number of times before falling back.
const maxRecoveryRetries = 2

// tryRecover handles the empty-completion case: a turn that returned end_turn
// with zero text and zero tool calls. Such empties are usually a transient
// provider hiccup, so it re-streams the SAME request up to maxRecoveryRetries
// times. If a retry yields any text or tool calls, recovery succeeds and that
// turnResult is returned. If every retry is still empty, recovery fails cleanly
// (recovered=false, err=nil) and the caller ends the turn so the existing
// empty-reply note surfaces to the user.
//
// CRITICAL: recovery does NOT inject a standalone user "nudge" message. Doing so
// after a tool-result would recreate the exact Goose MOIM consecutive-user bug
// this package exists to prevent. Any nudge would have to be folded into the
// system prompt or a properly-alternating message; here we simply re-ask, which
// is both safe and sufficient for transient empties.
func (l *Loop) tryRecover(ctx context.Context, conv *Conversation, system string, emit func(agent.Event)) (recovered bool, res turnResult, err error) {
	for attempt := 0; attempt < maxRecoveryRetries; attempt++ {
		if cErr := ctx.Err(); cErr != nil {
			return false, turnResult{}, cErr
		}
		emit(eventStatus("Empty reply from model; retrying"))

		// Guard still holds across the retry — the array is unchanged, but
		// keeping the assertion here documents that recovery never mutates it
		// into a malformed shape.
		if gErr := assertNoConsecutiveUserAfterTool(conv.Messages()); gErr != nil {
			return false, turnResult{}, gErr
		}

		r, sErr := l.runTurn(ctx, conv, system, emit)
		if sErr != nil {
			return false, turnResult{}, sErr
		}
		if r.text != "" || len(r.toolCalls) > 0 {
			return true, r, nil
		}
	}
	return false, turnResult{}, nil
}
