#!/bin/sh
# Installs the anet bundle for Trae and Trae CN on Linux and macOS (on
# Windows: install.cmd).
#
# Trae has no plugin package format. This script copies the parts of the
# bundle into a configuration directory of Trae (the skills, the command and
# the launcher of the MCP server) and prints the MCP configuration that Trae
# needs. It starts no process, opens no connection, and reads no file of Trae.
# The only file of Trae that it creates is a project mcp.json that does not
# exist yet; it never changes an existing one.
#
# Run `sh install.sh --help` for the options.
set -eu

SERVER={{SERVER_NAME}}
RUN_TIMEOUT_MS={{MCP_TIMEOUT_MS}}

usage() {
  cat <<'EOF'
Usage: sh install.sh [--project DIR] [--cn] [--identity NAME] [--anet PATH] [--remove]

Installs the anet bundle for Trae into the user directory ~/.trae (with --cn,
for Trae CN: ~/.trae-cn) or, with --project, into DIR/.trae.

  --project DIR    install into the project DIR (project scope)
  --cn             use the user directory ~/.trae-cn of Trae CN
  --identity NAME  the named anet identity (anet id ls) that the plugin uses;
                   --identity '' selects the default identity again
  --anet PATH      absolute path of the anet executable; --anet '' searches
                   PATH, ~/.local/bin, /usr/local/bin and /opt/homebrew/bin again
  --remove         remove the files that this script installed
  -h, --help       show this text

Without --identity and --anet, the settings of an earlier installation are kept.
EOF
}

die() {
  printf 'install.sh: %s\n' "$*" >&2
  exit 1
}

here=$(cd "$(dirname "$0")" && pwd) || die "cannot locate the bundle"
project=
cn=0
remove=0
set_id=0
set_bin=0
identity=
anet_path=

while [ $# -gt 0 ]; do
  case $1 in
    --project)
      [ $# -ge 2 ] || die "--project needs a directory"
      project=$2
      shift 2
      ;;
    --cn)
      cn=1
      shift
      ;;
    --identity)
      [ $# -ge 2 ] || die "--identity needs a name"
      identity=$2
      set_id=1
      shift 2
      ;;
    --anet)
      [ $# -ge 2 ] || die "--anet needs a path"
      anet_path=$2
      set_bin=1
      shift 2
      ;;
    --remove)
      remove=1
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      die "unknown option: $1"
      ;;
  esac
done

case $(uname -s) in
  Linux | Darwin) ;;
  *) die "this script installs the bundle on Linux and macOS; on Windows, run install.cmd of this bundle" ;;
esac

# Scope and target directory.
if [ -n "$project" ]; then
  [ "$cn" -eq 0 ] || die "--cn selects the user directory of Trae CN and cannot be combined with --project"
  [ -d "$project" ] || die "the project directory does not exist: $project"
  project=$(cd "$project" && pwd) || die "cannot enter $project"
  root=$project/.trae
  launcher='${workspaceFolder}/.trae/anet/bin/anet-mcp.sh'
else
  [ -n "${HOME:-}" ] || die "HOME is not set"
  if [ "$cn" -eq 1 ]; then root=$HOME/.trae-cn; else root=$HOME/.trae; fi
  launcher=$root/anet/bin/anet-mcp.sh
  # Trae rejects a command with spaces, and the path is written into JSON.
  case $launcher in
    *[[:cntrl:][:space:]\"\\]*) die "the path $launcher contains a space, a control character, a quote or a backslash, whose handling Trae does not document for an MCP server; use --project, or set HOME to another path" ;;
  esac
fi

FILES="skills/anet/SKILL.md skills/anet-setup/SKILL.md commands/anet.md commands/anet-status.md
anet/bin/anet-run.sh anet/bin/anet-mcp.sh anet/bin/anet-mcp anet/bin/anet-install.sh
anet/bin/anet-run.cmd anet/bin/anet-run.ps1 anet/bin/anet-mcp.cmd anet/bin/anet-install.cmd"

# Nothing is written or removed through a symbolic link: a project can hold a link
# under .trae, and a file or directory of the bundle that is a link could lead out of it.
no_link() {
  link_path=$root
  link_rest=$1
  while [ -n "$link_rest" ]; do
    link_seg=${link_rest%%/*}
    if [ "$link_seg" = "$link_rest" ]; then link_rest=; else link_rest=${link_rest#*/}; fi
    link_path=$link_path/$link_seg
    [ ! -L "$link_path" ] || die "$link_path is a symbolic link; this script does not write or remove through links"
  done
}
[ -z "$project" ] || [ ! -L "$root" ] || die "$root is a symbolic link; this script does not write or remove through links"
for f in $FILES anet/settings; do
  no_link "$f"
done
[ -z "$project" ] || no_link mcp.json

# An existing file of the same name that is not part of this bundle is neither replaced nor removed.
check_owner() {
  [ -f "$root/$1" ] || return 0
  grep -q "$2" "$root/$1" || die "$root/$1 exists and is not part of the anet bundle; move it away first"
}
check_owner skills/anet/SKILL.md '^name: anet$'
check_owner skills/anet-setup/SKILL.md '^name: anet-setup$'
check_owner commands/anet.md 'AgentNetwork (anet) node'
check_owner commands/anet-status.md 'local anet node'
check_owner anet/bin/anet-run.sh 'Runs the anet executable'
check_owner anet/bin/anet-mcp.sh 'Starts the MCP server of the local anet node'
check_owner anet/bin/anet-mcp 'Starts the MCP server of the local anet node'
check_owner anet/bin/anet-install.sh 'Installs the anet command line'
check_owner anet/bin/anet-run.cmd 'Runs the anet executable'
check_owner anet/bin/anet-run.ps1 'The Windows runner of the anet plugin'
check_owner anet/bin/anet-mcp.cmd 'Starts the MCP server of the local anet node'
check_owner anet/bin/anet-install.cmd 'Installs the anet command line'

if [ "$remove" -eq 1 ]; then
  [ "$set_id" -eq 0 ] && [ "$set_bin" -eq 0 ] || die "--remove takes no settings"
  for f in $FILES anet/settings; do
    rm -f "$root/$f"
  done
  for d in skills/anet skills/anet-setup anet/bin anet commands skills; do
    rmdir "$root/$d" 2>/dev/null || true
  done
  # Only an empty directory is removed; a directory that holds other files stays.
  rmdir "$root" 2>/dev/null || true
  printf 'Removed the anet files from %s.\n' "$root"
  printf 'Delete the MCP server "%s" in Trae (Settings, MCP)' "$SERVER"
  if [ -n "$project" ]; then
    printf ' or remove it from %s/mcp.json' "$root"
  fi
  printf ', and start a new session.\n'
  exit 0
fi

for f in $FILES; do
  [ -f "$here/$f" ] || die "the bundle is incomplete: $f is missing next to install.sh"
done

# Settings: an option that is not given keeps the value of the earlier installation.
settings=$root/anet/settings
if [ -r "$settings" ]; then
  [ "$set_bin" -eq 1 ] || anet_path=$(sed -n 's/^anetPath=//p' "$settings" | sed -n 1p)
  [ "$set_id" -eq 1 ] || identity=$(sed -n 's/^identity=//p' "$settings" | sed -n 1p)
fi
case $identity in
  *[!A-Za-z0-9._-]*) die "the identity name may contain only letters, digits, dot, underscore and hyphen: $identity" ;;
