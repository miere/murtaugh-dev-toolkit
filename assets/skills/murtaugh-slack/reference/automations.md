# Automations: scheduled scripts

Conventions for the scripts that drive Murtaugh on a schedule — the clock-tick
jobs that mirror state into Slack. These conventions are Slack-flavoured here
because that's the common case, but the layout applies to any Murtaugh automation.

## Defaults

- **Language & location:** unless the user says otherwise, write a **Python**
  script in **`./automations/`**. Overwrite an existing script that serves the
  same purpose rather than adding a parallel one.
- **Top-of-file docstring:** state what it does, how it's triggered, required env
  vars, and how to run it by hand.
- **Secrets via environment variables** only — never hardcoded. Document each one.
- **Pin dependencies** (requirements file or inline metadata) so a scheduled run
  is reproducible.

## Suggested layout

```
automations/
  <name>.py              # the script (entry point)
  state/
    <name>.json          # persisted state between runs (see below)
```

## Triggering: the clock-tick model

Automations are designed to be **stateless per run** and fired on a schedule by an
external trigger. Murtaugh can register and run jobs (see `jobs.yaml`):

```yaml
# jobs.yaml
jobs:
  <name>:
    command: /usr/bin/python3
    args: ["/Users/<you>/.config/murtaugh/automations/<name>.py"]
    # workdir: /Users/<you>/.config/murtaugh
    # timeout: 5m
```

Define/run jobs with `murtaugh jobs define …` / `murtaugh jobs run --name <name>`
(also exposed as MCP tools `jobs_define` / `jobs_run`). Murtaugh schedules jobs
itself: add `schedule:` (cron) or `every:` (interval) to the job and the
gateway runs it automatically — see the **`murtaugh-jobs`** skill for the full
configuration. Whatever the cadence, make every run a full reconcile so a
skipped or doubled tick is harmless.

## State between runs

Because each run is independent, anything that must survive (the
message-`ts`-per-entity map, one-shot flags like "already tagged") lives in a JSON
file under `automations/state/`.

- Key by a stable entity id (e.g. `repo#number`).
- Store at least `{ ts, last_state }` per entity, plus any one-shot flags.
- **Missing or corrupt file → start fresh**, don't crash.
- Write atomically (temp file + rename) so a mid-write crash can't corrupt it.

```json
{
  "UpsideRealty/upside#19420": { "ts": "1718000000.123456", "last_state": "checks_processing", "tagged": false }
}
```

## Reconcile, idempotently

Each run: list current entities → derive each one's state → post-or-update its
message → fire one-shot follow-ups → save state. Running twice back-to-back must
change nothing the second time. The full loop and the post-vs-update logic are in
`outbound.md`.

## Operational hygiene

- Log per-entity outcomes; exit non-zero only if **every** entity failed (so one
  bad item doesn't mark the whole tick as failed).
- Keep the derive-state step a **pure function** of the entity's data — easy to
  test, and it's what makes "update only when the state actually changed" correct.
