package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	askbroker "github.com/miere/murtaugh-dev-toolkit/internal/slack/interaction"
)

// ScheduledRunner executes the named job to completion and returns a non-nil
// error when it fails (process error or non-zero exit). The composition root
// injects a closure over the jobs.run tool so scheduled runs reuse the exact
// execution path — timeout, workdir, exit-code handling — as manual ones,
// and the gateway stays free of any dependency on the tools layer.
type ScheduledRunner func(ctx context.Context, name string) error

// startScheduler builds and starts the gocron scheduler for every job whose
// profile carries a cron `schedule` or interval `every`. It is a no-op — and
// pays no cost — when no runner is wired (CLI/MCP and most tests) or when no
// job opts into scheduling. The returned shutdown function is always safe to
// call, including when nothing started.
//
// Job definitions are read from the config snapshot captured at construction;
// edits to jobs.yaml are picked up on the next restart (the config watcher
// already suggests one), matching how every other config value is applied.
func (a *Gateway) startScheduler(ctx context.Context) func() {
	if a.runJob == nil || len(a.scheduledJobs) == 0 {
		return func() {}
	}

	scheduler, err := gocron.NewScheduler()
	if err != nil {
		a.logger.Error("job scheduler disabled: failed to create scheduler", "error", err)
		return func() {}
	}

	registered := 0
	for name, job := range a.scheduledJobs {
		if job.ScheduleKind() == config.ScheduleManual {
			continue
		}
		// Agent-defined jobs (Confirmed non-nil false) are still scheduled, but
		// runScheduledJob asks the admin to approve the FIRST run before executing
		// (the command can't run headless and ungated without a human OK).
		definition, err := scheduleDefinition(job)
		if err != nil {
			a.logger.Error("skipping scheduled job: invalid schedule", "job", name, "error", err)
			continue
		}
		taskName := name
		// LimitModeReschedule drops a trigger that fires while the previous
		// run of the same job is still in flight, rather than running two
		// copies concurrently or queueing a backlog.
		if _, err := scheduler.NewJob(
			definition,
			gocron.NewTask(func() { a.runScheduledJob(ctx, taskName) }),
			gocron.WithName(taskName),
			gocron.WithSingletonMode(gocron.LimitModeReschedule),
		); err != nil {
			a.logger.Error("skipping scheduled job: failed to register", "job", name, "error", err)
			continue
		}
		registered++
		a.logger.Info("scheduled job registered", "job", name, "schedule", scheduleSummary(job))
	}

	if registered == 0 {
		// Nothing was registered (every candidate failed). Shut the empty
		// scheduler down so we don't leak its goroutine.
		if err := scheduler.Shutdown(); err != nil {
			a.logger.Error("job scheduler shutdown failed", "error", err)
		}
		return func() {}
	}

	scheduler.Start()
	a.logger.Info("job scheduler started", "jobs", registered)
	return func() {
		if err := scheduler.Shutdown(); err != nil {
			a.logger.Error("job scheduler shutdown failed", "error", err)
		}
	}
}

// runScheduledJob executes one scheduled job and logs the outcome. Errors are
// logged, never propagated: a failing scheduled run must not take down the
// daemon, and the next trigger fires on schedule regardless.
func (a *Gateway) runScheduledJob(ctx context.Context, name string) {
	// A held agent-defined job needs the admin to approve its first run this
	// process before it executes. Confirmed and hand-written jobs run straight
	// away. gocron's singleton mode drops triggers that fire while we are still
	// blocked awaiting the decision, so there is at most one prompt per job.
	if job, ok := a.scheduledJobs[name]; ok && job.AwaitingConfirmation() && !a.isJobConfirmed(name) {
		if !a.confirmHeldJob(ctx, name, job) {
			return
		}
	}
	a.logger.Info("running scheduled job", "job", name)
	if err := a.runJob(ctx, name); err != nil {
		a.logger.Error("scheduled job failed", "job", name, "error", err)
		return
	}
	a.logger.Info("scheduled job completed", "job", name)
}

