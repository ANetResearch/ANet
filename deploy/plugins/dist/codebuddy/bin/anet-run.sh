#!/bin/sh
# Runs the anet executable that the plugin settings select, for the identity
# that they select: `sh anet-run.sh <arguments>` is `anet [--id <name>]
# <arguments>`. The MCP launcher and the skills use this script, so that every
# command reaches the same executable and the same node.
#
# Settings. The MCP server receives them from CodeBuddy in ANET_PLUGIN_BIN and
# ANET_PLUGIN_IDENTITY, with ANET_PLUGIN_SETTINGS=env. In any other context the
# script reads the two options of this plugin, and nothing else, from the user
# settings of the host (settings.json in WORKBUDDY_CONFIG_DIR or
# CODEBUDDY_CONFIG_DIR, default ~/.codebuddy, then ~/.workbuddy; key
# pluginConfigs."anet@...".options).
#
# Executable. A configured anetPath is used as it is and is never replaced by
# another copy. Without it, PATH and the install locations of the anet
# installer and Homebrew are searched, because a desktop application does not
# always inherit the PATH of a login shell.
#
# `sh anet-run.sh --resolve` prints the selected executable, version and
# identity, and the command line of this script, and always exits 0. Errors go
# to stderr with exit status 1.
set -u

# read_settings_file prints two lines, anetPath and identity, from the file
# that is named by its argument, and returns 0. It returns 1 when the file or
# a JSON reader is missing or holds no option of this plugin. When several
# entries of this plugin exist (installed from different marketplaces), it
# prints two empty lines: an ambiguous store selects nothing.
read_settings_file() {
  [ -r "$1" ] || return 1
  if command -v python3 >/dev/null 2>&1 && { [ "$(uname -s)" != Darwin ] || xcode-select -p >/dev/null 2>&1; }; then
    python3 -I - "$1" <<'PY' 2>/dev/null
import json, sys
try:
    configs = json.load(open(sys.argv[1], encoding="utf-8")).get("pluginConfigs", {})
except Exception:
    sys.exit(1)
found = [v for k, v in sorted(configs.items()) if k.startswith("anet@") and isinstance(v, dict)]
if not found:
    sys.exit(1)
entry = found[0].get("options") if len(found) == 1 else None
entry = entry if isinstance(entry, dict) else {}
for key in ("anetPath", "identity"):
    value = entry.get(key, "")
    print(value if isinstance(value, str) and "\n" not in value and "\r" not in value else "")
PY
    return $?
  fi
  if command -v plutil >/dev/null 2>&1; then
    # Same rule as above: an entry of exactly one marketplace is read, and only string values count.
    found=
    count=0
    for market in agentnetwork codebuddy-plugins-official; do
      if plutil -extract "pluginConfigs.anet@$market" json -o /dev/null "$1" >/dev/null 2>&1; then
        found=$market
        count=$((count + 1))
      fi
    done
    [ "$count" -gt 0 ] || return 1
    for key in anetPath identity; do
      value=
      if [ "$count" -eq 1 ] && [ "$(plutil -type "pluginConfigs.anet@$found.options.$key" "$1" 2>/dev/null)" = string ]; then
        value=$(plutil -extract "pluginConfigs.anet@$found.options.$key" raw -o - "$1" 2>/dev/null) || value=
      fi
      # A value with a line break is not a valid path or name.
      case $value in
        *"
"*|*"$(printf '\r')"*) value= ;;
      esac
      printf '%s\n' "$value"
    done
    return 0
  fi
  return 1
}

