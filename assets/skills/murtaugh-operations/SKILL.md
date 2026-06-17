---
name: murtaugh-operations
description: Operator guide for running and diagnosing the long-lived `murtaugh slack gateway` Socket Mode daemon that handles all Slack events (slash commands, button clicks, mentions, DMs, link previews) and runs scheduled jobs. Use when debugging or operating a running gateway — inspecting its launchd stdout/stderr logs at `~/Library/Logs/murtaugh/slack.out.log` and `slack.err.log`, tracing startup/warmup/scheduler lifecycle, applying config changes or doing a graceful restart (`/murtaugh restart`), or troubleshooting auth issues like the bot ignoring users (`admin_user`/`allowed_users` fail-closed authorization). Concerns the `murtaugh --config PATH slack gateway` command and its `reference/lifecycle.md`, `reference/config-and-restart.md`, and `reference/auth-and-troubleshooting.md` files; for installing the daemon use murtaugh-setup instead.
---

# Skill: Murtaugh Gateway Operations

Running and debugging the `slack gateway` daemon — the long-lived Socket Mode
process that handles every Slack event (slash commands, button clicks, mentions,
DMs, link previews) and runs scheduled jobs. This is **operator-facing**: for
*installing* the daemon see `murtaugh-setup`; this skill is about what it does
once it's running and how to diagnose it.

## What the daemon is

`murtaugh slack gateway` connects to Slack over Socket Mode and stays up. Under
launchd (macOS) it auto-restarts on crash and logs to:

- **`~/Library/Logs/murtaugh/slack.out.log`** (stdout)
- **`~/Library/Logs/murtaugh/slack.err.log`** (stderr)

**Start your debugging in those logs** — startup, agent warmup, event handling,
job runs, and errors are all there.

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Tracing what happens at startup (warmup, ping, scheduler) | `reference/lifecycle.md` |
| Applying config changes or doing a graceful restart | `reference/config-and-restart.md` |
| Diagnosing auth/"bot ignores me", or anything not working | `reference/auth-and-troubleshooting.md` |

## Key operational facts

- **Config changes need a restart.** The gateway loads config once at startup; it
  *suggests* a restart when a config file changes but never hot-reloads. →
  `reference/config-and-restart.md`
- **Authorization is fail-closed.** Only `admin_user` + `allowed_users` may
  interact; with both empty the bot is locked down. →
  `reference/auth-and-troubleshooting.md`
- **Restart is admin-only** (`/murtaugh restart` or the suggestion button) and
  preserves a "restarting… / back online" notice across the restart.
- The daemon also runs the **job scheduler** (see the `murtaugh-jobs` skill) and
  the **chat** and **unfurl** handlers (see `murtaugh-agents`, `murtaugh-unfurl`).
- **The daemon takes no tool flags** — only the global `--config PATH`
  (`murtaugh --config /path/slack.yaml slack gateway`). Run
  `murtaugh help slack gateway` for the full reference, or `murtaugh help` to
  list every command.
