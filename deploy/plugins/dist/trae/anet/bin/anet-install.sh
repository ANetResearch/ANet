#!/bin/sh
# Installs the anet command line with the official installer.
#
# `sh anet-install.sh [installer arguments]` downloads https://agentnetwork.org.cn/install.sh
# into a new private directory, runs it only when the download succeeded, and
# removes the directory afterwards. The installer checks the signed release
# manifest and the SHA-256 of the binary and stops without changing anything
# when a check fails; by default it installs anet for this user, starts the
# node and joins the public hub. The script exists so that an agent host that
# refuses command substitution in a shell command can still run the
# installation as one plain command (the anet-setup skill).
#
# `sh anet-install.sh --mcp` is the first step of the MCP launcher
# (anet-mcp.sh): it installs anet when the runner finds no anet executable, so
# that enabling the plugin is enough to have anet installed and its node
# running. It does nothing when the environment variable ANET_PLUGIN_NO_INSTALL
# is set to a value other than 0, when the plugin setting anetPath is set (an
# invalid one is reported by the runner, and no other copy is installed), and
# in a POSIX shell on Windows (anet-mcp.cmd installs there). Otherwise it runs
# the installer without arguments: nothing from the workspace or the session
# reaches it, the user's settings select nothing of it either. The installer
# runs
#   * with no input and no controlling terminal (in a session of its own, with
#     setsid or Perl where available), so that it never asks for a password
#     on the terminal of the host, and never reads the MCP session;
#   * in the user's home directory, not in the working directory of the
#     session (a workspace);
#   * with its output written to install.log in the state directory, which is
#     copied to standard error afterwards (the launcher sends this script's
#     output to standard error: standard output is the MCP session). Because
#     the installer does not write to the host's pipe and has a session of
#     its own, a host that gives up on a slow start does not end it halfway;
#     it finishes, and the next session finds anet.
# It waits for the installer at most 15 minutes, and exits with status 0 in
# every case: the launcher goes on, and the runner reports anet as missing
# when the installation did not succeed.
#
# One installation at a time. Both forms hold the lock directory
# <state>/install.lock while the installer runs, with the process ID of the
# holder and its start time (as ps prints it) in the file pid; <state> is
# $XDG_CACHE_HOME/anet-plugin, or ~/.cache/anet-plugin (an absolute path only;
# without one the --mcp form installs nothing). Another host that starts at
# the same moment waits for it, at most 10 minutes (5 for the plain form), and
# then installs nothing itself when the first installation left an anet
# behind. A lock whose holder no longer runs (no process with that ID, or one
# that started at another time: the ID was reused), or that is older than 30
# minutes, is removed.
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

here=$(cd "$(dirname "$0")" 2>/dev/null && pwd) || here=$(dirname "$0")
self="$here/anet-install.sh"

say() { printf '%s\n' "$*" >&2; }

# state_dir prints the directory of the lock and the log, or fails.
state_dir() {
  _base=${XDG_CACHE_HOME:-}
  case $_base in
    /*) ;;
    *) _base=${HOME:-}/.cache ;;
  esac
  case $_base in
    /*) printf '%s/anet-plugin\n' "$_base" ;;
    *) return 1 ;;
  esac
}

# anet_missing: whether the runner finds no anet executable and no anetPath is
# set (its --resolve prints "executable: not found" exactly then).
anet_missing() {
  [ "$(sh "$here/anet-run.sh" --resolve 2>/dev/null | sed -n 1p)" = "executable: not found" ]
}

# started PID prints when the process PID started, as one line, or nothing
# where ps cannot say.
started() {
  ps -o lstart= -p "$1" 2>/dev/null | sed -e 's/  */ /g' -e 's/^ //' -e 's/ $//' -e 1q
}

# holder_runs LINE: whether the holder that the line of a lock names ("PID
# START") still runs: a process with that ID exists and, when the line has a
# start time and ps gives one, it started then. (kill -0 sends no signal; it
# only checks that the process exists.)
holder_runs() {
  _pid=${1%% *}
  case $_pid in
    '' | *[!0-9]*) return 1 ;;
  esac
  kill -0 "$_pid" 2>/dev/null || return 1
  _was=
  case $1 in
    *" "*) _was=${1#* } ;;
  esac
  [ -n "$_was" ] || return 0
  _now=$(started "$_pid")
  [ -z "$_now" ] || [ "$_now" = "$_was" ]
}

# take_lock SECONDS: takes the lock, waiting at most SECONDS. Sets waited=1
# when another installation held it.
waited=0
take_lock() {
  _left=$1
  while :; do
    if mkdir "$lock" 2>/dev/null; then
      _me=$(started "$$")
      printf '%s\n' "$$${_me:+ $_me}" >"$lock/pid" && return 0
      rmdir "$lock" 2>/dev/null
      return 1
    fi
    _holder=$(cat "$lock/pid" 2>/dev/null) || _holder=
    _stale=0
    if [ -n "$(find "$lock" -prune -mmin +30 2>/dev/null)" ]; then
      _stale=1
    elif [ -n "$_holder" ]; then
      holder_runs "$_holder" || _stale=1
    elif [ -n "$(find "$lock" -prune -mmin +1 2>/dev/null)" ]; then
      _stale=1 # created more than a minute ago and never given a holder
    fi
    if [ "$_stale" -eq 1 ]; then
      # Moved aside first, so that of two processes that found it stale only
      # one removes it; a lock that changed holder in the meantime is put back.
      _aside="$lock.stale.$$"
      if mv "$lock" "$_aside" 2>/dev/null; then
        if [ "$(cat "$_aside/pid" 2>/dev/null)" = "$_holder" ]; then
          rm -f "$_aside/pid"
          rmdir "$_aside" 2>/dev/null
        else
          mv "$_aside" "$lock" 2>/dev/null || { rm -f "$_aside/pid"; rmdir "$_aside" 2>/dev/null; }
        fi
      fi
      continue
    fi
    [ "$_left" -gt 0 ] || return 1
    if [ "$waited" -eq 0 ]; then
      say "Another installation of anet is running; waiting for it to finish."
    fi
    waited=1
    sleep 1
    _left=$((_left - 1))
  done
}

