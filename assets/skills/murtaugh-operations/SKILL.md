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
