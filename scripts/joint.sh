#!/usr/bin/env bash
# joint.sh — drive every module end to end: real daemons, a real hub, real peer processes.
#
# Self-contained. On a clean Linux host with bash, curl and python3 it gets its own binaries, starts
# its own hub (and hub admin), three daemons and two peer processes — and for section C a second hub,
# a recording tap, three more daemons and two anet-official backends — on a loopback port block of its
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
#                      anet-hub-admin, anet-official and ANetHub's hub-db-roll.sh (scripts/testnet/build.sh
#                      makes the binaries, in its linux-<arch> directory — copy deploy/hub-db-roll.sh in
#                      beside them; scripts/mutations/mutate.sh build makes a mutated set with it).
#                      They are copied into $J/bin. Unset: built here with go from this checkout and
#                      HUB_SRC.
#   HUB_SRC            ANetHub checkout to build from (default: ../ANetHub beside this repository)
#   JOINT_PORT_BASE    first of 16 consecutive loopback ports; unset = a random free block in
#                      20000-32000. A port already in use aborts the run; nothing is killed to free it.
#   JOINT_HUB_ADMIN    0 = do not start anet-hub-admin (started by default when the binary is there;
#                      section C counts its absence as a failure: its data and API are part of SI-1)
#   JOINT_CANARY       0 = skip section C, the SI-1 canary (run by default)
#   JOINT_CANARY_ONLY  1 = stop after section C (what scripts/mutations/mutate.sh runs)
#   JOINT_HUB_ROLL     the hub's backup script, run once in section C (after the first search, since
#                      it truncates the WAL) to put a backup into the hub's data directory, which is then
#                      searched again (default: hub-db-roll.sh in JOINT_BIN, else deploy/hub-db-roll.sh of
#                      HUB_SRC or ../ANetHub). Where the sqlite3 CLI it uses is not installed, canary.py
#                      stands in for it. Without the script the backup is not made and the run says so.
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
# Section C is SI-1 (A2A-DESIGN §1): the hub and its admin never hold task content. Three more daemons
# — a requester, a provider and an official agent (anet-official's net.echo behind a closed daemon with
# public_capabilities, as deploy/official ships it) — reach the hub through a recording tap
# (scripts/canary.py) and exchange a prose goal, chat turns, attachments both ways, a capability call,
# a paid call settled at the hub and a call to the official agent, each carrying its own random
# canary. After two admin harvest/snapshot periods, the hub's data directory (WAL as the run left it),
# the admin's, the tap's record of everything the hub was sent and answered, the hub's and the admin's
# HTTP responses (/agents/{aid}, /fed/v1/reviews, /api/sessions*, …) and their logs are searched for
# the canaries as raw bytes, hex and base64 (every alignment): no hit may be found; then the hub's
# weekly backup is taken (deploy/hub-db-roll.sh) and its data directory searched again. The same search over
# the recipients' own data must find them (so the search can see what it looks for), the recipients
# must read each canary back, every /x402/settle body must carry no resource, description or extra, and
# the admin's removed official-agent routes must answer 404. The search runs again at the end of the
# run. scripts/mutations/si1-*.patch are mutations it must catch (scripts/mutations/mutate.sh).
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
# lib.sh: peer_allow, the stop-by-path helpers and the canary helpers. ANET is set first so sourcing it
# looks nothing up; CANARY_PY names the copy of canary.py this run puts in $BIN (see section 0).
ANET=$BIN/anet; CANARY_PY=$BIN/canary.py
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
J=$(cd "$J" && pwd -P); BIN=$J/bin; RUN=$J/run; ANET=$BIN/anet; CANARY_PY=$BIN/canary.py
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

# The last three are section C's canary nodes: requester, provider, official agent.
home_of(){ case $1 in req) echo "$REQ" ;; prov) echo "$PROV" ;; str) echo "$STR" ;;
                      cr) echo "$CR" ;; cp) echo "$CP" ;; off) echo "$OFF" ;; esac; }
addr_of(){ case $1 in req) echo "$RC" ;; prov) echo "$PC" ;; str) echo "$SC" ;;
                      cr) echo "$CRC" ;; cp) echo "$CPC" ;; off) echo "$OFC" ;; esac; }
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

# ── section C: the SI-1 canary (A2A-DESIGN §1 SI-1, §17) ────────
# The functions are here; section C calls canary_flow after the stack is up, and canary_sweep again at
# the end of the run. Everything they write is under $CAN; what they search is outside it (the hub's and
# the admin's data directories, their logs) or is the hub side's own output captured under $CAN (the
# tap's record, the HTTP responses) — never the canary list or a recipient's copy of the content.

