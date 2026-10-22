#!/bin/sh
# Runs the anet executable that the plugin settings select, for the identity
# that they select: `sh anet-run.sh <arguments>` is `anet [--id <name>]
# <arguments>`. The MCP launcher and the skills use this script, so that every
# command reaches the same executable and the same node.
#
# Settings. The MCP server receives them from Claude Code in ANET_PLUGIN_BIN
# and ANET_PLUGIN_IDENTITY, with ANET_PLUGIN_SETTINGS=env. In any other
# context the script reads the two options of this plugin, and nothing else,
# from the user settings of Claude Code (settings.json in CLAUDE_CONFIG_DIR,
# default ~/.claude, key pluginConfigs."anet@...".options).
#
# Executable. A configured anetPath is used as it is and is never replaced by
# another copy. Without it, PATH and the install locations of the anet
# installer and Homebrew are searched, because a desktop application does not
# always inherit the PATH of a login shell. In a POSIX shell on Windows (Git
# Bash, MSYS2, Cygwin) a drive path is also absolute, and the install locations
# of the Windows installer are searched for anet.exe; anet-run.cmd is the
# runner for PowerShell and cmd.
#
# `sh anet-run.sh --resolve` prints the selected executable, version and
# identity, and the command line of this script, and always exits 0. Errors go
# to stderr with exit status 1.
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

claude_settings=${CLAUDE_CONFIG_DIR:-${HOME:-}/.claude}/settings.json

# read_settings prints two lines, anetPath and identity, from the user settings
# of Claude Code. It prints nothing when the file or a JSON reader is missing.
read_settings() {
  [ -r "$claude_settings" ] || return 0
  if command -v python3 >/dev/null 2>&1 && { [ "$(uname -s)" != Darwin ] || xcode-select -p >/dev/null 2>&1; }; then
    python3 -I - "$claude_settings" <<'PY' 2>/dev/null
import json, sys
try:
    configs = json.load(open(sys.argv[1], encoding="utf-8")).get("pluginConfigs", {})
except Exception:
    sys.exit(0)
found = [v.get("options") for k, v in sorted(configs.items()) if k.startswith("anet@") and isinstance(v, dict)]
entry = found[0] if len(found) == 1 and isinstance(found[0], dict) else {}
for key in ("anetPath", "identity"):
    value = entry.get(key, "")
    print(value if isinstance(value, str) and "\n" not in value and "\r" not in value else "")
PY
    return 0
  fi
  if command -v plutil >/dev/null 2>&1; then
    # Only string values count; an error message of plutil is never used as a value.
    if plutil -extract "pluginConfigs.anet@agentnetwork.options" json -o /dev/null "$claude_settings" >/dev/null 2>&1; then
      for key in anetPath identity; do
        value=
        if [ "$(plutil -type "pluginConfigs.anet@agentnetwork.options.$key" "$claude_settings" 2>/dev/null)" = string ]; then
          value=$(plutil -extract "pluginConfigs.anet@agentnetwork.options.$key" raw -o - "$claude_settings" 2>/dev/null) || value=
        fi
        # A value with a line break is not a valid path or name.
        case $value in
          *"
"*|*"$(printf '\r')"*) value= ;;
        esac
        printf '%s\n' "$value"
      done
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

case $(uname -s 2>/dev/null) in
  MINGW* | MSYS* | CYGWIN*) on_windows=1 ;;
  *) on_windows=0 ;;
esac

find_anet() {
  if [ -n "$anet_path" ]; then
    # A relative path would be resolved against the working directory of the session,
    # which can be an untrusted workspace; only an absolute path is accepted.
    case $anet_path in
      /*) ;;
      [A-Za-z]:[\\/]*) [ "$on_windows" -eq 1 ] || {
          printf 'The plugin setting anetPath names %s, which is not an absolute path. Correct or clear the setting.\n' "$anet_path" >&2
          return 1
        } ;;
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
  # A relative directory of PATH (such as ".") would find a file of the working directory.
  case $found in
    /*) ;;
    *) found= ;;
  esac
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
  if [ "$on_windows" -eq 1 ]; then
    for dir in "${LOCALAPPDATA:-}/Programs/anet" "${LOCALAPPDATA:-}/anet/bin" "${USERPROFILE:-}/.anet-bin"; do
      case $dir in
        /* | [A-Za-z]:[\\/]*) ;;
        *) continue ;;
      esac
      if [ -f "$dir/anet.exe" ]; then
        printf '%s\n' "$dir/anet.exe"
        return 0
      fi
    done
  fi
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
