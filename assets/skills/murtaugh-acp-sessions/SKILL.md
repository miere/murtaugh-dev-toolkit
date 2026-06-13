# Skill: Reviewing ACP Chat Session Logs

Murtaugh persists its ACP chat conversations to the journal's **`acp_session`**
stream so a maintainer or curator can review **how users actually experience
Murtaugh** — what they ask, how the agent answers, and how turns end. Use this
when the task is auditing or curating real conversations (quality, UX, failure
patterns), as opposed to debugging a gateway interaction (that's the
`murtaugh-journal` / Gateway Debug Mode skill).

## How it's stored (two layers)

- **A journal row per turn** (`kind: session.turn`) — the queryable index:
  `session_id` + channel/user/thread keys, the `outcome`, and metrics (agent,
  source, duration, chunk/byte counts). **No message text in the row.**
- **A per-session transcript file** — the full prompt + response text, one JSON
  line per turn, under the journal's `blob_dir`. The row points at it via
  `blob_ref`. This is where you read what was actually said.

So: **query rows to find sessions, read the transcript file for content.**

## Finding and scoping sessions

```
# Recent chat turns across all conversations
murtaugh journal query --stream acp_session --since 24h

# One conversation's turns (session id comes from a row's keys)
murtaugh journal query --stream acp_session --session <session_id>

# A specific user or channel, failures only
murtaugh journal query --stream acp_session --user U123 --level warn
```

Each row's `blob_ref` is a path **relative to the journal `blob_dir`** (see
`journal.yaml`; default `~/.local/state/murtaugh/journal-blobs`). Read that file
to see the transcript — it is NDJSON, one object per turn:
`{ time, agent, source, outcome, prompt, response }`.

## Turn outcomes

| outcome | level | meaning |
|---------|-------|---------|
| `completed` | info | The agent finished the turn normally. |
| `interrupted` | info | A new message or `/stop` cut the turn short (not a failure). |
| `timed_out` | warn | The agent went silent past the idle timeout and was asked to stop. |
| `errored` | error | The agent or transport failed mid-turn. |

Filter for `--level warn` to surface the turns worth reviewing (timeouts +
errors); `--level error` for failures only.

## Retention & privacy

Transcripts contain **real user and agent message content**. The `acp_session`
stream is on by default with a 90-day retention (tunable in `journal.yaml`; set
`enabled: false` to stop recording). Pruning — the daemon's automatic sweep or
`journal prune` — deletes both the rows and the transcript files once a whole
session has aged out, so retention applies to the bodies too.
