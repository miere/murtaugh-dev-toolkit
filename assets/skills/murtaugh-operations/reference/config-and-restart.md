# Config changes & graceful restart

## Config is loaded once

The gateway reads `slack.yaml`, `agents.yaml`, and `jobs.yaml` **at startup
only**. Editing them on disk changes nothing until the daemon restarts — this
applies to allowlists, chat routing, agents, workflow/unfurl rules, and job
schedules alike.

## The config watcher

When enabled, a watcher polls those three files every **5 seconds**. On a
detected change it DMs the admin a Block Kit **restart suggestion** naming the
changed file, with two buttons:

- **Restart now** (`murtaugh_restart_suggestion_confirm`)
- **Dismiss** (`murtaugh_restart_suggestion_dismiss`)

The watcher only *suggests*; it never restarts on its own. Dismiss edits the
message to a dismissed note; confirm goes through the same admin-gated restart
path as the slash command.

## Triggering a restart

Two ways, both **admin-only**:

- **`/murtaugh restart`** (slash command), or
- the **Restart now** button on a suggestion.

Guards:
- Requires `IsAdminUser` — a non-admin gets an ephemeral/edited "only the admin
  can restart" message.
- A **cool-down** prevents back-to-back restarts; a request during cool-down (or
  while one is already in flight) is declined with a "busy, try again" message.
- If no restart coordinator is wired, it reports the feature unavailable.

The restart itself is a clean process exit; the supervisor (launchd `KeepAlive`,
or your own) brings it back.

## The "restarting… / back online" notice

Across a restart the gateway preserves a **single** notice so the requester sees
it complete:

1. Before exiting it posts **":hourglass_flowing_sand: Restarting Murtaugh
   now…"** and writes a **resume marker** to disk —
   `$XDG_STATE_HOME/murtaugh/restart.json` (else `~/.local/state/murtaugh/restart.json`).
   When the restart was approved via a card (the `restart` tool or a restart
   suggestion), this notice is posted **in a thread under that approval card**, so
   the whole exchange nests where it was approved.
2. On reconnect it consumes the marker **once** and edits that same message into
   the **":white_check_mark: Murtaugh is back online."** ping card — the
   back-online confirmation *is* the Test communication card, so there is one
   restart message, not three. The standalone startup ping is suppressed while a
   marker is being consumed.

A marker older than **1 hour** is treated as stale and ignored (so a crash long
after the request doesn't post a misleading "back online"). The marker is
best-effort: if posting or persisting fails, the restart still happens — just
without the confirmation edit.