# delegate_to <node> <provider-aid> <capability> <args-json> — a capability call from that node; prints the id.
delegate_to(){ ctl "$1" /delegate "{\"provider\":\"$2\",\"capability\":\"$3\",\"args\":$4}" | jget interaction_id; }
# thread_has <node> <interaction_id> <text> [seconds] — wait until the node's copy of that task holds text
# (the goal, a message, the final answer): what a recipient decrypted is what its /thread shows.
thread_has(){
  local i t
  for ((i = 0; i < ${4:-30} * 2; i++)); do
    # Not piped into grep -q: under pipefail, grep leaving early fails the pipeline it matched in.
    t=$(ctl "$1" /thread "{\"interaction_id\":\"$2\"}")
    grep -qF -- "$3" <<<"$t" && return 0
    sleep 0.5
  done
  return 1
}
# admin_call <METHOD> <path> [json] — the admin API under /admin/api with the operator token (handed to curl
# on a file descriptor, not in argv); the body lands in $CAN/admin.out and the HTTP status is printed.
admin_call(){
  local tok extra=()
  tok=$(cat "$RUN/admin.token" 2>/dev/null)
  [ $# -ge 3 ] && extra=(-H 'Content-Type: application/json' --data-binary "$3")
  curl -s -m 20 -o "$CAN/admin.out" -w '%{http_code}' -X "$1" \
       -H @<(printf 'Authorization: Bearer %s\n' "$tok") "${extra[@]}" "http://$ADMIN_ADDR/admin/api$2"
}
# fetch_to <file> <url> [admin] — GET into file, with the admin token when asked; prints the HTTP status.
fetch_to(){
  local tok
  if [ "${3:-}" = admin ]; then
    tok=$(cat "$RUN/admin.token" 2>/dev/null)
    curl -s -m 20 -o "$1" -w '%{http_code}' -H @<(printf 'Authorization: Bearer %s\n' "$tok") "$2"
  else
    curl -s -m 20 -o "$1" -w '%{http_code}' "$2"
  fi
}
# surface <phase> <name> <what> <canary_scan options and paths…> — one SI-1 search: a pass is no canary,
# in any encoding, anywhere under the paths. The report is $CAN/scan-<phase>-<name>.json.
surface(){
  local ph=$1 name=$2 what=$3 out rc; shift 3
  out=$(canary_scan "$CANARIES" "$CAN/scan-$ph-$name.json" "$what" "$@" 2>&1); rc=$?
  case $rc in
    0) ok "$out" ;;
    1) no "CANARY FOUND — $out (details: $CAN/scan-$ph-$name.json)" ;;
    *) no "not searched — $out" ;;
  esac
}
# control <name> <what> <canary_scan --want options and paths…> — the positive control: the same search
# over a recipient's own data finds the canaries it must hold, so a zero elsewhere is not blindness.
control(){
  local name=$1 what=$2 out rc; shift 2
  out=$(canary_scan "$CANARIES" "$CAN/control-$name.json" "$what" "$@" 2>&1); rc=$?
  [ "$rc" = 0 ] && ok "the same search sees them where they belong — $out" \
    || no "the search does not find what the recipient holds, so its zeros prove nothing — $out"
}

CANARY_RAN=0
# canary_flow — section C up to its first search: the tap, the official agent, the three canary nodes,
# the admin's official-agent routes, and the content, each piece with its own canary, read back by its
# recipient. Returns non-zero when it could not get far enough to search anything.
canary_flow(){
  local i b n code r1 r2 r3 man legacy c1 c2 c3 c4 c5 st
  local G ATT1 ASK ATT2 CHAT DONE ARG PAY OFFA T AIX PIX OIX R
  rm -rf "$CAN"; mkdir -p "$CAN/files" "$CAN/pull-cp" "$CAN/pull-cr"; : > "$CANARIES"
  if [ ! -x "$BIN/anet-official" ]; then
    no "no anet-official in $BIN: a call to an official agent is part of SI-1 (build it, or put it in JOINT_BIN)"
    return 1
  fi
  HAVE_ADMIN=0
  if curl -sf -m 3 "http://$ADMIN_ADDR/admin/healthz" >/dev/null 2>&1; then HAVE_ADMIN=1
  else no "the hub admin is not running (JOINT_HUB_ADMIN=0, or no anet-hub-admin): its data and its API are part of SI-1"
  fi

  # The tap. Only the canary nodes use it: their hub URL is the tap's, so everything they send the hub
  # and everything the hub answers them is on record, byte for byte.
  if TAP_PID=$(canary_tap "$CAN/tap" "$TAP_ADDR" "$HUB_URL" "$CAN/tap.log") \
     && curl -sf -m 5 "$TAP_URL/healthz" >/dev/null 2>&1; then
    ok "a recording tap in front of the hub for the canary nodes ($TAP_URL)"
  else
    no "the tap did not come up: $(tail -2 "$CAN/tap.log" 2>/dev/null)"; return 1
  fi

  # Two anet-official backends, one per identity, each with its own token (deploy/official).
  for b in off cp; do
    ( umask 077; python3 -c 'import secrets;print(secrets.token_urlsafe(32))' > "$CAN/$b.token" )
  done
  ( cd "$RUN" && exec setsid "$BIN/anet-official" serve -listen "$OFF_BACK" -token-file "$CAN/off.token" \
      -groups echo ) >"$CAN/off-backend.log" 2>&1 </dev/null 9>&- &
  ( cd "$RUN" && exec setsid "$BIN/anet-official" serve -listen "$CP_BACK" -token-file "$CAN/cp.token" \
      -groups echo ) >"$CAN/cp-backend.log" 2>&1 </dev/null 9>&- &
  code=
  for b in "$OFF_BACK" "$CP_BACK"; do
    for ((i = 0; i < 40; i++)); do
      [ "$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://$b/")" = 401 ] && break
      sleep 0.25
    done
    code="$code $(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://$b/")"
  done
  [ "$code" = " 401 401" ] && ok "two anet-official backends, each refusing a caller without its token (401)" \
    || { no "the anet-official backends answer${code}: $(tail -2 "$CAN/off-backend.log")"; return 1; }

  # The nodes. The requester pays small quotes by itself (auto tier, A2A-DESIGN §8.6) to the payees on
  # its list. The provider runs the default closed policy with the requester on its allow list, and
  # serves anet-official's echo twice, free and for 3 credits. The official agent is configured with
  # what `anet-official service-config` generates, as deploy/official ships it: closed, net.echo public.
  mkdir -p "$CR/.anet" "$CP/.anet" "$OFF/.anet"
  "$BIN/anet-official" service-config -groups echo -url "http://$OFF_BACK" -token-file "$CAN/off.token" \
    > "$CAN/off-service.json" 2>"$CAN/off-service.err" \
    || { no "anet-official service-config failed: $(head -c 200 "$CAN/off-service.err")"; return 1; }
  python3 - "$CR/.anet/config.json" "$CP/.anet/config.json" "$OFF/.anet/config.json" "$CAN/off-service.json" \
            "$CRC" "$CPC" "$OFC" "$TAP_URL" "http://$CP_BACK/v1/echo/net.echo" "$CAN/cp.token" <<'PY'
