package gateway

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/agent"
)

// attachmentUploader delivers an agent-produced file into a Slack thread. It is
// the seam the chat renderers use so tests can substitute a fake without slack-go.
type attachmentUploader interface {
	UploadAttachment(ctx context.Context, channelID, threadTS string, a *agent.AttachmentEvent) error
}

// slackUploadAPI is the narrow slack-go surface the uploader needs. *slack.Client
// satisfies it, so the gateway wraps the same client it uses for streaming.
type slackUploadAPI interface {
	UploadFileContext(ctx context.Context, params slack.UploadFileParameters) (*slack.FileSummary, error)
}

// slackAttachmentUploader is the production attachmentUploader. It maps an
// AttachmentEvent onto Slack's external-upload flow (files.getUploadURLExternal
// -> PUT -> files.completeUploadExternal, which slack-go's UploadFileContext
// runs), choosing a byte source from the event: in-memory Data (ACP) or a Path
// on disk (native tools). The byte count is set explicitly because the external
// flow needs it up front — Slack rejects a zero-length upload otherwise.
type slackAttachmentUploader struct{ api slackUploadAPI }

func (u slackAttachmentUploader) UploadAttachment(ctx context.Context, channelID, threadTS string, a *agent.AttachmentEvent) error {
	if a == nil {
		return nil
	}
	params := slack.UploadFileParameters{
		Channel:         channelID,
		ThreadTimestamp: threadTS,
		Filename:        a.Filename,
		Title:           a.Title,
		InitialComment:  a.Comment,
	}
	switch {
	case len(a.Data) > 0:
		params.Reader = bytes.NewReader(a.Data)
		params.FileSize = len(a.Data)
	case a.Path != "":
		info, err := os.Stat(a.Path)
		if err != nil {
			return fmt.Errorf("stat attachment %q: %w", a.Path, err)
		}
		if info.Size() == 0 {
			return fmt.Errorf("attachment %q is empty (0 bytes); Slack rejects empty uploads", a.Path)
		}
		params.File = a.Path
		params.FileSize = int(info.Size())
	default:
		return fmt.Errorf("attachment %q has neither data nor a path", a.Filename)
	}
	if params.Filename == "" {
		params.Filename = "attachment"
	}
	if params.Title == "" {
		params.Title = params.Filename
	}
	if _, err := u.api.UploadFileContext(ctx, params); err != nil {
		return fmt.Errorf("upload attachment: %w", err)
	}
	return nil
}
