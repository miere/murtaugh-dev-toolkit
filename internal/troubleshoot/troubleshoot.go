// Package troubleshoot assembles a self-contained diagnostics bundle for
// Murtaugh: a consistent snapshot of the event journal, the daemon logs, the
// (redacted) config files, optional downstream-provider artifacts (e.g. Goose
// sessions and logs), a manifest, and an embedded instructions file telling an
// AI agent how to read the bundle.
//
// The bundler is deterministic Go that runs inside the daemon (or the CLI). It
// never delegates to an agent — the agent is precisely what tends to be broken
// when this is needed — and it never reaches below the ACP boundary into a
// provider's live request path; it only collects files the provider has
// already written to disk.
package troubleshoot

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver for the journal snapshot

	"github.com/miere/murtaugh/assets"
)

// defaultMaxLogBytes caps how much of each (potentially unbounded) log file is
// captured. We keep the tail, since the most recent activity is what matters
// when something just broke.
const defaultMaxLogBytes int64 = 5 << 20 // 5 MiB

const instructionsAsset = "troubleshoot/INSTRUCTIONS.md"

// Options controls what a bundle contains.
type Options struct {
	// Note is the user's free-text symptom description, recorded in the
	// manifest so whoever investigates starts with context.
	Note string
	// Providers names the downstream MCP providers whose on-disk diagnostics
	// to include (see KnownProviders), e.g. "goose". Unknown names are
	// recorded as a non-fatal manifest error.
	Providers []string
	// MaxLogBytes caps per-log-file capture (tail). Zero uses the default.
	MaxLogBytes int64
	// NoRedact disables secret redaction. Off by default (redaction on), and
	// never reachable from the Slack path.
	NoRedact bool
	// OutPath is an explicit destination for the zip. Empty writes a temp file.
	OutPath string
}

func (o Options) maxLogBytes() int64 {
	if o.MaxLogBytes > 0 {
		return o.MaxLogBytes
	}
	return defaultMaxLogBytes
}

// Sources are the resolved on-disk locations the bundler reads from. ResolveSources
// derives them from config; keeping them explicit makes the bundler testable.
type Sources struct {
	Version     string
	JournalDB   string   // EffectivePath of journal.db
	BlobDir     string   // EffectiveBlobDir (ACP transcripts)
	ConfigFiles []string // gateway.yaml, agents.yaml, jobs.yaml, journal.yaml
	LogFiles    []string // daemon stdout/stderr logs
	Home        string
	GOOS        string
}

