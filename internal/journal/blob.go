package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BlobStore holds large event bodies as files outside the database — currently
// ACP session transcripts. The journal row keeps the queryable envelope and
// references the file via blob_ref; the body itself lives here so a row stays
// small and the 16 KiB payload cap never truncates a transcript.
type BlobStore struct {
	dir string
}

// NewBlobStore returns a store rooted at dir. A blank dir makes every append a
// no-op (returning an empty ref), so callers need no nil/enabled checks.
func NewBlobStore(dir string) *BlobStore { return &BlobStore{dir: dir} }

// TranscriptTurn is one appended line in a session's transcript file: the user
// prompt, the agent's full response, and how the turn ended.
type TranscriptTurn struct {
	Time     time.Time `json:"time"`
	Agent    string    `json:"agent,omitempty"`
	Source   string    `json:"source,omitempty"`
	Outcome  string    `json:"outcome"`
	Prompt   string    `json:"prompt"`
	Response string    `json:"response"`
}

// AppendTranscript appends one turn to the session's NDJSON transcript file and
// returns the ref to store on the journal row — a path relative to the blob
// directory, so it resolves the same way the sweeper resolves it for cleanup. A
// store with no directory configured is a no-op (empty ref, no error).
func (b *BlobStore) AppendTranscript(sessionID string, turn TranscriptTurn) (string, error) {
	if b == nil || b.dir == "" {
		return "", nil
	}
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		return "", err
	}
	ref := safeBlobName(sessionID) + ".ndjson"
	line, err := json.Marshal(turn)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(filepath.Join(b.dir, ref), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return "", err
	}
	return ref, nil
}

// safeBlobName turns a session id into a single filename-safe component: only
// [A-Za-z0-9._-] survive, everything else becomes '_'. Leading dots are dropped
// so the name can never be hidden or a "../" traversal (it has no separators
// anyway), and an empty result falls back to "unknown".
func safeBlobName(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	name := strings.TrimLeft(b.String(), ".")
	if name == "" {
		return "unknown"
	}
	return name
}