import json, sys
crp, cpp, offp, offsvc, crc, cpc, ofc, tap, cpurl, cptok = sys.argv[1:11]
def write(p, c):
    with open(p, "w") as f:
        json.dump(c, f, indent=1)
write(crp, {"control_addr": crc, "hub_url": tap, "name": "canary-requester",
            "payments": {"auto_max": 5, "agent_max": 0, "agent_daily_max": 20, "explicit_max": 10,
                         "daily_max": 50, "payees_file": "payees.allow"}})
svc = {"token_file": cptok, "capabilities": [
    {"id": "canary.echo", "url": cpurl, "timeout_ms": 5000,
     "description": "echo (joint.sh SI-1 canary)"},
    {"id": "canary.echo.paid", "url": cpurl, "price": 3, "timeout_ms": 5000,
     "description": "echo for 3 credits (joint.sh SI-1 canary)"}]}
write(cpp, {"control_addr": cpc, "hub_url": tap, "name": "canary-provider", "modules": {"service": svc}})
off = json.load(open(offsvc))
off.update({"control_addr": ofc, "hub_url": tap, "name": "canary-official"})
write(offp, off)
PY
  for n in cr cp off; do start_node $n "$RUN/$n.log"; done
  for n in cr cp off; do
    wait_up "$(addr_of $n)" 30 \
      || { no "canary node $n did not come up: $(tail -2 "$RUN/$n.log") $(tail -2 "$(home_of $n)/.anet/daemon.log" 2>/dev/null)"; return 1; }
  done
  CR_AID=$("$FIX" aid --home "$CR/.anet"); CP_AID=$("$FIX" aid --home "$CP/.anet"); OFF_AID=$("$FIX" aid --home "$OFF/.anet")
  [ -n "$CR_AID" ] && [ -n "$CP_AID" ] && [ -n "$OFF_AID" ] || { no "the canary nodes have no identities"; return 1; }
  peer_allow "$CP/.anet" "$CR_AID"
  _peer_add "$CR/.anet/payees.allow" "$CP_AID"
  r1=$(ctl cr /hub-register "{\"hub\":\"$TAP_URL\",\"name\":\"canary-requester\"}" | jget status)
  r2=$(ctl cp /hub-register "{\"hub\":\"$TAP_URL\",\"name\":\"canary-provider\"}" | jget status)
  r3=$(ctl off /hub-register "{\"hub\":\"$TAP_URL\",\"name\":\"canary-official\"}" | jget status)
  [ "$r1 $r2 $r3" = "registered registered registered" ] \
    && ok "canary requester, provider and official agent registered, through the tap" \
    || { no "canary registrations: requester '$r1', provider '$r2', official '$r3'"; return 1; }
  printf '  canary requester %s\n  canary provider  %s\n  official agent   %s\n' "$CR_AID" "$CP_AID" "$OFF_AID"

  # The official agent in the admin: a registry entry and nothing more (A2A-DESIGN §9, §15, [C39]).
  if [ "$HAVE_ADMIN" = 1 ]; then
    man=$(printf '{"id":"canary-echo","name":"canary echo","tier":"official","product_line":"agentnetwork","aid":"%s","hub":"%s","caps":["net.echo"]}' \
            "$OFF_AID" "$HUB_URL")
    code=$(admin_call POST /official "$man")
    [ "$code" = 200 ] && ok "the official agent is registered in the admin by id, aid, hub and caps" \
      || no "registering the official agent in the admin answered $code: $(head -c 200 "$CAN/admin.out")"
    # What the admin could once be given: the agent's console (monitor, with its token) and a harvest
    # of its jobs — a channel into the official agent's tasks from the hub host. Refused; a build that
    # takes it harvests them (scripts/mutations/si1-restore-official-harvest.patch).
    legacy=$(printf '{"id":"canary-echo","name":"canary echo","tier":"official","product_line":"agentnetwork","aid":"%s","hub":"%s","caps":["net.echo"],"monitor":{"url":"http://%s","auth":"token","token_file":"%s"},"datasets":{"harvest":true}}' \
            "$OFF_AID" "$HUB_URL" "$OFC" "$OFF/.anet/control_token.txt")
    code=$(admin_call POST /official "$legacy")
    [ "$code" = 400 ] && ok "a manifest carrying a monitor and a harvest is refused (400): no channel into the official agent" \
      || no "the admin answered $code to a manifest carrying a monitor and a harvest: $(head -c 200 "$CAN/admin.out")"
    code=$(admin_call GET /official)
    [ "$code" = 200 ] && grep -qF '"canary-echo"' "$CAN/admin.out" \
      && c1=$(admin_call GET /official/canary-echo/insights) \
      && c2=$(admin_call POST /official/canary-echo/acl '{"deny":[]}') \
      && c3=$(admin_call GET /official/canary-echo/monitor/state) \
      && c4=$(admin_call GET /official/canary-echo/monitor/logs) \
      && c5=$(admin_call POST /official/canary-echo/ops '{"op":"status"}')
    [ "$code" = 200 ] && [ "${c1:-} ${c2:-} ${c3:-} ${c4:-} ${c5:-}" = "404 404 404 404 404" ] \
      && ok "for the registered official agent, /insights, /acl, /monitor/state, /monitor/logs and /ops answer 404" \
      || no "the admin's official-agent routes: list $code, insights ${c1:-?}, acl ${c2:-?}, monitor/state ${c3:-?}, monitor/logs ${c4:-?}, ops ${c5:-?} (want 404 each)"
  fi

  # The content. Every piece gets its own canary, and every canary is read back by its recipient.
  G=$(canary_new "$CANARIES" goal);             ATT1=$(canary_new "$CANARIES" attachment-up)
  ASK=$(canary_new "$CANARIES" question);       ATT2=$(canary_new "$CANARIES" attachment-down)
  CHAT=$(canary_new "$CANARIES" chat);          DONE=$(canary_new "$CANARIES" answer)
  ARG=$(canary_new "$CANARIES" capability-args); PAY=$(canary_new "$CANARIES" paid-args)
  OFFA=$(canary_new "$CANARIES" official-args)
  # Every one, not the first and the last: an empty canary is a piece of content with nothing to find,
  # and grep -F "" matches any thread, so its read-back would pass as well.
  for b in "$G" "$ATT1" "$ASK" "$ATT2" "$CHAT" "$DONE" "$ARG" "$PAY" "$OFFA"; do
    [ -n "$b" ] || { no "a canary was not minted ($(wc -l < "$CANARIES") of 9 in $CANARIES)"; return 1; }
  done
  CANARY_RAN=1
  # Attachments: the canary between random bytes, as content sits in a binary file.
  python3 - "$CAN/files/report.bin" "$ATT1" "$CAN/files/figure.bin" "$ATT2" <<'PY'
