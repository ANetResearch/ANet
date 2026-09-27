#!/usr/bin/env bash
# joint.sh — drive every module end to end: real daemons, a real hub, real peer processes.
#
# Self-contained. On a clean Linux host with bash, curl and python3 it gets its own binaries, starts
# its own hub (and hub admin), three daemons and two peer processes on a loopback port block of its
# own, runs, and at the end stops exactly what it started — by path, never by process name.
#
#   J=/tmp/jx bash scripts/joint.sh                  build from this checkout (needs go)
#   JOINT_BIN=DIR J=/tmp/jx bash scripts/joint.sh    prebuilt binaries, no go on the host
#
# Environment:
#   J                  work directory (default /tmp/joint-<uid>). It must be this user's and writable by
#                      no one else (lib.sh own_dir): binaries are built there and run. An existing
#                      non-empty directory is used only if an earlier run of this script made it (it
#                      holds .joint-dir); each run replaces $J/bin and $J/run and, besides its marker and
#                      lock, touches nothing else there.
#   JOINT_BIN          directory with prebuilt anet, anetfixture, anetpeer, anet-hub and optionally
#                      anet-hub-admin (scripts/testnet/build.sh makes this set, in its linux-<arch>
#                      directory). They are copied into $J/bin. Unset: built here with go from this
#                      checkout and HUB_SRC.
#   HUB_SRC            ANetHub checkout to build from (default: ../ANetHub beside this repository)
#   JOINT_PORT_BASE    first of 10 consecutive loopback ports; unset = a random free block in
#                      20000-32000. A port already in use aborts the run; nothing is killed to free it.
#   JOINT_HUB_ADMIN    0 = do not start anet-hub-admin (started by default when the binary is there)
#   JOINT_DEVICES      0 = no device chain: anetlink is not configured and 1/11 is skipped;
#                      1 = the device chain is required; unset = used when it is reachable
#   JOINT_MOCK         ANetMock API address (default 127.0.0.1:29080)
#   JOINT_LINK_SOCKET  anetlinkd's C1 socket (default $J/link/c1.sock)
#   JOINT_KEEP         1 = leave everything running at the end (default: stop it)
#
# On the test hosts (scripts/testnet, docs/notes/0015) take the binaries from scripts/testnet/build.sh
# and a JOINT_PORT_BASE inside the test network's port range. Nothing here stops a process by name,
# and the daemons get a private XDG_RUNTIME_DIR, so a production daemon of the same user is neither
# stopped nor shadowed.
#
# Building: ANet and ANetHub speak wire 2 (sealed envelopes, relayauth v2) and must be built from
# the same generation — a wire-2 daemon refuses a wire-1 hub and a wire-2 hub answers 426 to a
# wire-1 daemon. While the ANetCore release they need is not tagged, build with GOWORK naming a
# workspace that holds the three repositories. When GOWORK is unset and ../go.work exists (the
# work-tree layout) it is used; the build refuses when ANet and ANetHub would resolve ANetCore to
# different places.
#
# The device chain lives in two other repositories (ANetLink, ANetMock) and is not started here.
# To include it, run it yourself before this script:
#   anetmock -venue office -api 127.0.0.1:29080 -base-port 29200
#   anetlinkd --config anetlink.json --c1-socket $J/link/c1.sock
#
#   ANetMock (real ONVIF/Zigbee endpoints) ← ANetLink (adapters, C1 socket)
#     ← anet daemon "provider" → ANetHub (relay) ← anet daemon "requester"
#                                               ← anet daemon "stranger" (on no list)
#
# Sections 8-10 attack the provider (A2A-DESIGN SI-4, SI-10) with anetfixture seal / relay-send /
# relay-sign: the stranger injects envelopes that claim the requester, replays captured ones, and
# the same envelope is delivered over p2p and the hub at once. They assert on the provider's receive
# counters (/status .receive), its inbox, its task threads and its evidence chain.
#
# Every call below crosses at least two process boundaries. That is the point: both repos' suites
# fake each other, and every defect this run has found so far lived exactly in the gap the fakes
# cover.
set -uo pipefail
umask 077
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
SCRIPTS=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPTS/.." && pwd -P)

J=${J:-/tmp/joint-$(id -u)}
BIN=$J/bin; RUN=$J/run
# lib.sh: peer_allow and the stop-by-path helpers. ANET is set first so sourcing it looks nothing up.
ANET=$BIN/anet
# shellcheck source=lib.sh
. "$SCRIPTS/lib.sh"

pass=0; fail=0
ok(){   printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; pass=$((pass+1)); }
no(){   printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; fail=$((fail+1)); }
hd(){   printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }
note(){ printf '\033[1;33m  ! %s\033[0m\n' "$*"; }
die(){  printf '\033[1;31mjoint.sh: %s\033[0m\n' "$*" >&2; exit 2; }
for c in curl python3 setsid; do command -v "$c" >/dev/null || die "$c is required"; done

