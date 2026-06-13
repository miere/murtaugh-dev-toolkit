package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/miere/murtaugh-dev-toolkit/internal/journal"
	"github.com/miere/murtaugh-dev-toolkit/internal/unfurl"
	"github.com/miere/murtaugh-dev-toolkit/internal/workflow"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

const maxUnfurlLinks = 10

// numericTS distinguishes a real message timestamp (e.g. 1700000000.000100)
// from the composer-mode UUID Slack sends before a message is posted.
var numericTS = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

// Unfurler posts link previews. *slack.Client satisfies it via
// UnfurlMessageContext.
type Unfurler interface {
	UnfurlMessageContext(ctx context.Context, channelID, timestamp string, unfurls map[string]slack.Attachment, options ...slack.MsgOption) (string, string, string, error)
}

// UnfurlDelegator runs a delegate-to-agent unfurl action, requiring the agent's
// output to be a JSON Slack attachment. *agentdelegate.Runner satisfies it.
type UnfurlDelegator interface {
	RunForJSON(ctx context.Context, agent, prompt string) ([]byte, error)
}

// LinkUnfurlHandler renders custom previews for shared links.
type LinkUnfurlHandler struct {
	matcher   *unfurl.Matcher
	renderer  *unfurl.Renderer
	runner    workflow.CommandRunner
	delegator UnfurlDelegator
	api       Unfurler
	botUserID string
	logger    *slog.Logger
	recorder  journal.Recorder
}

// LinkSharedRequest is the typed projection of a link_shared event.
type LinkSharedRequest struct {
	TeamID    string
	ChannelID string
	UserID    string
	MessageTS string
	ThreadTS  string
	Links     []slackevents.SharedLinks
}

// NewLinkUnfurlHandler builds a handler. A nil runner defaults to the OS runner.
// A nil delegator disables delegate-to-agent unfurl actions (they fail with a
// clear error if configured).
func NewLinkUnfurlHandler(matcher *unfurl.Matcher, renderer *unfurl.Renderer, runner workflow.CommandRunner, delegator UnfurlDelegator, api Unfurler, logger *slog.Logger) *LinkUnfurlHandler {
	if logger == nil {
		logger = slog.Default()
	}
	if runner == nil {
		runner = workflow.OSCommandRunner{}
	}
	return &LinkUnfurlHandler{matcher: matcher, renderer: renderer, runner: runner, delegator: delegator, api: api, logger: logger, recorder: journal.NopRecorder{}}
}

// WithRecorder wires the journal recorder that receives gateway-stream unfurl
// events (per-link render outcome, the chat.unfurl post). A nil recorder is
// ignored, leaving the no-op default in place. Returns the receiver for fluent
// wiring at the composition root.
func (h *LinkUnfurlHandler) WithRecorder(recorder journal.Recorder) *LinkUnfurlHandler {
	if recorder != nil {
		h.recorder = recorder
	}
	return h
}

// record emits a gateway-stream journal event for an unfurl, stamping the
// correlation id the gateway minted for this link_shared event (carried on
// ctx) and the request's channel/team/user keys.
func (h *LinkUnfurlHandler) record(ctx context.Context, kind string, level journal.Level, summary string, req LinkSharedRequest, ruleID string, payload any) {
	h.recorder.Record(ctx, journal.Event{
		Stream:  journal.StreamGateway,
		Kind:    kind,
		Level:   level,
		Summary: summary,
		CorrID:  journal.CorrIDFromContext(ctx),
		Keys: journal.Keys{
			TeamID:    req.TeamID,
			ChannelID: req.ChannelID,
			UserID:    req.UserID,
			RuleID:    ruleID,
		},
		Payload: payload,
	})
}

