---
name: murtaugh-jobs
description: Defines, runs, and schedules Murtaugh jobs — named units of work in `jobs.yaml` that execute either a shell command (args, workdir, timeout) or an agent (`agent` + `prompt`), with one of three trigger modes (manual, `schedule:` cron, or `every:` interval). Use when creating or editing `jobs.yaml`, wiring the `jobs_run`/`jobs_define` CLI or MCP tools, or setting up backups, syncs, reconcile scripts, or agent-delegated chores that Murtaugh runs on demand or automatically. Use when choosing a cron/interval value, configuring the `slack gateway` daemon that owns the scheduler, or running `murtaugh jobs run`/`murtaugh jobs define`.
---

# Skill: Murtaugh Jobs

A **job** is a named unit of work defined in `jobs.yaml`. It runs **either** a
shell command (with args, working directory, and timeout) **or** an agent
(`agent` + `prompt`, fire-and-forget) — the two are mutually exclusive. Jobs run
**on demand** (CLI, MCP, or a workflow trigger) and can additionally run
**automatically** on a schedule. Use this whenever a task involves defining,
running, or scheduling work that Murtaugh executes — backups, syncs, reconcile
scripts, clock-tick automations, or agent-delegated chores.

## The three trigger modes (at a glance)

Every job has exactly one trigger mode, decided by two optional, **mutually
exclusive** fields:

| Mode | Fields set | Runs when |
|---|---|---|
| **manual** | neither | only when you invoke it (`jobs_run`, MCP, or a workflow) |
| **cron** | `schedule:` | automatically, on a 5-field cron expression (e.g. `"0 2 * * *"`) |
| **interval** | `every:` | automatically, on a fixed interval (e.g. `"1h"`, `"30m"`) |

Scheduled modes (`schedule`/`every`) only fire while the **`slack gateway`
daemon** is running — it owns the in-process scheduler.

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Defining a job's command / agent+prompt / args / workdir / timeout | `reference/configuring.md` |
| Choosing or writing a `schedule` / `every` value | `reference/scheduling.md` |
| Running a job by hand or wiring `jobs_run` / `jobs_define` | `reference/running.md` |
| Wanting a working `jobs.yaml` | `examples/jobs.yaml` |

## Global guidelines (defaults — follow unless the user says otherwise)

- **Read `jobs.yaml` first.** It is the source of truth for existing job names;
  reuse / overwrite a job that serves the same purpose rather than adding a
  parallel one.
- **One trigger mode per job.** Never set both `schedule` and `every` — Murtaugh
  rejects that at validation time. Leave both unset for a manual-only job.
- **Schedule edits apply on the next gateway restart**, not live. After editing
  `jobs.yaml`, restart the gateway (the config watcher already suggests it).
- **`command` should be an absolute path** (or a binary on `PATH`); a relative
  `command` resolves against the job's `workdir`, which defaults to the
  workspace (`~/.config/murtaugh`).
- **Scheduled runs are best-effort.** A run that would fire while the gateway is
  down is **skipped, not caught up** (see `reference/scheduling.md`). Don't rely
  on a scheduled job for must-not-miss accounting without external safeguards.
- **Ask the binary for exact flags.** `murtaugh help jobs run` /
  `murtaugh help jobs define` (or `murtaugh jobs <run|define> --help`) print the
  full flag reference — which are required, the repeatable `--args` form, and
  the `--timeout`/`--schedule`/`--every` value formats.
