package troubleshoot

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeJournalDB creates a small WAL-mode SQLite database so the snapshot path
// (VACUUM INTO) is exercised against a real file.
func makeJournalDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open journal db: %v", err)
	}
	defer db.Close()
	stmts := []string{
		"PRAGMA journal_mode=WAL",
		"CREATE TABLE events (id INTEGER PRIMARY KEY, summary TEXT)",
		"INSERT INTO events (summary) VALUES ('completed turn via default (0 bytes)')",
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

func readZip(t *testing.T, path string) map[string][]byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	out := make(map[string][]byte)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open zip entry %s: %v", f.Name, err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = b
	}
	return out
}

// TestResolveSources_NeverIncludesDotEnv guards the secrets boundary: the
// bundler collects a FIXED list of YAML config siblings and must never reach
// for .env, where every credential now lives. If someone later switches
// collectConfigs to glob the config dir, this fails loudly.
func TestResolveSources_NeverIncludesDotEnv(t *testing.T) {
	src := ResolveSources("/x/journal.db", "/x/blobs", "/x/config", "v1")
	for _, p := range src.ConfigFiles {
		if strings.EqualFold(filepath.Base(p), ".env") || strings.HasSuffix(p, ".env") {
			t.Fatalf("ResolveSources included a .env path %q — credentials must never enter a bundle", p)
		}
	}
}

// TestBuild_DoesNotCaptureDotEnv proves that even when a .env sits in the config
// dir alongside the collected YAML, it does not end up in the bundle.
func TestBuild_DoesNotCaptureDotEnv(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	slackYAML := filepath.Join(cfgDir, "slack.yaml")
	if err := os.WriteFile(slackYAML, []byte("oauth:\n  bot_token: ${SLACK_BOT_TOKEN}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(cfgDir, ".env")
	if err := os.WriteFile(envFile, []byte("SLACK_BOT_TOKEN=xoxb-realsecretvalue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "bundle.zip")
	// Use the real resolver so this exercises the production file list.
	src := ResolveSources("", "", cfgDir, "v1")
	if _, err := Build(context.Background(), Options{OutPath: out}, src); err != nil {
		t.Fatalf("Build: %v", err)
	}
	files := readZip(t, out)
	for name, content := range files {
		if strings.Contains(name, ".env") {
			t.Fatalf("bundle captured a .env entry %q", name)
		}
		if strings.Contains(string(content), "realsecretvalue") {
			t.Fatalf("a real secret leaked into bundle entry %q", name)
		}
	}
}

func TestBuild_AssemblesRedactedBundle(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "config")
	blobDir := filepath.Join(dir, "blobs")
	logDir := filepath.Join(dir, "logs")
	for _, d := range []string{cfgDir, blobDir, logDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	journalPath := filepath.Join(dir, "journal.db")
	makeJournalDB(t, journalPath)

	slackYAML := filepath.Join(cfgDir, "slack.yaml")
	if err := os.WriteFile(slackYAML, []byte("oauth:\n  bot_token: xoxb-12-34-supersecretvalue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(blobDir, "20260615_3.ndjson")
	if err := os.WriteFile(transcript, []byte(`{"prompt":"use token xapp-1-AAA-bbb-cccsecret"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A log larger than the cap so truncation is exercised.
	logPath := filepath.Join(logDir, "slack.err.log")
	big := strings.Repeat("line of log output\n", 2000)
	if err := os.WriteFile(logPath, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "bundle.zip")
	src := Sources{
		Version:     "v9.9.9",
		JournalDB:   journalPath,
		BlobDir:     blobDir,
		ConfigFiles: []string{slackYAML},
		LogFiles:    []string{logPath},
		GOOS:        "testos",
	}
	res, err := Build(context.Background(), Options{Note: "bot went silent", MaxLogBytes: 1024, OutPath: out}, src)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Path != out {
		t.Fatalf("Path = %q, want %q", res.Path, out)
	}
	files := readZip(t, out)

	// Manifest present and well-formed.
	mraw, ok := files["manifest.json"]
	if !ok {
		t.Fatal("manifest.json missing from bundle")
	}
	var m Manifest
	if err := json.Unmarshal(mraw, &m); err != nil {
		t.Fatalf("manifest not valid JSON: %v", err)
	}
	if m.MurtaughVersion != "v9.9.9" || m.Symptom != "bot went silent" || !m.RedactionApplied {
		t.Fatalf("manifest fields wrong: %+v", m)
	}

	// Journal snapshot present and non-trivial.
	if jb, ok := files["journal/journal.db"]; !ok || len(jb) == 0 {
		t.Fatalf("journal snapshot missing/empty (present=%v)", ok)
	}

	// Config present and redacted.
	cfgBytes, ok := files["config/slack.yaml"]
	if !ok {
		t.Fatal("config/slack.yaml missing")
	}
	if strings.Contains(string(cfgBytes), "supersecretvalue") {
		t.Errorf("slack token leaked into bundle:\n%s", cfgBytes)
	}

	// Transcript present and redacted.
	if tb, ok := files["journal/transcripts/20260615_3.ndjson"]; !ok || strings.Contains(string(tb), "cccsecret") {
		t.Errorf("transcript missing or token leaked: present=%v body=%s", ok, tb)
	}

	// Log present and tail-truncated.
	lb, ok := files["logs/slack.err.log"]
	if !ok {
		t.Fatal("logs/slack.err.log missing")
	}
	if int64(len(lb)) > 1024+256 { // cap + truncation banner slack
		t.Errorf("log not truncated: %d bytes", len(lb))
	}
	if !strings.Contains(string(lb), "truncated") {
		t.Errorf("expected truncation banner in log")
	}

	// Instructions present.
	if _, ok := files["INSTRUCTIONS.md"]; !ok {
		t.Error("INSTRUCTIONS.md missing from bundle")
	}
}

func TestResolveProviderSources_GooseViaPathRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GOOSE_PATH_ROOT", root)
	sessions := filepath.Join(root, "sessions")
	logs := filepath.Join(root, "logs")
	for _, d := range []string{sessions, logs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sessions, "sessions.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "cli.log"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := resolveProviderSources("goose", root, "testos")
	if len(got["sessions"]) == 0 {
		t.Errorf("expected sessions files, got %v", got["sessions"])
	}
	if len(got["logs"]) == 0 {
		t.Errorf("expected log files, got %v", got["logs"])
	}
	if resolveProviderSources("unknown", root, "testos") != nil {
		t.Error("unknown provider should resolve to nil")
	}
}

func TestBuild_MissingSourcesAreNonFatal(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "bundle.zip")
	// Everything points at non-existent paths.
	src := Sources{
		Version:     "dev",
		JournalDB:   filepath.Join(dir, "nope.db"),
		BlobDir:     filepath.Join(dir, "no-blobs"),
		ConfigFiles: []string{filepath.Join(dir, "missing.yaml")},
		LogFiles:    []string{filepath.Join(dir, "missing.log")},
		GOOS:        "testos",
	}
	res, err := Build(context.Background(), Options{OutPath: out}, src)
	if err != nil {
		t.Fatalf("Build should not fail on missing sources: %v", err)
	}
	files := readZip(t, out)
	if _, ok := files["manifest.json"]; !ok {
		t.Fatal("manifest.json should still be produced")
	}
	if _, ok := files["INSTRUCTIONS.md"]; !ok {
		t.Fatal("INSTRUCTIONS.md should still be produced")
	}
	// The journal error should be recorded, not fatal.
	if len(res.Manifest.Errors) == 0 {
		t.Error("expected non-fatal collection errors to be recorded")
	}
}
