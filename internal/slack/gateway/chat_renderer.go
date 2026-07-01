package gateway

import (
	"context"
	"log/slog"
	"strings"

	"github.com/miere/murtaugh/internal/agent"
)

// chatRenderer turns an agent's event stream into Slack UI. The ChatHandler
// drives it event-by-event and is otherwise rendering-agnostic.
//
// There is a single implementation — sectionRenderer — which renders the turn as
// an ordered SEQUENCE of separate Slack messages: contiguous tool activity
// becomes a tool-block message (updated in place), and reply text becomes its own
// streamed message. A switch between the two seals the open section and opens a
// new one, so tool execution is always separated from the reply and never mixed
// into it, regardless of how the model interleaves text and tool calls. This is
// the coherence guarantee: ordering is decided here, by event boundaries — never
// by the delivery layer's wall-clock timers.
//
// The tool-block cosmetics are the only per-agent choice (the toolBlock seam): a
// compact status line (simplified) or grouped task cards (tasks). Both ride the
// same segmentation, so the ordering guarantee holds either way. This is
// backend-agnostic: native and ACP feed the same agent.Event stream, so they get
// an identical rendered surface.
//
// All methods are called from the single ChatHandler event loop (no concurrency).
type chatRenderer interface {
	// Text renders a streamed reply-text delta.
	Text(ctx context.Context, text string) error
	// Task renders a tool/task progress update.
	Task(ctx context.Context, ev *agent.TaskEvent) error
	// Attachment delivers a file the agent produced into the turn's thread, as a
	// separate Slack upload alongside the streamed reply.
	Attachment(ctx context.Context, a *agent.AttachmentEvent) error
	// Note appends a non-reply notice (idle-timeout marker) to the reply surface.
	Note(ctx context.Context, text string) error
	// BeginInterjection settles any open reply text so an out-of-band message
	// (e.g. an ACP approval card posted by the broker) lands below a committed
	// message rather than interleaved with an unfinished stream. The next text
	// event opens a fresh reply section after it. It does not disturb an open tool
	// block — a tool awaiting approval stays visible, as it does for native.
	BeginInterjection(ctx context.Context)
	// Finish finalises a successful turn, closing every open section. emptyNote,
	// when non-empty, is posted because the turn produced no reply text.
	Finish(ctx context.Context, emptyNote string) error
	// Fail finalises a turn that errored, surfacing err on the reply surface.
	Fail(ctx context.Context, err error) error
	// Interrupted finalises a caller-cancelled turn: a best-effort "_interrupted_"
	// marker on the reply surface plus closing any open tool block. Never paints a
	// tool red — the agent did not fail.
	Interrupted(ctx context.Context)
	// EnsureStopped is the idempotent safety net run on every exit path.
	EnsureStopped(ctx context.Context)
}

// --- toolBlock: the tool-run sink (cosmetics only) -------------------------

// toolBlock is one yellow run — a contiguous sequence of tool activity — rendered
// as its own Slack message. The segmenter opens one per tool run and resolves it
// (FinishWith) when reply text or the turn's end seals the run. Two cosmetics
// satisfy it, chosen per-agent:
//
//   - StatusLineWriter — a single context-block line updated in place, resolved
//     to a compact "✓ read · skill · write" summary (simplified mode).
//   - cardToolBlock — grouped task cards in their own stream message (tasks mode).
//
// Both own a message distinct from the reply text, so a tool card can never land
// inside an unflushed text run — the mid-paragraph interleaving that motivated
// this design.
type toolBlock interface {
	UpdateFromEvent(ctx context.Context, ev *agent.TaskEvent) error
	FinishWith(ctx context.Context, done string) error
}

// cardToolBlock renders a tool run as grouped task cards (TaskCardWriter) in its
// OWN stream message, kept separate from the reply text. It tracks which cards
// are still spinning so FinishWith can bring them to a terminal state before it
// closes the message — a card is never stranded mid-spinner.
type cardToolBlock struct {
	stream  *StreamWriter
	cards   *TaskCardWriter
	running map[string]agent.TaskStatus
	logger  *slog.Logger
}

func newCardToolBlock(api StreamAPI, channelID string, opts StreamWriterOptions, logger *slog.Logger) *cardToolBlock {
	if logger == nil {
		logger = slog.Default()
	}
	stream := NewStreamWriter(api, channelID, opts)
	return &cardToolBlock{
		stream:  stream,
		cards:   NewTaskCardWriter(api, stream, 0, logger),
		running: map[string]agent.TaskStatus{},
		logger:  logger,
	}
}

func (b *cardToolBlock) UpdateFromEvent(ctx context.Context, ev *agent.TaskEvent) error {
	if ev == nil {
		return nil
	}
	if err := b.cards.UpdateFromEvent(ctx, ev); err != nil {
		return err
	}
	// An explicit terminal status retires a card; an update that merely refines a
	// title (no status) keeps it tracked so FinishWith resolves it.
	if isTerminalTaskStatus(ev.Status) {
		delete(b.running, ev.ID)
	} else {
		b.running[ev.ID] = ev.Status
	}
	return nil
}

// FinishWith resolves every still-spinning card to complete, then closes the
// block's own message. done is the section's tool summary; task cards carry their
// own titles, so it is unused here (it labels the status-line cosmetic instead).
func (b *cardToolBlock) FinishWith(ctx context.Context, _ string) error {
	for id := range b.running {
		if err := b.cards.Complete(ctx, id, ""); err != nil {
			b.logger.Debug("failed to resolve task card", "error", err, "task_id", id)
		}
		delete(b.running, id)
	}
	return b.stream.Stop(ctx)
}