release_lock() {
  _line=$(cat "${lock:-/nonexistent}/pid" 2>/dev/null) || _line=
  if [ -n "${lock:-}" ] && [ "${_line%% *}" = "$$" ]; then
    rm -f "$lock/pid"
    rmdir "$lock" 2>/dev/null
  fi
}

# lock_live: whether a running process holds the lock.
lock_live() {
  _h=$(cat "$lock/pid" 2>/dev/null) || _h=
  [ -n "$_h" ] && holder_runs "$_h"
}

# cleanup runs on exit: the lock of this process and the download directory.
_dir=
cleanup() {
  release_lock
  if [ -n "$_dir" ]; then
    rm -rf "$_dir"
  fi
}

# run_installer [installer arguments]: download and run the official installer.
run_installer() {
  umask 077
  _dir=$(mktemp -d 2>/dev/null) || {
    _dir=
    echo "Error: a temporary directory could not be created." >&2
    return 1
  }
  if ! curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh -o "$_dir/install.sh"; then
    echo "Error: the installer could not be downloaded." >&2
    return 1
  fi
  # The home directory, not the working directory of the session: the node
  # that the installer starts keeps its working directory.
  case ${HOME:-} in
    /*) cd "$HOME" 2>/dev/null || cd / ;;
    *) cd / ;;
  esac
  sh "$_dir/install.sh" "$@"
}

# detached COMMAND...: COMMAND in a session of its own (no controlling
# terminal), where setsid or Perl can make one; otherwise as it is.
detached() {
  if command -v setsid >/dev/null 2>&1; then
    exec setsid "$@"
  fi
  if command -v perl >/dev/null 2>&1; then
    exec perl -MPOSIX -e 'POSIX::setsid(); exec { $ARGV[0] } @ARGV or exit 127' -- "$@"
  fi
  exec "$@"
}

opted_out() {
  case ${ANET_PLUGIN_NO_INSTALL:-} in
    "" | 0) return 1 ;;
    *) return 0 ;;
  esac
}

case ${1:-} in
  --mcp)
    opted_out && exit 0
    case $(uname -s 2>/dev/null) in
      MINGW* | MSYS* | CYGWIN*) exit 0 ;;
    esac
    anet_missing || exit 0
    state=$(state_dir) || {
      say "anet is not installed, and it was not installed automatically: HOME is not an absolute path."
      exit 0
    }
    (umask 077 && mkdir -p "$state") || exit 0
    lock="$state/install.lock"
    log="$state/install.log"
    result=$(mktemp "$state/result.XXXXXX" 2>/dev/null) || exit 0
    say "anet is not installed. Installing it with the official installer (https://agentnetwork.org.cn/install.sh),"
    say "which checks the signed release manifest, installs anet for this user, starts its node and joins the public hub."
    say "Set the environment variable ANET_PLUGIN_NO_INSTALL=1 to turn this off."
    (detached sh "$self" --mcp-worker "$result" </dev/null >/dev/null 2>&1) &
    worker=$!
    left=900
    gone=0
    while [ ! -s "$result" ] && [ "$left" -gt 0 ]; do
      # The worker is gone without a result and no installation holds the
      # lock, for three seconds in a row: nothing is coming.
      if ! kill -0 "$worker" 2>/dev/null && ! lock_live; then
        gone=$((gone + 1))
        [ "$gone" -lt 3 ] || break
      else
        gone=0
      fi
      sleep 1
      left=$((left - 1))
    done
    outcome=$(cat "$result" 2>/dev/null) || outcome=
    rm -f "$result"
    case $outcome in
      installed:*)
        cat "$log" >&2 2>/dev/null
        say "The anet installer ended with status ${outcome#installed:}."
        ;;
      other) say "Another installation of anet has finished." ;;
      busy) say "Another installation of anet is still running; anet was not installed by this session." ;;
      *) say "The installation of anet has not finished; it goes on in the background (log: $log)." ;;
    esac
    exit 0
    ;;
  --mcp-worker)
    # The detached part of --mcp; $2 is the file for the outcome.
    result=${2:-}
    state=$(state_dir) || exit 0
    lock="$state/install.lock"
    log="$state/install.log"
    if ! take_lock 600; then
      printf 'busy\n' >"$result"
      exit 0
    fi
    trap cleanup EXIT
    trap 'exit 1' HUP INT TERM
    if [ "$waited" -eq 1 ] && ! anet_missing; then
      printf 'other\n' >"$result"
      exit 0
    fi
    (umask 077 && : >"$log")
    run_installer </dev/null >"$log" 2>&1
    printf 'installed:%s\n' "$?" >"$result"
    exit 0
    ;;
esac

# The plain form: the installer with the given arguments, its output on this
# terminal.
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
if state=$(state_dir) && (umask 077 && mkdir -p "$state"); then
  lock="$state/install.lock"
  if ! take_lock 300; then
    echo "Error: another installation of anet is still running; try again when it has finished." >&2
    exit 1
  fi
  if [ "$waited" -eq 1 ] && ! anet_missing; then
    say "Another installation of anet has finished. Run the runner with --resolve to see it."
    exit 0
  fi
fi
run_installer "$@"
