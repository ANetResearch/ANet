#!/usr/bin/env bash
# lib.sh — shared helpers for the role scripts (source it; don't run it).
#
# The role scripts model a tiny real network against the official Hub (or any Hub given via $HUB_URL):
#   2-provider.sh   服务方：一个后台 daemon（独立身份）登记为「能干活」的 provider，等着接单
#   3-requester.sh  委派方：一个后台 daemon（独立身份），去 find → 委派 → 多轮沟通 → 结束 → 评价
#
# 访客模式已删除（A2A-DESIGN §9）：hub 不再代陌生人发起委派，也就没有 4-guest.sh。
#
# Hub 是官方托管服务；用环境变量 HUB_URL 指定要接入的 Hub（默认官方 https://hub.agentnetwork.org.cn）。
# 这里集中放「所有角色都要的」东西，让每个角色脚本保持薄薄一层、易读。

# loopback 必须绕过系统代理（很多人开着 Clash/VPN），否则 CLI↔本地 daemon 会被劫持成 502。
export NO_PROXY="127.0.0.1,localhost${NO_PROXY:+,$NO_PROXY}"
export no_proxy="127.0.0.1,localhost${no_proxy:+,$no_proxy}"

_LIB_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLI="$_LIB_ROOT"
HUB_URL="${HUB_URL:-https://hub.agentnetwork.org.cn}"

info(){ printf '\033[1;36m%s\033[0m\n' "$*"; }
ok(){   printf '\033[1;32m%s\033[0m\n' "$*"; }
warn(){ printf '\033[1;33m%s\033[0m\n' "$*"; }
die(){  printf '\033[1;31m%s\033[0m\n' "$*" >&2; exit 1; }

# resolve the anet binary: prefer the one on PATH (installed via install.sh), else the repo build.
_resolve_anet(){
  local b; b="$(command -v anet || true)"
  if [ -z "$b" ]; then
    [ -x "$CLI/anet" ] || ( cd "$_LIB_ROOT" && ./build.sh >/dev/null )
    b="$CLI/anet"
  fi
  printf '%s' "$b"
}
# A caller that has its own binary (the joint scripts, which build one) sets ANET before sourcing, and
# nothing is looked up or built here.
ANET="${ANET:-$(_resolve_anet)}"

# a: run anet as DATA's identity (each role has its own data dir = its own AID). Requires $DATA set.
a(){ ANET_DATA_DIR="$DATA" "$ANET" "$@"; }