// FileEntry describes one file captured into the bundle.
type FileEntry struct {
	Path      string `json:"path"`  // location within the zip
	Bytes     int64  `json:"bytes"` // size as captured
	Redacted  bool   `json:"redacted,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Note      string `json:"note,omitempty"`
}

// Manifest is the bundle's table of contents and provenance, written as
// manifest.json at the bundle root.
type Manifest struct {
	Tool                 string      `json:"tool"`
	MurtaughVersion      string      `json:"murtaugh_version"`
	OS                   string      `json:"os"`
	Arch                 string      `json:"arch"`
	GeneratedAt          string      `json:"generated_at"`
	Symptom              string      `json:"symptom,omitempty"`
	Providers            []string    `json:"providers,omitempty"`
	RedactionApplied     bool        `json:"redaction_applied"`
	RedactionLimitations string      `json:"redaction_limitations"`
	MaxLogBytes          int64       `json:"max_log_bytes"`
	Files                []FileEntry `json:"files"`
	Errors               []string    `json:"errors,omitempty"` // non-fatal collection problems
}

// Result is returned by Build.
type Result struct {
	Path     string
	Bytes    int64
	Manifest Manifest
}

// ResolveSources derives the bundler's read locations from the loaded config.
// baseDir is where the sibling config files live (cfg.BaseDir, else the dir of
// configPath, else ~/.config/murtaugh). Log paths are reconstructed from the
// launchd convention (~/Library/Logs/murtaugh) since they are not discoverable
// at runtime; absent files are simply skipped during Build.
func ResolveSources(journalDB, blobDir, baseDir, version string) Sources {
	home, _ := os.UserHomeDir()
	if strings.TrimSpace(baseDir) == "" {
		baseDir = filepath.Join(home, ".config", "murtaugh")
	}
	configFiles := make([]string, 0, 6)
	for _, name := range []string{"gateway.yaml", "agents.yaml", "jobs.yaml", "journal.yaml", "workflow-rules.yaml", "unfurl-rules.yaml"} {
		configFiles = append(configFiles, filepath.Join(baseDir, name))
	}
	var logFiles []string
	if home != "" {
		logsDir := filepath.Join(home, "Library", "Logs", "murtaugh")
		logFiles = []string{
			filepath.Join(logsDir, "slack.out.log"),
			filepath.Join(logsDir, "slack.err.log"),
		}
	}
	return Sources{
		Version:     version,
		JournalDB:   journalDB,
		BlobDir:     blobDir,
		ConfigFiles: configFiles,
		LogFiles:    logFiles,
		Home:        home,
		GOOS:        runtime.GOOS,
	}
}

// Build assembles the bundle and returns the zip path plus the manifest.
// Collection of an individual artifact is best-effort: a missing or unreadable
// source is recorded in Manifest.Errors and skipped, so a partial bundle is
// always produced. Only failure to create the zip itself is fatal.
func Build(ctx context.Context, opts Options, src Sources) (Result, error) {
	staging, err := os.MkdirTemp("", "murtaugh-troubleshoot-stage-")
	if err != nil {
		return Result{}, fmt.Errorf("create staging dir: %w", err)
	}
	defer os.RemoveAll(staging)

	b := &builder{
		staging: staging,
		opts:    opts,
		manifest: Manifest{
			Tool:                 "troubleshoot.bundle",
			MurtaughVersion:      src.Version,
			OS:                   src.GOOS,
			Arch:                 runtime.GOARCH,
			GeneratedAt:          time.Now().UTC().Format(time.RFC3339),
			Symptom:              strings.TrimSpace(opts.Note),
			Providers:            opts.Providers,
			RedactionApplied:     !opts.NoRedact,
			RedactionLimitations: RedactionLimitations,
			MaxLogBytes:          opts.maxLogBytes(),
		},
	}

	b.collectJournal(ctx, src.JournalDB)
	b.collectBlobs(src.BlobDir)
	b.collectConfigs(src.ConfigFiles)
	b.collectLogs(src.LogFiles)
	b.collectProviders(opts.Providers, src.Home, src.GOOS)
	b.collectInstructions()
	b.writeManifest()

	outPath := opts.OutPath
	if strings.TrimSpace(outPath) == "" {
		outPath = filepath.Join(os.TempDir(), fmt.Sprintf("murtaugh-troubleshoot-%s.zip", time.Now().UTC().Format("20060102-150405")))
	}
	if err := zipDir(staging, outPath); err != nil {
		return Result{}, fmt.Errorf("write bundle zip: %w", err)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		return Result{}, fmt.Errorf("stat bundle zip: %w", err)
	}
	return Result{Path: outPath, Bytes: info.Size(), Manifest: b.manifest}, nil
}

// builder accumulates files under a staging dir and records them in a manifest.
type builder struct {
	staging  string
	opts     Options
	manifest Manifest
}

func (b *builder) errf(format string, args ...any) {
	b.manifest.Errors = append(b.manifest.Errors, fmt.Sprintf(format, args...))
}

// collectJournal snapshots the live WAL-mode SQLite journal with VACUUM INTO,
// which produces a consistent, self-contained copy (no -wal/-shm sidecars) —
// unlike a plain file copy, which can capture a torn database mid-write.
func (b *builder) collectJournal(ctx context.Context, dbPath string) {
	if strings.TrimSpace(dbPath) == "" {
		return
	}
	if _, err := os.Stat(dbPath); err != nil {
		b.errf("journal db %s not found: %v", filepath.Base(dbPath), err)
		return
	}
	rel := filepath.Join("journal", "journal.db")
	dest := filepath.Join(b.staging, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		b.errf("journal staging dir: %v", err)
		return
	}
	if err := snapshotSQLite(ctx, dbPath, dest); err != nil {
		b.errf("snapshot journal db: %v", err)
		return
	}
	b.record(rel, dest, false, false, "consistent VACUUM INTO snapshot; binary — NOT redacted, may contain conversation content")
}

// collectBlobs captures the ACP transcript NDJSON files. They are text, so they
// are redacted, but note they can still carry secrets a user pasted into chat.
func (b *builder) collectBlobs(blobDir string) {
	if strings.TrimSpace(blobDir) == "" {
		return
	}
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		if !os.IsNotExist(err) {
			b.errf("read blob dir: %v", err)
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b.copyText(filepath.Join("journal", "transcripts", e.Name()), filepath.Join(blobDir, e.Name()), false)
	}
}

func (b *builder) collectConfigs(paths []string) {
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			continue // a config file that does not exist is normal
		}
		b.copyText(filepath.Join("config", filepath.Base(p)), p, false)
	}
}

func (b *builder) collectLogs(paths []string) {
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		b.copyText(filepath.Join("logs", filepath.Base(p)), p, true)
	}
}

func (b *builder) collectProviders(providers []string, home, goos string) {
	known := make(map[string]struct{}, len(KnownProviders()))
	for _, k := range KnownProviders() {
		known[k] = struct{}{}
	}
	for _, provider := range providers {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "" {
			continue
		}
		if _, ok := known[provider]; !ok {
			b.errf("unknown provider %q (known: %s)", provider, strings.Join(KnownProviders(), ", "))
			continue
		}
		sources := resolveProviderSources(provider, home, goos)
		if len(sources) == 0 {
			b.errf("provider %q: no diagnostic files found on this machine", provider)
			continue
		}
		for label, files := range sources {
			for _, f := range files {
				rel := filepath.Join("providers", provider, label, filepath.Base(f))
				if isTextFile(f) {
					b.copyText(rel, f, isLogFile(f))
				} else {
					b.copyBinary(rel, f)
				}
			}
		}
	}
}

func (b *builder) collectInstructions() {
	data, err := assets.FS.ReadFile(instructionsAsset)
	if err != nil {
		b.errf("read embedded instructions: %v", err)
		return
	}
	rel := "INSTRUCTIONS.md"
	dest := filepath.Join(b.staging, rel)
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		b.errf("write instructions: %v", err)
		return
	}
	b.record(rel, dest, false, false, "how to read this bundle")
}

func (b *builder) writeManifest() {
	dest := filepath.Join(b.staging, "manifest.json")
	data, err := json.MarshalIndent(b.manifest, "", "  ")
	if err != nil {
		b.errf("marshal manifest: %v", err)
		return
	}
	if err := os.WriteFile(dest, append(data, '\n'), 0o644); err != nil {
		b.errf("write manifest: %v", err)
	}
}

// copyText copies a text file into the bundle, redacting secrets (unless
// disabled) and, when isLog, keeping only the trailing maxLogBytes.
func (b *builder) copyText(rel, srcPath string, isLog bool) {
	data, truncated, err := readCapped(srcPath, b.opts.maxLogBytes(), isLog)
	if err != nil {
		b.errf("read %s: %v", filepath.Base(srcPath), err)
		return
	}
	redacted := false
	if !b.opts.NoRedact {
		if out, changed := redactText(data); changed {
			data, redacted = out, true
		}
	}
	dest := filepath.Join(b.staging, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		b.errf("staging dir for %s: %v", rel, err)
		return
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		b.errf("write %s: %v", rel, err)
		return
	}
	note := ""
	if truncated {
		note = fmt.Sprintf("tail-truncated to %d bytes", b.opts.maxLogBytes())
	}
	b.record(rel, dest, redacted, truncated, note)
}

func (b *builder) copyBinary(rel, srcPath string) {
	dest := filepath.Join(b.staging, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		b.errf("staging dir for %s: %v", rel, err)
		return
	}
	in, err := os.Open(srcPath)
	if err != nil {
		b.errf("open %s: %v", filepath.Base(srcPath), err)
		return
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		b.errf("create %s: %v", rel, err)
		return
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		b.errf("copy %s: %v", rel, err)
		return
	}
	out.Close()
	b.record(rel, dest, false, false, "binary — NOT redacted, may contain secrets/conversation content")
}

func (b *builder) record(rel, dest string, redacted, truncated bool, note string) {
	var size int64
	if info, err := os.Stat(dest); err == nil {
		size = info.Size()
	}
	b.manifest.Files = append(b.manifest.Files, FileEntry{
		Path:      filepath.ToSlash(rel),
		Bytes:     size,
		Redacted:  redacted,
		Truncated: truncated,
		Note:      note,
	})
}

// snapshotSQLite copies a live SQLite database via VACUUM INTO, which the
// modernc.org/sqlite driver supports as a SQL statement (it exposes no backup C
// API). The destination must not already exist.
func snapshotSQLite(ctx context.Context, srcPath, destPath string) error {
	db, err := sql.Open("sqlite", "file:"+srcPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", destPath); err != nil {
		return err
	}
	return nil
}

// readCapped reads a file fully, or — when isLog and the file exceeds max —
// only its trailing max bytes (advancing to the next line boundary so the first
// retained line is not a fragment).
func readCapped(path string, max int64, isLog bool) (data []byte, truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !isLog || info.Size() <= max {
		all, err := io.ReadAll(f)
		return all, false, err
	}
	if _, err := f.Seek(info.Size()-max, io.SeekStart); err != nil {
		return nil, false, err
	}
	tail, err := io.ReadAll(f)
	if err != nil {
		return nil, false, err
	}
	if i := indexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:]
	}
	banner := fmt.Sprintf("‹… truncated: showing last %d bytes of %d …›\n", max, info.Size())
	return append([]byte(banner), tail...), true, nil
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// zipDir writes every regular file under root into a zip at outPath, using
// paths relative to root as the archive names.
func zipDir(root, outPath string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(w, in)
		return err
	})
	if walkErr != nil {
		zw.Close()
		return walkErr
	}
	return zw.Close()
}

func isTextFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml", ".json", ".jsonl", ".ndjson", ".log", ".txt", ".md", ".toml", ".conf", ".ini", ".env":
		return true
	default:
		return false
	}
}

func isLogFile(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".log")
}
