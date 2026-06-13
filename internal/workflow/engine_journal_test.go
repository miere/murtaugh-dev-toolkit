package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/miere/murtaugh-dev-toolkit/internal/journal"
)

// fakeRecorder captures the journal events an engine emits.
type fakeRecorder struct {
	mu     sync.Mutex
	events []journal.Event
}

func (f *fakeRecorder) Record(_ context.Context, e journal.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func (f *fakeRecorder) byKind(kind string) []journal.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []journal.Event
	for _, e := range f.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// erroringRunner always fails, to exercise the trigger-failure event path.
type erroringRunner struct{}

func (erroringRunner) Run(context.Context, config.RunTriggerConfig, []byte) ([]byte, error) {
	return nil, errors.New("boom")
}

func TestEngineRecordsMatchAndTriggers(t *testing.T) {
	templateDir := t.TempDir()
	writeTemplate(t, templateDir, `{"text":"approved"}`)

	rec := &fakeRecorder{}
	engine := NewEngine(workflowConfig(), Options{
		Poster:      &recordingPoster{},
		Runner:      &recordingRunner{},
		TemplateDir: templateDir,
		Recorder:    rec,
	})

	ctx := journal.WithCorrID(context.Background(), "gw_test")
	if err := engine.Execute(ctx, approvalInteraction()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	matched := rec.byKind("workflow.matched")
	if len(matched) != 1 {
		t.Fatalf("expected one workflow.matched event, got %d", len(matched))
	}
	if matched[0].Keys.RuleID != "code-review-approval" {
		t.Fatalf("matched event rule = %q", matched[0].Keys.RuleID)
	}
	if matched[0].Stream != journal.StreamGateway || matched[0].Level != journal.LevelInfo {
		t.Fatalf("unexpected matched envelope: %+v", matched[0])
	}
	if matched[0].CorrID != "gw_test" {
		t.Fatalf("matched event corr id = %q, want gw_test", matched[0].CorrID)
	}

	// Two triggers (reply-to-slack, run) each produce a success event, both
	// carrying the rule id and correlation id.
	triggers := rec.byKind("workflow.trigger")
	if len(triggers) != 2 {
		t.Fatalf("expected two workflow.trigger events, got %d", len(triggers))
	}
	for _, e := range triggers {
		if e.Level != journal.LevelInfo {
			t.Fatalf("expected info trigger, got %v", e.Level)
		}
		if e.Keys.RuleID != "code-review-approval" || e.CorrID != "gw_test" {
			t.Fatalf("trigger event missing keys/corr: %+v", e)
		}
	}
}

func TestEngineRecordsNoMatch(t *testing.T) {
	rec := &fakeRecorder{}
	engine := NewEngine(workflowConfig(), Options{Poster: &recordingPoster{}, Recorder: rec})

	// pingInteraction does not satisfy the code-review-approval rule's match.
	if err := engine.Execute(context.Background(), pingInteraction()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	noMatch := rec.byKind("workflow.no_match")
	if len(noMatch) != 1 {
		t.Fatalf("expected one workflow.no_match event, got %d", len(noMatch))
	}
	if noMatch[0].Level != journal.LevelDebug {
		t.Fatalf("no_match should be debug, got %v", noMatch[0].Level)
	}
	if len(rec.byKind("workflow.matched")) != 0 {
		t.Fatalf("did not expect a matched event")
	}
}

func TestEngineRecordsTriggerFailure(t *testing.T) {
	cfg := workflowConfig()
	rule := cfg.WorkflowRules["code-review-approval"]
	rule.Triggers = []config.TriggerConfig{{Type: "run", Run: &config.RunTriggerConfig{Cmd: "/bin/false"}}}
	cfg.WorkflowRules["code-review-approval"] = rule

	rec := &fakeRecorder{}
	engine := NewEngine(cfg, Options{Poster: &recordingPoster{}, Runner: erroringRunner{}, Recorder: rec})

	if err := engine.Execute(context.Background(), approvalInteraction()); err == nil {
		t.Fatalf("expected Execute to fail")
	}
	triggers := rec.byKind("workflow.trigger")
	if len(triggers) != 1 || triggers[0].Level != journal.LevelError {
		t.Fatalf("expected one error trigger event, got %+v", triggers)
	}
	payload, ok := triggers[0].Payload.(map[string]any)
	if !ok || payload["error"] == nil {
		t.Fatalf("error trigger should carry an error payload, got %+v", triggers[0].Payload)
	}
}
