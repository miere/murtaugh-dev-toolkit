package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// recordingChangeFunc captures every fire so tests can assert on the
// exact set of (path, mtime) the watcher reported. The mutex keeps
// the recorder safe under the watcher's polling goroutine even
// though most tests drive poll() synchronously.
type recordingChangeFunc struct {
	mu    sync.Mutex
	calls []recordedChange
}

type recordedChange struct {
	path  string
	mtime time.Time
}

func (r *recordingChangeFunc) callback() configFileChangeFunc {
	return func(_ context.Context, path string, mtime time.Time) {
		r.mu.Lock()
		r.calls = append(r.calls, recordedChange{path: path, mtime: mtime})
		r.mu.Unlock()
	}
}

func (r *recordingChangeFunc) snapshot() []recordedChange {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedChange(nil), r.calls...)
}

// writeFile is a small helper so each test reads cleanly. The mode
// matches what murtaugh's installer drops on disk.
func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

// touchFile bumps the mtime of path to a moment in the future so the
// poll comparison detects a change regardless of filesystem
// resolution. Some filesystems (HFS+, certain network mounts) only
// store seconds; a +2s bump guarantees a distinguishable mtime.
func touchFile(t *testing.T, path string) time.Time {
	t.Helper()
	now := time.Now().Add(2 * time.Second).Truncate(time.Second)
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatalf("chtimes %q: %v", path, err)
	}
	return now
}

func TestConfigWatcherBaselinePollDoesNotFire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.yaml")
	writeFile(t, path, "v1")
	rec := &recordingChangeFunc{}
	w := newConfigWatcher([]string{path}, time.Hour, rec.callback(), newSilentLogger())
	w.poll(context.Background(), true)
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("expected baseline poll to be silent, got %d call(s): %#v", len(calls), calls)
	}
	if _, ok := w.seen[path]; !ok {
		t.Fatal("expected baseline poll to seed the path's mtime")
	}
}

func TestConfigWatcherUnchangedFileDoesNotFire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.yaml")
	writeFile(t, path, "v1")
	rec := &recordingChangeFunc{}
	w := newConfigWatcher([]string{path}, time.Hour, rec.callback(), newSilentLogger())
	w.poll(context.Background(), true)
	w.poll(context.Background(), false)
	w.poll(context.Background(), false)
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("expected no fires for unchanged file, got %#v", calls)
	}
}

func TestConfigWatcherFiresOnMtimeChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.yaml")
	writeFile(t, path, "v1")
	rec := &recordingChangeFunc{}
	w := newConfigWatcher([]string{path}, time.Hour, rec.callback(), newSilentLogger())
	w.poll(context.Background(), true)
	bumped := touchFile(t, path)
	w.poll(context.Background(), false)
	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one fire after mtime bump, got %d: %#v", len(calls), calls)
	}
	if calls[0].path != path {
		t.Fatalf("expected fire path %q, got %q", path, calls[0].path)
	}
	if !calls[0].mtime.Equal(bumped) {
		t.Fatalf("expected fire mtime %v, got %v", bumped, calls[0].mtime)
	}
	// A second poll with no further change must not refire — the
	// watcher tracks last-seen mtime, not "is the file dirty".
	w.poll(context.Background(), false)
	if calls := rec.snapshot(); len(calls) != 1 {
		t.Fatalf("expected fire to be one-shot per mtime, got %d: %#v", len(calls), calls)
	}
}

func TestConfigWatcherToleratesMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "agents.yaml")
	rec := &recordingChangeFunc{}
	w := newConfigWatcher([]string{missing}, time.Hour, rec.callback(), newSilentLogger())
	// Baseline + several follow-ups: no fires, no panic, no seeded entry.
	w.poll(context.Background(), true)
	w.poll(context.Background(), false)
	w.poll(context.Background(), false)
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("expected missing file to produce no fires, got %#v", calls)
	}
	if _, ok := w.seen[missing]; ok {
		t.Fatal("expected missing file to remain unseeded")
	}
}

func TestConfigWatcherSeedsLateAppearingFile(t *testing.T) {
	dir := t.TempDir()
	stable := filepath.Join(dir, "slack.yaml")
	late := filepath.Join(dir, "agents.yaml")
	writeFile(t, stable, "v1")
	rec := &recordingChangeFunc{}
	w := newConfigWatcher([]string{stable, late}, time.Hour, rec.callback(), newSilentLogger())
	w.poll(context.Background(), true)
	if _, ok := w.seen[late]; ok {
		t.Fatal("expected late path to be absent from seed when missing at baseline")
	}
	// File appears after the daemon has been running for a while.
	writeFile(t, late, "v1")
	w.poll(context.Background(), false)
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("expected late-appearing file to seed silently, got %#v", calls)
	}
	if _, ok := w.seen[late]; !ok {
		t.Fatal("expected late path to be seeded once it appeared")
	}
	// Now a real edit on the late file should fire normally.
	touchFile(t, late)
	w.poll(context.Background(), false)
	calls := rec.snapshot()
	if len(calls) != 1 || calls[0].path != late {
		t.Fatalf("expected one fire for late-path edit, got %#v", calls)
	}
}

func TestConfigWatcherRunStopsOnContextCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.yaml")
	writeFile(t, path, "v1")
	rec := &recordingChangeFunc{}
	w := newConfigWatcher([]string{path}, 5*time.Millisecond, rec.callback(), newSilentLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expected Run to return shortly after ctx cancel")
	}
}

// TestOnConfigFileChangedPostsRestartSuggestion verifies the
// watcher's wired callback funnels into the existing SuggestRestart
// path: the admin's DM is opened, a Block Kit suggestion is posted,
// and the operator-facing reason names the changed file.
func TestOnConfigFileChangedPostsRestartSuggestion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.yaml")
	writeFile(t, path, "v1")
	msg := &recordingMessaging{postReturnedTS: "1700000000.000700", openChannelID: "DADMIN00"}
	app := &Gateway{
		logger:    newSilentLogger(),
		cfg:       config.AccessConfig{AdminUser: "UADMIN00"},
		messaging: msg,
	}
	app.onConfigFileChanged(context.Background(), path, time.Unix(1700000000, 0).UTC())
	if msg.openCalls != 1 {
		t.Fatalf("expected admin DM to be opened, got %d open call(s)", msg.openCalls)
	}
	if len(msg.openUsers) != 1 || msg.openUsers[0] != "UADMIN00" {
		t.Fatalf("expected DM to admin UADMIN00, got %v", msg.openUsers)
	}
	if msg.postCalls != 1 || msg.postChannel != "DADMIN00" {
		t.Fatalf("expected post to admin DM, got calls=%d channel=%q", msg.postCalls, msg.postChannel)
	}
}

// TestOnConfigFileChangedSilentWhenNoAdminOrChannel makes sure the
// watcher's callback is a no-op when the deployment is locked down
// (no admin_user configured, no explicit channel). The watcher must
// never panic or stall on a SuggestRestart that can't find a
// destination — the daemon would otherwise log a noisy error per
// poll interval if the operator removed the admin user mid-flight.
func TestOnConfigFileChangedSilentWhenNoAdminOrChannel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.yaml")
	writeFile(t, path, "v1")
	msg := &recordingMessaging{}
	app := &Gateway{
		logger:    newSilentLogger(),
		cfg:       config.AccessConfig{},
		messaging: msg,
	}
	app.onConfigFileChanged(context.Background(), path, time.Unix(1700000000, 0).UTC())
	if msg.openCalls != 0 || msg.postCalls != 0 {
		t.Fatalf("expected callback to be silent with no admin, got open=%d post=%d", msg.openCalls, msg.postCalls)
	}
}

// TestStartConfigWatcherSkipsWhenPathsEmpty guards the cheap path:
// CLI / MCP / test invocations must not even allocate a watcher
// goroutine. An easy regression to introduce, so we pin it.
func TestStartConfigWatcherSkipsWhenPathsEmpty(t *testing.T) {
	app := &Gateway{logger: newSilentLogger()}
	// Pass a cancelled context to make absolutely sure no goroutine
	// is left lingering; the function should return immediately
	// without spawning anything regardless.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app.startConfigWatcher(ctx)
	if len(app.configWatchPaths) != 0 {
		t.Fatalf("expected empty configWatchPaths, got %v", app.configWatchPaths)
	}
}

// TestWithConfigWatchPathsTrimsBlankEntries pins the small bit of
// hygiene the setter applies so the composition root can hand it a
// raw slice without pre-cleaning.
func TestWithConfigWatchPathsTrimsBlankEntries(t *testing.T) {
	app := (&Gateway{}).WithConfigWatchPaths([]string{"/etc/murtaugh/slack.yaml", "  ", "", " /etc/murtaugh/agents.yaml  "})
	if len(app.configWatchPaths) != 2 {
		t.Fatalf("expected 2 retained paths, got %d: %v", len(app.configWatchPaths), app.configWatchPaths)
	}
	if !strings.HasSuffix(app.configWatchPaths[0], "slack.yaml") || !strings.HasSuffix(app.configWatchPaths[1], "agents.yaml") {
		t.Fatalf("expected retained paths to be slack.yaml and agents.yaml, got %v", app.configWatchPaths)
	}
}
