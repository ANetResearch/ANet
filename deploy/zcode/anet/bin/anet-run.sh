#!/bin/sh
# Runs the anet executable that the plugin settings select, for the identity
# that they select: `sh anet-run.sh <arguments>` is `anet [--id <name>]
# <arguments>`. The MCP launcher and the skills use this script, so that every
# command reaches the same executable and the same node.
#
# Settings. The MCP server receives them from ZCode in ANET_PLUGIN_BIN and
# ANET_PLUGIN_IDENTITY, with ANET_PLUGIN_SETTINGS=env. In any other context the
# script reads the two settings of this plugin, and nothing else, from the
# ZCode configuration (~/.zcode/cli/config.json, plugins.options."anet@...").
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

zcode_config=${ZCODE_CONFIG_FILE:-${HOME:-}/.zcode/cli/config.json}

# read_settings prints two lines, anetPath and identity, from the ZCode
# configuration. It prints nothing when the file or a JSON reader is missing.
read_settings() {
  [ -r "$zcode_config" ] || return 0
  if command -v python3 >/dev/null 2>&1 && { [ "$(uname -s)" != Darwin ] || xcode-select -p >/dev/null 2>&1; }; then
    python3 -I - "$zcode_config" <<'PY' 2>/dev/null
import json, sys
try:
    options = json.load(open(sys.argv[1], encoding="utf-8")).get("plugins", {}).get("options", {})
except Exception:
    sys.exit(0)
found = [v for k, v in sorted(options.items()) if k.startswith("anet@") and isinstance(v, dict)]
entry = found[0] if len(found) == 1 else {}
for key in ("anetPath", "identity"):
    value = entry.get(key, "")
    print(value if isinstance(value, str) else "")
PY
    return 0
  fi
  if command -v plutil >/dev/null 2>&1; then
    for market in zcode-plugins-official agentnetwork; do
      if plutil -extract "plugins.options.anet@$market" json -o /dev/null "$zcode_config" >/dev/null 2>&1; then
        plutil -extract "plugins.options.anet@$market.anetPath" raw -o - "$zcode_config" 2>/dev/null || echo
        plutil -extract "plugins.options.anet@$market.identity" raw -o - "$zcode_config" 2>/dev/null || echo
        return 0
      fi
    done
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
    if [ -f "$anet_path" ] && [ -x "$anet_path" ]; then
      printf '%s\n' "$anet_path"
      return 0
    fi
    printf 'The plugin setting anetPath names %s, which is not an executable file. Correct or clear the setting.\n' "$anet_path" >&2
    return 1
  fi
  if command -v anet >/dev/null 2>&1; then
    command -v anet
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
  printf 'runner: sh %s\n' "$0"
  exit 0
fi

bin=$(find_anet) || exit 1
if [ -n "$identity" ]; then
  exec "$bin" --id "$identity" "$@"
fi
exec "$bin" "$@"
