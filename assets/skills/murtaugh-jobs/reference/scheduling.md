# Scheduling a job

A job runs automatically when it sets **one** of `schedule` (cron) or `every`
(interval). Neither set → manual-only. Both set → rejected at validation.

The scheduler lives **inside the `slack gateway` daemon** (an in-process
[`go-co-op/gocron`](https://github.com/go-co-op/gocron) v2 scheduler). Nothing
fires unless the gateway is running.

## `schedule:` — cron

Standard **5-field** cron: `minute hour day-of-month month day-of-week`.

```yaml
  nightly-backup:
    command: /usr/local/bin/backup.sh
    schedule: "0 2 * * *"      # 02:00 every day
```

Common patterns: `*/15 * * * *` (every 15 min), `0 9 * * 1-5` (09:00 weekdays),
`0 0 1 * *` (midnight on the 1st). Quote the value so YAML doesn't choke on `*`.

## `every:` — fixed interval

A Go duration, run repeatedly at that spacing:

```yaml
  hourly-sync:
    command: /usr/local/bin/sync.sh
    every: 1h                  # also: 30m, 90s, 2h30m
```

The first run happens one interval after the gateway starts (not immediately).

## How runs behave

- **Same execution path as manual.** A scheduled run goes through the same
  `jobs_run` machinery (`reference/running.md`) — same `timeout`, same
  `workdir`, same exit-code handling. Output streams to the daemon's
  stdout/stderr, which launchd captures into the Murtaugh log files.
- **No overlap.** Each job runs in singleton mode (`LimitModeReschedule`): if a
  run is still in flight when the next trigger fires, that trigger is
  **skipped**, not queued. A slow job sheds ticks instead of stacking up or
  running two copies at once.
- **Failures don't stop the schedule.** A non-zero exit (or error) is logged;
  the next trigger still fires on time.
- **Bad schedules degrade gracefully.** A malformed cron expression is logged at
  startup and that one job is skipped — the gateway and the other jobs still run.

## Two things to know

1. **Edits apply on restart.** Schedules are read from `jobs.yaml` once, at
   gateway startup. After editing, restart the gateway (the config watcher
   already prompts the admin to). There is no live reload.
2. **No missed-run catch-up.** If the gateway is down (or the host asleep) when a
   run was due, that run is simply **skipped** — gocron does not backfill. For
   must-not-miss work, add an external safeguard (e.g. a run-on-startup check, or
   host cron) rather than relying on the in-process scheduler alone.

## Choosing cron vs every

- Pick **`every`** for "every N minutes/hours" cadences with no wall-clock
  anchor — simplest to read.
- Pick **`schedule`** when the run must land at a specific clock time or weekday
  (02:00 daily, Monday mornings, the 1st of the month).
