# Gateway Debug Mode (the event journal)

Murtaugh records what it does — gateway interactions, workflow rules, link
unfurls, job runs, and chat sessions — as **structured events** in a local
SQLite journal. So when something misbehaves you answer *"why?"* by **querying
the journal**, not by grepping log files.

This is the engine behind **Gateway Debug Mode (GDM)**. The journal is separate
from Murtaugh's ordinary logs (which still go to stderr / the launchd log
files): logs are for a human tailing the daemon; the journal is for *you* (or
the chatbot) to filter and reason over.

It is **on by default** — the events are already there when a user asks the bot
why something failed.

---

## Streams

Events are grouped into independent streams, each enabled and retained on its
own:

| Stream | Records | Default retention |
|---|---|---|
| `gateway` | Slack interactions: slash commands, button clicks / form submissions, workflow rules, link unfurls, **and socket-connection health** (connect / reconnect / stalled / heartbeat). This is the GDM stream. | 7 days |
| `job` | `jobs run` executions (command or agent), with exit code and duration. | 30 days |
| `acp_session` | Chat session turns — one `session.turn` row per turn, plus a full transcript under `blob_dir`. | 90 days |

The `connection` events on the `gateway` stream are the place to look for *"why
did the daemon go silent?"*.

---

## Querying

Three tools, spaced on the CLI (`murtaugh journal query …`) and dotted over MCP
(`journal.query`):

| Tool | Use it to… |
|---|---|
| `journal query` | Pull events with filters — the workhorse. |
| `journal stats` | See per-stream row counts and time span (confirm a stream is recording). |
| `journal prune` | Delete events past their retention (rarely needed; the daemon sweeps automatically). |

```sh
murtaugh journal query --stream gateway --channel C123 --since 1h --level error
murtaugh journal query --corr-id gw_3f9c2b1a       # the whole story of one interaction
murtaugh journal query --stream acp_session --session <id>
murtaugh journal stats                             # per-stream counts and time span
murtaugh journal prune                             # drop events past their retention
```

**Filters** (all optional, ANDed): `--stream` `--kind` `--level` (at-least
severity) `--channel` `--user` `--session` `--corr-id` `--rule` `--since`
`--until` `--limit`. `--since`/`--until` take a Go duration ago (`2h`, `30m`) or
an RFC3339 timestamp. Results are most-recent-first; default limit 50, capped at
500.

---

## The debugging move

Every event from one interaction shares a **correlation id**. The usual flow:

1. **Scope by what the user gave you** — a channel (`--channel C…`), a recent
   window (`--since 1h`), errors only (`--level error`).
2. **Find the failure, then follow its `corr_id`.** Re-query with
   `--corr-id <that id>` to get the *whole* story of that one click — ingress →
   rule match → each trigger → outcome.
3. **Read the `payload`** of the failing event: it carries the template path, the
   render error, the non-JSON agent output, the command error, etc.

Worked example — *"I clicked Approve in #reviews and nothing happened"*:

```sh
# 1. Recent failures in that channel
murtaugh journal query --stream gateway --channel C0REVIEWS --since 1h --level warn
#    → a workflow.trigger error, corr_id gw_3f9c…, rule "code-review-approval"

# 2. The whole interaction
murtaugh journal query --corr-id gw_3f9c2b1a
#    → interactive.received → workflow.matched → workflow.trigger (error:
#      "render Slack response: template execute … map has no entry for key …")
```

If `journal stats` shows the `gateway` stream at **0 rows**, recording is off —
check `journal.yaml` (`streams.gateway.enabled`) and that the daemon restarted.

---

## Chat session review

The `acp_session` stream is for **reviewing real conversations** (quality, UX,
failure patterns) — distinct from debugging a gateway interaction. Each turn is
one `session.turn` row (queryable like above) plus a full per-session transcript
written under `blob_dir` and referenced by the row's `blob_ref`:

```sh
murtaugh journal query --stream acp_session --session <id>
# then read the referenced transcript file for the message bodies
```

Pruning removes a transcript along with its rows.

---

## Configuration

Tune it in `journal.yaml` — per-stream `enabled` and `retention`, the database
`path`, the transcript `blob_dir`, and the sweep cadence:

```yaml
# ~/.config/murtaugh/journal.yaml
journal:
  # path: ~/.local/state/murtaugh/journal.db          # default
  # blob_dir: ~/.local/state/murtaugh/journal-blobs    # default beside the DB
  streams:
    gateway:     { enabled: true, retention: 168h }   # 7d
    job:         { enabled: true, retention: 720h }   # 30d
    acp_session: { enabled: true, retention: 2160h }  # 90d
  sweep:
    every: 24h                                        # also runs once at startup
```

An absent `journal.yaml` keeps every stream on with the defaults above. Changes
apply on the next gateway restart. The daemon prunes past-retention events
automatically (at startup and every `sweep.every`); `journal prune` is the manual
equivalent.
