#!/usr/bin/env bash
#
# Murtaugh macOS installer (thin orchestrator).
#
# This script only handles things that must happen before a Murtaugh binary
# exists locally: platform gate, flag/env parsing, install-dir choice,
# downloading + version-comparing + atomically replacing the binary, and
# restarting the LaunchAgent when one was previously loaded.
#
# Everything else — writing gateway.yaml, agents.yaml, dev.murtaugh.plist, and
# MCP client config — is delegated to the freshly-installed binary via
# `murtaugh setup ...` tools, which share the exact same code path the CLI
# and MCP frontends use. See internal/tools/setup/* for the implementations.

set -euo pipefail

REPO_OWNER="miere"
REPO_NAME="murtaugh-dev-toolkit"
RELEASE_API_URL="${MURTAUGH_RELEASE_API_URL:-https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/latest}"

ASSUME_YES=0
SKIP_CONFIG=0
RECONFIGURE=0
FORCE_INSTALL=0
DRY_RUN=0
LOCAL_BUILD=0
TARGET_VERSION=""
CUSTOM_AGENT_ARGS=()

usage() {
  cat <<'EOF'
Usage: install.sh [--yes] [--version VERSION] [--force] [--skip-config] [--reconfigure] [--dry-run] [--local-build]

Installs or updates the Murtaugh macOS release. Re-running this installer
updates the binary and preserves existing config by default. Use --reconfigure
to force a full config rewrite.

Options:
  --yes                 Skip interactive prompts (use env vars for all values).
  --version VERSION     Install a specific version instead of the latest.
  --force               Reinstall even if the current version matches latest.
  --skip-config         Update binary only; do not write or modify any config.
  --reconfigure         Always rewrite config files, backing up existing ones.
  --dry-run             Show what would happen without making changes.
  --local-build         Compile from the local checkout instead of fetching a
                        release. Useful for testing changes before cutting a
                        new tag. Requires a Go toolchain on PATH.
  --help, -h            Show this message.

Environment overrides:
  MURTAUGH_INSTALL_DIR
  MURTAUGH_SLACK_APP_TOKEN
  MURTAUGH_SLACK_BOT_TOKEN
  MURTAUGH_ADMIN_USER
  MURTAUGH_CHAT_AGENT             skip|native|opencode|goose|auggie|custom
  MURTAUGH_NATIVE_PROVIDER        gemini|anthropic|openai (native agent)
  MURTAUGH_NATIVE_MODEL           provider model id (native agent)
  MURTAUGH_NATIVE_API_KEY         provider API key, stored in ~/.config/murtaugh/.env
  MURTAUGH_CUSTOM_AGENT_COMMAND
  MURTAUGH_CUSTOM_AGENT_ARGS      shell-style argument string
  MURTAUGH_ENABLE_LAUNCH_AGENT    yes|no
  MURTAUGH_LOAD_LAUNCH_AGENT      yes|no
  MURTAUGH_MCP_CLIENT             skip|opencode|auggie|goose
  MURTAUGH_RELEASE_JSON_PATH      local file used instead of GitHub API
  MURTAUGH_INSTALL_ARCH           override uname arch for testing
  MURTAUGH_DRY_RUN                yes|no
  MURTAUGH_FORCE_INSTALL          yes|no
  MURTAUGH_RECONFIGURE            yes|no
  MURTAUGH_SKIP_CONFIG            yes|no
  MURTAUGH_TARGET_VERSION         install specific version
EOF
}

log() { printf '[murtaugh-installer] %s\n' "$*" >&2; }
die() { printf '[murtaugh-installer] ERROR: %s\n' "$*" >&2; exit 1; }
timestamp() { date +%Y%m%d%H%M%S; }

