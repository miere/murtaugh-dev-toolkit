// Package attach implements the `attach` tool: deliver a file from the agent's
// workspace to the user as a first-class reply attachment.
//
// Unlike `slack.send-msg`, this tool is transport-agnostic — it knows nothing
// about Slack channels or threads. It returns an *agent.AttachmentEvent and the
// native loop turns that into an EventAttachment, which the chat handler uploads
// into the turn's own conversation. That keeps "reply with a file" consistent
// across backends and surfaces (the file always lands in the thread the user is
// already talking in), rather than asking the model to address a destination.
//
// The file path is confined to the agent's workspace root (the same rooting the
// `files` tools use): a path that escapes the root is rejected. Without that, an
// agent — including one steered by prompt injection — could attach and exfiltrate
// any host file the daemon can read (secrets, keys) into the channel. Rooting
// brings attach to parity with the read side of `files`.
package attach

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/files"
)

// maxAttachmentBytes bounds the file size the tool will hand off. It is a guard
// against a model accidentally attaching a huge artifact; Slack itself rejects
// very large uploads anyway. 100 MiB mirrors Slack's per-file ceiling.
const maxAttachmentBytes = 100 << 20

// Tool is the `attach` capability. It is rooted at an agent's workspace so the
// files it can deliver are confined to that directory.
type Tool struct {
	root *files.Root
}

// New constructs the attach tool rooted at root. The root confines which files
// the tool may attach; it is the same per-agent workspace root the `files` tools
// are built on.
func New(root *files.Root) *Tool { return &Tool{root: root} }

// Name returns the registry key.
func (t *Tool) Name() string { return "attach" }

// Description returns the human-facing summary advertised to the model.
func (t *Tool) Description() string {
	return "Attach a file from your workspace to your reply so the user receives it as a downloadable file. Use this to return generated artifacts (reports, images, archives, exports) instead of pasting their contents. The path must be inside your workspace."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"path":    {Type: "string", Description: "File path relative to the workspace root (absolute paths must stay inside it). Must exist and be non-empty."},
			"title":   {Type: "string", Description: "Optional display title for the file. Defaults to the filename."},
			"comment": {Type: "string", Description: "Optional message posted alongside the file (a caption)."},
		},
		Required: []string{"path"},
	}
}

// Invoke validates the requested file and returns an *agent.AttachmentEvent for
// the native loop to emit. The path is resolved against the workspace root and
// rejected if it escapes; the bytes are NOT read here — the resolved path is
// carried through to the chat handler, which streams the file to Slack at upload
// time — so a large attachment is never buffered in the conversation. Validation
// (in-root, exists, non-empty, regular file, size ceiling) happens here so the
// model gets an actionable error it can recover from rather than a late failure.
func (t *Tool) Invoke(_ context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	title, _ := args["title"].(string)
	comment, _ := args["comment"].(string)

	if path == "" {
		return nil, fmt.Errorf("error: path is required")
	}
	if t.root == nil {
		return nil, fmt.Errorf("error: attach has no workspace root configured")
	}
	abs, err := t.root.Resolve(path)
	if err != nil {
		return nil, fmt.Errorf("error: %v", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("error: cannot stat %q: %v", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("error: %q is a directory, not a file", path)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("error: %q is empty (0 bytes); Slack rejects empty uploads", path)
	}
	if info.Size() > maxAttachmentBytes {
		return nil, fmt.Errorf("error: %q is %d bytes, over the %d-byte attachment limit", path, info.Size(), int64(maxAttachmentBytes))
	}

	return &agent.AttachmentEvent{
		Filename: filepath.Base(abs),
		Title:    title,
		Comment:  comment,
		Path:     abs,
	}, nil
}
