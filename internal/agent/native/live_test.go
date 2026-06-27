package native

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/config"
)

// TestLive_NativeTurn exercises a REAL native turn against Gemini: provider
// streaming plus the in-process tool loop (files.ls + files.read). It is the
// committed form of the manual smoke test.
//
// It is skipped unless MURTAUGH_LIVE_GEMINI=1 and GEMINI_API_KEY are set, so the
// default `go test ./...` stays hermetic (no network, no cost). Run it with:
//
//	MURTAUGH_LIVE_GEMINI=1 go test ./internal/agent/native -run TestLive_NativeTurn -v
//
// GEMINI_API_KEY is read from the environment; it is also loaded from
// ~/.config/murtaugh/.env when present, matching production.
func TestLive_NativeTurn(t *testing.T) {
	if os.Getenv("MURTAUGH_LIVE_GEMINI") == "" {
		t.Skip("set MURTAUGH_LIVE_GEMINI=1 (and GEMINI_API_KEY) to run the live Gemini smoke test")
	}
	if home, err := os.UserHomeDir(); err == nil {
		_ = config.LoadDotEnv(filepath.Join(home, ".config", "murtaugh"))
	}
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("GEMINI_API_KEY not set")
	}

	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "notes.txt"), []byte("the secret project codename is BLUEJAY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	client, err := Build(config.AgentProfile{
		WorkDir: workdir,
		Tools:   []string{"files", "terminal"},
		Native: &config.NativeProfile{
			Provider:  "gemini",
			Model:     "gemini-3.1-pro-preview",
			APIKeyEnv: "GEMINI_API_KEY",
			MaxTurns:  8,
		},
	}, BuildDeps{BaseDir: workdir, Logger: logger})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sess, err := client.NewSession(ctx, agent.SessionMetadata{Source: "live-test"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ch, err := client.Prompt(ctx, sess.ID, agent.PromptRequest{
		Text: "Use your tools: list the files in your working directory, then read notes.txt and tell me the secret project codename. Answer in one sentence.",
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var text strings.Builder
	var toolRuns int
	for ev := range ch {
		switch ev.Type {
		case agent.EventText:
			text.WriteString(ev.Text)
		case agent.EventTask:
			if ev.Task != nil && ev.Task.Status == agent.TaskStatusInProgress {
				toolRuns++
			}
		case agent.EventError:
			t.Fatalf("stream error: %v", ev.Error)
		}
	}

	reply := strings.TrimSpace(text.String())
	t.Logf("tools invoked: %d | reply: %s", toolRuns, reply)
	if toolRuns == 0 {
		t.Error("expected at least one tool invocation")
	}
	if !strings.Contains(strings.ToUpper(reply), "BLUEJAY") {
		t.Errorf("reply did not contain the codename read via tools: %q", reply)
	}
}
