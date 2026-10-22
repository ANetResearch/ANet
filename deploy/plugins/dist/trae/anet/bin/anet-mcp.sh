#!/bin/sh
# Starts the MCP server of the local anet node (`anet mcp`, stdio) for the
# executable and identity of the plugin settings; see anet-run.sh.
#
# When the runner finds no anet executable, anet-install.sh --mcp installs it
# first with the official installer (which also starts the node and joins the
# public hub), unless ANET_PLUGIN_NO_INSTALL=1 is set; see that script. All of
# its output goes to standard error, because standard output is the MCP
# session. `anet mcp` then starts the node of the selected identity when it is
# not running, and serves the tools even when the node cannot run: node_status
# then names the reason and the command that fixes it.
set -u

# Programs (sh, sed, uname and the others) are found only in the absolute
# directories of PATH: a relative entry (such as "." or an empty one) would
# find a file of the working directory of the session, which can be an
# untrusted workspace. This comes before the first command that is not built
# into the shell, and the scripts started from here inherit it.
_abs=
_ifs=$IFS
IFS=:
set -f
for _d in ${PATH:-}; do
  case $_d in
    /*) _abs=${_abs:+$_abs:}$_d ;;
  esac
done
IFS=$_ifs
set +f
PATH=${_abs:-/usr/bin:/bin}
export PATH
unset _abs _ifs _d

ANET_PLUGIN_SETTINGS=env
export ANET_PLUGIN_SETTINGS
sh "$(dirname "$0")/anet-install.sh" --mcp </dev/null >&2
exec sh "$(dirname "$0")/anet-run.sh" mcp
