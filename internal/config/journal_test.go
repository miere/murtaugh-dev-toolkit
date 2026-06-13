package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// parseJournal unmarshals a journal.yaml body into a JournalConfig.
func parseJournal(t *testing.T, body string) JournalConfig {
	t.Helper()
	var wrap struct {
		Journal JournalConfig `yaml:"journal"`
	}
	if err := yaml.Unmarshal([]byte(body), &wrap); err != nil {
		t.Fatalf("unmarshal journal yaml: %v", err)
	}
	return wrap.Journal
}

func TestJournalDefaultsAllStreamsOn(t *testing.T) {
	// A zero JournalConfig models an absent journal.yaml: every stream on, with
	// the default retentions.
	var c JournalConfig

	for _, stream := range []string{journalStreamGateway, journalStreamJob, journalStreamACPSession} {
		if !c.EffectiveEnabled(stream) {
			t.Errorf("stream %q should default to enabled", stream)
		}
	}
	if got := c.EffectiveRetention(journalStreamGateway); got != 168*time.Hour {
		t.Errorf("gateway retention = %v, want 168h", got)
	}
	if got := c.EffectiveRetention(journalStreamACPSession); got != 2160*time.Hour {
		t.Errorf("acp_session retention = %v, want 2160h", got)
	}
	if got := c.EffectiveSweepEvery(); got != 24*time.Hour {
		t.Errorf("sweep every = %v, want 24h", got)
	}
	if c.EffectivePath() == "" {
		t.Errorf("EffectivePath should never be empty")
	}
	if !strings.HasSuffix(c.EffectivePath(), "journal.db") {
		t.Errorf("EffectivePath = %q, want it to end in journal.db", c.EffectivePath())
	}
	enabled := c.EnabledStreams()
	for _, stream := range []string{journalStreamGateway, journalStreamJob, journalStreamACPSession} {
		if !enabled[stream] {
			t.Errorf("EnabledStreams[%q] should be true by default", stream)
		}
	}
}

func TestJournalDisableStreamOptOut(t *testing.T) {
	c := parseJournal(t, `journal:
  streams:
    gateway:
      enabled: false
      retention: 12h
`)
	if c.EffectiveEnabled(journalStreamGateway) {
		t.Errorf("gateway should be disabled when enabled: false")
	}
	// Other streams remain on by default.
	if !c.EffectiveEnabled(journalStreamJob) {
		t.Errorf("job should remain enabled by default")
	}
	if got := c.EffectiveRetention(journalStreamGateway); got != 12*time.Hour {
		t.Errorf("gateway retention = %v, want 12h", got)
	}
	if c.EnabledStreams()[journalStreamGateway] {
		t.Errorf("EnabledStreams should reflect the opt-out")
	}
}

func TestJournalRetentionByStream(t *testing.T) {
	c := parseJournal(t, `journal:
  streams:
    job:
      retention: 1h
`)
	r := c.RetentionByStream()
	if r[journalStreamJob] != time.Hour {
		t.Errorf("job retention = %v, want 1h", r[journalStreamJob])
	}
	if r[journalStreamGateway] != 168*time.Hour {
		t.Errorf("gateway retention should fall back to default, got %v", r[journalStreamGateway])
	}
}

func TestJournalValidate(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"valid", `journal:
  streams:
    gateway:
      enabled: true
      retention: 7h
  sweep:
    every: 1h
`, ""},
		{"unknown stream", `journal:
  streams:
    bogus:
      retention: 1h
`, "not a known stream"},
		{"bad retention", `journal:
  streams:
    gateway:
      retention: notaduration
`, "must be a valid duration"},
		{"zero retention", `journal:
  streams:
    gateway:
      retention: 0s
`, "greater than zero"},
		{"bad sweep", `journal:
  sweep:
    every: nope
`, "journal.sweep.every"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := parseJournal(t, tc.body).Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestConfigValidateSurfacesJournalErrors(t *testing.T) {
	cfg, err := Parse(testConfig(``))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cfg.Journal = parseJournal(t, `journal:
  streams:
    bogus:
      retention: 1h
`)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "not a known stream") {
		t.Fatalf("Config.Validate should surface journal errors, got %v", err)
	}
}
