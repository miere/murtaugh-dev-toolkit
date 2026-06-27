package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/miere/murtaugh/internal/slack/pingcard"
	"github.com/slack-go/slack"
)

// ResumeMarker captures the Slack-side context of an in-flight restart so
// the next process startup can confirm the restart visibly. The
// originating channel and the TS of the "restarting…" notice are the only
// fields we strictly need; the rest is recorded for audit and forensics.
type ResumeMarker struct {
	Channel     string    `json:"channel"`
	ThreadTS    string    `json:"thread_ts,omitempty"`
	MessageTS   string    `json:"message_ts"`
	RequestedBy string    `json:"requested_by,omitempty"`
	RequestedAt time.Time `json:"requested_at"`
	Source      string    `json:"source,omitempty"`
	Reason      string    `json:"reason,omitempty"`
}

// ResumeMarkerStore persists at most one in-flight restart marker. Save
// overwrites any existing marker; Load returns (nil, nil) when no marker
// exists; Clear is idempotent.
type ResumeMarkerStore interface {
	Save(ResumeMarker) error
	Load() (*ResumeMarker, error)
	Clear() error
}

// FileResumeMarkerStore writes the marker as JSON to a fixed path with
// 0o600 permissions. The parent directory is created lazily on first Save
// with 0o700 so a freshly-installed deployment does not need bootstrap.
type FileResumeMarkerStore struct {
	path string
}

// NewFileResumeMarkerStore returns a store that persists to the given
// path. The path must be absolute; relative paths are accepted as-is and
// resolved against the process working directory.
func NewFileResumeMarkerStore(path string) *FileResumeMarkerStore {
	return &FileResumeMarkerStore{path: path}
}

// Path returns the on-disk location used by this store. Exposed for
// logging and tests; callers should not write to it directly.
func (s *FileResumeMarkerStore) Path() string { return s.path }

func (s *FileResumeMarkerStore) Save(marker ResumeMarker) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create resume marker dir: %w", err)
	}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("encode resume marker: %w", err)
	}
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		return fmt.Errorf("write resume marker %q: %w", s.path, err)
	}
	return nil
}

func (s *FileResumeMarkerStore) Load() (*ResumeMarker, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read resume marker %q: %w", s.path, err)
	}
	var marker ResumeMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return nil, fmt.Errorf("decode resume marker %q: %w", s.path, err)
	}
	return &marker, nil
}

func (s *FileResumeMarkerStore) Clear() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove resume marker %q: %w", s.path, err)
	}
	return nil
}

// slackMessagingAPI is the Slack surface needed by the resume helpers and
// the restart-suggestion flow. *slack.Client satisfies it. Kept narrow so
// unit tests can substitute a fake without re-implementing the full
// client. OpenConversationContext is only used by SuggestRestart's
// admin-DM fallback; resume.go itself never opens a conversation.
type slackMessagingAPI interface {
	PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error)
	UpdateMessageContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error)
	OpenConversationContext(ctx context.Context, params *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error)
}

const (
	restartNoticeText = ":hourglass_flowing_sand: Restarting Murtaugh now…"
	// resumeMarkerMaxAge bounds how stale a marker may be before it is
	// dropped instead of consumed. Protects against markers left behind
	// by a crash that never produced a real restart.
	resumeMarkerMaxAge = 1 * time.Hour
)

// postRestartNoticeAndSaveMarker posts the "restarting…" notice to the
// originating channel and persists a marker for the next startup to
// consume. Both operations are best-effort: failures are logged and the
// restart sequence continues regardless, since the alternative (aborting
// the restart) is worse UX than skipping the confirmation message.
//
// When channel is empty (e.g. a future trigger without Slack context) or
// no resume store is wired, this is a no-op.
func (a *Gateway) postRestartNoticeAndSaveMarker(ctx context.Context, channel, threadTS, userID, source, reason string) {
	if a.resumeStore == nil || channel == "" || a.messaging == nil {
		return
	}
	options := []slack.MsgOption{slack.MsgOptionText(restartNoticeText, false)}
	if threadTS != "" {
		options = append(options, slack.MsgOptionTS(threadTS))
	}
	postedChannel, ts, err := a.messaging.PostMessageContext(ctx, channel, options...)
	if err != nil {
		a.logger.Error("post restart notice failed", "channel", channel, "error", err)
		return
	}
	marker := ResumeMarker{
		Channel:     postedChannel,
		ThreadTS:    threadTS,
		MessageTS:   ts,
		RequestedBy: userID,
		RequestedAt: time.Now().UTC(),
		Source:      source,
		Reason:      reason,
	}
	if err := a.resumeStore.Save(marker); err != nil {
		a.logger.Error("save resume marker failed", "channel", marker.Channel, "ts", marker.MessageTS, "error", err)
		return
	}
	a.logger.Info("restart notice posted", "channel", marker.Channel, "ts", marker.MessageTS, "user", userID)
}

// consumeResumeMarker is invoked once after Socket Mode connects. It loads the
// marker (if any) and, when one is present and fresh, edits the original
// "restarting…" notice in place into the back-online ping card — so the single
// restart message becomes the operator's communication self-test (points 2c/2d
// of the redesign). The marker is always cleared, regardless of whether the
// edit succeeded, to avoid retry storms on every reconnect.
//
// It returns true only when it actually rendered the back-online card. The
// caller (notifyConnected) uses that to suppress the otherwise-redundant
// standalone startup ping. A missing, stale, or un-editable marker returns
// false, leaving the normal startup greeting to run.
func (a *Gateway) consumeResumeMarker(ctx context.Context) bool {
	if a.resumeStore == nil || a.messaging == nil {
		return false
	}
	marker, err := a.resumeStore.Load()
	if err != nil {
		a.logger.Error("load resume marker failed", "error", err)
		return false
	}
	if marker == nil {
		return false
	}
	defer func() {
		if err := a.resumeStore.Clear(); err != nil {
			a.logger.Error("clear resume marker failed", "error", err)
		}
	}()
	if !marker.RequestedAt.IsZero() && time.Since(marker.RequestedAt) > resumeMarkerMaxAge {
		a.logger.Warn("resume marker stale, dropping",
			"channel", marker.Channel,
			"ts", marker.MessageTS,
			"age", time.Since(marker.RequestedAt).String(),
		)
		return false
	}
	if _, _, _, err := a.messaging.UpdateMessageContext(ctx, marker.Channel, marker.MessageTS,
		slack.MsgOptionText(pingcard.BackOnlineText, false),
		slack.MsgOptionBlocks(pingcard.BuildBackOnline()...),
	); err != nil {
		a.logger.Error("update restart notice failed", "channel", marker.Channel, "ts", marker.MessageTS, "error", err)
		return false
	}
	a.logger.Info("restart notice updated", "channel", marker.Channel, "ts", marker.MessageTS, "user", marker.RequestedBy)
	return true
}
