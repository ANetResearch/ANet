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

# ── the SI-1 canary (A2A-DESIGN §1 SI-1, §17; joint.sh section C) ─
# The search, the tap and the settlement check are scripts/canary.py (python3, standard library only);
# these are the shell side. CANARY_PY may name a copy: joint.sh runs the tap from its own bin directory,
# so that stop_under finds it there.
CANARY_PY=${CANARY_PY:-$_LIB_ROOT/scripts/canary.py}
# CANARY_TMP: where canary.py puts its private copies of the databases it reads (default $TMPDIR, /tmp).
# joint.sh points it into its run directory, so that a search killed half way leaves nothing elsewhere.

# canary_new FILE LABEL: mint the canary for one piece of content, record it in FILE (label<TAB>value) and
# print it. Random, so it can turn up only where that content went. Never a capability id, a profile or
# anything else that is public by design: those are found in the hub legitimately.
canary_new(){
  local v; v="anet-canary-$2-$(python3 -c 'import secrets;print(secrets.token_hex(12))')" || return 1
  printf '%s\t%s\n' "$2" "$v" >> "$1" && printf '%s' "$v"
}

# canary_scan FILE REPORT LABEL [--expect BASENAME]… [--want LABEL]… PATH…: search every byte under
# the paths for the canaries in FILE, in every encoding canary.py knows; the JSON report goes to REPORT
# and a one-line summary to stdout. Exit 0 = nothing found (with --want: each wanted label found),
# 1 = found (with --want: one missing), 2 = nothing to search, an --expect file not among it, or a file
# that could not be searched in full.
canary_scan(){ TMPDIR=${CANARY_TMP:-${TMPDIR:-/tmp}} python3 "$CANARY_PY" scan --canaries "$1" --out "$2" --label "$3" "${@:4}"; }

# canary_tap DIR LISTEN UPSTREAM LOG: start a recording reverse proxy on LISTEN in front of the hub at
# UPSTREAM, capturing into DIR; print its pid once it listens. Fails when it does not come up.
canary_tap(){
  local i pid
  mkdir -p "$1" && rm -f "$1/ready"
  ( exec setsid python3 "$CANARY_PY" tap --listen "$2" --upstream "$3" --dir "$1" ) >"$4" 2>&1 </dev/null 9>&- &
  pid=$!
  for ((i = 0; i < 40; i++)); do
    [ -s "$1/ready" ] && { printf '%s' "$pid"; return 0; }
    kill -0 "$pid" 2>/dev/null || return 1
    sleep 0.25
  done
  return 1
}

# canary_settle TAPDIR REPORT [HUB_DB]: the structured check of every /x402/settle body the tap saw
# (A2A-DESIGN SI-1: resource, description and extra empty; nothing outside x402 v2's objects), and with
# HUB_DB, of the hub's settlement tables. Exit 0 when at least one settled and nothing is wrong.
canary_settle(){ TMPDIR=${CANARY_TMP:-${TMPDIR:-/tmp}} python3 "$CANARY_PY" settle --dir "$1" --out "$2" ${3:+--hub-db "$3"}; }

# canary_report_hits REPORT…: the number of canary hits recorded in canary_scan reports (0 when there are
# none). (canary_hits, below, is the other search: one needle in files, for joint-official.sh.)
canary_report_hits(){
  python3 - "$@" <<'PY'
import json, sys
n = 0
for p in sys.argv[1:]:
    try:
        n += len(json.load(open(p)).get("hits") or [])
    except (OSError, ValueError):
        pass
print(n)
PY
}

# ── searching for a canary ───────────────────────────────────────
# canary_hits NEEDLE PATH…: where NEEDLE is, in the files at PATH (a directory is walked). A byte search
# for the plain text alone misses most of the places a leak would sit: the hub keeps and serves envelopes,
# receipts, KELs and authorizations as base64 in JSON; a daemon's evidence chain is base64 CoreDet-CBOR
# per line; anything may log bytes as hex. So each file is searched for NEEDLE as it is, in hex, and in
# base64 and base64url at each of the three byte alignments (for alignment i, the encoding of NEEDLE's
# bytes from i on, cut to whole 3-byte groups: it occurs inside the base64 of any data holding NEEDLE at
# an offset ≡ -i mod 3, whatever surrounds it). One line per hit: "PATH (encoding)". A file that cannot
# be read, or a PATH that is missing, is a line too — a search that could not look is not a clean one.
# The last line is "# searched N": the number of files read, so a caller can tell "nothing found" from
# "nothing searched". NEEDLE should be 12 bytes or more.
canary_hits(){
  python3 - "$@" <<'PY'
import base64, binascii, os, sys
needle, paths = sys.argv[1].encode(), sys.argv[2:]
forms = [("plain", needle), ("hex", binascii.hexlify(needle)), ("HEX", binascii.hexlify(needle).upper())]
for i in range(3):
    s = needle[i:]
    s = s[:len(s) // 3 * 3]
    if len(s) >= 9:
        forms += [("base64/%d" % i, base64.b64encode(s)), ("base64url/%d" % i, base64.urlsafe_b64encode(s))]
seen, uniq = set(), []
for name, f in forms:
    if f not in seen:
        seen.add(f)
        uniq.append((name, f))
n = 0
def check(p):
    global n
    try:
        with open(p, "rb") as fh:
            data = fh.read()
    except OSError as e:
        print("%s (unreadable: %s)" % (p, e.strerror))
        return
    n += 1
    for name, f in uniq:
        if f in data:
            print("%s (%s)" % (p, name))
for p in paths:
    if os.path.isdir(p) and not os.path.islink(p):
        for root, dirs, files in os.walk(p):
            dirs.sort()
            for fn in sorted(files):
                fp = os.path.join(root, fn)
                if os.path.isfile(fp) and not os.path.islink(fp):
                    check(fp)
    elif os.path.isfile(p):
        check(p)
    else:
        print("%s (missing)" % p)
print("# searched %d" % n)
PY
}
