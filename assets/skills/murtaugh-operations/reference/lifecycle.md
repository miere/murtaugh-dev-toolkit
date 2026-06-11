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

Once connected, the gateway DMs the admin a one-time **":zap: The server has
started."** notice (rendered from the embedded `templates/ping/01-ping.json`).
It fires once per process — a reconnect won't repeat it. Seeing this DM is the
quickest confirmation the daemon is up and the admin user resolved correctly. If
it never arrives, check that `admin_user` is set and resolvable.

## Event deduplication

Slack's Events API delivers **at least once**, so a mention or DM can arrive
twice (e.g. after a reconnect). The gateway suppresses duplicates by
`teamID|channelID|messageTS` for ~15 minutes, so a redelivery doesn't spawn a
second chat that interrupts the first. If you see a single message handled twice,
suspect something *other* than redelivery (e.g. two running daemons).
