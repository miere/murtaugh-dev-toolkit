---
name: murtaugh-setup
description: Install and configure Murtaugh from scratch with the idempotent setup_* tools — binary on PATH, config dir, gateway.yaml, .env secrets, agents.yaml, the macOS daemon, and self-update.
requires: [setup]
files:
  reference/config-tools.md:       { requires: [setup], summary: "seed config / write gateway.yaml / .env secrets / agents.yaml" }
  reference/daemon-and-clients.md: { requires: [setup], summary: "install the daemon, register an MCP client, self-update" }
  reference/mcp-server.md:         { requires: [setup], summary: "run Murtaugh as an MCP server for another tool" }
---

# Skill: Murtaugh Setup & Install

How to install and configure Murtaugh from scratch using the `setup_*` tools.
This is **operator-facing**: getting the binary in place, writing the config
files, and (on macOS) installing the daemon. For *running and debugging* the
daemon afterward, see the `murtaugh-operations` skill.

A legacy config dir (the old `slack.yaml` layout) is auto-migrated to the new
schema on the first run of a new binary — backed up, validated, and rolled back
on failure — or you can convert it ahead of time with `murtaugh config migrate`.

Every `setup_*` tool is idempotent, so re-running is safe. The config writers
(`setup_slack`, `setup_env`, `setup_agents`, `setup_launchd`, `setup_mcp-register`)
back up any file they replace (`<file>.bak.<timestamp>`). `setup_bootstrap` seeds
the workspace and is safe to re-run (config files and templates are preserved).
The bundled agent skills are served in-binary (not written to disk), so there's
no on-disk skill copy to keep in sync — see `reference/config-tools.md`.

## Install order (the workflow)

1. **Get the binary** on `PATH` (download a release, or `go build`).
2. **`setup_bootstrap`** — seed the config dir with defaults (must run first, so
   later steps edit real files). → `reference/config-tools.md`
3. **`setup_slack`** — write `gateway.yaml` (OAuth tokens, admin user, chat).
4. **`setup_env`** — upsert provider keys into `.env` (a native agent can't
   authenticate without its key here; run before/with `setup_agents`).
5. **`setup_agents`** — write `agents.yaml` (runtime block + a native or ACP
   agent, or leave chat disabled).
6. **`setup_launchd`** *(macOS, optional)* — install the daemon as a LaunchAgent.
   → `reference/daemon-and-clients.md`
7. **`setup_mcp-register`** *(optional)* — register Murtaugh in an MCP client.

Later: **`setup_update`** self-updates the binary from a GitHub release.

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Seeding config / writing gateway.yaml / .env secrets / agents.yaml | `reference/config-tools.md` |
| Installing the daemon, registering an MCP client, or self-updating | `reference/daemon-and-clients.md` |
| Running Murtaugh as an MCP server for another tool | `reference/mcp-server.md` |
| Wanting a copy-paste install sequence | `examples/install-sequence.sh` |

## Global guidelines (defaults — follow unless the user says otherwise)

- **`setup_bootstrap` first.** It creates the workspace (`~/.config/murtaugh`)
  and seeds templates/skills; the other tools edit files that must already exist.
- **`gateway.yaml` and `.env` hold secrets** — they're written `0600`. Provider
  API keys live in `.env` (referenced from `agents.yaml` by variable name via
  `api_key_env`), never in YAML. Don't commit them or echo tokens into logs.
- **`setup_launchd` is macOS-only**; on other platforms run the gateway under
  your own supervisor (`murtaugh slack gateway`).
- Tools run as `murtaugh setup <tool> …` on the CLI and as `setup_<tool>` over
  MCP. Setup tools work **before** a valid config exists (they create it).
- **CLI flags always carry a value — booleans included.** Write `--load true`
  and `--force true`; a bare `--load` is rejected. snake_case arg names map to
  kebab flags (`binary_path` → `--binary-path`, `app_token` → `--app-token`).
- **When in doubt, ask the binary.** `murtaugh help` lists every command;
  `murtaugh help setup <tool>` (or `murtaugh setup <tool> --help`) prints that
  tool's full flag reference — required/optional, types, defaults, examples.