# ── the work directory ──────────────────────────────────────────
# This script deletes $J/bin and $J/run and stops whatever runs from $J/bin, so it has to be sure
# $J is its own: a fresh or empty directory, or one an earlier run marked.
case "$J" in /*) ;; *) J=$PWD/$J ;; esac
# Judged before own_dir creates anything, and again once symlinks are resolved.
_own_path "$J" >/dev/null || die "J=$J is not a directory this script may own (pick something like /tmp/joint-x)"
own_dir "$J" || die "J=$J is not a private directory of this user (another user owns or can write to it, or to a directory above it); use another J"
J=$(cd "$J" && pwd -P); BIN=$J/bin; RUN=$J/run; ANET=$BIN/anet
_own_path "$J" >/dev/null || die "J=$J is not a directory this script may own (pick something like /tmp/joint-x)"
# Unix socket paths are limited to 107 bytes; the peer sockets are the longest.
[ ${#RUN} -le 80 ] || die "J is too long for the peer sockets under it ($RUN); use a shorter J"
if [ ! -e "$J/.joint-dir" ] && [ -n "$(ls -A "$J" 2>/dev/null)" ]; then
  die "$J is not empty and was not made by joint.sh (no .joint-dir); use an empty or new J"
fi
: > "$J/.joint-dir"
# The lock is fd 9, closed in every process started below (9>&-): a daemon left behind by a killed
# run must not hold it and lock the next run out of the directory it could clean up.
if command -v flock >/dev/null; then
  exec 9>"$J/.joint-lock"
  flock -n 9 || die "another joint.sh run is using $J"
fi

cleanup(){
  if [ "${JOINT_KEEP:-0}" = 1 ]; then
    printf '\n  JOINT_KEEP=1: left running from %s (rerun, or: . scripts/lib.sh; stop_under %s)\n' "$BIN" "$BIN"
    return
  fi
  stop_under "$BIN" 10
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# ── helpers ─────────────────────────────────────────────────────
# jget KEY… — one value out of the JSON on stdin (nested keys walk objects); empty when absent.
jget(){ python3 -c '
import sys, json
try:
    v = json.load(sys.stdin)
except Exception:
    v = None
for k in sys.argv[1:]:
    v = v.get(k) if isinstance(v, dict) else None
print("" if v is None else v)' "$@"; }
rand(){ python3 -c 'import secrets;print(secrets.token_hex(8))'; }

home_of(){ case $1 in req) echo "$REQ" ;; prov) echo "$PROV" ;; str) echo "$STR" ;; esac; }
addr_of(){ case $1 in req) echo "$RC" ;; prov) echo "$PC" ;; str) echo "$SC" ;; esac; }
# ctl <node> <path> <json> — a node's control API. The bearer token goes to curl through a file
# descriptor, not the command line: on a shared host other users can read argv.
ctl(){
  local tok; tok=$(cat "$(home_of "$1")/.anet/control_token.txt" 2>/dev/null)
  curl -s -m 30 -H @<(printf 'Authorization: Bearer %s\n' "$tok") -H 'Content-Type: application/json' \
       -d "$3" "http://$(addr_of "$1")$2"
}
rc(){ ctl req "$@"; }
pc(){ ctl prov "$@"; }
sc(){ ctl str "$@"; }

up(){   curl -sf -m 2 "http://$1/ping" >/dev/null 2>&1; }
# wait_up <addr> [seconds] / wait_down <addr> [seconds] — a control port answering, or no longer.
wait_up(){   local i; for ((i = 0; i < ${2:-20} * 4; i++)); do up "$1" && return 0; sleep 0.25; done; return 1; }
wait_down(){ local i; for ((i = 0; i < ${2:-20} * 4; i++)); do up "$1" || return 0; sleep 0.25; done; return 1; }

# start_node <node> <log> — run a daemon as that node. Its own HOME, and an XDG_RUNTIME_DIR under
# $RUN: otherwise the daemon writes this user's "current daemon" pointer and identity registry
# (/tmp/anet-<uid>, $XDG_RUNTIME_DIR/anet), and on a host that runs a real daemon as the same user
# every later `anet` command there would talk to this test node instead.
declare -A NODE_PID=()
start_node(){
  ( cd "$RUN" && exec setsid env -u ANET_DATA_DIR -u ANET_HOME -u ANET_ID \
      HOME="$(home_of "$1")" XDG_RUNTIME_DIR="$RUN/xdg" "$BIN/anet" daemon ) >"$2" 2>&1 </dev/null 9>&- &
  NODE_PID[$1]=$!   # the daemon itself: the subshell execs setsid, which (not a group leader) execs env, then anet
}
# stop_node <node> — graceful shutdown through the control API; wait for the port to close and then for
# the process to exit. The port closes first and the store and the evidence chain after it, and the next
# start on the same data directory must not overlap that.
stop_node(){
  local i p=${NODE_PID[$1]:-}
  ctl "$1" /shutdown '{}' >/dev/null
  wait_down "$(addr_of "$1")" 15 || return 1
  [ -n "$p" ] || return 0
  for ((i = 0; i < 60; i++)); do kill -0 "$p" 2>/dev/null || return 0; sleep 0.25; done
  return 1
}

# node_config <node> <python-expression for "modules"> — a fresh config: the pinned control port,
# the modules given, nothing else.
node_config(){
  python3 - "$(home_of "$1")/.anet/config.json" "$(addr_of "$1")" "$2" <<'PY'
import json, sys
path, addr, modules = sys.argv[1], sys.argv[2], sys.argv[3]
c = json.load(open(path))
c = {k: v for k, v in c.items() if k not in ("modules", "accept_delegations", "inbound")}
c["control_addr"] = addr
c["modules"] = json.loads(modules)
json.dump(c, open(path, "w"), indent=1)
PY
}

# delegate_cap <node> <capability> <args-json> — a capability call to the provider; prints the id.
delegate_cap(){
  ctl "$1" /delegate "{\"provider\":\"$PROV_AID\",\"capability\":\"$2\",\"args\":$3}" | jget interaction_id
}
# result_of <node> <interaction_id> [polls] — wait for that call's result and print the effect.
result_of(){
  local r i
  for ((i = 0; i < ${3:-60}; i++)); do
    r=$(ctl "$1" /results '{}' | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = {}
for x in d.get('results') or []:
    if x.get('interaction_id') == '$2': print(x.get('result') or ''); break
")
    [ -n "$r" ] && { echo "$r"; return 0; }
    sleep 0.5
  done
  echo '{"error":"timed out waiting for the result"}'; return 1
}
# cap <capability> <args-json> — the requester calls the provider and prints the effect.
cap(){
  local ix; ix=$(delegate_cap req "$1" "$2")
  [ -n "$ix" ] || { echo '{"error":"delegate refused"}'; return 1; }
  result_of req "$ix"
}
# thread_end <node> <interaction_id> — wait until the node's task is terminal; print "state reason",
# the reason being the anet.reason of the last status message that carried one.
thread_end(){
  local t i st
  for ((i = 0; i < 60; i++)); do
    t=$(ctl "$1" /thread "{\"interaction_id\":\"$2\"}")
    st=$(printf '%s' "$t" | jget thread state)
    case "$st" in completed|failed|canceled|rejected) break ;; esac
    sleep 0.5
  done
  printf '%s' "$t" | python3 -c '
import sys, json
try:
    t = json.load(sys.stdin).get("thread") or {}
except Exception:
    t = {}
reason = ""
for m in t.get("messages") or []:
    md = m.get("metadata")
    if isinstance(md, dict) and md.get("anet.reason"):
        reason = md["anet.reason"]
print(t.get("state", ""), reason)'
}
# in_inbox <interaction_id> — the provider's inbox entry for that id as "trust", "absent" when there
# is none, or "unreadable" when the inbox could not be read (a provider that is down has an empty
# inbox too, and that must not pass for "nothing got in").
in_inbox(){
  pc /inbox '{}' | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = None
if not isinstance(d, dict) or "inbox" not in d:
    print("unreadable"); sys.exit()
for x in d["inbox"] or []:
    if x.get("interaction_id") == sys.argv[1]:
        print(x.get("trust") or "none"); sys.exit()
print("absent")' "$1"
}
# state pulls what a capability actually read out of the result it returned. It lives in
# evidence.observed_state: metrics are float64 and cannot hold a CID, a blob or a list.
state(){ python3 -c "import sys,json;print((json.load(sys.stdin).get('evidence') or {}).get('observed_state',''))" 2>/dev/null; }

# ── the provider's receive side, for the attack sections (8-10) ─
# rx <reason> — the provider's receive counter for that reason (A2A-DESIGN §3.6; /status .receive);
# 0 when it has not counted any, or when the status cannot be read.
rx(){ pc /status '{}' | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = {}
r = d.get("receive") if isinstance(d, dict) else None
print((r or {}).get(sys.argv[1], 0))' "$1"; }
# rx_above <reason> <n> [seconds] — wait until that counter is above n; print the value it reached. A copy
# that came through the hub is counted at the provider's next mailbox poll, not when the hub took it.
rx_above(){ local i v=0; for ((i = 0; i < ${3:-20} * 2; i++)); do v=$(rx "$1"); [ "${v:-0}" -gt "$2" ] && break; sleep 0.5; done; echo "${v:-0}"; }
# chain_len <node> — the node's evidence chain length, less its refusal summaries: those are written when
# a ten-minute window closes, whenever that falls, and "nothing was written" must not depend on how long
# the run took. Empty when the chain cannot be read.
chain_len(){ ctl "$1" /evidence '{"event_type":"anet.delegation.refused_summary","limit":1000}' | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = {}
n = (d.get("head") or {}).get("length") if isinstance(d, dict) else None
print("" if n is None else n - len(d.get("records") or []))'; }
# inbox_n — how many interactions the provider holds as the provider; empty when unreadable.
inbox_n(){ pc /inbox '{}' | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = None
print(len(d["inbox"] or []) if isinstance(d, dict) and "inbox" in d else "")'; }
# received_n <interaction_id> — the provider's anet.delegation.received records naming that interaction;
# empty when the chain cannot be read (an unreadable chain holds no record either, and that must not pass
# for "nothing was recorded").
received_n(){ pc /evidence '{"event_type":"anet.delegation.received","limit":1000}' | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = None
if not isinstance(d, dict) or not isinstance(d.get("records"), list):
    print(""); sys.exit()
print(sum(1 for r in d["records"] if (r.get("payload") or {}).get("interaction_id") == sys.argv[1]))' "$1"; }
# said_n <interaction_id> <text> — how many messages of the provider's side of that task say exactly <text>;
# empty when the task cannot be read (no such task, or the provider is down), for the same reason.
said_n(){ pc /thread "{\"interaction_id\":\"$1\"}" | python3 -c '
import sys, json
try:
    t = json.load(sys.stdin).get("thread")
except Exception:
    t = None
if not isinstance(t, dict):
    print(""); sys.exit()
print(sum(1 for m in (t.get("messages") or []) if m.get("body") == sys.argv[1]))' "$2"; }
# took <relay-send-output> <path> — "yes" when that path (hub, p2p) took the envelope. The JSON is the first
# line; relay-send exits non-zero when a path failed, and its error line follows on the merged stderr.
took(){ printf '%s\n' "$1" | python3 -c '
import sys, json
try:
    d = json.loads(sys.stdin.readline())
except Exception:
    d = None
p = (d.get(sys.argv[1]) if isinstance(d, dict) else None) or {}
print("yes" if p.get("code") == 200 or p.get("ok") is True else "no")' "$2"; }

hd "0/11  binaries, ports, and the stack"
if [ -n "${JOINT_BIN:-}" ]; then
  SRC=$(cd "$JOINT_BIN" 2>/dev/null && pwd -P) || die "JOINT_BIN=$JOINT_BIN is not a directory"
  for b in anet anetfixture anetpeer anet-hub; do
    [ -x "$SRC/$b" ] || die "JOINT_BIN has no $b"
  done
  case "$SRC" in "$BIN"|"$BIN"/*|"$RUN"|"$RUN"/*) die "JOINT_BIN must not be under $BIN or $RUN: both are replaced" ;; esac
fi
# Leftovers of an earlier run in this J — interrupted before its own cleanup — hold ports and files.
stop_under "$BIN" 10
rm -rf -- "$BIN" "$RUN"
mkdir -p "$BIN" "$RUN"

if [ -n "${JOINT_BIN:-}" ]; then
  # Copied, so that everything this run starts runs from $J/bin and the cleanup can find it there.
  for b in anet anetfixture anetpeer anet-hub anet-hub-admin; do
    [ -x "$SRC/$b" ] || continue
    cp "$SRC/$b" "$BIN/$b" || die "cannot copy $SRC/$b into $BIN"
  done
  echo "  binaries: $SRC"
else
  command -v go >/dev/null || die "go is not on PATH; build elsewhere (scripts/testnet/build.sh) and pass JOINT_BIN"
  HUB_SRC=${HUB_SRC:-$ROOT/../ANetHub}
  [ -d "$HUB_SRC/cmd/anet-hub" ] || die "ANetHub not found at $HUB_SRC (set HUB_SRC)"
  HUB_SRC=$(cd "$HUB_SRC" && pwd -P)
  if [ -z "${GOWORK:-}" ] && [ -f "$ROOT/../go.work" ]; then
    GOWORK=$(cd "$ROOT/.." && pwd -P)/go.work; export GOWORK
  fi
  # The workspace has to build THIS checkout, and both sides against one ANetCore.
  modir(){ go -C "$1" list -m -f '{{.Dir}}' "$2" 2>/dev/null; }
  real(){ [ -n "$1" ] && (cd "$1" 2>/dev/null && pwd -P); }
  [ "$(real "$(modir "$ROOT" github.com/ANetResearch/ANet)")" = "$ROOT" ] \
    || die "go builds github.com/ANetResearch/ANet from somewhere other than $ROOT (GOWORK=$(go env GOWORK))"
  CORE_A=$(real "$(modir "$ROOT" github.com/ANetResearch/ANetCore)")
  CORE_H=$(real "$(modir "$HUB_SRC" github.com/ANetResearch/ANetCore)")
  [ -n "$CORE_A" ] && [ "$CORE_A" = "$CORE_H" ] \
    || die "ANet and ANetHub resolve ANetCore differently (${CORE_A:-?} vs ${CORE_H:-?}); set GOWORK to one workspace"
  echo "  building: ANet $ROOT, ANetHub $HUB_SRC, ANetCore $CORE_A (GOWORK=$(go env GOWORK))"
  export CGO_ENABLED=0
  go build -C "$ROOT" -o "$BIN/anet" ./cmd/anet                   || die "build anet failed"
  go build -C "$ROOT" -o "$BIN/anetfixture" ./tools/anetfixture   || die "build anetfixture failed"
  go build -C "$ROOT" -o "$BIN/anetpeer" ./tools/anetpeer         || die "build anetpeer failed"
  go build -C "$HUB_SRC" -o "$BIN/anet-hub" ./cmd/anet-hub        || die "build anet-hub failed"
  if [ -d "$HUB_SRC/cmd/anet-hub-admin" ]; then
    go build -C "$HUB_SRC" -o "$BIN/anet-hub-admin" ./cmd/anet-hub-admin || die "build anet-hub-admin failed"
  fi
fi
FIX=$BIN/anetfixture

# Ten loopback ports: +0 hub, +1 hub admin, +2 requester, +3 provider, +4 stranger, +5..+9 spare.
PORT_BASE=$(python3 - "${JOINT_PORT_BASE:-}" 10 <<'PY'
import random, socket, sys
want, n = sys.argv[1], int(sys.argv[2])
def free(b):
    for p in range(b, b + n):
        s = socket.socket()
        try:
            s.bind(("127.0.0.1", p))
        except OSError:
            return False
        finally:
            s.close()
    return True
if want:
    b = int(want)
    if not 1024 <= b <= 65535 - n:
        sys.exit("JOINT_PORT_BASE=%s is out of range" % want)
    if not free(b):
        sys.exit("a port in %d-%d is in use; pick another JOINT_PORT_BASE" % (b, b + n - 1))
    print(b); sys.exit()
for _ in range(200):
    b = random.randrange(20000, 32000, n)
    if free(b):
        print(b); sys.exit()
sys.exit("no free block of %d ports in 20000-32000" % n)
PY
) || die "no ports"
HUB_ADDR=127.0.0.1:$PORT_BASE; HUB_URL=http://$HUB_ADDR
ADMIN_ADDR=127.0.0.1:$((PORT_BASE + 1))
RC=127.0.0.1:$((PORT_BASE + 2)); PC=127.0.0.1:$((PORT_BASE + 3)); SC=127.0.0.1:$((PORT_BASE + 4))
REQ=$RUN/req; PROV=$RUN/prov; STR=$RUN/stranger
echo "  ports:    $PORT_BASE-$((PORT_BASE + 9))   work dir: $J"

# The device chain: used when asked for, or when it is there.
MOCK=${JOINT_MOCK:-127.0.0.1:29080}
LINK_SOCK=${JOINT_LINK_SOCKET:-$J/link/c1.sock}
link_live(){ python3 -c '
import socket, sys
s = socket.socket(socket.AF_UNIX)
s.settimeout(2)
s.connect(sys.argv[1])' "$LINK_SOCK" 2>/dev/null; }
mock_live(){ curl -sf -m 3 "http://$MOCK/api/scene" >/dev/null 2>&1; }
case "${JOINT_DEVICES:-}" in
  0) DEVICES=0 ;;
  1) link_live || die "JOINT_DEVICES=1 but nothing answers on $LINK_SOCK (start anetlinkd, see the header)"
     mock_live || die "JOINT_DEVICES=1 but ANetMock does not answer at $MOCK"
     DEVICES=1 ;;
  *) if link_live && mock_live; then DEVICES=1; else DEVICES=0; fi ;;
esac
[ "$DEVICES" = 1 ] && echo "  devices:  anetlinkd at $LINK_SOCK, ANetMock at $MOCK" \
                   || echo "  devices:  none (anetlink not configured, 1/11 skipped)"

# The hub, on an empty data directory.
( cd "$RUN" && exec setsid "$BIN/anet-hub" --addr "$HUB_ADDR" --data "$RUN/hub" ) >"$RUN/hub.log" 2>&1 </dev/null 9>&- &
for _ in $(seq 1 40); do curl -sf -m 2 "$HUB_URL/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
curl -sf -m 5 "$HUB_URL/healthz" >/dev/null && ok "hub up on an empty data directory" \
  || { no "hub down: $(tail -2 "$RUN/hub.log")"; exit 1; }

# The hub admin beside it, reading the hub's data directory the way it does in production. It
# gets its token through the environment of a subshell, never through argv.
if [ -x "$BIN/anet-hub-admin" ] && [ "${JOINT_HUB_ADMIN:-1}" != 0 ]; then
  python3 -c 'import secrets;print(secrets.token_hex(32))' > "$RUN/admin.token"
  ( cd "$RUN" && ADMIN_TOKEN=$(cat "$RUN/admin.token") && export ADMIN_TOKEN \
      && exec setsid "$BIN/anet-hub-admin" --addr "$ADMIN_ADDR" --hub-data "$RUN/hub" --data "$RUN/admin" \
           --snapshot-every 2s --harvest-every 2s ) >"$RUN/admin.log" 2>&1 </dev/null 9>&- &
  for _ in $(seq 1 40); do curl -sf -m 2 "http://$ADMIN_ADDR/admin/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
  curl -sf -m 5 "http://$ADMIN_ADDR/admin/healthz" >/dev/null && ok "hub admin up beside it" \
    || no "hub admin down: $(tail -2 "$RUN/admin.log")"
else
  echo "  hub admin: not started"
fi

# First start creates each identity. No AID exists before it, so nothing that names one — the
# allow list, the org genesis — can be written before it either.
mkdir -p "$REQ/.anet" "$PROV/.anet" "$STR/.anet"
mkdir -p -m 700 "$RUN/xdg"
for n in req prov str; do
  printf '{"control_addr":"%s"}\n' "$(addr_of $n)" > "$(home_of $n)/.anet/config.json"
  start_node $n "$RUN/$n-first.log"
done
for n in req prov str; do
  wait_up "$(addr_of $n)" 30 || { no "$n did not come up: $(tail -2 "$RUN/$n-first.log")"; exit 1; }
done
REQ_AID=$("$FIX" aid --home "$REQ/.anet")
PROV_AID=$("$FIX" aid --home "$PROV/.anet")
STR_AID=$("$FIX" aid --home "$STR/.anet")
for n in req prov str; do stop_node $n || { no "$n did not stop"; exit 1; }; done
[ -n "$REQ_AID" ] && [ -n "$PROV_AID" ] && [ -n "$STR_AID" ] || { no "identities were not created"; exit 1; }

GENESIS=$("$FIX" org-genesis --home "$REQ/.anet" --nonce joint 2>"$RUN/orgid.txt")
ORG_ID=$(sed 's/^org id: //' "$RUN/orgid.txt")
[ -n "$GENESIS" ] && [ -n "$ORG_ID" ] || { no "org genesis failed: $(cat "$RUN/orgid.txt")"; exit 1; }

# The provider runs the default closed inbound policy (A2A-DESIGN §5) and accepts the requester by
# name. The file is written directly: the CLI's `anet peers allow` asks for confirmation on a
# terminal, which a script does not have. The daemon reads the file on every decision. The
# stranger is on no list.
peer_allow "$PROV/.anet" "$REQ_AID"

# Two peer processes sharing a rendezvous directory. Without them the p2p module is the one module
# that changes how delegations travel and the one module no joint run could exercise.
# The requester's keeps a copy of every envelope it carried (--tee-dir), for the replay section (9/11).
mkdir -p "$RUN/rv" "$RUN/peer"
( cd "$RUN" && exec setsid "$BIN/anetpeer" --socket "$RUN/peer/req.sock" --peer "$RUN/peer/req.wire" \
    --rendezvous "$RUN/rv" --tee-dir "$RUN/tee" ) >"$RUN/peer-req.log" 2>&1 </dev/null 9>&- &
( cd "$RUN" && exec setsid "$BIN/anetpeer" --socket "$RUN/peer/prov.sock" --peer "$RUN/peer/prov.wire" \
    --rendezvous "$RUN/rv" ) >"$RUN/peer-prov.log" 2>&1 </dev/null 9>&- &
for _ in $(seq 1 20); do [ -S "$RUN/peer/req.sock" ] && [ -S "$RUN/peer/prov.sock" ] && break; sleep 0.25; done

# The provider gets every module: cas, blackboard, org, p2p, and anetlink when there are devices.
# The requester gets the transport too — a direct path is only a path if both ends have one. The
# stranger gets nothing: it reaches the provider through the hub or not at all.
PROV_MODULES=$(python3 - "$GENESIS" "$RUN" "$DEVICES" "$LINK_SOCK" <<'PY'
import json, sys
genesis, run, devices, link = sys.argv[1:5]
m = {
    "cas":        {"dir": run + "/cas"},
    "blackboard": {"enabled": True},
    "org":        {"genesis": genesis},
    "p2p":        {"socket": run + "/peer/prov.sock"},
}
if devices == "1":
    m["anetlink"] = {"socket": link}
print(json.dumps(m))
PY
)
node_config prov "$PROV_MODULES"
node_config req "{\"p2p\":{\"socket\":\"$RUN/peer/req.sock\"}}"
node_config str '{}'

for n in req prov str; do start_node $n "$RUN/$n.log"; done
wait_up "$RC" 30 && ok "requester up" || no "requester down: $(tail -2 "$REQ/.anet/daemon.log" 2>/dev/null)"
wait_up "$PC" 30 && ok "provider up"  || no "provider down: $(tail -2 "$PROV/.anet/daemon.log" 2>/dev/null)"
wait_up "$SC" 30 && ok "stranger up"  || no "stranger down: $(tail -2 "$STR/.anet/daemon.log" 2>/dev/null)"
grep -m1 'modules compiled in' "$PROV/.anet/daemon.log" 2>/dev/null | sed 's/.*modules compiled in: /  provider modules: /'
REG_R=$(rc /hub-register "{\"hub\":\"$HUB_URL\",\"name\":\"Requester\"}" | jget status)
REG_P=$(pc /hub-register "{\"hub\":\"$HUB_URL\",\"name\":\"JointNode\"}")
REG_S=$(sc /hub-register "{\"hub\":\"$HUB_URL\",\"name\":\"Stranger\"}" | jget status)
[ "$REG_R" = registered ] && [ "$(printf '%s' "$REG_P" | jget status)" = registered ] && [ "$REG_S" = registered ] \
  && ok "all three registered at the hub" || no "registration failed: req=$REG_R prov=$(printf '%s' "$REG_P" | head -c 120) str=$REG_S"
printf '  requester %s\n  provider  %s\n  stranger  %s\n  org       %s\n' "$REQ_AID" "$PROV_AID" "$STR_AID" "$ORG_ID"

hd "1/11  device control — daemon → ANetLink → ANetMock (ONVIF PTZ)"
if [ "$DEVICES" = 1 ]; then
  # ptz reads one camera's pan/tilt/zoom out of the mock's scene — the ground truth, independent
  # of anything the daemon reports about itself.
  ptz(){ curl -s -m 5 "http://$MOCK/api/scene" | python3 -c "
import sys,json
for d in json.load(sys.stdin)['devices']:
    if d['id']=='$1':
        st=d.get('state') or {}
        print(json.dumps({k:st.get(k) for k in ('pan','tilt','zoom')}))
        break
"; }
  CAM=$(curl -s -m 5 "http://$MOCK/api/scene" | python3 -c "
import sys,json
for d in json.load(sys.stdin)['devices']:
    if (d.get('state') or {}).get('pan') is not None: print(d['id']); break")
  echo "  camera:  ${CAM:-none with a pan axis}"
  echo "  before:  $(ptz "$CAM")"
  EFF=$(cap "ptz.absolute@onvif/$CAM" '{"pan":0.11,"tilt":-0.33,"zoom":0.55}')
  echo "  effect:  $(echo "$EFF" | head -c 400)"
  sleep 2
  AFTER=$(ptz "$CAM")
  echo "  after:   $AFTER"
  echo "$AFTER" | grep -q '"pan": 0.11' && ok "the mock camera moved to the commanded pan" \
    || no "the camera did not reach pan=0.11 (it may still be travelling — the motion is continuous by design)"
  # The device path is the one where provenance matters most, and it was the one carrying none:
  # ANetLink's C1 wire had no field for the quirk label and the daemon's shim ignored the
  # evidence block entirely.
  echo "$EFF" | python3 -c "import sys,json;e=json.load(sys.stdin).get('evidence');sys.exit(0 if e and e.get('protocol') and e.get('verify_trust') is not None else 1)" \
    && ok "the effect arrived with its provenance (protocol + trust), not just a number" \
    || no "the device effect still carries no provenance"
else
  note "skipped: no device chain (JOINT_DEVICES=0, or no anetlinkd/ANetMock reachable)"
fi

hd "2/11  distributed storage — cas.put / cas.get round trip"
BLOB=$(printf 'joint run %s' "$(date -u +%FT%TZ)" | base64 -w0)
PUT=$(cap cas.put "{\"body\":\"$BLOB\"}")
CID=$(echo "$PUT" | state)
echo "  put:     $(echo "$PUT" | head -c 300)"
[ -n "$CID" ] && ok "cas.put returned a CID: $CID" || no "cas.put returned no CID"
GET=$(cap cas.get "{\"cid\":\"$CID\"}")
echo "$GET" | grep -q "$BLOB" && ok "cas.get returned the same bytes, addressed by content" \
  || no "cas.get did not return the stored bytes: $(echo "$GET" | head -c 300)"
BAD=$(cap cas.get '{"cid":"bafyreiaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}')
echo "$BAD" | grep -qi 'failed\|not found\|no such' && ok "an unknown CID fails honestly rather than returning nothing" \
  || no "an unknown CID did not fail: $(echo "$BAD" | head -c 200)"

hd "3/11  shared brain — blackboard.add / snapshot (requester signs, provider verifies)"
UNIT=$("$FIX" cogunit --home "$REQ/.anet" --task joint-1 --type claim --body "the camera reached its commanded position" 2>"$RUN/unit.txt")
UNIT_ID=$(sed 's/^unit id: //' "$RUN/unit.txt")
ADD=$(cap blackboard.add "{\"unit\":\"$UNIT\"}")
echo "  add:     $(echo "$ADD" | head -c 300)"
echo "$ADD" | grep -q '"added": *1\|"added":1' && ok "the provider verified the requester's signature and merged the unit" \
  || no "blackboard.add did not merge: $(echo "$ADD" | head -c 300)"
SNAP=$(cap blackboard.snapshot '{"task_id":"joint-1"}')
echo "$SNAP" | state | grep -q "$UNIT" && ok "the unit comes back in the snapshot ($UNIT_ID)" \
  || no "the unit is missing from the snapshot: $(echo "$SNAP" | head -c 300)"
AGAIN=$(cap blackboard.add "{\"unit\":\"$UNIT\"}")
echo "$AGAIN" | grep -q '"added": *0\|"added":0' && ok "re-adding is idempotent (the 95×-faster path)" \
  || no "re-adding was not idempotent: $(echo "$AGAIN" | head -c 200)"
FORGED=$(python3 -c "
import base64,sys
b=bytearray(base64.b64decode('$UNIT'))
b[-1] ^= 0xff  # corrupt the detached signature's last byte
print(base64.b64encode(bytes(b)).decode())")
REJ=$(cap blackboard.add "{\"unit\":\"$FORGED\"}")
echo "$REJ" | grep -qi 'failed\|signature\|verif\|malformed' && ok "a tampered unit is refused" \
  || no "a tampered unit was accepted: $(echo "$REJ" | head -c 300)"

hd "4/11  org membership — org.verify against the configured genesis"
CRED=$("$FIX" org-credential --home "$REQ/.anet" --genesis "$GENESIS" --subject "$PROV_AID" --role member 2>/dev/null)
V=$(cap org.verify "{\"credential\":\"$CRED\"}")
echo "  verify:  $(echo "$V" | head -c 300)"
echo "$V" | grep -qi '"status": *"ok"\|"status":"ok"' && ok "a founder-issued member credential verifies" \
  || no "a valid credential failed to verify: $(echo "$V" | head -c 300)"
EXPIRED=$("$FIX" org-credential --home "$REQ/.anet" --genesis "$GENESIS" --subject "$PROV_AID" \
          --ttl 1s --issued-ago 2h 2>/dev/null)
E=$(cap org.verify "{\"credential\":\"$EXPIRED\"}")
echo "$E" | grep -qi 'expired\|window\|failed' && ok "an expired credential is refused" \
  || no "an expired credential passed: $(echo "$E" | head -c 300)"
I=$(cap org.info '{}')
[ "$(echo "$I" | state)" = "$ORG_ID" ] && ok "org.info names the org it actually serves" \
  || no "org.info did not report the configured org: $(echo "$I" | head -c 300)"

# INV-2: which org a node belongs to is not public. The realistic leak is not a field someone
# added on purpose — it is prose the agent wrote about itself, so the check belongs at the
# publish chokepoint.
LEAK=$(pc /profile "{\"summary\":\"I coordinate work for $ORG_ID\",\"readme\":\"\",\"pricing\":\"\"}")
echo "$LEAK" | grep -qi "inv2\|refusing to publish" && ok "publishing the org id is refused (INV-2)" \
  || no "the node published its org membership: $(echo "$LEAK" | head -c 200)"
# Judged by the answer it gives, not by the absence of an error: a provider that is down answers
# nothing, and nothing contains no error.
CLEAN=$(pc /profile '{"summary":"I operate devices over ANetLink","readme":"","pricing":""}')
[ "$(printf '%s' "$CLEAN" | jget status)" = profile_set ] && ok "an ordinary profile still publishes" \
  || no "an ordinary profile did not publish: $(printf '%s' "${CLEAN:-<no answer>}" | head -c 200)"

hd "5/11  peer-to-peer — a delegation that never touches the hub"
delivered(){ grep -c delivered "$1" 2>/dev/null || true; }
BEFORE_P2P=$(delivered "$RUN/peer-req.log")
BEFORE_BACK=$(delivered "$RUN/peer-prov.log")
P2PEFF=$(cap cas.stat "{\"cid\":\"$CID\"}")
echo "  effect:  $(echo "$P2PEFF" | head -c 220)"
echo "$P2PEFF" | grep -q '"status":"OK"' && ok "the call succeeded over the peer transport" \
  || no "the call failed: $(echo "$P2PEFF" | head -c 200)"
AFTER_P2P=$(delivered "$RUN/peer-req.log")
[ "$AFTER_P2P" -gt "$BEFORE_P2P" ] && ok "the requester's peer carried the delegation directly ($BEFORE_P2P → $AFTER_P2P)" \
  || no "the delegation still went through the hub (peer log unchanged at $AFTER_P2P)"
AFTER_BACK=$(delivered "$RUN/peer-prov.log")
[ "$AFTER_BACK" -gt "$BEFORE_BACK" ] && ok "the result came back the same way ($BEFORE_BACK → $AFTER_BACK)" \
  || no "the provider's peer carried nothing: $(tail -2 "$RUN/peer-prov.log")"

hd "6/11  a stranger is refused — closed is the default, not a setting"
# Nothing in the provider's config names a policy; closed is what it gets (A2A-DESIGN §5.1). The
# stranger is registered at the same hub, can look the provider up and seal to it, and is on no
# list: §5.2 row 6 refuses it with anet.reason=not_accepting, the same answer a denied peer gets,
# and the delegation never reaches the provider's inbox.
POL=$(pc /peers/list '{}' | jget policy)
[ "$POL" = closed ] && [ "$(printf '%s' "$REG_P" | jget inbound_policy)" = closed ] \
  && ok "the provider runs the default inbound policy, closed" \
  || no "the provider's inbound policy is '${POL:-unreadable}', expected closed"
GOAL="joint stranger task $(rand)"
SIX=$(sc /delegate "{\"provider\":\"$PROV_AID\",\"goal\":\"$GOAL\"}" | jget interaction_id)
if [ -n "$SIX" ]; then
  read -r SST SREASON <<<"$(thread_end str "$SIX")"
  [ "$SST" = rejected ] && ok "the stranger's task came back rejected" \
    || no "the stranger's task is '${SST:-unknown}', expected rejected"
  [ "$SREASON" = not_accepting ] && ok "with anet.reason=not_accepting" \
    || no "the refusal carried anet.reason='${SREASON}', expected not_accepting"
  case "$(in_inbox "$SIX")" in
    absent) ok "nothing reached the provider's inbox" ;;
    unreadable) no "the provider's inbox could not be read (is the provider up?)" ;;
    *) no "the stranger's task is in the provider's inbox" ;;
  esac
else
  no "the stranger's delegation was not even sent"; no "(refusal unchecked)"; no "(inbox unchecked)"
fi
# A capability call is no different: no capability is public unless the operator lists it.
CIX=$(delegate_cap str org.info '{}')
if [ -n "$CIX" ]; then
  read -r CST CREASON <<<"$(thread_end str "$CIX")"
  [ "$CST" = rejected ] && [ "$CREASON" = not_accepting ] \
    && ok "a capability call from the stranger is refused the same way" \
    || no "the stranger's capability call ended '${CST:-unknown}' (${CREASON:-no reason}), expected rejected/not_accepting"
  [ "$(in_inbox "$CIX")" = absent ] && ok "and it left nothing in the inbox either" \
    || no "the stranger's capability call reached the provider (or its inbox is unreadable)"
else
  # An id that was never sent is absent from every inbox; that must not read as a pass.
  no "the stranger's capability call was not even sent"; no "(inbox unchecked)"
fi

hd "7/11  the allow list is the way in"
# The same stranger, named on the list. The file is re-read on every decision, so this takes
# effect on the next delegation, with no restart; the call goes through the hub, since the
# stranger has no peer transport.
peer_allow "$PROV/.anet" "$STR_AID"
AIX=$(delegate_cap str org.info '{}')
AEFF=$(result_of str "${AIX:-none}")
[ "$(printf '%s' "$AEFF" | jget status)" = OK ] && ok "once allowed, the same call runs (status OK)" \
  || no "the allowed caller's call did not run: $(printf '%s' "$AEFF" | head -c 200)"
[ "$(printf '%s' "$AEFF" | state)" = "$ORG_ID" ] && ok "and answers with the provider's org" \
  || no "the answer is not the provider's org: $(printf '%s' "$AEFF" | head -c 200)"
ATRUST=$(in_inbox "${AIX:-none}")
[ "$ATRUST" = peer ] && ok "the provider holds it as a peer's delegation (trust=peer)" \
  || no "the provider's record of it says '${ATRUST}', expected trust=peer"
# And out again: taking the name off closes the door on the next call.
python3 - "$PROV/.anet/peers.allow" "$STR_AID" <<'PY'
import sys
p, aid = sys.argv[1], sys.argv[2]
lines = [l for l in open(p).read().splitlines() if l.strip() != aid]
open(p, "w").write("".join(l + "\n" for l in lines))
PY
RIX=$(delegate_cap str org.info '{}')
read -r RST RREASON <<<"$(thread_end str "${RIX:-none}")"
[ "$RST" = rejected ] && [ "$RREASON" = not_accepting ] \
  && ok "off the list again, the next call is refused (no restart)" \
  || no "after removal the call ended '${RST:-unknown}' (${RREASON:-no reason}), expected rejected/not_accepting"

hd "8/11  a forged sender is refused (SI-4)"
# The hub authenticates who POSTs an envelope (relay v2), never who the sealed inner message says it is
# from: it cannot read it. So any registered agent can put into the provider's mailbox an envelope whose
# inner from is the requester, a peer the provider allows. What stands in the way is the provider's own
# receive pipeline — step 6 (the inner KEL must replay to inner.from) and step 7 (the signature must
# verify under that KEL). The attacker is the stranger, off the list again since 7/11; anetfixture signs
# its relay v2 requests with the stranger's key, exactly as the stranger's daemon would.
#
# The target is a task the requester really has open at the provider, so a forgery that got past step 7
# would land in a real conversation. A daemon patched to skip step 7 turns this section red; the patch is
# scripts/mutations/si4-skip-step7.patch, applied in a scratch worktree and run with JOINT_BIN.
OGOAL="joint open task $(rand)"
OIX=$(rc /delegate "{\"provider\":\"$PROV_AID\",\"goal\":\"$OGOAL\"}" | jget interaction_id)
for _ in $(seq 1 40); do [ "$(in_inbox "${OIX:-none}")" = peer ] && break; sleep 0.5; done
[ -n "$OIX" ] && [ "$(in_inbox "$OIX")" = peer ] && ok "the requester has a task open at the provider ($OIX)" \
  || no "the requester's open task did not reach the provider"
sleep 1   # the acceptance's evidence record is written after the inbox row
EV0=$(chain_len prov); IN0=$(inbox_n); FM0=$(rx from-mismatch); BS0=$(rx bad-sig)

# A delegation that claims the requester and carries the stranger's own KEL. Sent with relay-sign and
# curl, which is also the check that the fixture's relay v2 headers are what a wire-2 hub accepts.
FDEL=$("$FIX" seal --home "$STR/.anet" --hub "$HUB_URL" --to "$PROV_AID" --type delegate \
         --as "$REQ_AID" --kel self --text "forged: run this for the requester" 2>"$RUN/forge-del.txt")
FDIX=$(sed -n 's/^ix: //p' "$RUN/forge-del.txt")
printf '{"to_aid":"%s","envelope":"%s"}' "$PROV_AID" "$FDEL" > "$RUN/forge-del.json"
"$FIX" relay-sign --home "$STR/.anet" --hub "$HUB_URL" --action send --method POST --path /relay/send \
  --body-file "$RUN/forge-del.json" > "$RUN/forge-del.hdr" 2>"$RUN/forge-del.err"
CODE=$(curl -s -o "$RUN/forge-del.out" -w '%{http_code}' -m 20 -H @"$RUN/forge-del.hdr" \
         -H 'Content-Type: application/json' --data-binary @"$RUN/forge-del.json" "$HUB_URL/relay/send")
[ -n "$FDEL" ] && [ "$CODE" = 200 ] \
  && ok "the hub took the stranger's send, signed by anetfixture relay-sign (it cannot see inside)" \
  || no "the relay-sign request was refused ($CODE): $(head -c 200 "$RUN/forge-del.out") $(head -c 200 "$RUN/forge-del.err") $(head -c 200 "$RUN/forge-del.txt")"
FM1=$(rx_above from-mismatch "$FM0")
[ "$FM1" -gt "$FM0" ] && ok "the provider refused it at step 6: from-mismatch $FM0 → $FM1" \
  || no "from-mismatch stayed at $FM1; receive counters: $(pc /status '{}' | jget receive | head -c 300)"

# A message into the open task that claims the requester and carries the requester's real KEL, as the
# hub serves it: step 6 passes, and the signature is the stranger's. Once through the hub, and once,
# with another text, straight over the peer wire — a direct path is a route, not a trust boundary.
FTEXT="forged $(rand)"
"$FIX" seal --home "$STR/.anet" --hub "$HUB_URL" --to "$PROV_AID" --type message --ix "${OIX:-none}" \
  --as "$REQ_AID" --kel claimed --text "$FTEXT" > "$RUN/forge-msg.env" 2>"$RUN/forge-msg.txt"
R=$("$FIX" relay-send --home "$STR/.anet" --hub "$HUB_URL" --to "$PROV_AID" --envelope @"$RUN/forge-msg.env" 2>&1)
[ "$(took "$R" hub)" = yes ] && ok "a message claiming the requester was queued at the hub" \
  || no "the forged message was not queued: $(printf '%s' "$R" | head -c 200) $(head -c 200 "$RUN/forge-msg.txt")"
BS1=$(rx_above bad-sig "$BS0")
[ "$BS1" -gt "$BS0" ] && ok "the provider refused it at step 7: bad-sig $BS0 → $BS1" \
  || no "bad-sig stayed at $BS1"
FTEXT2="forged over p2p $(rand)"
FMSG2=$("$FIX" seal --home "$STR/.anet" --hub "$HUB_URL" --to "$PROV_AID" --type message --ix "${OIX:-none}" \
          --as "$REQ_AID" --kel claimed --text "$FTEXT2" 2>/dev/null)
R=$("$FIX" relay-send --p2p "$RUN/peer/prov.wire" --to "$PROV_AID" --envelope "${FMSG2:-none}" 2>&1)
BS2=$(rx_above bad-sig "$BS1")
[ "$(took "$R" p2p)" = yes ] && [ "$BS2" -gt "$BS1" ] \
  && ok "the same forgery handed over the peer wire is refused the same way: bad-sig $BS1 → $BS2" \
  || no "over the peer wire: $(printf '%s' "$R" | head -c 200), bad-sig $BS1 → $BS2"

# The other half of SI-4: a message that is honestly signed — by the stranger, as the stranger — into a
# task whose peer is the requester. The signature proves who sent it, and that is not the task's peer:
# step 9 refuses it (not-peer), without writing anything and without an answer.
NP0=$(rx not-peer)
NTEXT="not your task $(rand)"
NMSG=$("$FIX" seal --home "$STR/.anet" --hub "$HUB_URL" --to "$PROV_AID" --type message --ix "${OIX:-none}" \
         --text "$NTEXT" 2>/dev/null)
R=$("$FIX" relay-send --home "$STR/.anet" --hub "$HUB_URL" --to "$PROV_AID" --envelope "${NMSG:-none}" 2>&1)
NP1=$(rx_above not-peer "$NP0")
[ "$(took "$R" hub)" = yes ] && [ "$NP1" -gt "$NP0" ] \
  && ok "the stranger's own signed message into the requester's task is refused: not-peer $NP0 → $NP1" \
  || no "the stranger's own message: $(printf '%s' "$R" | head -c 200), not-peer $NP0 → $NP1"

[ "$(said_n "${OIX:-none}" "$FTEXT")" = 0 ] && [ "$(said_n "${OIX:-none}" "$FTEXT2")" = 0 ] \
  && [ "$(said_n "${OIX:-none}" "$NTEXT")" = 0 ] \
  && ok "none of the three texts is in the requester's task" \
  || no "a message from someone other than the requester landed in its task (or the task is unreadable)"
[ "$(in_inbox "${FDIX:-none}")" = absent ] && [ "$(received_n "${FDIX:-none}")" = 0 ] \
  && ok "the forged delegation is in neither the inbox nor the evidence chain" \
  || no "the forged delegation ${FDIX:-?} reached the provider (inbox: $(in_inbox "${FDIX:-none}"))"
EV1=$(chain_len prov); IN1=$(inbox_n)
[ -n "$EV0" ] && [ "$EV1" = "$EV0" ] && [ -n "$IN0" ] && [ "$IN1" = "$IN0" ] \
  && ok "nothing was written: $IN1 interactions and $EV1 evidence records, as before" \
  || no "the provider changed: interactions ${IN0:-?} → ${IN1:-?}, evidence ${EV0:-?} → ${EV1:-?}"

hd "9/11  a replayed envelope does nothing"
# The hub keeps no record of what it relayed, and could not recognise a sealed envelope it had seen
# before; each relay-send below is signed afresh, so its replay cache for signatures does not apply
# either. Whether a replay does anything is decided by the provider alone: a forgery fails again, and a
# genuine envelope finds its (from, mid) in the replay table (§3.6 step 10).
BS3=$(rx bad-sig)
R=$("$FIX" relay-send --home "$STR/.anet" --hub "$HUB_URL" --to "$PROV_AID" --envelope @"$RUN/forge-msg.env" 2>&1)
BS4=$(rx_above bad-sig "$BS3")
[ "$(took "$R" hub)" = yes ] && [ "$BS4" -gt "$BS3" ] && [ "$(said_n "${OIX:-none}" "$FTEXT")" = 0 ] \
  && ok "the forged bytes again: queued by the hub, refused again by the provider (bad-sig $BS3 → $BS4)" \
  || no "the replayed forgery: $(printf '%s' "$R" | head -c 160), bad-sig $BS3 → $BS4"

# A genuine one: a capability call the requester's peer carried directly and kept a copy of
# (anetpeer --tee-dir), replayed by the stranger through the hub and over the peer wire at once.
TEE_N=$(ls "$RUN/tee" 2>/dev/null | wc -l)
RIX=$(delegate_cap req org.info '{}')
REFF=$(result_of req "${RIX:-none}")
[ "$(printf '%s' "$REFF" | jget status)" = OK ] && ok "a genuine call ran over the peer transport ($RIX)" \
  || no "the genuine call did not run: $(printf '%s' "$REFF" | head -c 200)"
# The peer writes its copy once the provider acknowledged the delegation, and a short call is answered
# before it is acknowledged: the result can be here a moment before the copy is.
TEEF=
for _ in $(seq 1 20); do
  TEEF=$(ls "$RUN/tee" 2>/dev/null | sort | tail -n +"$((TEE_N + 1))" | grep -m1 -F -- "-$PROV_AID.env")
  [ -n "$TEEF" ] && break
  sleep 0.25
done
if [ -n "$TEEF" ] && [ -n "$RIX" ]; then
  sleep 1   # the call's own evidence records are written around the result, not before it
  EV2=$(chain_len prov); IN2=$(inbox_n); DUP0=$(rx duplicate); RCV2=$(received_n "$RIX")
  R=$("$FIX" relay-send --home "$STR/.anet" --hub "$HUB_URL" --p2p "$RUN/peer/prov.wire" --to "$PROV_AID" \
        --envelope @"$RUN/tee/$TEEF" 2>&1)
  [ "$(took "$R" hub)" = yes ] && [ "$(took "$R" p2p)" = yes ] \
    && ok "the captured envelope was taken on both paths: nothing on the way can tell it is a replay" \
    || no "the replay was not delivered: $(printf '%s' "$R" | head -c 200)"
  DUP1=$(rx_above duplicate "$((DUP0 + 1))")
  [ "$DUP1" -ge "$((DUP0 + 2))" ] && ok "both copies were recognised as already processed: duplicate $DUP0 → $DUP1" \
    || no "duplicate went $DUP0 → $DUP1, expected at least +2"
  sleep 1
  EV3=$(chain_len prov); IN3=$(inbox_n); RCV3=$(received_n "$RIX")
  [ -n "$IN2" ] && [ "$IN3" = "$IN2" ] && [ -n "$EV2" ] && [ "$EV3" = "$EV2" ] && [ "$RCV2" = 1 ] && [ "$RCV3" = 1 ] \
    && ok "processed once: $IN3 interactions and $EV3 evidence records as before, one delegation.received for it" \
    || no "the replay changed the provider: interactions $IN2 → $IN3, evidence $EV2 → $EV3, delegation.received $RCV2 → $RCV3"
else
  no "the requester's peer kept no copy of the call to replay (anetpeer --tee-dir: $(ls "$RUN/tee" 2>/dev/null | wc -l) files)"
  no "(replay delivery unchecked)"; no "(replay processing unchecked)"
fi

hd "10/11  one envelope over p2p and the hub at once is processed once (SI-10)"
# A fresh message, sealed with the requester's own key — everything about it genuine — and carrying no
# message id of its own, so that nothing but the envelope's replay record can tell the two copies
# apart. anetfixture hands the same bytes to the provider's peer and to the hub at the same instant; the
# peer answers once the provider has decided, and the hub copy follows at the provider's next poll. The
# exact interleaving is the daemon's unit test (TestTheSameEnvelopeOverP2PAndHubIsProcessedOnce); this
# is the deployed binaries agreeing with it.
STEXT="one envelope, two paths $(rand)"
SMSG=$("$FIX" seal --home "$REQ/.anet" --hub "$HUB_URL" --to "$PROV_AID" --type message --ix "${OIX:-none}" \
         --no-msg-id --text "$STEXT" 2>/dev/null)
DUP2=$(rx duplicate)
R=$("$FIX" relay-send --home "$REQ/.anet" --hub "$HUB_URL" --p2p "$RUN/peer/prov.wire" --to "$PROV_AID" \
      --envelope "${SMSG:-none}" 2>&1)
[ "$(took "$R" hub)" = yes ] && [ "$(took "$R" p2p)" = yes ] && ok "both paths took the message" \
  || no "a path did not take the message: $(printf '%s' "$R" | head -c 200)"
DUP3=$(rx_above duplicate "$DUP2")
[ "$DUP3" -gt "$DUP2" ] && ok "the second copy was recognised: duplicate $DUP2 → $DUP3" \
  || no "no duplicate was counted ($DUP2 → $DUP3)"
N=$(said_n "${OIX:-none}" "$STEXT")
[ "$N" = 1 ] && ok "the message is in the task once" || no "the message is in the task ${N:-?} times"

# The same for a new task: one delegation on both paths at once is one interaction, with one
# anet.delegation.received record.
IN4=$(inbox_n); DUP4=$(rx duplicate)
SDEL=$("$FIX" seal --home "$REQ/.anet" --hub "$HUB_URL" --to "$PROV_AID" --type delegate \
         --text "joint two-path task $(rand)" 2>"$RUN/twopath.txt")
SDIX=$(sed -n 's/^ix: //p' "$RUN/twopath.txt")
R=$("$FIX" relay-send --home "$REQ/.anet" --hub "$HUB_URL" --p2p "$RUN/peer/prov.wire" --to "$PROV_AID" \
      --envelope "${SDEL:-none}" 2>&1)
[ "$(took "$R" hub)" = yes ] && [ "$(took "$R" p2p)" = yes ] && ok "both paths took the delegation" \
  || no "a path did not take the delegation: $(printf '%s' "$R" | head -c 200)"
DUP5=$(rx_above duplicate "$DUP4")
sleep 1
IN5=$(inbox_n); RCV5=$(received_n "${SDIX:-none}")
[ "$DUP5" -gt "$DUP4" ] && [ "$(in_inbox "${SDIX:-none}")" = peer ] && [ -n "$IN4" ] && [ "$IN5" = "$((IN4 + 1))" ] \
  && [ "$RCV5" = 1 ] \
  && ok "one task and one evidence record: interactions $IN4 → $IN5, duplicate $DUP4 → $DUP5" \
  || no "two-path delegation: interactions ${IN4:-?} → ${IN5:-?}, delegation.received $RCV5, duplicate $DUP4 → $DUP5"
# The last case of SI-10 — a copy that hits a store failure, is not acknowledged, and is delivered
# again — needs a fault a released daemon does not have. The daemon's unit tests cover it on both paths:
# TestAStoreFailureThenRedeliveryIsProcessedOnce (hub, then hub) and
# TestAStoreFailureOnTheP2PCopyThenRedeliveryThroughTheHubIsProcessedOnce (p2p, then hub, then p2p).
note "store failure then redelivery: covered by internal/daemon unit tests (no fault injection in a release build)"

hd "11/11  evidence — the chain survives the run and a restart"
# Both chains must hold what this run did. Zero records is a failure, not a pass: it is what a
# run looks like in which the provider never came up.
REC=$(rc /evidence '{"limit":1}' | jget head length)
PREC=$(pc /evidence '{"limit":1}' | jget head length)
[ "${REC:-0}" -gt 0 ] && ok "the requester's chain holds $REC records" \
  || no "the requester's chain is empty or unreadable"
[ "${PREC:-0}" -gt 0 ] && ok "the provider's chain holds $PREC records" \
  || no "the provider's chain is empty or unreadable (is the provider up?)"
stop_node req || no "the requester did not stop"
start_node req "$RUN/req-restart.log"
if wait_up "$RC" 30; then
  AFTER=$(rc /evidence '{"limit":1}' | jget head length)
  [ "${REC:-0}" -gt 0 ] && [ "${AFTER:-0}" -ge "$REC" ] \
    && ok "the requester restarted on the same chain ($REC → ${AFTER} records, receipts included)" \
    || no "after the restart the requester's chain holds ${AFTER:-nothing}, before it held ${REC:-nothing}"
else
  no "the requester will not restart: $(tail -3 "$REQ/.anet/daemon.log" 2>/dev/null)"
fi

printf '\n\033[1m── %d passed, %d failed ──\033[0m   logs: %s\n' "$pass" "$fail" "$RUN"
[ "$fail" -eq 0 ]
