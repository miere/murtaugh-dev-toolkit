# agents.yaml: ACP settings & agent profiles

`agents.yaml` has two sections: `acp` (global ACP behavior) and `agents` (the
agent processes Murtaugh can talk to).

```yaml
acp:
  enabled: true
  startup_timeout: 10s
  request_timeout: 10m
  session_idle_timeout: 30m
  max_sessions: 100
  stream_append_interval: 750ms
  stream_min_chunk_chars: 96
  cancel_grace_period: 2s

agents:
  default:
    command: /path/to/acp-agent
    args: [--stdio]
    workdir: /path/to/workspace
    # interruptible: false
```

## `acp` settings

Each field is a Go duration / int; the **effective default** below is what
applies when the field is omitted (the bootstrapped file ships tuned values).

| Field | Default if omitted | Controls |
|---|---|---|
| `enabled` | `false` | Master switch for DM/mention chat. Off → DMs and mentions are ignored. |
| `startup_timeout` | `10s` | Budget for the agent warmup probe at daemon start. |
| `request_timeout` | `10m` | Hard deadline for each chat response. |
| `session_idle_timeout` | `30m` | How long an idle ACP session is kept before teardown. |
| `max_sessions` | `100` | Concurrent session cap per agent. |
| `stream_append_interval` | `250ms` | How often buffered chunks are flushed to Slack. |
| `stream_min_chunk_chars` | `24` | Minimum characters before a chunk is flushed (avoids choppy edits). |
| `cancel_grace_period` | `2s` | After asking the agent to cancel, how long to let trailing chunks flush before hard-cancelling. |

## `agents` profiles

| Field | Required | Meaning |
|---|---|---|
| `command` | yes | Path to the ACP-speaking executable. |
| `args` | no | CLI args — commonly `[--stdio]`. |
| `workdir` | no | Working directory. Defaults to the **workspace** (`~/.config/murtaugh`) when unset. |
| `interruptible` | no | Override for session/cancel support (see below). |

### `interruptible` — the cancel capability

Controls whether a follow-up can interrupt an in-flight reply:

- **omitted (default)** — Murtaugh **probes** the agent at warmup for
  session/cancel support and logs the verdict. Use this unless you have a reason
  not to.
- **`false`** — the agent can't be interrupted; a follow-up that arrives mid-reply
  is **deferred** (it waits for the current reply to finish) rather than cutting
  it off with a misleading `_interrupted_`.
- **`true`** — force-enable and skip the probe.
