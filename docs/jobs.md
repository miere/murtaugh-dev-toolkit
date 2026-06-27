# Jobs

A **job** is a named unit of work defined in `jobs.yaml`. It runs **either** a
shell command (with args, working directory, and timeout) **or** an agent
(`agent` + `prompt`, fire-and-forget) — the two are mutually exclusive. Jobs run
**on demand** (CLI, MCP, or a workflow trigger) and can additionally run
**automatically** on a schedule.

Use jobs for backups, syncs, reconcile scripts, clock-tick automations, or any
chore you want to delegate to an agent.

---

## Trigger modes

Every job has exactly one trigger mode, decided by two optional, mutually
exclusive fields:

| Mode | Fields set | Runs when |
|---|---|---|
| **manual** | neither | only when you invoke it (`jobs run`, MCP, or a workflow) |
| **cron** | `schedule:` | automatically, on a 5-field cron expression (`"0 2 * * *"`) |
| **interval** | `every:` | automatically, on a fixed interval (`"1h"`, `"30m"`) |

Scheduled modes only fire while the **`slack gateway`** daemon is running — it
owns the in-process scheduler. Setting both `schedule` and `every` is rejected at
validation time.

---

## Defining a job

```yaml
# ~/.config/murtaugh/jobs.yaml
jobs:
  # Command job, manual.
  example-job:
    command: /bin/echo
    args: ["hello from murtaugh"]
    # workdir: /path/to/working/directory
    # timeout: 5m

  # Command job, cron-scheduled (daily at 02:00).
  nightly-backup:
    command: /usr/local/bin/backup.sh
    schedule: "0 2 * * *"

  # Command job, interval-scheduled (hourly).
  hourly-sync:
    command: /usr/local/bin/sync.sh
    every: 1h

  # Agent-delegated job.
  code-review-job:
    agent: default                 # an agent name from agents.yaml
    prompt: |
      Review the code changes in this PR and provide feedback.
      - pr: {{ 1 }}
      - local repository: {{ 2 }}
```

- **`command`** should be an absolute path (or a binary on `PATH`); a relative
  command resolves against `workdir`, which defaults to the workspace
  (`~/.config/murtaugh`).
- An **agent job** starts the named agent in an isolated one-shot session and
  sends the rendered prompt; it is fire-and-forget — the agent acts through its
  own tools.
- Prompts (and command args) support **positional placeholders** `{{ 1 }}`,
  `{{ 2 }}`, … that expand to the args passed at run time. When no run-time args
  are given, the job's own `args` fill them — handy for scheduled agent jobs.

---

## Running a job

```sh
murtaugh jobs run --name nightly-backup

# Pass positional args (fill {{ 1 }}, {{ 2 }}, …):
murtaugh jobs run --name code-review-job --args 1234 --args /path/to/repo
```

Define a job from the CLI or an MCP client:

```sh
murtaugh jobs define \
  --name nightly-deploy \
  --command /usr/local/bin/deploy \
  --args --env --args production \
  --workdir /srv/deploy \
  --timeout 15m
```

Both `jobs run` and `jobs define` are also MCP tools (`jobs.run`, `jobs.define`).
Run `murtaugh help jobs run` / `murtaugh help jobs define` for the full flag
reference, including the repeatable `--args` form and the
`--timeout`/`--schedule`/`--every` value formats.

---

## Trusted vs held jobs

How a scheduled job's first run is treated depends on **who wrote it**:

- A job you **hand-write** in `jobs.yaml` (no `confirmed:` field) is **trusted**:
  a scheduled one auto-runs as soon as the gateway is up.
- A job created by the **`jobs.define` tool** is stamped `confirmed: false` and
  **held**: it is still scheduled, but on its **first trigger** the scheduler
  DMs the admin to approve that run before it executes.

This exists because a defined job's command runs headless and ungated — so an
agent can never define-then-auto-run a command without a human OK. `jobs.define`
also always prompts a human at definition time, showing the rendered command and
schedule.

---

## Things to know

- **Read `jobs.yaml` first.** It is the source of truth for existing job names;
  reuse or overwrite a job that serves the same purpose rather than adding a
  parallel one.
- **Schedule edits apply on the next gateway restart**, not live. After editing
  `jobs.yaml`, restart the gateway (the config watcher suggests it).
- **Scheduled runs are best-effort.** A run that would fire while the gateway is
  down is **skipped, not caught up**. Don't rely on a scheduled job for
  must-not-miss accounting without external safeguards.
- **Job runs are journaled.** Every execution lands on the `job`
  [journal](journal.md) stream with its exit code and duration.
