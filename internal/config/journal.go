package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Known journal stream names. Duplicated as literals from internal/journal so
// that config (a low-level package) does not import a domain package; the two
// lists are kept in sync by hand.
const (
	journalStreamGateway    = "gateway"
	journalStreamJob        = "job"
	journalStreamACPSession = "acp_session"
)

// journalDefaultRetention is the fallback max age per stream when journal.yaml
// does not set one. Mirrors the table in specs-wip/011 §4.
var journalDefaultRetention = map[string]time.Duration{
	journalStreamGateway:    168 * time.Hour,  // 7d
	journalStreamJob:        720 * time.Hour,  // 30d
	journalStreamACPSession: 2160 * time.Hour, // 90d
}

const journalDefaultSweepEvery = 24 * time.Hour

// JournalConfig is the root of journal.yaml: the agent-facing event store's
// location, per-stream enablement/retention, and the sweep cadence. It lives in
// its own file (a sibling of agents.yaml/jobs.yaml) to keep each vertical
// legible; an absent file yields all-streams-on defaults.
type JournalConfig struct {
	Path    string                         `yaml:"path"`
	BlobDir string                         `yaml:"blob_dir"`
	Streams map[string]JournalStreamConfig `yaml:"streams"`
	Sweep   JournalSweepConfig             `yaml:"sweep"`
}

// JournalStreamConfig is the per-stream knob. Enabled is a *bool so an omitted
// stream (and an absent file) defaults to on, while an explicit `enabled:
// false` opts that stream out — the same tri-state pattern as
// AgentProfile.Interruptible.
type JournalStreamConfig struct {
	Enabled   *bool  `yaml:"enabled"`
	Retention string `yaml:"retention"`
}

// JournalSweepConfig controls the daemon's internal retention sweeper.
type JournalSweepConfig struct {
	Every string `yaml:"every"`
}

// EffectiveEnabled reports whether a stream is persisted. Streams default to on:
// an absent file, an omitted stream, or a stream with no explicit `enabled`
// all return true; only an explicit `enabled: false` turns one off.
func (c JournalConfig) EffectiveEnabled(stream string) bool {
	if sc, ok := c.Streams[stream]; ok && sc.Enabled != nil {
		return *sc.Enabled
	}
	return true
}

// EnabledStreams returns the set of streams that should be persisted, suitable
// for handing to journal.NewRecorder.
func (c JournalConfig) EnabledStreams() map[string]bool {
	out := make(map[string]bool, len(journalDefaultRetention))
	for _, stream := range []string{journalStreamGateway, journalStreamJob, journalStreamACPSession} {
		out[stream] = c.EffectiveEnabled(stream)
	}
	return out
}

// EffectiveRetention resolves a stream's max age: the configured value when
// valid, otherwise the per-stream default.
func (c JournalConfig) EffectiveRetention(stream string) time.Duration {
	if sc, ok := c.Streams[stream]; ok {
		if d, err := time.ParseDuration(strings.TrimSpace(sc.Retention)); err == nil && d > 0 {
			return d
		}
	}
	return journalDefaultRetention[stream]
}

// RetentionByStream returns every known stream's effective retention, suitable
// for handing to journal.Open.
func (c JournalConfig) RetentionByStream() map[string]time.Duration {
	out := make(map[string]time.Duration, len(journalDefaultRetention))
	for stream := range journalDefaultRetention {
		out[stream] = c.EffectiveRetention(stream)
	}
	return out
}

// EffectiveSweepEvery resolves the sweep cadence, defaulting to 24h.
func (c JournalConfig) EffectiveSweepEvery() time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(c.Sweep.Every)); err == nil && d > 0 {
		return d
	}
	return journalDefaultSweepEvery
}

// EffectivePath resolves the database path: the configured value (with ~
// expansion) or the XDG state default $XDG_STATE_HOME/murtaugh/journal.db
// (falling back to ~/.local/state/murtaugh/journal.db).
func (c JournalConfig) EffectivePath() string {
	if p := strings.TrimSpace(c.Path); p != "" {
		return expandHome(p)
	}
	return filepath.Join(journalStateDir(), "journal.db")
}

// EffectiveBlobDir resolves the directory for large-body blobs (used by the ACP
// session stream in a later PR), defaulting beside the database.
func (c JournalConfig) EffectiveBlobDir() string {
	if p := strings.TrimSpace(c.BlobDir); p != "" {
		return expandHome(p)
	}
	return filepath.Join(journalStateDir(), "journal-blobs")
}

// Validate checks journal.yaml: stream names must be known, durations must
// parse and be positive when set.
func (c JournalConfig) Validate() error {
	var errs []error
	for name, sc := range c.Streams {
		switch name {
		case journalStreamGateway, journalStreamJob, journalStreamACPSession:
		default:
			errs = append(errs, fmt.Errorf("journal.streams[%s] is not a known stream (gateway, job, acp_session)", name))
		}
		if r := strings.TrimSpace(sc.Retention); r != "" {
			if d, err := time.ParseDuration(r); err != nil {
				errs = append(errs, fmt.Errorf("journal.streams[%s].retention must be a valid duration: %w", name, err))
			} else if d <= 0 {
				errs = append(errs, fmt.Errorf("journal.streams[%s].retention must be greater than zero", name))
			}
		}
	}
	if e := strings.TrimSpace(c.Sweep.Every); e != "" {
		if d, err := time.ParseDuration(e); err != nil {
			errs = append(errs, fmt.Errorf("journal.sweep.every must be a valid duration: %w", err))
		} else if d <= 0 {
			errs = append(errs, errors.New("journal.sweep.every must be greater than zero"))
		}
	}
	return errors.Join(errs...)
}

// journalStateDir is the base directory for journal state, honouring
// XDG_STATE_HOME and falling back to ~/.local/state.
func journalStateDir() string {
	if x := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); x != "" {
		return filepath.Join(x, "murtaugh")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".local", "state", "murtaugh")
	}
	return filepath.Join(home, ".local", "state", "murtaugh")
}

// expandHome expands a leading ~ to the user's home directory.
func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	return path
}
