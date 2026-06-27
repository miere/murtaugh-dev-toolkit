# Operations

Running and debugging the `slack gateway` daemon — the long-lived Socket Mode
process that handles every Slack event (slash commands, button clicks, mentions,
DMs, link previews) and runs scheduled jobs. For *installing* the daemon see
[Getting started](getting-started.md); this page is about what it does once it's
running and how to keep it healthy.

---

## Running the gateway

```sh
murtaugh slack gateway
```

The gateway connects to Slack over Socket Mode and stays up. At startup it warms
up the configured agents, sends the **ping card** to the admin DM, and starts the
job scheduler.

### As a daemon (macOS)

The macOS installer can create `~/Library/LaunchAgents/dev.murtaugh.plist` (via
`murtaugh setup launchd`) so the gateway starts automatically on login and
restarts on crash. Under launchd it logs to:

- **`~/Library/Logs/murtaugh/slack.out.log`** — stdout
- **`~/Library/Logs/murtaugh/slack.err.log`** — stderr

**Start any debugging in those logs** — startup, agent warmup, event handling,
job runs, and errors all land there.

On other platforms, run `murtaugh slack gateway` under your own supervisor
(systemd, a process manager, etc.).

---

## Applying config changes

The gateway loads config **once at startup** — it never hot-reloads. When a
config file changes, the running daemon *suggests* a restart (an admin-only
button) but applies nothing until you restart.

**Restart** is admin-only — `/murtaugh restart`, or the suggestion button. It
preserves a "restarting… / back online" notice across the restart so users aren't
left wondering. Schedule edits, agent changes, access-list changes, and journal
settings all take effect on the next restart.

---

## A quiet turn may be waiting, not hung

Some chat turns now wait on a human before continuing:

- a native agent's `terminal` command can be **approval-gated** (see
  [Agent chat → The approval gate](agents.md#the-approval-gate));
- `ask` and `present_plan` **block** on your Approve/Deny or your answer in Slack;
- a held job's first scheduled run blocks on admin confirmation (see
  [Jobs → Trusted vs held jobs](jobs.md#trusted-vs-held-jobs)).

If a turn has gone quiet, check whether there's a card waiting for your click
before assuming it's stuck.

---

## Troubleshooting

### Access is fail-closed

Only `admin_user` plus everyone in `access.allowed_users` may interact. With
`allowed_users` empty, the bot is **admin-only** — so *"the bot ignores me"* is
most often an access-list problem, not a bug. Handles in the access lists are
resolved to IDs at startup, and **the gateway refuses to start if any entry can't
be resolved** — check the startup log for a resolution error.

### "The bot ignores me" checklist

1. Are you the admin, or in `access.allowed_users`?
2. In a channel, did you `@mention` the bot (unless you're in `chat.no_mention`)?
3. Is `chat.enabled: true` and `chat.default_agent` a real agent in
   `agents.yaml`?
4. Did the gateway actually start? Check `slack.err.log` for an auth or
   config-validation failure.

### Query the journal

For *"why did this workflow / unfurl / job misbehave?"*, don't grep logs — query
the structured [event journal](journal.md). It records each interaction with a
correlation id so you can replay one click end to end. The `connection` events on
the `gateway` stream are where to look for *"why did the daemon go silent?"*.

### Ship a diagnostics bundle

```sh
murtaugh slack send-msg ...        # if Slack itself works
/murtaugh troubleshoot             # from Slack: bundles the .yaml config files
```

`/murtaugh troubleshoot` collects your `.yaml` config files into an uploadable
bundle. It deliberately **never** includes `.env`, so secrets don't leak — which
is exactly why secrets must live only there (see [Configuration](configuration.md)).

---

## What the daemon owns

One process runs it all:

- the **Slack event loop** (slash commands, mentions, DMs, buttons, links);
- the **chat agents** and their streaming replies ([Agent chat](agents.md));
- the **workflow** and **unfurl** handlers ([Slack](slack.md));
- the **job scheduler** ([Jobs](jobs.md));
- the **event journal** writer ([Gateway Debug Mode](journal.md)).

If the gateway is down, scheduled jobs don't fire and Slack events go unanswered
— everything flows through it.
