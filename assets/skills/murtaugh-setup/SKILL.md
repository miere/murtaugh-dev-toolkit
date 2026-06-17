---
name: murtaugh-setup
description: Operator-facing guide for installing and configuring Murtaugh from scratch using the idempotent `setup_*` tools (CLI `murtaugh setup <tool>` / MCP `setup_<tool>`). Covers getting the binary on PATH, seeding the config dir (`~/.config/murtaugh`) with `setup_bootstrap`, writing `slack.yaml` via `setup_slack` and `agents.yaml` via `setup_agents`, installing the macOS LaunchAgent daemon with `setup_launchd`, registering an MCP client with `setup_mcp-register`, and self-updating with `setup_update`. Use when installing or bootstrapping Murtaugh, editing its config/secret files, troubleshooting setup tool flags (e.g. `--load true`, `--binary-path`, `--app-token`), or determining the correct install order; for running and debugging the daemon afterward, use the murtaugh-operations skill instead.
---

# Skill: Murtaugh Setup & Install

How to install and configure Murtaugh from scratch using the `setup_*` tools.
This is **operator-facing**: getting the binary in place, writing the config
files, and (on macOS) installing the daemon. For *running and debugging* the
daemon afterward, see the `murtaugh-operations` skill.

Every `setup_*` tool is idempotent, so re-running is safe. The config writers
(`setup_slack`, `setup_agents`, `setup_launchd`, `setup_mcp-register`) back up
any file they replace (`<file>.bak.<timestamp>`). `setup_bootstrap` is the one
exception that **refreshes in place**: it keeps the bundled agent skills in sync
with the shipped binary on every run (config files and templates stay
preserved) — see `reference/config-tools.md`.

## Install order (the workflow)

1. **Get the binary** on `PATH` (download a release, or `go build`).
2. **`setup_bootstrap`** — seed the config dir with defaults (must run first, so
   later steps edit real files). → `reference/config-tools.md`
3. **`setup_slack`** — write `slack.yaml` (OAuth tokens, admin user).
4. **`setup_agents`** — write `agents.yaml` (ACP block + an agent, or disable ACP).
5. **`setup_launchd`** *(macOS, optional)* — install the daemon as a LaunchAgent.
   → `reference/daemon-and-clients.md`
6. **`setup_mcp-register`** *(optional)* — register Murtaugh in an MCP client.

Later: **`setup_update`** self-updates the binary from a GitHub release.

## Read the right file (don't load everything)

| When you're… | Read |
|---|---|
| Seeding config / writing slack.yaml / agents.yaml | `reference/config-tools.md` |
| Installing the daemon, registering an MCP client, or self-updating | `reference/daemon-and-clients.md` |
| Running Murtaugh as an MCP server for another tool | `reference/mcp-server.md` |
| Wanting a copy-paste install sequence | `examples/install-sequence.sh` |

## Global guidelines (defaults — follow unless the user says otherwise)

- **`setup_bootstrap` first.** It creates the workspace (`~/.config/murtaugh`)
  and seeds templates/skills; the other tools edit files that must already exist.
- **`slack.yaml`/`agents.yaml` hold secrets** — they're written `0600`. Don't
  commit them or echo tokens into logs.
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