# resolve_path canonicalizes $1 without invoking python or GNU coreutils.
# Symlinks are resolved one hop (sufficient for our install dirs); for files
# the parent is resolved and the basename re-attached.
resolve_path() {
  local target=$1
  [[ -n "$target" ]] || { printf ''; return 0; }
  if [[ -L "$target" ]]; then
    local link
    link=$(readlink "$target")
    [[ "$link" = /* ]] || link="$(dirname "$target")/$link"
    target=$link
  fi
  if [[ -d "$target" ]]; then
    (cd "$target" >/dev/null 2>&1 && pwd -P) || printf '%s' "$target"
  else
    local d f
    d=$(dirname "$target"); f=$(basename "$target")
    if (cd "$d" >/dev/null 2>&1); then
      printf '%s/%s' "$(cd "$d" && pwd -P)" "$f"
    else
      printf '%s' "$target"
    fi
  fi
}

backup_file_if_exists() {
  local file=$1
  if [[ -e "$file" ]]; then
    local backup="${file}.bak.$(timestamp)"
    cp -p "$file" "$backup"
    log "Backed up ${file} to ${backup}"
  fi
}

require_darwin() {
  [[ "$(uname -s)" == "Darwin" ]] || die "this installer currently supports macOS only"
}

is_env_yes() {
  local val=${1:-}
  [[ "${val}" == "yes" || "${val}" == "true" || "${val}" == "1" ]]
}

installed_murtaugh_bin() { command -v murtaugh 2>/dev/null || true; }

detect_installed_version() {
  local bin=${1:-}
  if [[ -z "$bin" || ! -x "$bin" ]]; then printf '%s' ""; return 0; fi
  "$bin" version 2>/dev/null || true
}

strip_leading_v() {
  local v="$1"; v="${v#v}"; v="${v#V}"
  printf '%s' "$v"
}

version_compare() {
  local a b
  a="$(strip_leading_v "$1")"
  b="$(strip_leading_v "$2")"
  local IFS=. a_parts b_parts
  read -r -a a_parts <<< "$a"
  read -r -a b_parts <<< "$b"
  local max=$(( ${#a_parts[@]} > ${#b_parts[@]} ? ${#a_parts[@]} : ${#b_parts[@]} ))
  for (( i = 0; i < max; i++ )); do
    local av=${a_parts[i]:-0}
    local bv=${b_parts[i]:-0}
    if (( av > bv )); then return 1; fi
    if (( av < bv )); then return 2; fi
  done
  return 0
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --yes) ASSUME_YES=1 ;;
      --version)
        [[ -n "${2:-}" && "${2:-}" != -* ]] || die "--version requires a value"
        TARGET_VERSION="$2"; shift ;;
      --force) FORCE_INSTALL=1 ;;
      --skip-config) SKIP_CONFIG=1 ;;
      --reconfigure) RECONFIGURE=1 ;;
      --dry-run) DRY_RUN=1 ;;
      --local-build) LOCAL_BUILD=1 ;;
      --help|-h) usage; exit 0 ;;
      *) die "unknown argument: $1" ;;
    esac
    shift
  done
  is_env_yes "${MURTAUGH_DRY_RUN:-}" && DRY_RUN=1
  is_env_yes "${MURTAUGH_FORCE_INSTALL:-}" && FORCE_INSTALL=1
  is_env_yes "${MURTAUGH_RECONFIGURE:-}" && RECONFIGURE=1
  is_env_yes "${MURTAUGH_SKIP_CONFIG:-}" && SKIP_CONFIG=1
  is_env_yes "${MURTAUGH_LOCAL_BUILD:-}" && LOCAL_BUILD=1
  [[ -n "${MURTAUGH_TARGET_VERSION:-}" ]] && TARGET_VERSION="$MURTAUGH_TARGET_VERSION"
  return 0
}

prompt_required() {
  local env_name=$1 prompt=$2 secret=${3:-no} value=${!1:-}
  if [[ -n "$value" ]]; then printf '%s' "$value"; return 0; fi
  [[ $ASSUME_YES -eq 1 ]] && die "${env_name} is required when running with --yes"
  if [[ "$secret" == "yes" ]]; then
    read -r -s -p "$prompt: " value; printf '\n' >&2
  else
    read -r -p "$prompt: " value
  fi
  [[ -n "$value" ]] || die "$prompt is required"
  printf '%s' "$value"
}

# prompt_choice asks the user to pick one of a fixed set of options.
# Scripted callers (env var set, or --yes) get strict validation: an
# invalid value aborts immediately, which is what CI/automation needs.
# Interactive callers get a re-prompt loop with the available options
# rendered inline, so a typo or a "yes" answer to a multi-option
# question never aborts the install half-way through.
prompt_choice() {
  local env_name=$1 prompt=$2 default_value=$3
  shift 3
  local choices=("$@") value=${!env_name:-} choice choices_pretty
  choices_pretty=$(IFS='/'; printf '%s' "${choices[*]}")

  if [[ -n "$value" || $ASSUME_YES -eq 1 ]]; then
    [[ -z "$value" ]] && value=$default_value
    for choice in "${choices[@]}"; do
      [[ "$value" == "$choice" ]] && { printf '%s' "$value"; return 0; }
    done
    die "invalid value '${value}' for ${env_name}; expected one of: ${choices[*]}"
  fi

  while :; do
    read -r -p "${prompt} [${choices_pretty}] (default: ${default_value}): " value
    value=${value:-$default_value}
    for choice in "${choices[@]}"; do
      [[ "$value" == "$choice" ]] && { printf '%s' "$value"; return 0; }
    done
    printf '[murtaugh-installer] Invalid choice: %s. Please pick one of: %s\n' \
      "$value" "${choices[*]}" >&2
  done
}

choose_install_dir() {
  if [[ -n "${MURTAUGH_INSTALL_DIR:-}" ]]; then
    mkdir -p "$MURTAUGH_INSTALL_DIR"
    printf '%s' "$(resolve_path "$MURTAUGH_INSTALL_DIR")"
    return 0
  fi
  local candidates=() current dir
  current=$(command -v murtaugh 2>/dev/null || true)
  [[ -n "$current" ]] && candidates+=("$(dirname "$(resolve_path "$current")")")
  candidates+=("$HOME/.local/bin")
  [[ -d /opt/homebrew/bin ]] && candidates+=("/opt/homebrew/bin")
  [[ -d /usr/local/bin ]] && candidates+=("/usr/local/bin")
  for dir in "${candidates[@]}"; do
    [[ -n "$dir" ]] || continue
    if [[ "$dir" == "$HOME"/* ]]; then
      mkdir -p "$dir"; printf '%s' "$(resolve_path "$dir")"; return 0
    fi
    [[ -w "$dir" ]] && { printf '%s' "$(resolve_path "$dir")"; return 0; }
  done
  mkdir -p "$HOME/.local/bin"
  printf '%s' "$(resolve_path "$HOME/.local/bin")"
}

# release_json fetches the GitHub release metadata, or reads a local file
# when MURTAUGH_RELEASE_JSON_PATH is set (used by the integration tests).
#
# GitHub's /releases/latest endpoint deliberately excludes pre-releases, so
# when a project has only pre-release tags published it returns 404. We fall
# back to /releases?per_page=1 in that case, which returns the most recent
# release of any kind. The bash JSON extractors operate on regex matches and
# do not care whether the body is a single object or a one-element array.
release_json() {
  local target_version="${1:-}" body
  if [[ -n "${MURTAUGH_RELEASE_JSON_PATH:-}" ]]; then
    cat "$MURTAUGH_RELEASE_JSON_PATH"
    return $?
  fi
  if [[ -n "$target_version" ]]; then
    curl -fsSL "https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/tags/${target_version}"
    return $?
  fi
  if body=$(curl -fsSL "$RELEASE_API_URL" 2>/dev/null); then
    printf '%s' "$body"
    return 0
  fi
  log "No stable release found; checking for pre-releases."
  curl -fsSL "https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases?per_page=1"
}

detect_arch_suffix() {
  local arch=${MURTAUGH_INSTALL_ARCH:-$(uname -m)}
  case "$arch" in
    arm64|aarch64) printf 'darwin-arm64' ;;
    x86_64|amd64) printf 'darwin-amd64' ;;
    *) die "unsupported macOS architecture: $arch" ;;
  esac
}

# extract_tag_name pulls "tag_name": "<v>" from GitHub release JSON.
# Pure bash + grep/sed so the installer has no Python dependency.
extract_tag_name() {
  printf '%s' "$1" \
    | tr -d '\n' \
    | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' \
    | head -n1 \
    | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/'
}

# extract_asset_url finds the browser_download_url whose path ends with the
# expected asset filename. Works because release URLs are predictable:
# https://github.com/<owner>/<repo>/releases/download/<tag>/<asset>.
extract_asset_url() {
  local json=$1 want=$2
  printf '%s' "$json" \
    | tr -d '\n' \
    | grep -oE "\"browser_download_url\"[[:space:]]*:[[:space:]]*\"[a-z]+://[^\"]*/${want}\"" \
    | head -n1 \
    | sed -E 's/.*"([a-z]+:\/\/[^"]+)".*/\1/'
}

install_or_update_binary() {
  local install_dir=$1 suffix=$2 target_version=${3:-}
  local json tag asset_url tmpdir tmpbin dest installed_bin current_version want

  if ! json=$(release_json "$target_version" 2>/dev/null); then
    die "could not fetch release metadata from GitHub. The repository may have no published releases yet, or you are offline. Set MURTAUGH_RELEASE_JSON_PATH to a local release.json to install from a fixture, or pass --version <tag> to target a specific release."
  fi
  [[ -n "$json" ]] || die "release metadata was empty"

  tag=$(extract_tag_name "$json")
  [[ -n "$tag" ]] || die "release metadata did not contain a tag_name; the response may not be a GitHub release payload"
  want="murtaugh-${tag}-${suffix}"
  asset_url=$(extract_asset_url "$json" "$want")
  [[ -n "$asset_url" ]] || die "release ${tag} has no asset named ${want}"

  installed_bin=$(installed_murtaugh_bin)
  current_version=$(detect_installed_version "$installed_bin")

  if [[ -n "$current_version" && -n "$tag" && "$FORCE_INSTALL" -eq 0 ]]; then
    version_compare "$current_version" "$tag"
    local cmp=$?
    if [[ "$cmp" -eq 0 ]]; then
      log "Already running ${tag} — no update needed. Use --force to reinstall."
      printf '%s' "$(resolve_path "$installed_bin")"; return 0
    elif [[ "$cmp" -eq 1 ]]; then
      log "Already running a newer version (${current_version}) than ${tag} — skipping update."
      printf '%s' "$(resolve_path "$installed_bin")"; return 0
    fi
  fi

  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "[DRY-RUN] Would download ${tag} to ${install_dir}/murtaugh"
    printf '%s' "${install_dir}/murtaugh"; return 0
  fi

  tmpdir=$(mktemp -d); tmpbin="$tmpdir/murtaugh"
  curl -fsSL "$asset_url" -o "$tmpbin"
  chmod +x "$tmpbin"
  # Sanity-check the download executes at all. We accept any non-127/126
  # exit because not every released binary version has the same subcommand
  # set; earlier builds shipped without `version` and would otherwise be
  # rejected here even though they run fine.
  "$tmpbin" version >/dev/null 2>&1
  local check_rc=$?
  if [[ $check_rc -eq 127 || $check_rc -eq 126 ]]; then
    die "downloaded release asset for ${tag} could not be executed (exit ${check_rc}); the archive may be corrupted or for the wrong architecture"
  fi
  dest="$install_dir/murtaugh"
  backup_file_if_exists "$dest"
  cp "$tmpbin" "$dest"
  chmod 755 "$dest"
  rm -rf "$tmpdir"
  if [[ "$current_version" == "" ]]; then
    log "Installed Murtaugh ${tag} to ${dest}"
  else
    log "Updated Murtaugh from ${current_version} to ${tag}"
  fi
  printf '%s' "$(resolve_path "$dest")"
}

# find_repo_root locates the repository root when install.sh is invoked
# from a checkout (i.e. via `bash ./install/macos/install.sh`, not via
# `curl | bash`). The check is intentionally strict: we require both
# go.mod and cmd/murtaugh to exist so we never compile from an
# unrelated tree that happens to live two directories up.
find_repo_root() {
  local script_dir root
  # ${BASH_SOURCE[0]:-} guards the `curl | bash` path (empty BASH_SOURCE under
  # set -u): an empty source resolves to the cwd, which fails the go.mod/
  # cmd/murtaugh checks below and correctly reports "not a checkout".
  script_dir=$(cd "$(dirname "${BASH_SOURCE[0]:-}")" && pwd -P)
  root="${script_dir%/install/macos}"
  [[ "$root" != "$script_dir" ]] || { printf ''; return 0; }
  [[ -f "$root/go.mod" && -d "$root/cmd/murtaugh" ]] || { printf ''; return 0; }
  printf '%s' "$root"
}

# build_local_binary compiles ./cmd/murtaugh from the checkout and drops
# the artifact at $install_dir/murtaugh. The embedded version is stamped
# as "dev-<timestamp>" so `setup.update` (which refuses dev builds by
# default) treats it as a developer artifact rather than a real release.
build_local_binary() {
  local install_dir=$1 repo_root=$2
  command -v go >/dev/null 2>&1 || die "--local-build requires a 'go' toolchain on PATH"
  local dest="$install_dir/murtaugh" version
  version="dev-$(timestamp)"
  log "Building Murtaugh from ${repo_root} (version=${version})"
  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "[DRY-RUN] Would: go build -o ${dest} ./cmd/murtaugh"
    printf '%s' "$dest"; return 0
  fi
  backup_file_if_exists "$dest"
  ( cd "$repo_root" && go build -ldflags="-X main.version=${version}" -o "$dest" ./cmd/murtaugh ) \
    || die "go build failed; see output above"
  chmod 755 "$dest"
  log "Installed local-build Murtaugh ${version} to ${dest}"
  printf '%s' "$(resolve_path "$dest")"
}

# binary_supports_setup checks whether the freshly-installed murtaugh
# binary exposes the `setup` command group. Releases predating the
# installer rewrite do not, and calling `setup launchd` against them
# emits `unknown command: setup` mid-install. We use this to fail
# loudly with an actionable message before any state changes.
binary_supports_setup() {
  local bin=$1
  "$bin" setup --help >/dev/null 2>&1
}

# api_key_env_for maps a provider to the .env variable name that holds its key.
# agents.yaml references the agent's credential by this name (api_key_env), so the
# key value never lives in YAML.
api_key_env_for() {
  case "$1" in
    gemini) printf 'GEMINI_API_KEY' ;;
    anthropic) printf 'ANTHROPIC_API_KEY' ;;
    openai) printf 'OPENAI_API_KEY' ;;
    *) die "unsupported native provider: $1" ;;
  esac
}