import os, sys
for path, c in ((sys.argv[1], sys.argv[2]), (sys.argv[3], sys.argv[4])):
    with open(path, "wb") as f:
        f.write(os.urandom(700) + c.encode() + os.urandom(900))
PY

  # A prose task with a file, a question back with a file, an answer, a final answer, a review.
  T=$(ctl cr /delegate "{\"provider\":\"$CP_AID\",\"goal\":\"Please look into $G\",\"attachments\":[\"$CAN/files/report.bin\"]}" \
        | jget interaction_id)
  [ -n "$T" ] && thread_has cp "$T" "$G" \
    && ok "the provider reads the goal's canary in its copy of the task ($T)" \
    || no "the provider does not hold the goal (task '${T:-none}')"
  ctl cp /pull "{\"interaction_id\":\"${T:-none}\",\"out_dir\":\"$CAN/pull-cp\"}" >/dev/null
  grep -rqaF -- "$ATT1" "$CAN/pull-cp" && ok "and the attachment's bytes, pulled to disk" \
    || no "the provider's pulled attachment does not hold its canary ($(ls "$CAN/pull-cp" 2>/dev/null | head -3))"
  ctl cp /message "{\"interaction_id\":\"${T:-none}\",\"body\":\"Which part: $ASK?\",\"attachments\":[\"$CAN/files/figure.bin\"]}" >/dev/null
  thread_has cr "${T:-none}" "$ASK" && ok "the requester reads the provider's question" \
    || no "the provider's question did not reach the requester"
  ctl cr /pull "{\"interaction_id\":\"${T:-none}\",\"out_dir\":\"$CAN/pull-cr\"}" >/dev/null
  grep -rqaF -- "$ATT2" "$CAN/pull-cr" && ok "and the file that came with it" \
    || no "the requester's pulled attachment does not hold its canary ($(ls "$CAN/pull-cr" 2>/dev/null | head -3))"
  ctl cr /message "{\"interaction_id\":\"${T:-none}\",\"body\":\"This part: $CHAT\"}" >/dev/null
  thread_has cp "${T:-none}" "$CHAT" && ok "the provider reads the requester's answer" \
    || no "the requester's answer did not reach the provider"
  ctl cp /tasks/reply "{\"task_id\":\"${T:-none}\",\"text\":\"Done: $DONE\",\"state\":\"completed\"}" >/dev/null
  st=
  for ((i = 0; i < 60; i++)); do
    st=$(ctl cr /thread "{\"interaction_id\":\"${T:-none}\"}" | jget thread state)
    [ "$st" = completed ] && break
    sleep 0.5
  done
  [ "$st" = completed ] && thread_has cr "${T:-none}" "$DONE" 5 \
    && ok "the task completed, and the requester holds the final answer" \
    || no "the task ended '${st:-unknown}' at the requester, or without the final answer"
  R=$(ctl cr /review "{\"interaction_id\":\"${T:-none}\",\"rating\":5,\"comment\":\"joint canary run\"}")
  [ "$(printf '%s' "$R" | jget uploaded)" = True ] && ok "the requester's review of it went to the hub" \
    || no "the review was not uploaded: $(printf '%s' "$R" | head -c 200)"

  # A capability call, a paid one, and one to the official agent, each with a canary in its arguments;
  # the echo brings the arguments back, which is the recipient's backend having read them.
  AIX=$(delegate_to cr "$CP_AID" canary.echo "{\"note\":\"$ARG\"}")
  R=$(result_of cr "${AIX:-none}")
  grep -qF -- "$ARG" <<<"$R" && ok "a capability call's arguments reached the provider's backend and came back" \
    || no "the capability call: $(printf '%s' "$R" | head -c 200)"
  PIX=$(delegate_to cr "$CP_AID" canary.echo.paid "{\"note\":\"$PAY\"}")
  R=$(result_of cr "${PIX:-none}" 120)
  grep -qF -- "$PAY" <<<"$R" \
    && ok "a priced call was quoted, paid within auto_max, settled at the hub and answered" \
    || no "the paid call ($(ctl cr /thread "{\"interaction_id\":\"${PIX:-none}\"}" | jget thread state)): $(printf '%s' "$R" | head -c 200)"
  n=$(ctl cr /evidence '{"event_type":"anet.payment.settled","limit":50}' | python3 -c '
import sys, json
try:
    print(len(json.load(sys.stdin).get("records") or []))
except Exception:
    print(0)')
  [ "${n:-0}" -ge 1 ] && ok "the requester's chain records the settlement ($n anet.payment.settled)" \
    || no "no anet.payment.settled on the requester's chain"
  OIX=$(delegate_to cr "$OFF_AID" net.echo "{\"note\":\"$OFFA\"}")
  R=$(result_of cr "${OIX:-none}")
  grep -qF -- "$OFFA" <<<"$R" \
    && ok "the official agent (closed, net.echo public) served the requester, a stranger to it" \
    || no "the official agent's call: $(printf '%s' "$R" | head -c 200)"
  # The call line names the verified caller (the daemon's X-ANet-Caller); the arguments are nowhere.
  grep -qF -- "cap=net.echo status=200 caller=$CR_AID" "$CAN/off-backend.log" \
    && ! grep -qF -- "$OFFA" "$CAN/off-backend.log" \
    && ok "its backend logged the call, with the requester as caller, and not the arguments (A2A-DESIGN §15)" \
    || no "the official backend's log: $(tail -1 "$CAN/off-backend.log" | head -c 200)"

  # Two harvest and snapshot periods of the admin (2 s each), one harvest asked for outright. The hub's
  # backup is taken after the first search (canary_backup): the backup script checkpoints and truncates
  # the WAL, and the WAL as the run left it is one of the things searched.
  if [ "$HAVE_ADMIN" = 1 ]; then
    code=$(admin_call POST /harvest '{}')
    [ "$code" = 200 ] || no "POST /admin/api/harvest answered $code"
  fi
  sleep 5

  # The content went through the tap: the canary nodes' envelopes were sent and fetched there. Without
  # this, a node that reached the hub some other way would leave the tap's record empty of content, and
  # its search would pass for having found nothing.
  R=$(python3 - "$CAN/tap/index.jsonl" <<'PY'
import collections, json, sys
n = collections.Counter()
try:
    for line in open(sys.argv[1]):
        e = json.loads(line)
        if 200 <= e.get("status", 0) < 300:
            n[e.get("method", "") + " " + e.get("path", "").split("?", 1)[0]] += 1
except (OSError, ValueError):
    pass
print(n["POST /relay/send"], n["POST /relay/ack"], n["POST /x402/settle"])
PY
)
  read -r c1 c2 c3 <<<"$R"
  [ "${c1:-0}" -ge 4 ] && [ "${c2:-0}" -ge 1 ] && [ "${c3:-0}" -ge 1 ] \
    && ok "the canary nodes' traffic went through the tap: $c1 envelopes sent, $c2 acknowledged after a poll, $c3 settled" \
    || no "the tap did not see the canary nodes' traffic (relay/send ${c1:-0}, relay/ack ${c2:-0}, x402/settle ${c3:-0}; want 4+, 1+, 1+): its search would prove nothing"

  # What reached the hub's facilitator for the paid call (SI-1, X4): nothing about the work.
  R=$(canary_settle "$CAN/tap" "$CAN/settle.json" "$RUN/hub/hub.db" 2>&1) \
    && ok "every /x402/settle body carries no resource, description or extra, nor anything outside x402 v2 — $R" \
    || no "the settlement the hub saw — $R (details: $CAN/settle.json)"
  return 0
}