// --- sectionRenderer -------------------------------------------------------

type sectionMode int

const (
	sectionNone sectionMode = iota
	sectionText
	sectionTools
)

// sectionRenderer renders the event stream as an ordered sequence of Slack
// messages, alternating between streamed text messages and in-place tool blocks
// as the stream switches between text and tool activity.
type sectionRenderer struct {
	newText   func() *StreamWriter
	newBlock  func() toolBlock
	uploader  attachmentUploader
	channelID string
	threadTS  string
	logger    *slog.Logger

	mode   sectionMode
	text   *StreamWriter
	block  toolBlock
	titles []string // distinct tool titles in the current block, for its summary
}

func newSectionRenderer(newText func() *StreamWriter, newBlock func() toolBlock, uploader attachmentUploader, channelID, threadTS string, logger *slog.Logger) *sectionRenderer {
	if logger == nil {
		logger = slog.Default()
	}
	return &sectionRenderer{newText: newText, newBlock: newBlock, uploader: uploader, channelID: channelID, threadTS: threadTS, logger: logger}
}

// Attachment finalises the open text section first — so prose written before the
// file is committed above it and later text opens a fresh message below it — then
// uploads the file as its own message, preserving the agent's intended order.
func (r *sectionRenderer) Attachment(ctx context.Context, a *agent.AttachmentEvent) error {
	r.closeText(ctx)
	return deliverAttachment(ctx, r.uploader, r.channelID, r.threadTS, a)
}

func (r *sectionRenderer) Text(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}
	if r.mode == sectionTools {
		r.closeBlock(ctx)
	}
	if r.mode != sectionText || r.text == nil {
		r.text = r.newText()
		r.mode = sectionText
	}
	return r.text.Append(ctx, text)
}

func (r *sectionRenderer) Task(ctx context.Context, ev *agent.TaskEvent) error {
	if ev == nil {
		return nil
	}
	if r.mode == sectionText {
		r.closeText(ctx)
	}
	if r.mode != sectionTools || r.block == nil {
		r.block = r.newBlock()
		r.mode = sectionTools
	}
	if ev.Title != "" && !containsString(r.titles, ev.Title) {
		r.titles = append(r.titles, ev.Title)
	}
	if err := r.block.UpdateFromEvent(ctx, ev); err != nil {
		r.logger.Warn("failed to send task update", "error", err, "task_id", ev.ID)
	}
	return nil
}

// Note appends a notice to the reply surface — same routing as Text.
func (r *sectionRenderer) Note(ctx context.Context, text string) error {
	return r.Text(ctx, text)
}

// BeginInterjection closes the open reply-text section so an out-of-band card
// posts below a committed message; the next text event opens a fresh section after
// it. An open tool block is left untouched so a tool awaiting approval keeps its
// live state, matching native (whose approval fires with the tool's task in
// progress).
func (r *sectionRenderer) BeginInterjection(ctx context.Context) {
	r.closeText(ctx)
}

func (r *sectionRenderer) Finish(ctx context.Context, emptyNote string) error {
	if emptyNote != "" {
		if err := r.Text(ctx, emptyNote); err != nil {
			return err
		}
	}
	r.closeText(ctx)
	r.closeBlock(ctx)
	return nil
}

func (r *sectionRenderer) Fail(ctx context.Context, err error) error {
	r.closeBlock(ctx)
	if r.mode != sectionText || r.text == nil {
		r.text = r.newText()
		r.mode = sectionText
	}
	return r.text.Fail(ctx, err)
}

func (r *sectionRenderer) Interrupted(ctx context.Context) {
	if r.text != nil && r.text.Started() && !r.text.Stopped() {
		if err := r.text.Append(ctx, "\n\n_interrupted_"); err == nil {
			_ = r.text.Stop(ctx)
		}
	}
	r.closeBlock(ctx)
}

func (r *sectionRenderer) EnsureStopped(ctx context.Context) {
	r.closeText(ctx)
	r.closeBlock(ctx)
}

// closeText finalises the open text section, if any.
func (r *sectionRenderer) closeText(ctx context.Context) {
	if r.text != nil && r.text.Started() && !r.text.Stopped() {
		if err := r.text.Stop(ctx); err != nil {
			r.logger.Debug("failed to stop text section", "error", err)
		}
	}
	r.text = nil
	if r.mode == sectionText {
		r.mode = sectionNone
	}
}

// closeBlock resolves the open tool block to a compact summary of the tools it
// ran (e.g. "✓ read · skill · write"), then clears it. The summary is used by the
// status-line cosmetic; the card cosmetic carries its own per-card titles.
func (r *sectionRenderer) closeBlock(ctx context.Context) {
	if r.block != nil {
		done := statusLineDoneText
		if len(r.titles) > 0 {
			done = "✓ " + strings.Join(r.titles, " · ")
		}
		if err := r.block.FinishWith(ctx, done); err != nil {
			r.logger.Debug("failed to resolve tool block", "error", err)
		}
	}
	r.block = nil
	r.titles = nil
	if r.mode == sectionTools {
		r.mode = sectionNone
	}
}

// deliverAttachment uploads a as a separate Slack message in the turn's thread.
// A nil uploader (tests, or no upload surface wired) or a nil attachment is a
// silent no-op so the rest of the reply is unaffected.
func deliverAttachment(ctx context.Context, up attachmentUploader, channelID, threadTS string, a *agent.AttachmentEvent) error {
	if up == nil || a == nil {
		return nil
	}
	return up.UploadAttachment(ctx, channelID, threadTS, a)
}

// containsString reports whether want is present in xs.
func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