# default_model_for offers a sensible starting model per provider; the user can
# override it at the prompt or via MURTAUGH_NATIVE_MODEL.
default_model_for() {
  case "$1" in
    gemini) printf 'gemini-2.5-pro' ;;
    anthropic) printf 'claude-sonnet-4-6' ;;
    openai) printf 'gpt-5' ;;
    *) printf '' ;;
  esac
}

# configure_native_agent probes for the native-agent provider, model, and API
# key, populating the NATIVE_* globals consumed by write_slack_config. The key is
# read silently and only ever written to ~/.config/murtaugh/.env.
configure_native_agent() {
  NATIVE_PROVIDER=$(prompt_choice MURTAUGH_NATIVE_PROVIDER "Native LLM provider" gemini gemini anthropic openai)
  NATIVE_API_KEY_ENV=$(api_key_env_for "$NATIVE_PROVIDER")
  local default_model
  default_model=$(default_model_for "$NATIVE_PROVIDER")
  if [[ -n "${MURTAUGH_NATIVE_MODEL:-}" ]]; then
    NATIVE_MODEL="$MURTAUGH_NATIVE_MODEL"
  elif [[ $ASSUME_YES -eq 1 ]]; then
    NATIVE_MODEL="$default_model"
  else
    read -r -p "Model (default: ${default_model}): " NATIVE_MODEL
    NATIVE_MODEL=${NATIVE_MODEL:-$default_model}
  fi
  [[ -n "$NATIVE_MODEL" ]] || die "a model is required for the native agent"
  NATIVE_API_KEY=$(prompt_required MURTAUGH_NATIVE_API_KEY "${NATIVE_PROVIDER} API key (stored in ~/.config/murtaugh/.env)" yes)
}