# json_str: encode $1 as a JSON string literal (escapes backslash + double-quote; single-line values).
json_str(){ local s=${1//\\/\\\\}; s=${s//\"/\\\"}; printf '"%s"' "$s"; }

# port_alive PORT → 0 if a daemon answers /ping on that control port.
port_alive(){ curl -s --noproxy '*' -o /dev/null "http://127.0.0.1:$1/ping" 2>/dev/null; }

# aid: print the AID of DATA's daemon (empty if it isn't reachable).
aid(){ a status 2>/dev/null | sed -n 's/.*"aid": *"\([^"]*\)".*/\1/p'; }

# prep_identity: create an isolated, EMPTY data dir for this role and pin ONLY its control port (every
# daemon defaults to 127.0.0.1:39811, so on one machine several roles would clash without this). That is
# the whole job of the test scripts — pure isolation. Identity, `anet daemon --detach`, hub registration,
# capabilities, accept — everything else is left for the AGENT to do by following {{HUB_URL}}/llms.txt.
# Requires $DATA and $PORT. Won't clobber an existing config (an agent may have added hub_url to it).
prep_identity(){
  mkdir -p "$DATA"
  [ -f "$DATA/config.json" ] || printf '{"control_addr":"127.0.0.1:%s"}\n' "$PORT" > "$DATA/config.json"
}

# print_join: hand THIS window's agent a SELF-CONTAINED one-liner. $1 = the role-specific goal clause
# appended after "…用本机的 anet 命令把我加入这个网络，". The one-liner bakes in THIS role's data dir so the
# agent binds the right identity/port on its own — it must NOT depend on the human exporting ANET_DATA_DIR
# in the agent's shell (that was fragile: without it the agent falls back to ~/.anet on the default port
# 39811, and the console URL it returns won't match the port this script pinned in config.json).
print_join(){
  local goal="$1"
  echo
  info "把下面这段原样发给这个窗口里的 Cursor / Claude Code 等 agent —— 它会自己读 llms.txt 并用【这个专属身份】加入："
  echo "────────────────────────────────────────────────────────────"
  echo "我在本机为你准备了一个专属的 anet 身份目录：${DATA}（控制端口已排好为 ${PORT}）。请在每条 anet 命令前都设置环境变量 ANET_DATA_DIR=${DATA}（用这个目录，不要用默认的 ~/.anet），这样我的身份不会和这台机器上的其它 anet 身份冲突。"
  echo
  echo "AgentNetwork 是一个 AI agent 互相委派任务、用可验证回执积累信誉的网络。请阅读 ${HUB_URL}/llms.txt，然后用本机的 anet 命令把我加入这个网络，${goal}"
  echo "────────────────────────────────────────────────────────────"
  echo
  warn "agent 会自己完成：anet daemon --detach → hub-register → anet console --url，然后把控制台网址发回给你。"
  warn "（因为已排好端口 ${PORT}，它给你的网址应是 http://127.0.0.1:${PORT}/console?hub=…）"
  warn "你自己想手动看状态/清理：先 export ANET_DATA_DIR=\"${DATA}\"，再 $0 status ｜ $0 down"
}

# daemon_down: gracefully stop this role's daemon via `anet stop` (targets DATA's own identity); if that
# can't reach it, fall back to killing whatever listens on $PORT.
daemon_down(){
  if a stop >/dev/null 2>&1; then ok "已优雅停止 daemon（anet stop，数据保留在 ${DATA}）"; return 0; fi
  local pids; pids="$(lsof -ti "tcp:$PORT" 2>/dev/null || true)"
  if [ -n "$pids" ]; then kill $pids 2>/dev/null || true; ok "已停止 :${PORT} 上的 daemon（数据保留在 ${DATA}）"; else warn "没有在 :${PORT} 上运行的 daemon"; fi
}

# print_status: a compact status card. NAME is whatever the AGENT registered (not set by the script).
print_status(){
  local id name; id="$(aid)"; name="$(a status 2>/dev/null | sed -n 's/.*"name": *"\([^"]*\)".*/\1/p')"
  printf '%-10s %-14s %-7s %s\n' "ROLE" "NAME" "PORT" "AID"
  printf '%-10s %-14s %-7s %s\n' "$ROLE" "${name:-<未注册>}" "$PORT" "${id:-<daemon未起>}"
  echo "  数据目录   $DATA"
  echo "  控制台     http://127.0.0.1:$PORT/console?hub=$HUB_URL"
}

# peer_allow DATA_DIR AID…: put AIDs on that identity's inbound allow list (DATA_DIR/peers.allow).
# The inbound policy is closed by default (A2A-DESIGN §5): a provider takes delegations only from peers
# on this list. The CLI's `anet peers allow` asks for confirmation on a terminal, which a script does
# not have; the daemon reads the file on every decision, so no restart is needed.
peer_allow(){ local d="$1"; shift; _peer_add "$d/peers.allow" "$@"; }

# peer_trust DATA_DIR AID…: the same for peers.trust — peers whose tasks may drive this identity's exec
# auto-reply (the local coding agent).
peer_trust(){ local d="$1"; shift; _peer_add "$d/peers.trust" "$@"; }

# _peer_add FILE AID…: append each AID not yet on the list, one per line. The same shape check as the daemon's
# own writer (validAIDSyntax: 8–256 printable ASCII, no space, no '#'), because a stray argument or a '#' would
# otherwise land in the file as a line the daemon reads differently. A last line without its newline would
# glue onto the appended AID, so one is added first. New files are 0600, as the daemon writes them.
_peer_add(){
  local f="$1"; shift; local x
  for x in "$@"; do
    case "$x" in *[!!-~]*|*'#'*) die "不是 AID：$x" ;; esac
    [ "${#x}" -ge 8 ] && [ "${#x}" -le 256 ] || die "不是 AID：$x"
  done
  ( umask 077; touch "$f" ) || die "写不了 $f"
  [ -z "$(tail -c1 "$f")" ] || echo >> "$f"
  for x in "$@"; do grep -qxF "$x" "$f" || printf '%s\n' "$x" >> "$f"; done
}

# pin_a2a DATA_DIR PORT: where that identity's local A2A interface (module/a2a, on by default) listens:
# 127.0.0.1:PORT. Unpinned, its first start takes the first free loopback port from 43811
# (module/a2a/addr.go allocate) — outside the port block a run was given, and on the test hosts outside
# 47100-47499 (docs/notes/0015). This is the file the module itself records the address in; an address
# written here is bound as it stands, and a port taken by someone else fails the start rather than moving.
pin_a2a(){ ( umask 077; mkdir -p "$1/modules/a2a" && printf '127.0.0.1:%s\n' "$2" > "$1/modules/a2a/a2a_addr.txt" ); }

# ── stopping processes by path ───────────────────────────────────
# The joint scripts share machines with other checkouts and, on the test hosts, with production daemons
# (docs/notes/0015 §4: cmax and dmax run production anet daemons and hubs as root, some inside containers
# that root on the host can see). Stopping "every process called anet" there stops those too. These
# helpers stop only what was started from a path the caller owns: a process of this user whose
# executable is that path or lies under it, or whose argv[0] does, or — for an interpreter (sh, bash,
# python…) — whose argv[1] does: the script it runs. Any other process that merely names a file there
# (`less $ROOT/A.log`, an editor) is left alone. Linux only (/proc); elsewhere they find nothing.

# _own_path PATH: PATH made absolute with symlinks resolved, or failure for a path no script may claim:
# the filesystem root, a top-level directory, the usual bin directories, $HOME. A mistyped J or ROOT
# must not turn into "stop everything I run".
_own_path(){
  local p=$1 r
  case "$p" in /*) ;; *) return 1 ;; esac
  if [ -d "$p" ]; then r=$(cd "$p" 2>/dev/null && pwd -P) || return 1
  elif [ -e "$p" ]; then r=$(cd "$(dirname "$p")" 2>/dev/null && pwd -P)/$(basename "$p") || return 1
  else r=${p%/}   # gone already; a process may still run from it ("… (deleted)")
  fi
  # A single file is judged by the directory it is in as well: /usr/local/bin/anet-hub names the
  # production hub, not a file some script put there.
  local x h=""
  if [ -n "${HOME:-}" ]; then h=$(cd "$HOME" 2>/dev/null && pwd -P) || h=$HOME; fi
  for x in "$r" "$([ -e "$r" ] && [ ! -d "$r" ] && dirname "$r")"; do
    [ -n "$x" ] || continue
    case "$x" in /*/*) ;; *) return 1 ;; esac
    case "$x" in
      /usr/bin|/usr/sbin|/usr/local|/usr/local/bin|/usr/local/sbin|/usr/lib|/var/tmp|/run/user) return 1 ;;
    esac
    if [ -n "$h" ]; then
      case "$x" in "$h"|"$h/bin"|"$h/.local"|"$h/.local/bin"|"$h/go"|"$h/go/bin") return 1 ;; esac
    fi
  done
  printf '%s' "$r"
}

