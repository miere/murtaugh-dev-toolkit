# Configuring a job

Jobs live under the `jobs:` map in `jobs.yaml`, keyed by name. A job runs
**either** a command **or** an agent — the two are mutually exclusive.

```yaml
jobs:
  cleanup-logs:                      # command job
    command: /usr/bin/find          # absolute path or PATH binary
    args: ["/var/log", "-mtime", "+7", "-delete"]
    workdir: /tmp                    # optional — defaults to the workspace
    timeout: 5m                      # optional — Go duration, defaults to 10m
    # schedule: "0 3 * * *"          # optional — cron (see scheduling.md)
    # every: 1h                      # optional — interval (mutually exclusive)

  code-review-job:                   # agent-delegated job
    agent: default                  # an agent keyed in agents.yaml
    prompt: |
      Review the changes in PR {{ 1 }} at {{ 2 }} and post your feedback.
```

## Fields

| Field | Required | Meaning |
|---|---|---|
| `command` | one of | The executable. Absolute path, or a name resolved on `PATH`. A relative path resolves against `workdir`. Mutually exclusive with `agent`/`prompt`. |
| `agent` | one of | Name of an agent from `agents.yaml`. Runs it in an isolated one-shot session instead of a command. Requires `prompt`; mutually exclusive with `command`. |
| `prompt` | with `agent` | The agent prompt. Supports positional placeholders `{{ 1 }}`, `{{ 2 }}`, … (1-based) that expand to the run-time args (falling back to the job's `args`). |
| `args` | no | For a command job, positional process arguments (verbatim, no shell splitting). For an agent job, the default values for the prompt's `{{ N }}` placeholders when no run-time args are passed. |
| `workdir` | no | Working directory for the process. Defaults to the **workspace** (the config dir, e.g. `~/.config/murtaugh`). |
| `timeout` | no | A Go duration (`30s`, `5m`, `2h`). The run is killed if it exceeds this. Defaults to **10m**. |
| `schedule` | no | Cron expression for automatic runs. Mutually exclusive with `every`. → `scheduling.md` |
| `every` | no | Interval duration for automatic runs. Mutually exclusive with `schedule`. → `scheduling.md` |

## Agent jobs

An agent job is **fire-and-forget**: Murtaugh starts the agent, sends the
rendered prompt, and discards the agent's text output — the agent does its work
through its own tools/MCP (it might open a PR, post to Slack, etc.). Pass
positional args at run time to fill the prompt placeholders:

```sh
murtaugh jobs run --name code-review-job --args 1234 --args /path/to/repo
```

Here `{{ 1 }}` becomes `1234` and `{{ 2 }}` becomes `/path/to/repo`. For a
**scheduled** agent job (no run-time args), bake the values into the job's
`args` list so the placeholders still resolve.

## No shell interpretation

`args` are passed straight to the process, not through a shell. Pipes,
redirects, globbing, and `$VAR` expansion do **not** happen. If you need them,
make `command` a shell explicitly:

```yaml
  piped-report:
    command: /bin/sh
    args: ["-c", "generate | tee $HOME/report.txt"]
```

## Validation

`Validate()` rejects a job when:

- neither `command` nor `agent`+`prompt` is set (a job needs one or the other).
- both `command` and `agent`/`prompt` are set (they are mutually exclusive).
- `agent` is set without `prompt` (or vice versa).
- `agent` names an agent that is not defined in `agents.yaml`.
- `timeout` is set but not a valid Go duration.
- `every` is set but not a valid, positive Go duration.
- both `schedule` and `every` are set.

A bad `schedule` (malformed cron) is not caught at config load; instead the
gateway logs it and skips that one job at startup — see `scheduling.md`.

## Defining jobs programmatically

You usually edit `jobs.yaml` by hand, but `jobs.define` (CLI / MCP) writes an
entry for you and preserves the others. See `running.md`.