resolve_agent_command() {
  local choice=$1
  case "$choice" in
    skip) return 0 ;;
    opencode|goose|auggie)
      command -v "$choice" >/dev/null 2>&1 || die "${choice} is not installed or not on PATH"
      resolve_path "$(command -v "$choice")"
      ;;
    custom)
      local custom_cmd=${MURTAUGH_CUSTOM_AGENT_COMMAND:-}
      if [[ -z "$custom_cmd" ]]; then
        [[ $ASSUME_YES -eq 1 ]] && die "MURTAUGH_CUSTOM_AGENT_COMMAND is required for custom chat agent in --yes mode"
        read -r -p "Custom ACP command path: " custom_cmd
      fi
      [[ -x "$custom_cmd" ]] || die "custom command is not executable: ${custom_cmd}"
      resolve_path "$custom_cmd"
      ;;
    *) die "unsupported chat agent choice: ${choice}" ;;
  esac
}

# collect_custom_args splits MURTAUGH_CUSTOM_AGENT_ARGS using shell quoting
# rules. xargs -n1 honors quotes/escapes the same way a shell would when
# tokenizing a command line, so e.g. `--flag "two words" --other` becomes
# three array entries with the quoted span preserved.
collect_custom_args() {
  local arg_string=${MURTAUGH_CUSTOM_AGENT_ARGS:-}
  CUSTOM_AGENT_ARGS=()
  [[ -z "$arg_string" && $ASSUME_YES -eq 0 ]] && read -r -p "Custom ACP command args (optional): " arg_string
  [[ -z "$arg_string" ]] && return 0
  while IFS= read -r arg; do
    [[ -n "$arg" ]] && CUSTOM_AGENT_ARGS+=("$arg")
  done < <(printf '%s' "$arg_string" | xargs -n1 printf '%s\n' 2>/dev/null)
}