# canary_backup — the hub's weekly backup, taken the way production takes it (ANetHub
# deploy/hub-db-roll.sh, FORCE_WEEKLY=1) into the hub's data directory, and searched. Where the sqlite3
# CLI the script needs is not installed (docs/notes/0015 §2: only dmax has it), canary.py stands in for it
# on the script's PATH, on python3's own SQLite.
canary_backup(){
  local roll=${JOINT_HUB_ROLL:-} b sq=""
  if [ -z "$roll" ]; then
    for b in "$BIN/hub-db-roll.sh" "$ROOT/../ANetHub/deploy/hub-db-roll.sh"; do
      [ -f "$b" ] && { roll=$b; break; }
    done
  fi
  if [ -z "$roll" ]; then
    note "no hub backup taken: no hub-db-roll.sh (put ANetHub's deploy/hub-db-roll.sh in JOINT_BIN, or set JOINT_HUB_ROLL); a backup is not among what is searched"
    return 0
  fi
  if ! command -v sqlite3 >/dev/null; then
    mkdir -p "$BIN/sqlite3-standin"
    printf '#!/bin/sh\nexec python3 %q sqlite3 "$@"\n' "$BIN/canary.py" > "$BIN/sqlite3-standin/sqlite3"
    chmod 700 "$BIN/sqlite3-standin/sqlite3"
    sq=" (no sqlite3 CLI here: canary.py stood in for it)"
  fi
  rm -f "$RUN/hub"/hub-backup-*.db
  if PATH="${sq:+$BIN/sqlite3-standin:}$PATH" HUB_DATA_DIR="$RUN/hub" FORCE_WEEKLY=1 bash "$roll" >"$CAN/roll.log" 2>&1 \
     && b=$(cd "$RUN/hub" && ls hub-backup-*.db 2>/dev/null | head -1) && [ -n "$b" ]; then
    ok "the hub's weekly backup was taken into its data directory ($b)$sq"
    surface "$1" backup "the hub's backup ($b) and its data directory after the roll" --expect "$b" --expect hub.db "$RUN/hub"
  else
    no "the hub backup (hub-db-roll.sh$sq) failed: $(tail -2 "$CAN/roll.log")"
  fi
}

