#!/bin/sh
# Starts the MCP server of the local anet node (`anet mcp`, stdio).
#
# A desktop application does not always inherit the PATH of a login shell
# (macOS in particular), so the default install locations of the anet
# installer are searched as well. The server forwards every call to the
# running daemon of the selected identity and exits with status 1, with the
# reason on stderr, when no daemon runs for it.
set -u

find_anet() {
  if [ -n "${ANET_PLUGIN_BIN:-}" ]; then
    if [ -x "$ANET_PLUGIN_BIN" ]; then
      printf '%s\n' "$ANET_PLUGIN_BIN"
      return 0
    fi
    printf 'The configured anet executable is not an executable file: %s\n' "$ANET_PLUGIN_BIN" >&2
    return 1
  fi
  if command -v anet >/dev/null 2>&1; then
    command -v anet
    return 0
  fi
  for dir in "${HOME:-}/.local/bin" /usr/local/bin /opt/homebrew/bin; do
    if [ -x "$dir/anet" ]; then
      printf '%s\n' "$dir/anet"
      return 0
    fi
  done
  printf '%s\n' "anet is not installed (searched PATH, ~/.local/bin, /usr/local/bin and /opt/homebrew/bin)." \
    "Ask the agent to run the anet-setup skill of the anet plugin, or see https://docs.agentnetwork.org.cn/docs/en/latest/intro/install.html" >&2
  return 1
}

bin=$(find_anet) || exit 1
if [ -n "${ANET_PLUGIN_IDENTITY:-}" ]; then
  exec "$bin" --id "$ANET_PLUGIN_IDENTITY" mcp
fi
exec "$bin" mcp