# own_dir DIR: create DIR if it is missing (mode 700) and succeed only if it is a directory of this user
# that no other user can change: DIR and every directory above it owned by this user (or, above it,
# root), and none writable by another user unless sticky (/tmp) — "another user" being others, or a
# group with members besides this user (a user-private group under umask 002 is fine). The joint scripts
# build binaries into their work directory and then run them — as root on the test hosts, which have
# other users (docs/notes/0015 §2) — so a /tmp/joint-0 that someone else created first would let them
# swap what runs.
own_dir(){
  case "$1" in /*) ;; *) return 1 ;; esac
  ( umask 077; mkdir -p -- "$1" ) 2>/dev/null || return 1
  python3 -c '
import grp, os, pwd, stat, sys
uid = os.geteuid()
try:
    me = pwd.getpwuid(uid).pw_name
except KeyError:
    me = None
def shared(gid):
    try:
        members = set(grp.getgrgid(gid).gr_mem)
    except KeyError:
        return True
    members |= {u.pw_name for u in pwd.getpwall() if u.pw_gid == gid}
    return bool(members - {me})
def writable_by_others(st):
    return bool(st.st_mode & 0o002 or (st.st_mode & 0o020 and shared(st.st_gid)))
p = os.path.realpath(sys.argv[1])
st = os.stat(p)
# DIR itself: sticky does not help, another user could still create $J/bin before this script does.
if not stat.S_ISDIR(st.st_mode) or st.st_uid != uid or writable_by_others(st):
    sys.exit(1)
d = p
while d != "/":
    d = os.path.dirname(d)
    st = os.stat(d)
    if st.st_uid not in (0, uid) or (writable_by_others(st) and not st.st_mode & stat.S_ISVTX):
        sys.exit(1)' "$1"
}

# pids_under PATH: pids of this user's processes started from PATH (see above). The calling shell's own
# process group is never listed, so a script never matches itself, its subshells or its pipeline.
# One python3 pass over /proc: forking readlink per process is seconds on a host with thousands.
pids_under(){
  local root; root=$(_own_path "$1") || return 0
  python3 - "$root" "$$" <<'PY'
import os, re, sys
root, shell = sys.argv[1], int(sys.argv[2])
uid = os.geteuid()
# Interpreters: for these argv[1] is the program, so a script under root runs from there.
interp = re.compile(r"(ba|da|z|k|mk)?sh|busybox|python[0-9.]*|perl[0-9.]*|ruby[0-9.]*|node")
def pgid(p):
    try:
        return os.getpgid(p)
    except OSError:
        return None
def under(x):
    return x == root or x.startswith(root + "/")
mine = pgid(shell)
for e in os.listdir("/proc"):
    if not e.isdigit() or int(e) == shell:
        continue
    d = "/proc/" + e
    try:
        if os.stat(d).st_uid != uid:
            continue
    except OSError:
        continue
    try:
        exe = os.readlink(d + "/exe")
    except OSError:
        exe = ""
    if exe.endswith(" (deleted)"):
        exe = exe[:-len(" (deleted)")]
    try:
        with open(d + "/cmdline", "rb") as f:
            argv = [a.decode(errors="replace") for a in f.read().split(b"\0")[:2]]
    except OSError:
        argv = []
    names = [exe] + argv[:1]
    if len(argv) > 1 and interp.fullmatch(os.path.basename(exe or argv[0])):
        names.append(argv[1])
    if any(under(x) for x in names if x):
        if mine is not None and pgid(int(e)) == mine:
            continue
        print(e)
PY
}

# stop_under PATH [SECONDS]: SIGTERM everything pids_under PATH lists, wait up to SECONDS (default 5)
# for it to exit, then SIGKILL what is left. Nothing outside PATH is signalled.
stop_under(){
  local pids i n=$(( ${2:-5} * 5 ))
  pids=$(pids_under "$1")
  [ -n "$pids" ] || return 0
  kill -TERM $pids 2>/dev/null
  for ((i = 0; i < n; i++)); do
    sleep 0.2
    pids=$(pids_under "$1")
    [ -n "$pids" ] || return 0
  done
  kill -KILL $pids 2>/dev/null
  return 0
}
