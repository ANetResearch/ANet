#!/bin/sh
# Starts the MCP server of the local anet node (`anet mcp`, stdio) for the
# executable and identity of the plugin settings; see anet-run.sh.
#
# `anet mcp` forwards every call to the running daemon of the selected identity
# and exits with status 1, with the reason on stderr, when no daemon runs for
# it. Claude Code remembers a MCP server whose start failed and does not start
# it again in later sessions for a while, so a plugin that is installed before
# anet (the usual order) would stay without tools after anet has been set up.
# This launcher therefore never fails at the start:
#
#   - When the server can start, it is started as it is: `exec anet mcp`.
#   - Otherwise the launcher stays connected as a minimal MCP server with one
#     tool, `connect`, and with instructions that name the reason and the setup
#     skill. As soon as `anet mcp` can start (at the next request, or when the
#     agent calls `connect`), the launcher replays the handshake to it, tells
#     the client that the list of tools has changed, and forwards the rest of
#     the session unchanged.
#
# When the input ends in this waiting state the launcher exits with status 1
# and the reason on stderr, as `anet mcp` does.
set -u
ANET_PLUGIN_SETTINGS=env
export ANET_PLUGIN_SETTINGS
runner="$(dirname "$0")/anet-run.sh"

# `anet mcp` with a closed input exits with status 0 when the daemon answers and
# with status 1 otherwise. The reason is kept in $reason.
probe() {
  reason=$(sh "$runner" mcp </dev/null 2>&1 >/dev/null)
}

if probe; then
  exec sh "$runner" mcp
fi
printf '%s\n' "$reason" >&2

# JSON string content (without the quotes) of the text on standard input.
# LC_ALL=C keeps tr and sed byte-oriented, so that text that is not valid in the
# current locale (BSD tools) cannot make them fail.
json_text() {
  LC_ALL=C tr -d '\000-\010\013-\037' | LC_ALL=C tr '\t' ' ' | LC_ALL=C sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' |
    LC_ALL=C awk 'NR > 1 { printf "%s", "\\n" } { printf "%s", $0 }'
}

# Four lines for the JSON-RPC request on standard input: the "method" and the
# "id" of the top-level object (the id as written, a number or a quoted string),
# the "name" of its "params" and the "protocolVersion" of its "params". A key of
# a nested object, for example the arguments of a tool call, is never taken for
# one of these. Lines that are not objects give four empty lines.
request_fields() {
  LC_ALL=C awk '
    function value(raw, text) {
      if (depth == 1 && key[1] == "id") id = raw
      else if (depth == 1 && key[1] == "method") method = text
      else if (depth == 2 && key[1] == "params" && key[2] == "name") name = text
      else if (depth == 2 && key[1] == "params" && key[2] == "protocolVersion") version = text
    }
    {
      s = $0; n = length(s); depth = 0; i = 1
      while (i <= n) {
        c = substr(s, i, 1)
        if (c == "\"") {
          j = i + 1; text = ""
          while (j <= n) {
            d = substr(s, j, 1)
            if (d == "\\") { text = text substr(s, j, 2); j += 2; continue }
            if (d == "\"") break
            text = text d; j++
          }
          raw = substr(s, i, j - i + 1)
          k = j + 1
          while (k <= n && substr(s, k, 1) == " ") k++
          if (substr(s, k, 1) == ":") { key[depth] = text; i = k + 1 } else { value(raw, text); i = j + 1 }
        } else if (c == "{" || c == "[") { depth++; i++ }
        else if (c == "}" || c == "]") { depth--; i++ }
        else if (c ~ /[-0-9]/) {
          j = i
          while (j <= n && substr(s, j, 1) ~ /[-+.eE0-9]/) j++
          raw = substr(s, i, j - i); value(raw, raw); i = j
        } else i++
      }
      print method; print id; print name; print version
    }
    BEGIN { method = ""; id = ""; name = ""; version = "" }
  '
}

reply() { printf '{"jsonrpc":"2.0","id":%s,"result":%s}\n' "$1" "$2"; }

# The text of a failed or waiting state, as JSON string content.
state_text() {
  {
    printf 'anet is not ready: %s\n' "$reason"
    printf 'Ask the user to set up anet (the anet-setup skill of the anet plugin installs it, starts the node and joins a hub). '
    printf 'Then call the connect tool of this server; the anet tools become available without a new session.\n'
  } | json_text
}

# Replays the handshake to the real server, answers nothing of it twice, and
# forwards the rest of the session. $1 is a request that is still unanswered.
handoff() {
  {
    [ -n "$init_line" ] && printf '%s\n' "$init_line"
    [ -n "$inited_line" ] && printf '%s\n' "$inited_line"
    [ -n "${1:-}" ] && printf '%s\n' "$1"
    exec cat
  } | sh "$runner" mcp | {
    IFS= read -r _skipped_initialize_response
    exec cat
  }
  exit 0
}

init_line=
inited_line=
while IFS= read -r line; do
  [ -n "$line" ] || continue
  fields=$(printf '%s\n' "$line" | request_fields)
  method=$(printf '%s\n' "$fields" | sed -n 1p)
  id=$(printf '%s\n' "$fields" | sed -n 2p)
  name=$(printf '%s\n' "$fields" | sed -n 3p)
  version=$(printf '%s\n' "$fields" | sed -n 4p)
  case $method in
    initialize)
      init_line=$line
      reply "$id" "{\"protocolVersion\":\"${version:-2025-03-26}\",\"capabilities\":{\"tools\":{\"listChanged\":true}},\"serverInfo\":{\"name\":\"anet\",\"version\":\"launcher\"},\"instructions\":\"$(state_text)\"}"
      ;;
    notifications/initialized)
      inited_line=$line
      ;;
    ping)
      reply "$id" '{}'
      ;;
    tools/list)
      if probe; then handoff "$line"; fi
      reply "$id" "{\"tools\":[{\"name\":\"connect\",\"description\":\"Connects this server to the local anet node. The anet node is not reachable yet: install and start anet first (the anet-setup skill does this), then call this tool. The anet tools are available afterwards.\",\"inputSchema\":{\"type\":\"object\",\"properties\":{}}}]}"
      ;;
    tools/call)
      if probe; then
        if [ "$name" = connect ]; then
          reply "$id" '{"content":[{"type":"text","text":"Connected to the anet node. The anet tools are available now."}]}'
          printf '{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}\n'
          handoff ""
        fi
        handoff "$line"
      fi
      reply "$id" "{\"content\":[{\"type\":\"text\",\"text\":\"$(state_text)\"}],\"isError\":true}"
      ;;
    *)
      if [ -n "$id" ]; then
        printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}\n' "$id"
      fi
      ;;
  esac
done
exit 1