restart_launch_agent_if_needed() {
  local plist="$HOME/Library/LaunchAgents/dev.murtaugh.plist" uid
  [[ -f "$plist" ]] || return 0
  command -v launchctl >/dev/null 2>&1 || return 0
  uid=$(id -u)
  launchctl print "gui/${uid}/dev.murtaugh" >/dev/null 2>&1 || return 0
  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "[DRY-RUN] Would restart LaunchAgent dev.murtaugh"; return 0
  fi
  log "Restarting LaunchAgent dev.murtaugh"
  launchctl bootout "gui/${uid}" "$plist" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/${uid}" "$plist"
  # bootstrap registers the agent but does not reliably honor RunAtLoad, so
  # force the (re)start explicitly — otherwise the daemon sits loaded but
  # never spawns and Slack stays unreachable.
  launchctl kickstart -k "gui/${uid}/dev.murtaugh"
  log "Restarted LaunchAgent dev.murtaugh"
}

# write_slack_config delegates gateway.yaml writing to `murtaugh setup slack`.
# The agents.yaml write is paired here so the daemon never sees an
# inconsistent intermediate state where one file points at an agent the other
# doesn't define.
write_slack_config() {
  local bin=$1 app=$2 bot=$3 admin=$4 chat_choice=$5 chat_cmd=$6
  shift 6
  local setup_args=(setup slack --app-token "$app" --bot-token "$bot" --admin-user "$admin")
  if [[ "$chat_choice" != "skip" ]]; then
    setup_args+=(--default-agent default)
  fi
  "$bin" "${setup_args[@]}" >&2

  # Native agent: write the provider key to .env, then a native profile that
  # references it. No external binary, no ACP.
  if [[ "$chat_choice" == "native" ]]; then
    "$bin" setup env --set "${NATIVE_API_KEY_ENV}=${NATIVE_API_KEY}" >&2
    "$bin" setup agents --provider "$NATIVE_PROVIDER" --model "$NATIVE_MODEL" \
      --api-key-env "$NATIVE_API_KEY_ENV" \
      --tools files --tools terminal --tools skills >&2
    return 0
  fi

  local agents_args=(setup agents)
  if [[ "$chat_choice" != "skip" ]]; then
    agents_args+=(--command "$chat_cmd")
    local a
    for a in "$@"; do agents_args+=(--args "$a"); done
  fi
  "$bin" "${agents_args[@]}" >&2
}