# canary_sweep <phase> [controls] — fetch the hub's and the admin's HTTP answers, then search every
# SI-1 surface; with "controls", also run the positive controls over the recipients' own data.
canary_sweep(){
  local ph=$1 d=$CAN/http-$1 p aid code n=0 src sid
  rm -rf "$d"; mkdir -p "$d"
  hubget(){ n=$((n + 1)); printf '%s %s\n' "$(fetch_to "$d/hub-$n.body" "$HUB_URL$1")" "$1" >> "$d/index.txt"; }
  for p in / /llms.txt /healthz /stats /graph /agents /a2a/v1/agents /p2p/peers /x402/supported /x402/supply \
           /x402/issuance /x402/issuance/head /x402/witnesses /fed/v1/cards /fed/v2/cards; do
    hubget "$p"
  done
  code=$(fetch_to "$d/fed-reviews.body" "$HUB_URL/fed/v1/reviews")
  printf '%s /fed/v1/reviews\n' "$code" >> "$d/index.txt"
  for aid in "$REQ_AID" "$PROV_AID" "$STR_AID" "${CR_AID:-}" "${CP_AID:-}" "${OFF_AID:-}"; do
    [ -n "$aid" ] || continue
    for p in "" /card /kel /reputation /p2p /jwks.json; do hubget "/agents/$aid$p"; done
    hubget "/a2a/v1/agents/$aid/card"
  done
  # Key sets only for the canary nodes: the hub rate-limits key lookups per client address.
  for aid in "${CR_AID:-}" "${CP_AID:-}" "${OFF_AID:-}"; do [ -n "$aid" ] && hubget "/agents/$aid/keys"; done
  # The review stream must answer and carry the canary task's review, or its zero hits mean nothing.
  [ "$code" = 200 ] && python3 -c '
import json, sys
sys.exit(0 if (json.load(open(sys.argv[1])).get("reviews") or []) else 1)' "$d/fed-reviews.body" 2>/dev/null \
    && ok "the hub's review stream (/fed/v1/reviews) answers and carries the review" \
    || no "the hub's review stream answered $code without the review: $(head -c 160 "$d/fed-reviews.body" 2>/dev/null)"
  if [ "${HAVE_ADMIN:-0}" = 1 ]; then
    # /discover and /vision included: their vector-search client points at this run's dead address
    # (DEAD_ADDR), so they answer from the lexical fallback and reach nothing outside the run.
    for p in /overview /agents /official /capabilities /store /sessions /reviews /audit /deleted \
             '/discover?task=echo' /vision; do
      n=$((n + 1)); printf '%s admin%s\n' "$(fetch_to "$d/admin-$n.body" "http://$ADMIN_ADDR/admin/api$p" admin)" "$p" >> "$d/index.txt"
    done
    for aid in "$REQ_AID" "$PROV_AID" "$STR_AID" "${CR_AID:-}" "${CP_AID:-}" "${OFF_AID:-}"; do
      [ -n "$aid" ] || continue
      n=$((n + 1)); printf '%s admin/agents/%s\n' "$(fetch_to "$d/admin-$n.body" "http://$ADMIN_ADDR/admin/api/agents/$aid" admin)" "$aid" >> "$d/index.txt"
    done
    # Every session the admin lists, one by one (/api/sessions/{source}/{id}).
    fetch_to "$d/admin-sessions.json" "http://$ADMIN_ADDR/admin/api/sessions?limit=500" admin >/dev/null
    while IFS=$'\t' read -r src sid; do
      [ -n "$src" ] || continue
      n=$((n + 1)); printf '%s admin/sessions/%s/%s\n' \
        "$(fetch_to "$d/admin-$n.body" "http://$ADMIN_ADDR/admin/api/sessions/$src/$sid" admin)" "$src" "$sid" >> "$d/index.txt"
    done < <(python3 -c '
import json, sys, urllib.parse
try:
    rows = json.load(open(sys.argv[1])).get("sessions") or []
except Exception:
    rows = []
for r in rows:
    q = lambda s: urllib.parse.quote(str(s or ""), safe="")
    print(q(r.get("source")) + "\t" + q(r.get("session_id")))' "$d/admin-sessions.json")
  fi

  # The answers searched must be answers: a hub or an admin that is down, or refuses, leaves empty or
  # error bodies, and those hold no canary either. /agents/{aid} of the canary nodes and the admin's
  # session index are the surfaces SI-1 names; they must have answered 200.
  local want=()
  for aid in "${CR_AID:-}" "${CP_AID:-}" "${OFF_AID:-}"; do [ -n "$aid" ] && want+=("/agents/$aid"); done
  [ "${HAVE_ADMIN:-0}" = 1 ] && want+=(admin/overview admin/agents admin/official admin/sessions)
  p=$(python3 - "$d/index.txt" "${want[@]}" <<'PY'
import sys
got = {}
try:
    for line in open(sys.argv[1]):
        code, _, path = line.rstrip("\n").partition(" ")
        got[path] = code
except OSError:
    pass
print(" ".join("%s=%s" % (w, got.get(w, "none")) for w in sys.argv[2:] if got.get(w) != "200"))
PY
)
  [ -z "$p" ] && ok "the hub and the admin answered what is searched (${#want[@]} required answers 200)" \
    || no "answers missing from the search, so its zero hits would not cover them: $p"

  surface "$ph" hubdir "hub data directory (hub.db, WAL, backups, federation.db)" --expect hub.db "$RUN/hub"
  if [ "${HAVE_ADMIN:-0}" = 1 ]; then
    surface "$ph" admindir "admin data directory (admin.db, WAL, datasets)" --expect admin.db "$RUN/admin"
  fi
  surface "$ph" tap "everything the hub was sent and answered for the canary nodes" --expect traffic.log "$CAN/tap"
  surface "$ph" http "the hub's and the admin's HTTP answers ($n fetched; list: $d/index.txt)" --expect index.txt "$d"
  surface "$ph" logs "the hub's and the admin's output" "$RUN/hub.log" "$RUN/admin.log"
  surface "$ph" peer "the federation peer hub" "$RUN/hub2" "$RUN/hub2.log"
  if [ "${2:-}" = controls ]; then
    control provider "the provider's own data" --want goal --want chat --want capability-args "$CP/.anet"
    control requester "the requester's own data" --want question --want answer --want paid-args --want official-args "$CR/.anet"
  fi
}

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
  for b in anet anetfixture anetpeer anet-hub anet-hub-admin anet-official; do
    [ -x "$SRC/$b" ] || continue
    cp "$SRC/$b" "$BIN/$b" || die "cannot copy $SRC/$b into $BIN"
  done
  # The hub's backup script, for section C (scripts/mutations/mutate.sh build puts it there).
  if [ -f "$SRC/hub-db-roll.sh" ]; then cp "$SRC/hub-db-roll.sh" "$BIN/" || die "cannot copy $SRC/hub-db-roll.sh"; fi
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
  if [ -d "$ROOT/cmd/anet-official" ]; then
    go build -C "$ROOT" -o "$BIN/anet-official" ./cmd/anet-official || die "build anet-official failed"
  fi
  if [ -f "$HUB_SRC/deploy/hub-db-roll.sh" ]; then cp "$HUB_SRC/deploy/hub-db-roll.sh" "$BIN/" || die "cannot copy hub-db-roll.sh"; fi
fi
FIX=$BIN/anetfixture
# The canary tooling runs from $BIN as well: the tap is a long-running process, and stop_under finds a
# script by the path it runs from.
cp "$SCRIPTS/canary.py" "$BIN/canary.py" || die "cannot copy canary.py into $BIN"

# Sixteen loopback ports: +0 hub, +1 hub admin, +2 requester, +3 provider, +4 stranger; section C's
# +5 tap, +6 canary requester, +7 canary provider, +8 official agent, +9 and +10 their anet-official
# backends, +11 the federation peer hub, +12 an address nothing listens on (the admin's vector
# service, so that it reaches nothing on a shared host); +13..+15 spare.
PORT_BASE=$(python3 - "${JOINT_PORT_BASE:-}" 16 <<'PY'
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
# Section C (the SI-1 canary).
TAP_ADDR=127.0.0.1:$((PORT_BASE + 5)); TAP_URL=http://$TAP_ADDR
CRC=127.0.0.1:$((PORT_BASE + 6)); CPC=127.0.0.1:$((PORT_BASE + 7)); OFC=127.0.0.1:$((PORT_BASE + 8))
OFF_BACK=127.0.0.1:$((PORT_BASE + 9)); CP_BACK=127.0.0.1:$((PORT_BASE + 10))
HUB2_ADDR=127.0.0.1:$((PORT_BASE + 11)); DEAD_ADDR=127.0.0.1:$((PORT_BASE + 12))
CR=$RUN/cr; CP=$RUN/cp; OFF=$RUN/off; CAN=$RUN/canary; CANARIES=$CAN/canaries.tsv
CANARY_TMP=$RUN/tmp; mkdir -p "$CANARY_TMP"   # canary.py's database copies; nothing searches it
CANARY=${JOINT_CANARY:-1}
echo "  ports:    $PORT_BASE-$((PORT_BASE + 15))   work dir: $J"

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

# For section C the hub federates its directory with a second hub (discovery only, no delivery, no
# witnessing), so that /fed/v1/reviews — the review stream peers copy from it, one of the SI-1
# surfaces — answers at all: without a discovery peer it is refused and would pass for having nothing.
# The second hub is only a peer identity at an address; it pulls nothing.
mkdir -p "$RUN/hub"
if [ "$CANARY" != 0 ]; then
  ( cd "$RUN" && exec setsid "$BIN/anet-hub" --addr "$HUB2_ADDR" --data "$RUN/hub2" ) >"$RUN/hub2.log" 2>&1 </dev/null 9>&- &
  for _ in $(seq 1 40); do curl -sf -m 2 "http://$HUB2_ADDR/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
  HUB2_AID=$(curl -s -m 5 "http://$HUB2_ADDR/hub/identity" | jget aid)
  if [ -n "$HUB2_AID" ]; then
    printf '{"delivery":"off","discovery":"allowlist","witness":"off","home":"%s","peers":[{"aid":"%s","endpoint":"http://%s"}]}\n' \
      "$HUB_URL" "$HUB2_AID" "$HUB2_ADDR" > "$RUN/hub/federation.json"
    echo "  federation: discovery peer $HUB2_AID at $HUB2_ADDR (for /fed/v1/reviews)"
  else
    note "the federation peer hub did not come up: /fed/v1/reviews will be refused ($(tail -1 "$RUN/hub2.log"))"
  fi
fi

# The hub, on an empty data directory.
( cd "$RUN" && exec setsid "$BIN/anet-hub" --addr "$HUB_ADDR" --data "$RUN/hub" ) >"$RUN/hub.log" 2>&1 </dev/null 9>&- &
for _ in $(seq 1 40); do curl -sf -m 2 "$HUB_URL/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
curl -sf -m 5 "$HUB_URL/healthz" >/dev/null && ok "hub up on an empty data directory" \
  || { no "hub down: $(tail -2 "$RUN/hub.log")"; exit 1; }

# The hub admin beside it, reading the hub's data directory the way it does in production. It
# gets its token through the environment of a subshell, never through argv. Its vector-search client
# points at an address in this run's block where nothing listens: its default, 127.0.0.1:8600, may be
# someone else's service on a shared host.
if [ -x "$BIN/anet-hub-admin" ] && [ "${JOINT_HUB_ADMIN:-1}" != 0 ]; then
  python3 -c 'import secrets;print(secrets.token_hex(32))' > "$RUN/admin.token"
  ( cd "$RUN" && ADMIN_TOKEN=$(cat "$RUN/admin.token") && export ADMIN_TOKEN ANET_VEC_URL="http://$DEAD_ADDR" \
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

if [ "$CANARY" != 0 ]; then
  hd "C  SI-1 canary — the hub and its admin never hold task content"
  canary_flow
  if [ "$CANARY_RAN" = 1 ]; then canary_sweep c controls; canary_backup c; fi
  if [ "${JOINT_CANARY_ONLY:-0}" = 1 ]; then
    printf '\n\033[1m── %d passed, %d failed (JOINT_CANARY_ONLY=1: sections 1-11 not run) ──\033[0m   logs: %s\n' \
      "$pass" "$fail" "$RUN"
    [ "$fail" -eq 0 ] && exit 0 || exit 1
  fi
fi

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

if [ "$CANARY" != 0 ] && [ "$CANARY_RAN" = 1 ]; then
  hd "C′  SI-1 canary, searched again at the end of the run"
  # Whatever the hub side wrote since section C — admin harvest and snapshot periods, WAL checkpoints,
  # the canary nodes' later polls — is searched the same way.
  canary_sweep end
fi

printf '\n\033[1m── %d passed, %d failed ──\033[0m   logs: %s\n' "$pass" "$fail" "$RUN"
[ "$fail" -eq 0 ]