# read_settings prints the two lines from the user settings of the host: the
# directory that WORKBUDDY_CONFIG_DIR or CODEBUDDY_CONFIG_DIR names when one is
# set, otherwise ~/.codebuddy or ~/.workbuddy. The runner cannot tell which of
# the two hosts started it, so when both files hold options of this plugin and
# the options differ, nothing is selected (two empty lines). It prints nothing
# when no settings file holds an option of this plugin.
read_settings() {
  if [ -n "${WORKBUDDY_CONFIG_DIR:-}" ]; then
    read_settings_file "$WORKBUDDY_CONFIG_DIR/settings.json"
  elif [ -n "${CODEBUDDY_CONFIG_DIR:-}" ]; then
    read_settings_file "$CODEBUDDY_CONFIG_DIR/settings.json"
  else
    from_codebuddy=$(read_settings_file "${HOME:-}/.codebuddy/settings.json") && have_codebuddy=1 || have_codebuddy=0
    from_workbuddy=$(read_settings_file "${HOME:-}/.workbuddy/settings.json") && have_workbuddy=1 || have_workbuddy=0
    if [ "$have_codebuddy" -eq 1 ] && [ "$have_workbuddy" -eq 1 ] && [ "$from_codebuddy" != "$from_workbuddy" ]; then
      printf '\n\n'
    elif [ "$have_codebuddy" -eq 1 ]; then
      printf '%s\n' "$from_codebuddy"
    elif [ "$have_workbuddy" -eq 1 ]; then
      printf '%s\n' "$from_workbuddy"
    fi
  fi
  return 0
}

if [ "${ANET_PLUGIN_SETTINGS:-}" = env ]; then
  anet_path=${ANET_PLUGIN_BIN:-}
  identity=${ANET_PLUGIN_IDENTITY:-}
else
  settings=$(read_settings)
  anet_path=$(printf '%s\n' "$settings" | sed -n 1p)
  identity=$(printf '%s\n' "$settings" | sed -n 2p)
fi

find_anet() {
  if [ -n "$anet_path" ]; then
    # A relative path would be resolved against the working directory of the session,
    # which can be an untrusted workspace; only an absolute path is accepted.
    case $anet_path in
      /*) ;;
      *)
        printf 'The plugin setting anetPath names %s, which is not an absolute path. Correct or clear the setting.\n' "$anet_path" >&2
        return 1
        ;;
    esac
    if [ -f "$anet_path" ] && [ -x "$anet_path" ]; then
      printf '%s\n' "$anet_path"
      return 0
    fi
    printf 'The plugin setting anetPath names %s, which is not an executable file. Correct or clear the setting.\n' "$anet_path" >&2
    return 1
  fi
  found=$(command -v anet 2>/dev/null) || found=
  if [ -n "$found" ] && [ -f "$found" ] && [ -x "$found" ]; then
    printf '%s\n' "$found"
    return 0
  fi
  for dir in "${HOME:-}/.local/bin" /usr/local/bin /opt/homebrew/bin; do
    if [ -f "$dir/anet" ] && [ -x "$dir/anet" ]; then
      printf '%s\n' "$dir/anet"
      return 0
    fi
  done
  printf '%s\n' "anet is not installed (searched PATH, ~/.local/bin, /usr/local/bin and /opt/homebrew/bin)." \
    "Ask the agent to run the anet-setup skill of the anet plugin, or see https://docs.agentnetwork.org.cn/docs/en/latest/intro/install.html" >&2
  return 1
}

if [ "${1:-}" = --resolve ]; then
  if bin=$(find_anet 2>/dev/null); then
    printf 'executable: %s\n' "$bin"
    printf 'version: %s\n' "$("$bin" --version 2>/dev/null | head -n 1)"
  elif [ -n "$anet_path" ]; then
    printf 'executable: invalid setting anetPath=%s\n' "$anet_path"
  else
    printf 'executable: not found\n'
  fi
  printf 'identity: %s\n' "${identity:-default}"
  # The path is quoted when it holds a character that the shell would split or expand
  # (for example a space in "Application Support"), because the agent runs this line.
  case $0 in
    *[!A-Za-z0-9_./:@%+=,-]*) runner_path="'$(printf '%s' "$0" | sed "s/'/'\\\\''/g")'" ;;
    *) runner_path=$0 ;;
  esac
  printf 'runner: sh %s\n' "$runner_path"
  exit 0
fi

bin=$(find_anet) || exit 1
if [ -n "$identity" ]; then
  exec "$bin" --id "$identity" "$@"
fi
exec "$bin" "$@"