// isJobConfirmed reports whether a held job has been approved for its first run
// during this daemon lifetime. Confirmation is session-scoped — it is not
// written back to jobs.yaml, so a restart re-asks (a deliberately safe default;
// persisting it is a follow-up).
func (a *Gateway) isJobConfirmed(name string) bool {
	a.confirmedJobsMu.Lock()
	defer a.confirmedJobsMu.Unlock()
	return a.confirmedJobs[name]
}

func (a *Gateway) markJobConfirmed(name string) {
	a.confirmedJobsMu.Lock()
	defer a.confirmedJobsMu.Unlock()
	if a.confirmedJobs == nil {
		a.confirmedJobs = make(map[string]bool)
	}
	a.confirmedJobs[name] = true
}

// confirmHeldJob asks the admin (in their DM) to approve a held job's first run,
// showing the actual command and schedule, and blocks until they answer. It
// returns true only on approval, recording the confirmation for this process so
// later triggers run without asking again. With no broker or no admin DM
// available the job is not run (and is re-asked on the next trigger).
func (a *Gateway) confirmHeldJob(ctx context.Context, name string, job config.JobProfile) bool {
	if a.interactions == nil {
		a.logger.Info("held scheduled job cannot be confirmed: no interaction broker; not run", "job", name)
		return false
	}
	dest, err := a.resolveSuggestionDestination(ctx, "")
	if err != nil || dest == "" {
		a.logger.Warn("held scheduled job cannot be confirmed: no admin DM available; not run", "job", name, "error", err)
		return false
	}
	question := fmt.Sprintf("Scheduled job `%s` is about to run for the first time:\n```%s```\nSchedule: %s. Approve its first run?",
		name, jobCommandLine(job), scheduleSummary(job))
	decision, err := a.interactions.Ask(ctx, askbroker.Destination{ChannelID: dest}, askbroker.PromptSpec{
		Title:    ":alarm_clock: First run of a scheduled job",
		Question: question,
		Options: []askbroker.Option{
			{ID: "approve", Label: "Approve", Style: "primary"},
			{ID: "deny", Label: "Deny", Style: "danger"},
		},
	})
	if err != nil {
		a.logger.Warn("held scheduled job confirmation failed; not run", "job", name, "error", err)
		return false
	}
	if decision.OptionID == "approve" {
		a.markJobConfirmed(name)
		a.logger.Info("scheduled job approved for first run", "job", name, "user", decision.UserID)
		return true
	}
	a.logger.Info("scheduled job first run not approved; will ask again on next trigger", "job", name)
	return false
}

// jobCommandLine renders a job's command and args for the confirmation prompt.
func jobCommandLine(job config.JobProfile) string {
	if len(job.Args) == 0 {
		return job.Command
	}
	return job.Command + " " + strings.Join(job.Args, " ")
}

// scheduleDefinition maps a job profile onto the gocron job definition for
// its schedule kind. Manual jobs have no definition and must be filtered out
// by the caller before this is reached.
func scheduleDefinition(job config.JobProfile) (gocron.JobDefinition, error) {
	switch job.ScheduleKind() {
	case config.ScheduleCron:
		// withSeconds=false → standard 5-field cron syntax. gocron returns a
		// parse error from NewJob if the expression is malformed.
		return gocron.CronJob(strings.TrimSpace(job.Schedule), false), nil
	case config.ScheduleEvery:
		d, err := time.ParseDuration(strings.TrimSpace(job.Every))
		if err != nil {
			return nil, fmt.Errorf("every %q is not a valid duration: %w", job.Every, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("every %q must be greater than zero", job.Every)
		}
		return gocron.DurationJob(d), nil
	default:
		return nil, fmt.Errorf("job has no schedule")
	}
}

// scheduleSummary renders a short human-facing description of a job's trigger
// for log lines.
func scheduleSummary(job config.JobProfile) string {
	switch job.ScheduleKind() {
	case config.ScheduleCron:
		return "cron " + strings.TrimSpace(job.Schedule)
	case config.ScheduleEvery:
		return "every " + strings.TrimSpace(job.Every)
	default:
		return "manual"
	}
}
