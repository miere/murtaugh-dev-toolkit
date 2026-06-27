package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/slack/client"
	"github.com/slack-go/slack"
)

// backfillMaxPages bounds conversations.replies pagination so a pathological
// thread can never spin forever. A page holds up to 1000 replies, so this caps
// a single backfill at ~50k messages — far beyond any real Slack thread while
// still being a hard stop.
const backfillMaxPages = 50

// backfillAPI is the slice of the Slack client the backfiller needs: read a
// thread's replies and resolve a user id to a profile. *slack.Client satisfies
// it; tests inject a fake.
type backfillAPI interface {
	GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error)
	GetUserInfoContext(ctx context.Context, user string) (*slack.User, error)
}

// ThreadBackfiller renders an existing Slack thread into the transcript block a
// freshly opened ACP session needs to start with context. ACP's session/prompt
// is a single user turn with no way to replay prior turns, so the only way to
// hand a cold session its backstory is as text in the first prompt — this type
// produces that text, author-labelled and framed.
type ThreadBackfiller struct {
	api       backfillAPI
	botUserID string
	logger    *slog.Logger

	mu    sync.Mutex
	names map[string]string // user id -> display name, resolved lazily
}

// NewThreadBackfiller builds a backfiller. botUserID is this bot's own Slack
// user id (from auth.test); it marks which transcript lines were the agent's
// own replies. An empty botUserID disables "(you)" tagging but still renders
// the thread.
func NewThreadBackfiller(api backfillAPI, botUserID string, logger *slog.Logger) *ThreadBackfiller {
	if logger == nil {
		logger = slog.Default()
	}
	return &ThreadBackfiller{api: api, botUserID: botUserID, logger: logger, names: map[string]string{}}
}

// Backfill fetches the thread rooted at threadTS and renders it as a framed,
// author-labelled transcript suitable for PromptRequest.History. The message
// whose timestamp equals excludeTS — the live prompt that triggered this turn —
// is omitted so it is not duplicated ahead of the user's own text. An empty
// string is returned (with no error) when the thread has no prior content worth
// sending.
func (b *ThreadBackfiller) Backfill(ctx context.Context, channelID, threadTS, excludeTS string) (string, error) {
	msgs, err := b.replies(ctx, channelID, threadTS)
	if err != nil {
		return "", err
	}
	lines := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m.Timestamp == excludeTS {
			continue
		}
		if strings.TrimSpace(m.Text) == "" {
			continue // join/leave and other contentless system messages
		}
		lines = append(lines, b.renderLine(ctx, m))
	}
	if len(lines) == 0 {
		return "", nil
	}
	return b.frame(ctx, lines), nil
}

// replies pages through conversations.replies, oldest-first. Slack returns the
// parent message first followed by replies in chronological order; we sort
// defensively by timestamp so ordering never depends on that contract.
func (b *ThreadBackfiller) replies(ctx context.Context, channelID, threadTS string) ([]slack.Message, error) {
	var out []slack.Message
	cursor := ""
	for page := 0; page < backfillMaxPages; page++ {
		params := &slack.GetConversationRepliesParameters{ChannelID: channelID, Timestamp: threadTS, Cursor: cursor}
		msgs, hasMore, next, err := b.api.GetConversationRepliesContext(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("conversations.replies: %w", err)
		}
		out = append(out, msgs...)
		if !hasMore || next == "" {
			break
		}
		cursor = next
	}
	sortByTS(out)
	return out, nil
}

// frame wraps the rendered lines in a delimited block with a one-line preamble.
// The preamble names the bot's own Slack alias once so the agent — which knows
// itself by its configured name, not the display name whoever installed it
// chose — can recognise its prior replies and answer when a human @-mentions
// that alias.
func (b *ThreadBackfiller) frame(ctx context.Context, lines []string) string {
	preamble := "Earlier messages in this Slack thread, oldest first. Lines marked (you) are your own prior replies."
	if b.botUserID != "" {
		preamble += fmt.Sprintf(" In this workspace you post as @%s.", b.resolveName(ctx, b.botUserID))
	}
	var sb strings.Builder
	sb.WriteString("<thread-transcript>\n")
	sb.WriteString(preamble)
	sb.WriteString("\n")
	sb.WriteString(strings.Join(lines, "\n"))
	sb.WriteString("\n</thread-transcript>")
	return sb.String()
}

// renderLine formats one message as `[HH:MM] @name: text` in Sydney time,
// tagging the bot's own messages with "(you)".
func (b *ThreadBackfiller) renderLine(ctx context.Context, m slack.Message) string {
	name := b.resolveName(ctx, m.User)
	label := "@" + name
	if b.botUserID != "" && m.User == b.botUserID {
		label += " (you)"
	}
	return fmt.Sprintf("[%s] %s: %s", formatHHMM(m.Timestamp), label, m.Text)
}

// resolveName maps a user id to a display name (display → real → handle → id),
// caching each lookup. On any error or an empty id it falls back to the raw id
// so a flaky users.info call degrades the label rather than failing the turn.
func (b *ThreadBackfiller) resolveName(ctx context.Context, userID string) string {
	if userID == "" {
		return "unknown"
	}
	b.mu.Lock()
	if name, ok := b.names[userID]; ok {
		b.mu.Unlock()
		return name
	}
	b.mu.Unlock()

	name := userID
	user, err := b.api.GetUserInfoContext(ctx, userID)
	if err != nil {
		b.logger.Debug("user lookup failed; using id", "user", userID, "error", err)
	} else {
		switch {
		case user.Profile.DisplayName != "":
			name = user.Profile.DisplayName
		case user.Profile.RealName != "":
			name = user.Profile.RealName
		case user.Name != "":
			name = user.Name
		}
	}
	b.mu.Lock()
	b.names[userID] = name
	b.mu.Unlock()
	return name
}

// sortByTS orders messages by their Slack timestamp ascending. Timestamps are
// "seconds.micros" decimal strings, so a numeric compare is correct; an
// unparseable timestamp sorts as 0 (effectively oldest) rather than panicking.
func sortByTS(msgs []slack.Message) {
	parse := func(ts string) float64 {
		f, _ := strconv.ParseFloat(ts, 64)
		return f
	}
	for i := 1; i < len(msgs); i++ {
		for j := i; j > 0 && parse(msgs[j-1].Timestamp) > parse(msgs[j].Timestamp); j-- {
			msgs[j-1], msgs[j] = msgs[j], msgs[j-1]
		}
	}
}

// formatHHMM renders a Slack timestamp as HH:MM in Sydney time, matching the
// Slack tools' transcript format. A malformed timestamp yields "??:??".
func formatHHMM(ts string) string {
	loc, err := time.LoadLocation(client.SydneyTZ)
	if err != nil {
		return "??:??"
	}
	f, err := strconv.ParseFloat(ts, 64)
	if err != nil {
		return "??:??"
	}
	sec, nsec := int64(f), int64((f-float64(int64(f)))*1e9)
	return time.Unix(sec, nsec).In(loc).Format("15:04")
}