# write_launch_agent delegates the plist to `murtaugh setup launchd`, and
# optionally loads it via launchctl. Returns silently when the user declined.
write_launch_agent() {
  local bin=$1 enable_choice load_choice setup_args
  enable_choice=$(prompt_choice MURTAUGH_ENABLE_LAUNCH_AGENT "Create a launchd LaunchAgent?" yes yes no)
  [[ "$enable_choice" == "yes" ]] || return 0
  load_choice=$(prompt_choice MURTAUGH_LOAD_LAUNCH_AGENT "Load the LaunchAgent now?" yes yes no)
  setup_args=(setup launchd --binary-path "$bin")
  [[ "$load_choice" == "yes" ]] && setup_args+=(--load true)
  "$bin" "${setup_args[@]}" >&2
}

# configure_mcp_client delegates the per-client merge to
# `murtaugh setup mcp-register`. The bash side still gates on whether the
# client is installed locally so we keep the same error semantics as before.
configure_mcp_client() {
  local bin=$1 mcp_client target backup
  mcp_client=$(prompt_choice MURTAUGH_MCP_CLIENT "Configure Murtaugh as an MCP server in a client?" skip skip opencode auggie goose)
  case "$mcp_client" in
    skip) return 0 ;;
    opencode)
      command -v opencode >/dev/null 2>&1 || die "OpenCode is not installed or not on PATH"
      target="$HOME/.config/opencode/opencode.json"
      "$bin" setup mcp-register --client opencode --binary-path "$bin" >&2 || die "failed to update ${target}; if it contains JSONC comments, please edit it manually"
      log "Configured OpenCode MCP in ${target}" ;;
    auggie)
      command -v auggie >/dev/null 2>&1 || die "Auggie is not installed or not on PATH"
      target="$HOME/.augment/settings.json"
      "$bin" setup mcp-register --client auggie --binary-path "$bin" >&2 || die "failed to update ${target}"
      log "Configured Auggie MCP in ${target}" ;;
    goose)
      command -v goose >/dev/null 2>&1 || die "Goose is not installed or not on PATH"
      target="$HOME/.config/goose/config.yaml"
      "$bin" setup mcp-register --client goose --binary-path "$bin" >&2 || die "failed to update ${target}"
      log "Configured Goose MCP in ${target}" ;;
  esac
}

