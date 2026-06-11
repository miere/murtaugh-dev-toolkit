# Authorization & troubleshooting

## Who can interact

Two settings in `slack.yaml` `configuration:`:

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
| No startup DM | `admin_user` unset or unresolvable; or the daemon didn't start — check `slack.err.log`. |
| Bot ignores my DM / @-mention | Your user isn't in `admin_user`/`allowed_users` (mentions/DMs fail **silently**); or ACP chat is disabled (`acp.enabled: false`, no `chat.default_agent`). See `murtaugh-agents`. |
| "you are not authorized" on a slash command | Same allowlist issue, surfaced because slash commands deny loudly. |
| Config edit had no effect | Config loads once — **restart** to apply. See `reference/config-and-restart.md`. |
| Link previews don't appear | The domain isn't in the Slack app's **App Unfurl Domains** (no `link_shared` is delivered), or no matching `unfurl-rules`. See `murtaugh-unfurl`. |
| A scheduled job didn't run | The gateway was down at fire time (no catch-up), or the schedule edit needs a restart. See `murtaugh-jobs`. |
| A message handled twice | Not redelivery (that's de-duped) — check you don't have **two daemons** running. |
| Need to see what happened | `~/Library/Logs/murtaugh/slack.out.log` and `…/slack.err.log` (launchd). |

## Logs

Under launchd the daemon's stdout/stderr go to
`~/Library/Logs/murtaugh/slack.out.log` and `~/Library/Logs/murtaugh/slack.err.log`.
Running the gateway in a terminal instead sends the same logs to that terminal.
They record startup, allowlist resolution, agent warmup verdicts, event
handling, unfurl/job outcomes, and errors — the first place to look for anything
in this table.
