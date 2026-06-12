# Running and defining jobs

Jobs are invoked the same way whether or not they're scheduled. The two tools
are exposed identically on the CLI and over MCP (when Murtaugh runs as
`murtaugh mcp`).

For the full per-command reference (every flag, required/optional, defaults),
run `murtaugh help jobs run` / `murtaugh help jobs define`, or
`murtaugh jobs <run|define> --help`.

## `jobs.run` — execute a job

```bash
murtaugh jobs run --name cleanup-logs

# Agent job: pass positional args for the prompt's {{ 1 }}, {{ 2 }}, … markers
murtaugh jobs run --name code-review-job --args 1234 --args /path/to/repo
```

`--name` is **required**. `--args` is optional and repeatable (once per value);
it fills an **agent** job's positional prompt placeholders and is ignored by
command jobs.

- Resolves the job by name from `jobs.yaml`.
- Applies the job's `timeout` (default 10m) as a hard deadline.
- **Command job:** runs `command` with `args` in `workdir`; streams the child's
  stdout/stderr to the caller (your terminal on the CLI; captured into the JSON
  result over MCP) and reports the **exit code**.
- **Agent job:** renders the prompt (positional `{{ N }}` from `--args`, falling
  back to the job's `args`) and runs the agent fire-and-forget — its text output
  is discarded; the agent acts through its own tools. The result reports the
  `agent` it ran instead of an exit code.

A non-zero exit is returned as the result's `exit_code` (it is not, by itself, a
tool error). A failure to *start* the process (missing binary, etc.) is an error.

## `jobs.define` — register / update a job

```bash
murtaugh jobs define --name hourly-sync \
  --command /usr/local/bin/sync.sh \
  --every 1h
```

- Reads `jobs.yaml`, adds or replaces the named entry, writes it back. Other
  jobs are preserved verbatim.
- **Required:** `--name` and `--command`.
- **Optional:** `--args` (repeatable — once per argument, e.g.
  `--args --full --args /data`), `--workdir`, `--timeout` (Go duration like
  `5m`), and `--schedule` (5-field cron) / `--every` (Go duration), which are
  **mutually exclusive**. Same validation as `configuring.md`.
- Does **not** run the job — only defines it.

```bash
murtaugh jobs define --name nightly-backup \
  --command /usr/local/bin/backup --args --full --args /data \
  --workdir /srv --timeout 30m --schedule "0 2 * * *"
```

## Who runs a job

| Caller | How |
|---|---|
| You, by hand | `murtaugh jobs run --name <n>` |
| An MCP client / agent | the `jobs.run` tool |
| A Slack workflow | a `run` trigger in `workflow-rules` (see the `murtaugh-slack` skill) |
| The scheduler | automatically, per `schedule` / `every` (see `scheduling.md`) |

All paths share one execution path, so a job behaves the same no matter who
fires it.

## Output and logs

For scheduled runs there is no terminal attached: stdout/stderr flow to the
gateway process, which launchd writes to the Murtaugh log files (e.g. under
`~/.local/share/murtaugh/`). The scheduler also logs a line when a job starts,
completes, or fails — grep those logs by job name to see history.