main() {
  parse_args "$@"
  require_darwin

  local install_dir arch_suffix installed_bin repo_root
  install_dir=$(choose_install_dir)
  arch_suffix=$(detect_arch_suffix)
  if [[ "$LOCAL_BUILD" -eq 1 ]]; then
    repo_root=$(find_repo_root)
    [[ -n "$repo_root" ]] || die "--local-build requires running install.sh from a checkout containing go.mod and cmd/murtaugh"
    installed_bin=$(build_local_binary "$install_dir" "$repo_root")
  else
    installed_bin=$(install_or_update_binary "$install_dir" "$arch_suffix" "$TARGET_VERSION")
  fi

  if [[ "$SKIP_CONFIG" -eq 1 ]]; then
    log "Done. Binary updated; config untouched."
    [[ "$DRY_RUN" -eq 1 ]] && log "[DRY-RUN] No changes were made."
    log "Murtaugh MCP command: ${installed_bin} mcp"
    return 0
  fi

  # Bail out early with an actionable message when the binary we just put
  # in place does not expose the `setup` command group. Otherwise the
  # next thing the user sees is `unknown command: setup` mid-install.
  if [[ "$DRY_RUN" -eq 0 ]] && ! binary_supports_setup "$installed_bin"; then
    repo_root=$(find_repo_root)
    if [[ -n "$repo_root" ]]; then
      die "the installed Murtaugh (${installed_bin}) does not support 'setup' yet. Re-run with --local-build to compile from ${repo_root}, or use --skip-config to update the binary only."
    fi
    die "the installed Murtaugh (${installed_bin}) does not support 'setup' yet. Upgrade to a release that includes the setup tools, or pass --skip-config to update the binary only."
  fi

  local config_dir gateway_yaml agents_yaml has_config
  config_dir="$HOME/.config/murtaugh"
  gateway_yaml="$config_dir/gateway.yaml"
  agents_yaml="$config_dir/agents.yaml"
  has_config=0
  [[ -f "$gateway_yaml" || -f "$agents_yaml" ]] && has_config=1

  local app_token bot_token admin_user chat_choice chat_command=""
  local -a chat_args=()

  if [[ "$has_config" -eq 1 && "$RECONFIGURE" -eq 0 && "$DRY_RUN" -eq 0 ]]; then
    log "Existing config detected. Preserving Slack and agent configs by default."
    log "Use --reconfigure to rewrite them."
  else
    if [[ "$DRY_RUN" -eq 1 && "$has_config" -eq 1 && "$RECONFIGURE" -eq 0 ]]; then
      log "[DRY-RUN] Would preserve existing config files."
    elif [[ "$DRY_RUN" -eq 1 && "$RECONFIGURE" -eq 1 ]]; then
      log "[DRY-RUN] Would rewrite config files with backups."
    elif [[ "$DRY_RUN" -eq 1 && "$has_config" -eq 0 ]]; then
      log "[DRY-RUN] Would write new config files."
    fi

    app_token=$(prompt_required MURTAUGH_SLACK_APP_TOKEN "Slack app token (xapp-...)" yes)
    bot_token=$(prompt_required MURTAUGH_SLACK_BOT_TOKEN "Slack bot token (xoxb-...)" yes)
    admin_user=$(prompt_required MURTAUGH_ADMIN_USER "Slack admin handle or user ID")
    [[ "$app_token" == xapp-* ]] || die "Slack app token must start with xapp-"
    [[ "$bot_token" == xoxb-* ]] || die "Slack bot token must start with xoxb-"

    chat_choice=$(prompt_choice MURTAUGH_CHAT_AGENT "Slack Chat agent" skip skip native opencode goose auggie custom)
    if [[ "$chat_choice" == "native" ]]; then
      configure_native_agent
    elif [[ "$chat_choice" != "skip" ]]; then
      chat_command=$(resolve_agent_command "$chat_choice")
    fi
    case "$chat_choice" in
      opencode) chat_args=(acp) ;;
      goose) chat_args=(acp) ;;
      auggie) chat_args=(--acp --allow-indexing) ;;
      custom) collect_custom_args; chat_args=("${CUSTOM_AGENT_ARGS[@]}") ;;
    esac

    mkdir -p "$config_dir"
    chmod 700 "$config_dir" 2>/dev/null || true

    if [[ "$DRY_RUN" -eq 1 ]]; then
      log "[DRY-RUN] Would write ${gateway_yaml} and ${agents_yaml}"
    else
      # Seed embedded defaults first so skills/ + docs land before the
      # user-provided slack/agents writes overlay on top. On --reconfigure also
      # refresh the bundled default system prompt to the shipped version (user
      # config, secrets, and AGENTS.md are always preserved).
      local boot_args=(setup bootstrap)
      [[ "$RECONFIGURE" -eq 1 ]] && boot_args+=(--force true)
      "$installed_bin" "${boot_args[@]}" >&2
      write_slack_config "$installed_bin" "$app_token" "$bot_token" "$admin_user" "$chat_choice" "$chat_command" ${chat_args[@]+"${chat_args[@]}"}
      log "Wrote Slack config to ${gateway_yaml}"
      log "Wrote agent config to ${agents_yaml}"
    fi
  fi

  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "[DRY-RUN] Would configure LaunchAgent and MCP clients if applicable."
  else
    write_launch_agent "$installed_bin"
    configure_mcp_client "$installed_bin"
  fi

  restart_launch_agent_if_needed

  log "Murtaugh MCP command: ${installed_bin} mcp"
  log "Done. Re-run this installer any time to update or regenerate config."
}

# Only run main when executed directly, so unit tests can source the
# script to exercise individual helpers like prompt_choice in isolation.
#
# The `:-$0` default matters for the `curl … | bash` install path: bash then
# reads the script from stdin, BASH_SOURCE is empty, and a bare
# ${BASH_SOURCE[0]} would trip `set -u` ("unbound variable") before main runs.
# Defaulting to $0 makes the comparison true when piped or executed, and still
# false when sourced (BASH_SOURCE[0] is the script path, $0 is the parent shell).
if [[ "${BASH_SOURCE[0]:-$0}" == "${0}" ]]; then
  main "$@"
fi

