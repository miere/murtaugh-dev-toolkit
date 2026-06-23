package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
)

func TestScheduleDefinition(t *testing.T) {
	t.Run("cron", func(t *testing.T) {
		def, err := scheduleDefinition(config.JobProfile{Schedule: "0 2 * * *"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if def == nil {
			t.Fatal("expected a job definition")
		}
	})
	t.Run("every", func(t *testing.T) {
		def, err := scheduleDefinition(config.JobProfile{Every: "1h"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if def == nil {
			t.Fatal("expected a job definition")
		}
	})
	t.Run("every-invalid", func(t *testing.T) {
		if _, err := scheduleDefinition(config.JobProfile{Every: "nope"}); err == nil {
			t.Fatal("expected error for invalid duration")
		}
	})
	t.Run("manual", func(t *testing.T) {
		if _, err := scheduleDefinition(config.JobProfile{Command: "/bin/echo"}); err == nil {
			t.Fatal("expected error for a manual (unscheduled) job")
		}
	})
}

func TestStartSchedulerNoOpWithoutRunner(t *testing.T) {
	a := &Gateway{
		logger:        discardLogger(),
		scheduledJobs: map[string]config.JobProfile{"j": {Command: "/bin/echo", Every: "1h"}},
		// runJob is nil → scheduling disabled.
	}
	stop := a.startScheduler(context.Background())
	stop() // must be safe to call even though nothing started.
}

func TestStartSchedulerNoOpWhenNoScheduledJobs(t *testing.T) {
	a := &Gateway{
		logger:        discardLogger(),
		scheduledJobs: map[string]config.JobProfile{"manual": {Command: "/bin/echo"}},
		runJob:        func(context.Context, string) error { return nil },
	}
	stop := a.startScheduler(context.Background())
	stop()
}

func TestStartSchedulerHoldsUnconfirmedJobWithoutBroker(t *testing.T) {
	unconfirmed := false
	fired := make(chan string, 1)
	a := &Gateway{
		logger: discardLogger(),
		scheduledJobs: map[string]config.JobProfile{
			"held": {Command: "/bin/echo", Every: "20ms", Confirmed: &unconfirmed},
		},
		runJob: func(_ context.Context, name string) error {
			select {
			case fired <- name:
			default:
			}
			return nil
		},
		// No interaction broker wired, so the first-run confirmation can never be
		// granted and the held job must not run.
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := a.startScheduler(ctx)
	defer stop()

	select {
	case got := <-fired:
		t.Fatalf("unconfirmed job %q ran without confirmation, want it held back", got)
	case <-time.After(300 * time.Millisecond):
		// Expected: the held job fires its trigger but is never executed.
	}
}

func TestStartSchedulerFiresConfirmedAndManualNilJobs(t *testing.T) {
	confirmed := true
	fired := make(chan string, 4)
	a := &Gateway{
		logger: discardLogger(),
		scheduledJobs: map[string]config.JobProfile{
			// Confirmed (non-nil true) interval job: must fire.
			"go": {Command: "/bin/echo", Every: "20ms", Confirmed: &confirmed},
			// Hand-written (Confirmed nil) interval job: must fire (unaffected).
			"plain": {Command: "/bin/echo", Every: "20ms"},
		},
		runJob: func(_ context.Context, name string) error {
			select {
			case fired <- name:
			default:
			}
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := a.startScheduler(ctx)
	defer stop()

	select {
	case <-fired:
		// At least one of the two eligible jobs fired.
	case <-time.After(2 * time.Second):
		t.Fatal("no eligible scheduled job fired within 2s")
	}
}

func TestStartSchedulerFiresIntervalJob(t *testing.T) {
	var (
		mu    sync.Mutex
		names []string
	)
	fired := make(chan string, 4)
	a := &Gateway{
		logger:        discardLogger(),
		scheduledJobs: map[string]config.JobProfile{"tick": {Command: "/bin/echo", Every: "50ms"}},
		runJob: func(_ context.Context, name string) error {
			mu.Lock()
			names = append(names, name)
			mu.Unlock()
			select {
			case fired <- name:
			default:
			}
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := a.startScheduler(ctx)
	defer stop()

	select {
	case got := <-fired:
		if got != "tick" {
			t.Fatalf("scheduled job ran with name %q, want \"tick\"", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled interval job did not fire within 2s")
	}
}