esac
if [ -n "$anet_path" ]; then
  case $anet_path in
    /*) ;;
    *) die "--anet needs an absolute path: $anet_path" ;;
  esac
  case $anet_path in
    *[[:cntrl:][:space:]\"\\]*) die "the anet path contains a space, a control character, a quote or a backslash: $anet_path" ;;
  esac
fi

for f in $FILES; do
  mkdir -p "$root/$(dirname "$f")"
  cp "$here/$f" "$root/$f"
done
if [ -n "$anet_path" ] || [ -n "$identity" ] || [ -e "$settings" ]; then
  printf 'anetPath=%s\nidentity=%s\n' "$anet_path" "$identity" >"$settings"
fi

# The MCP configuration.
env_pretty=$(printf '        "RUN_MCP_TIMEOUT_MS": "%s"' "$RUN_TIMEOUT_MS")
env_compact=$(printf '"RUN_MCP_TIMEOUT_MS":"%s"' "$RUN_TIMEOUT_MS")
if [ -n "$anet_path" ]; then
  env_pretty=$(printf '%s,\n        "ANET_PLUGIN_BIN": "%s"' "$env_pretty" "$anet_path")
  env_compact=$(printf '%s,"ANET_PLUGIN_BIN":"%s"' "$env_compact" "$anet_path")
fi
if [ -n "$identity" ]; then
  env_pretty=$(printf '%s,\n        "ANET_PLUGIN_IDENTITY": "%s"' "$env_pretty" "$identity")
  env_compact=$(printf '%s,"ANET_PLUGIN_IDENTITY":"%s"' "$env_compact" "$identity")
fi
pretty=$(printf '{\n  "mcpServers": {\n    "%s": {\n      "command": "/bin/sh",\n      "args": [\n        "%s"\n      ],\n      "env": {\n%s\n      }\n    }\n  }\n}' \
  "$SERVER" "$launcher" "$env_pretty")

printf 'Installed the anet bundle into %s:\n' "$root"
for f in $FILES; do
  printf '  %s\n' "$f"
done
if [ -e "$settings" ]; then
  printf '  anet/settings (anetPath=%s, identity=%s)\n' "${anet_path:-<search>}" "${identity:-<default>}"
fi
printf '\n'

if [ -n "$project" ]; then
  if [ -e "$root/mcp.json" ]; then
    printf '%s/mcp.json exists and was not changed. Add the member "%s" of this object to its "mcpServers":\n\n%s\n\n' "$root" "$SERVER" "$pretty"
  else
    printf '%s\n' "$pretty" >"$root/mcp.json"
    printf 'Wrote %s/mcp.json:\n\n%s\n\n' "$root" "$pretty"
  fi
  printf 'In Trae, open the project, turn on "Project MCP" in Settings, MCP, and confirm the prompt.\n'
else
  printf 'Register the MCP server in Trae. Open Settings, MCP, Add, Create Manually (or Raw Config) and enter:\n\n%s\n\n' "$pretty"
  if [ "$cn" -eq 0 ] && command -v base64 >/dev/null 2>&1; then
    compact=$(printf '{"command":"/bin/sh","args":["%s"],"env":{%s}}' "$launcher" "$env_compact")
    encoded=$(printf '%s' "$compact" | base64 | tr -d '\n' | sed -e 's/+/%2B/g' -e 's#/#%2F#g' -e 's/=/%3D/g')
    printf 'Alternatively open this link in a browser and confirm the prompts of Trae:\n\n  trae://trae.ai-ide/mcp-import?type=stdio&name=%s&config=%s\n\n' "$SERVER" "$encoded"
  fi
fi
printf 'Then start a new session in Trae and ask: Set up anet.\n'
