# Gateway lifecycle

What `murtaugh slack gateway` does, in order, when it starts:

1. **Resolve the allowlist.** `admin_user` and `allowed_users` (handles or IDs)
   are resolved to Slack user IDs up front, with one `users.list` call if any
   entry is a handle. Unresolvable entries are **fatal** (fail-closed). If both
   lists are empty, it logs a warning and runs locked down. →
   `reference/auth-and-troubleshooting.md`
2. **Connect** to Slack over Socket Mode (in the background).
3. **Warm the agents.** Each ACP agent is probed for session/cancel support
   (bounded by `startup_timeout`); the verdict is logged. A failed warmup is
   logged, not fatal. (See the `murtaugh-agents` skill.)
4. **Start the config watcher** (if config-watch paths are set) — polls
   `slack.yaml`, `agents.yaml`, `jobs.yaml`. → `reference/config-and-restart.md`
5. **Start the job scheduler** — registers cron/`every` jobs from `jobs.yaml`
   (manual jobs are ignored here). (See the `murtaugh-jobs` skill.)
6. **Run the event loop** — dispatch slash commands, interactions, mentions,
   DMs, and `link_shared` until shutdown.

## Startup ping

Once connected, the gateway greets the admin **once per process** — and exactly
one of two things happens:

- **Fresh boot:** it DMs the admin a **":zap: The server has started."** card
  with a **Test communication** button. The card is built in Go
  (`internal/slack/pingcard`), and clicking the button is answered by the binary
  itself (`:recycle: …functional.`) — no workflow rule or template involved, so
  the self-test can't be broken by config edits.
- **Returning from a restart:** the startup ping is suppressed; instead the
  pending "restarting…" notice is edited in place into a **":white_check_mark:
  Murtaugh is back online."** card carrying the same Test communication button
  (see `config-and-restart.md`).

A reconnect won't repeat the greeting. Seeing it is the quickest confirmation the
daemon is up and the admin user resolved correctly. If it never arrives, check
that `admin_user` is set and resolvable.

## Event deduplication

Slack's Events API delivers **at least once**, so a mention or DM can arrive
twice (e.g. after a reconnect). The gateway suppresses duplicates by
`teamID|channelID|messageTS` for ~15 minutes, so a redelivery doesn't spawn a
second chat that interrupts the first. If you see a single message handled twice,
suspect something *other* than redelivery (e.g. two running daemons).
