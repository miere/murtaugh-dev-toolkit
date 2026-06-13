# Skill: Debugging Murtaugh with the Event Journal

Murtaugh records what happened — gateway interactions, workflow rules, link
unfurls, and job runs — as **structured events** in a queryable journal (a local
SQLite database). Use this whenever someone asks *"why did my workflow button do
nothing?"*, *"why is this unfurl wrong?"*, or *"did that job run?"*. You answer
by **querying the journal**, not by reading log files.

This is the engine behind **Gateway Debug Mode (GDM)**. The journal is separate
from Murtaugh's ordinary logs (which go to stderr / the launchd log files): logs
are for a human tailing the daemon; the journal is for *you* to filter and
reason over.

## The tools

| Tool | Use it to… |
|------|-----------|
| `journal.query` | Pull events with filters (the workhorse). |
| `journal.stats` | See per-stream row counts and time span — confirm a stream is recording. |
| `journal.prune` | Delete events past their retention (rarely needed; the daemon sweeps automatically). |

Over MCP the names are dotted (`journal.query`); on the CLI they are spaced
(`murtaugh journal query …`).

## Streams (what's recorded where)

- **`gateway`** — everything from a Slack interaction: slash commands, button
  clicks / form submissions, workflow rules, and link unfurls. This is the GDM
  stream.
- **`job`** — `jobs.run` executions (command or agent), with exit code/duration.
- **`acp_session`** — chat session events (when enabled).

## The debugging move (mental model)

1. **Scope by what the user gave you.** A channel? `--channel C…`. A time? a
   recent window with `--since`. Errors only? `--level error`.
2. **Find the failure**, then **follow its `corr_id`.** Every event from one
   interaction shares a correlation id. Once you have a failing event, re-query
   with `--corr-id <that id>` to get the *whole* story of that one click —
   ingress → rule match → each trigger → outcome.
3. **Read the `payload`** of the failing event: it carries the template path, the
   render error, the non-JSON agent output, the command error, etc.

Worked example — "I clicked Approve in #reviews and nothing happened":

```
# 1. Recent failures in that channel
murtaugh journal query --stream gateway --channel C0REVIEWS --since 1h --level warn

#    → a workflow.trigger error, corr_id gw_3f9c…, rule "code-review-approval"

# 2. The whole interaction
murtaugh journal query --corr-id gw_3f9c2b1a…
#    → interactive.received → workflow.matched (rule) → workflow.trigger (error:
#      "render Slack response: template execute … map has no entry for key …")
```

If `journal.stats` shows the `gateway` stream at **0 rows**, recording is off —
check `journal.yaml` (`streams.gateway.enabled`) and that the daemon restarted.

## Useful filters (all optional, ANDed)

`--stream` `--kind` `--level` (at-least severity) `--channel` `--user`
`--session` `--corr-id` `--rule` `--since` `--until` `--limit`.

`--since`/`--until` take a Go duration ago (`2h`, `30m`) **or** an RFC3339
timestamp. Results are most-recent-first; default limit 50, capped at 500.

## Read the right file (don't load everything)

| When you're… | Read |
|--------------|------|
| Figuring out which event kind means what (gateway/job catalog) | `reference/event-kinds.md` |
