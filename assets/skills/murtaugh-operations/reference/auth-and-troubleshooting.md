# Authorization & troubleshooting

## Who can interact

Two settings in `gateway.yaml` `access:`:

- **`admin_user`** — a single user (handle or ID). Can do everything, including
  **restart**.
- **`allowed_users`** — a list (handles or IDs). Can use slash commands, send
  mentions/DMs, and click buttons.

At startup both are resolved from handles to Slack user IDs (fail-closed: an
unresolvable entry aborts startup). After that, checks are ID-only:

- **`allowed`** = matches `admin_user` (when it's an ID) or any `allowed_users`.
- **`admin`** = matches the resolved `admin_user` only.

**Both empty → the bot is locked down** (no one can interact); a warning is
logged at startup.

### How unauthorized requests fail

- **Slash command** from a non-allowed user → ephemeral *"you are not
  authorized"*.
- **Mention / DM** from a non-allowed user → **silently ignored** (no reply — by
  design, to avoid noise).
- **Restart** from a non-admin allowed user → ephemeral/edited *"only the admin
  can restart"*.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| No startup DM | `access.admin_user` unset or unresolvable; or the daemon didn't start — check `slack.err.log`. |
| Bot ignores my DM / @-mention | Your user isn't in `access.admin_user`/`access.allowed_users` (mentions/DMs fail **silently**); or the chat surface is disabled (`chat.enabled: false`, no `chat.defaults.agent`). See `murtaugh-agents`. |
| "you are not authorized" on a slash command | Same allowlist issue, surfaced because slash commands deny loudly. |
| Config edit had no effect | Config loads once — **restart** to apply. See `reference/config-and-restart.md`. |
| Link previews don't appear | The domain isn't in the Slack app's **App Unfurl Domains** (no `link_shared` is delivered), or no matching `unfurl-rules`. See the `murtaugh-slack` skill's `unfurl.md`. |
| A scheduled job didn't run | The gateway was down at fire time (no catch-up), or the schedule edit needs a restart. See `murtaugh-jobs`. |
| A message handled twice | Not redelivery (that's de-duped) — check you don't have **two daemons** running. |
| A chat turn "hangs" with no reply | It may be **legitimately waiting on a human**, not stuck — see *A turn that's waiting, not hung* below. |
| Need to see what happened | `~/Library/Logs/murtaugh/slack.out.log` and `…/slack.err.log` (launchd). |

### A turn that's waiting, not hung

A native chat turn can now legitimately **block waiting on a person** — this is by
design, not a hang, and the turn stays alive (a keep-alive heartbeat keeps the
idle watchdog from cancelling it). Before treating a quiet turn as stuck, look in
the **Slack thread** for a pending prompt the agent posted and is waiting on:

- A **terminal** Approve / Deny — an operational `terminal` command an agent ran
  in live chat is approval-gated; the command doesn't run until someone clicks.
  Deny skips it with a note to the model — the turn still completes normally.
- An **`ask`** question or a **`present_plan`** Proceed / Revise / Cancel prompt —
  the agent is waiting for the answer / sign-off and will not assume one.
- A **held job's first-run confirmation** — an agent-defined job awaiting its
  first-run go-ahead asks before it runs.

Answer (or Approve/Deny) in Slack and the turn proceeds. If the prompt was missed
and timed out, the agent is told no answer came — it won't proceed on a guess.

## Logs

Under launchd the daemon's stdout/stderr go to
`~/Library/Logs/murtaugh/slack.out.log` and `~/Library/Logs/murtaugh/slack.err.log`.
Running the gateway in a terminal instead sends the same logs to that terminal.
They record startup, allowlist resolution, agent warmup verdicts, event
handling, unfurl/job outcomes, and errors — the first place to look for anything
in this table.
