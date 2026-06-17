// Package files holds Murtaugh's native file tools (read, write, edit, ls) and
// the small pieces of shared state they need: a path-rooting helper that keeps
// every tool confined to a configured root directory, and a read-before-write
// stamp store that real coding agents use to guard edits against stale or
// blind writes.
package files

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Root anchors a file tool to a single directory. Every path a tool receives is
// resolved against the root and rejected if it escapes it (path-traversal
// guard). A tool is constructed with a Root and routes all filesystem access
// through Resolve.
type Root struct {
	dir string
}

// NewRoot returns a Root anchored at dir. dir is cleaned and made absolute so
// the traversal guard compares apples to apples; a relative dir is resolved
// against the process working directory. An empty dir is an error.
func NewRoot(dir string) (*Root, error) {
	if dir == "" {
		return nil, fmt.Errorf("files: root directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("files: resolve root %q: %w", dir, err)
	}
	return &Root{dir: filepath.Clean(abs)}, nil
}

// Dir returns the absolute, cleaned root directory.
func (r *Root) Dir() string { return r.dir }

// Resolve turns a caller-supplied path into an absolute path guaranteed to live
// inside the root. A relative path is taken relative to the root; an absolute
// path must already be inside it. Any path that resolves outside the root —
// including via "..", absolute escapes, or symlink-free trickery — is rejected.
//
// Resolve performs a lexical guard (filepath.Clean + prefix check). It does not
// follow symlinks; callers that need link-hardening should layer that on top.
func (r *Root) Resolve(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("files: path is required")
	}
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(r.dir, candidate)
	}
	candidate = filepath.Clean(candidate)

	// In-root iff it is the root itself or sits beneath root + separator.
	if candidate != r.dir {
		prefix := r.dir + string(os.PathSeparator)
		if len(candidate) < len(prefix) || candidate[:len(prefix)] != prefix {
			return "", fmt.Errorf("files: path %q escapes root %q", path, r.dir)
		}
	}
	return candidate, nil
}

// Rel returns the slash-separated path of an in-root absolute path relative to
// the root, for stable display in results. It assumes abs came from Resolve.
func (r *Root) Rel(abs string) string {
	rel, err := filepath.Rel(r.dir, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

// stamp records what a tool last observed about a file when it was read, so a
// later write/edit can detect that the file is unread or has drifted.
type stamp struct {
	size    int64
	modTime int64 // UnixNano
}

// ReadState is the shared read-before-write stamp store. Read tools call Mark
// after a successful read; Write/Edit tools call Verify before mutating. The
// zero value is not usable — construct with NewReadState. It is safe for
// concurrent use.
type ReadState struct {
	mu     sync.Mutex
	stamps map[string]stamp
}

// NewReadState returns an empty ReadState.
func NewReadState() *ReadState {
	return &ReadState{stamps: make(map[string]stamp)}
}

// Mark records the current on-disk identity of the file at abs as "seen". abs
// must be an absolute path (i.e. the output of Root.Resolve). A stat failure is
// reported so a read of a vanished file does not silently authorise a write.
func (s *ReadState) Mark(abs string) error {
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	s.set(abs, info)
	return nil
}

// MarkInfo records an already-obtained FileInfo for abs, avoiding a redundant
// stat when the caller has just read the file.
func (s *ReadState) MarkInfo(abs string, info os.FileInfo) {
	s.set(abs, info)
}

func (s *ReadState) set(abs string, info os.FileInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stamps[abs] = stamp{size: info.Size(), modTime: info.ModTime().UnixNano()}
}

// Verify checks that the file at abs was read before this write/edit and has not
// changed on disk since. It returns an error describing the violation otherwise:
//   - never read   → caller must read the file first;
//   - changed      → the file drifted; caller must re-read before mutating.
//
// A non-existent file is treated as "not read" so blind creation goes through
// the dedicated create path, not Edit/overwrite.
func (s *ReadState) Verify(abs string) error {
	s.mu.Lock()
	prev, seen := s.stamps[abs]
	s.mu.Unlock()
	if !seen {
		return fmt.Errorf("files: %q must be read before it can be modified", abs)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("files: %q can no longer be stat'd; re-read it first: %w", abs, err)
	}
	if info.Size() != prev.size || info.ModTime().UnixNano() != prev.modTime {
		return fmt.Errorf("files: %q changed on disk since it was read; re-read it before modifying", abs)
	}
	return nil
}

// Seen reports whether abs has been recorded by Mark (independent of drift).
func (s *ReadState) Seen(abs string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.stamps[abs]
	return ok
}