// Handle matches each shared link, builds previews, and posts a single
// chat.unfurl call. Per-link failures are logged and skipped.
func (h *LinkUnfurlHandler) Handle(ctx context.Context, req LinkSharedRequest) error {
	if h == nil || h.matcher == nil || h.matcher.Len() == 0 {
		return nil
	}
	if !numericTS.MatchString(req.MessageTS) {
		h.logger.Debug("skipping composer-mode link_shared", "channel", req.ChannelID, "message_ts", req.MessageTS)
		return nil
	}
	if h.botUserID != "" && req.UserID == h.botUserID {
		return nil
	}

	unfurls := make(map[string]slack.Attachment)
	seen := make(map[string]struct{})
	processed := 0
	for _, link := range req.Links {
		if processed >= maxUnfurlLinks {
			break
		}
		if link.URL == "" {
			continue
		}
		if _, ok := seen[link.URL]; ok {
			continue
		}
		seen[link.URL] = struct{}{}
		processed++

		match, ok := h.matcher.Match(link.URL, link.Domain, req.ChannelID)
		if !ok {
			h.record(ctx, "unfurl.no_match", journal.LevelDebug, "no unfurl rule matched", req, "",
				map[string]any{"url": link.URL, "domain": link.Domain})
			continue
		}
		attachment, err := h.build(ctx, match, req, link)
		if err != nil {
			h.logger.Warn("failed to build unfurl", "rule", match.Rule.Name, "url", link.URL, "error", err)
			h.record(ctx, "unfurl.render", journal.LevelError, "failed to build unfurl", req, match.Rule.Name,
				map[string]any{"url": link.URL, "error": err.Error()})
			continue
		}
		h.record(ctx, "unfurl.render", journal.LevelInfo, "built unfurl preview", req, match.Rule.Name,
			map[string]any{"url": link.URL})
		unfurls[link.URL] = attachment
	}

	if len(unfurls) == 0 {
		return nil
	}
	if _, _, _, err := h.api.UnfurlMessageContext(ctx, req.ChannelID, req.MessageTS, unfurls); err != nil {
		h.record(ctx, "unfurl.post", journal.LevelError, "chat.unfurl failed", req, "",
			map[string]any{"count": len(unfurls), "error": err.Error()})
		return fmt.Errorf("unfurl message: %w", err)
	}
	h.logger.Info("unfurled shared links", "channel", req.ChannelID, "count", len(unfurls))
	h.record(ctx, "unfurl.post", journal.LevelInfo, "posted link unfurls", req, "",
		map[string]any{"count": len(unfurls)})
	return nil
}

func (h *LinkUnfurlHandler) build(ctx context.Context, match unfurl.Match, req LinkSharedRequest, link slackevents.SharedLinks) (slack.Attachment, error) {
	data := unfurl.Data{
		URL:       link.URL,
		Domain:    link.Domain,
		Channel:   req.ChannelID,
		User:      req.UserID,
		MessageTS: req.MessageTS,
		ThreadTS:  req.ThreadTS,
		TeamID:    req.TeamID,
		Captures:  match.Captures,
	}
	action := match.Rule.Config.Unfurl
	switch {
	case action.DelegateToAgent != nil:
		if h.delegator == nil {
			return slack.Attachment{}, fmt.Errorf("delegate-to-agent requires ACP to be enabled")
		}
		prompt, err := unfurl.RenderPrompt(action.DelegateToAgent.Prompt, data)
		if err != nil {
			return slack.Attachment{}, err
		}
		// RunForJSON validates the output is JSON and, on failure, logs a warning
		// with the raw output before returning an error; Handle then skips this
		// link rather than posting a malformed unfurl.
		body, err := h.delegator.RunForJSON(ctx, action.DelegateToAgent.Agent, prompt)
		if err != nil {
			return slack.Attachment{}, err
		}
		return unfurl.ParseAttachment(body)
	case action.Run != nil:
		input, err := json.Marshal(data)
		if err != nil {
			return slack.Attachment{}, fmt.Errorf("marshal unfurl context: %w", err)
		}
		stdout, err := h.runner.Run(ctx, *action.Run, input)
		if err != nil {
			return slack.Attachment{}, err
		}
		return unfurl.ParseAttachment(stdout)
	default:
		return h.renderer.Render(action.Template, data)
	}
}
