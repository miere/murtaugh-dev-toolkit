# Skill: Murtaugh Setup & Install

How to install and configure Murtaugh from scratch using the `setup.*` tools.
This is **operator-facing**: getting the binary in place, writing the config
files, and (on macOS) installing the daemon. For *running and debugging* the
daemon afterward, see the `murtaugh-operations` skill.

Every `setup.*` tool is idempotent and backs up any file it would overwrite
(`<file>.bak.<timestamp>`), so re-running is safe.

## Install order (the workflow)

1. **Get the binary** on `PATH` (download a release, or `go build`).
2. **`setup.bootstrap`** — seed the config dir with defaults (must run first, so
   later steps edit real files). → `reference/config-tools.md`
3. **`setup.slack`** — write `slack.yaml` (OAuth tokens, admin user).
4. **`setup.agents`** — write `agents.yaml` (ACP block + an agent, or disable ACP).
5. **`setup.launchd`** *(macOS, optional)* — install the daemon as a LaunchAgent.
   → `reference/daemon-and-clients.md`
6. **`setup.mcp-register`** *(optional)* — register Murtaugh in an MCP client.

Later: **`setup.update`** self-updates the binary from a GitHub release.

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Seeding config / writing slack.yaml / agents.yaml | `reference/config-tools.md` |
| Installing the daemon, registering an MCP client, or self-updating | `reference/daemon-and-clients.md` |
| Running Murtaugh as an MCP server for another tool | `reference/mcp-server.md` |
| Wanting a copy-paste install sequence | `examples/install-sequence.sh` |

## Global guidelines (defaults — follow unless the user says otherwise)

- **`setup.bootstrap` first.** It creates the workspace (`~/.config/murtaugh`)
  and seeds templates/skills; the other tools edit files that must already exist.
- **`slack.yaml`/`agents.yaml` hold secrets** — they're written `0600`. Don't
  commit them or echo tokens into logs.
- **`setup.launchd` is macOS-only**; on other platforms run the gateway under
  your own supervisor (`murtaugh slack gateway`).
- Tools run as `murtaugh setup <tool> …` on the CLI and as `setup.<tool>` over
  MCP. Setup tools work **before** a valid config exists (they create it).
