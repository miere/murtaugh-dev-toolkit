# Daemon, MCP clients & updates: launchd, mcp-register, update

> **CLI flags carry values — booleans included.** `load` and `force` below are
> booleans, but the CLI parser has no bare switches: write `--load true` and
> `--force true` (a bare `--load` fails with `flag --load requires a value`).
> snake_case `Arg` names map to kebab flags (`binary_path` → `--binary-path`,
> `release_json_url` → `--release-json-url`). Over MCP, pass a real JSON
> boolean. Run `murtaugh help setup <tool>` or `murtaugh setup <tool> --help`
> for the full reference.

## `setup_launchd` — install the daemon (macOS)

*Write the dev.murtaugh LaunchAgent plist (macOS) and optionally load it.*

| Arg | Required | Meaning |
|---|---|---|
| `binary_path` | yes | Absolute path to the murtaugh binary. |
| `load` | no | Boolean. `--load true` (re)loads the agent via `launchctl` after writing. |

**macOS only** (errors on other platforms). It writes
`~/Library/LaunchAgents/dev.murtaugh.plist` with:

- **Label** `dev.murtaugh`, **ProgramArguments** `[<binary>, slack, gateway]`
- **RunAtLoad** + **KeepAlive** `true` (starts at login, restarts on crash)
- logs to **`~/Library/Logs/murtaugh/slack.out.log`** and **`…/slack.err.log`**

With `load: true` it lints the plist (`plutil`) then `launchctl bootout` →
`bootstrap` → `kickstart` to (re)start it immediately.

```bash
murtaugh setup launchd --binary-path "$(which murtaugh)" --load true
```

## `setup_mcp-register` — register Murtaugh in an MCP client

*Register Murtaugh as an MCP server in opencode, auggie, or goose.*

| Arg | Required | Meaning |
|---|---|---|
| `client` | yes | One of `opencode`, `auggie`, `goose`. |
| `binary_path` | yes | Absolute path to the murtaugh binary. |

Merges a `murtaugh` MCP entry into the client's config, preserving everything
else, and backs up the file:

| Client | Config file |
|---|---|
| `opencode` | `~/.config/opencode/opencode.json` |
| `auggie` | `~/.augment/settings.json` |
| `goose` | `~/.config/goose/config.yaml` |

The entry runs `murtaugh mcp` (see `reference/mcp-server.md`).

```bash
murtaugh setup mcp-register --client opencode --binary-path "$(which murtaugh)"
```

## `setup_update` — self-update the binary

*Update the running Murtaugh binary from a GitHub release asset.*

| Arg | Required | Meaning |
|---|---|---|
| `version` | no | Release tag (e.g. `v0.0.2`). Default: latest. |
| `force` | no | Boolean. `--force true` is required to replace a `dev` build or re-install the current version. |
| `release_json_url` | no | Override the GitHub API URL (testing). |

Fetches the matching `murtaugh-<tag>-<os>-<arch>` asset from
`github.com/miere/murtaugh-dev-toolkit`, sanity-checks it (`<asset> version`),
backs up the current binary, and swaps it in. **Skips** if already on the target
version; refuses to replace a `dev` build unless `--force true`. After updating a
daemon, reload it (`setup_launchd --load true`, or your supervisor).

```bash
murtaugh setup update                 # latest release; refuses on a dev build
murtaugh setup update --force true    # required when replacing a dev build
murtaugh setup update --version v0.5.0 --force true
```
