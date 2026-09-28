#!/usr/bin/env bash
# joint-official.sh — the official public agents behind their five doors (A2A-DESIGN §15, §5, SI-1).
#
# An official agent answers anyone. What keeps that safe is not one check but five, each owned by a
# different process, and each can only be shown across process boundaries: a unit test of one side
# fakes the other. This run puts real ones in a line and knocks on every door:
#
#   1  admission    inbound.policy closed + public_capabilities: a task in prose and a capability the
#                   node serves but did not make public are refused (rejected, anet.reason
#                   not_accepting) and leave nothing on the official node; a public one runs, for anyone
#   2  kernel       Admit (§5.4): max_args_bytes, per_caller_per_min, global_per_min — refused with
#                   anet.retry_after_ms (the relay's 429), one caller's quota does not touch another's,
#                   the global one binds everybody, and a refused call leaves no record
#   3  backend      anet-official answers only the daemon's bearer token and checks it before it
#                   routes (401 for every path without it); it is told the verified caller
#                   (X-ANet-Caller = the requester's AID) and logs who, never what
#   4  compute      a small input built to explode (json.validate) comes back as "budget exhausted"
#                   within the capability's deadline, and the backend keeps serving
#   5  evidence     the official node's chain keeps the result CID and metrics, not the result
#                   (public capability evidence in cid mode, 0017 Q15)
#
# and around them: the hub and the hub admin never hold the content (SI-1, a random canary through
# every official call), a denied caller is told exactly what a stranger is told, an agent that
# borrows an official name is not marked anet.official, and the paid demo — demo.digest.paid —
# takes one payment through the a2a-x402 flow inside the task (§8.3): quote, operator decision,
# settlement at the hub, result, one debit.
#
# Self-contained, like joint.sh: on a clean Linux host with bash, curl and python3 it gets its own
# binaries, starts a hub (and hub admin), three anet-official backends with a daemon each, two
# requesters, a stranger and an impostor on a loopback port block of its own, and at the end stops
# exactly what it started — by path, never by process name.
#
#   J=/tmp/jo bash scripts/joint-official.sh                  build from this checkout (needs go)
#   JOINT_BIN=DIR J=/tmp/jo bash scripts/joint-official.sh    prebuilt binaries, no go on the host
#
# Environment:
#   J                  work directory (default /tmp/joint-official-<uid>). It must be this user's and
#                      writable by no one else (lib.sh own_dir): binaries are built there and run. An
#                      existing non-empty directory is used only if an earlier run of this script made
#                      it (it holds .joint-official-dir); each run replaces $J/bin and $J/run.
#   JOINT_BIN          directory with prebuilt anet, anet-official, anet-hub and optionally
#                      anet-hub-admin (scripts/testnet/build.sh makes this set). Copied into $J/bin.
#                      Unset: built here with go from this checkout and HUB_SRC.
#   HUB_SRC            ANetHub checkout to build from (default: ../ANetHub beside this repository)
#   JOINT_PORT_BASE    first of 24 consecutive loopback ports; unset = a random free block in
#                      20000-32000. On the test hosts give one inside the test network's range: a
#                      base in 47x60-47x76 (47460, 47160, …). 47x00-47x59 of each hundred belong to
#                      the test network's nodes (scripts/testnet/topology.env: hub x01, daemons x11-19,
#                      anetpeer x31-39, official x41-50, hub admin x51) and are refused, since a node
#                      that is down for a test leaves its port free. A port already in use aborts the
#                      run; nothing is killed.
#   JOINT_HUB_ADMIN    0 = do not start anet-hub-admin (started by default when the binary is there)
#   JOINT_OFFICIAL_MARK
#                      1 = show the anet.official mark both ways in 8/8: an observer daemon runs a test
#                      build whose signed manifest lists this run's tools agent (scripts/official-
#                      testbin.sh, B5-01; needs go, ssh-keygen and this checkout) and must see that
#                      agent marked and the impostor of the same name not. Default 1 when this run
#                      builds its binaries, 0 with JOINT_BIN (the test hosts have no go). Without it the
#                      mark is "not shown": nothing marks an agent of this run, so "the impostor is not
#                      marked" would hold of any build and is not counted as passed.
#   JOINT_KEEP         1 = leave everything running at the end (default: stop it)
#   JOINT_OFFICIAL_MUTATE
#                      publish-private = mutation for layer 1: the capability the tools node serves
#                      but keeps private (a2a.x402.check) is put into its public_capabilities. 1/8 must
#                      go red. The other two layer mutations are code patches, applied in a scratch
#                      worktree and run with JOINT_BIN:
#                        scripts/mutations/official-no-quota.patch          2/8 red
#                        scripts/mutations/official-no-backend-token.patch  3/8 red
#
# Section 2/8 waits for the start of a fresh UTC minute (the kernel's quota windows are fixed
# minutes), so a run takes up to a minute longer than its work.
#
# What this run does not show, and where it is shown instead: the seven-day pruning of public_cap
# interactions (Q15, a clock this run does not move: B3-10's unit test); the structure of the
# /x402/settle body at the hub (joint.sh's SI-1 section captures it); max_inflight (a short capability
# call runs inside the provider's poll loop, so calls through the relay are never two in flight at
# once: internal/daemon tests); without JOINT_OFFICIAL_MARK, the anet.official mark (see above).
# A check that could not be made is counted "not shown" in the last line, never as passed.
#
# On the test hosts (scripts/testnet, docs/notes/0015) take the binaries from scripts/testnet/build.sh
# and a JOINT_PORT_BASE in 47x60-47x76. Nothing here stops a process by name, and every daemon
# gets a private XDG_RUNTIME_DIR, so a production daemon of the same user is neither stopped nor
# shadowed.
#
# Building: see joint.sh — ANet and ANetHub must be built from the same wire generation, against one
# ANetCore; with GOWORK unset and ../go.work present (the work-tree layout) that workspace is used.
set -uo pipefail
umask 077
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
SCRIPTS=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPTS/.." && pwd -P)

J=${J:-/tmp/joint-official-$(id -u)}
BIN=$J/bin; RUN=$J/run
# lib.sh: _peer_add and the stop-by-path helpers. ANET is set first so sourcing it looks nothing up.
ANET=$BIN/anet
# shellcheck source=lib.sh
. "$SCRIPTS/lib.sh"

pass=0; fail=0; unshown=0
ok(){   printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; pass=$((pass+1)); }
no(){   printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; fail=$((fail+1)); }
# ns — a check this run could not make: neither passed nor failed, and counted apart in the last line.
ns(){   printf '\033[1;33m  ~ not shown: %s\033[0m\n' "$*"; unshown=$((unshown+1)); }
hd(){   printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }
note(){ printf '\033[1;33m  ! %s\033[0m\n' "$*"; }
die(){  printf '\033[1;31mjoint-official.sh: %s\033[0m\n' "$*" >&2; exit 2; }
for c in curl python3 setsid; do command -v "$c" >/dev/null || die "$c is required"; done

MUTATE=${JOINT_OFFICIAL_MUTATE:-}
case "$MUTATE" in ""|publish-private) ;; *) die "JOINT_OFFICIAL_MUTATE=$MUTATE: the only value is publish-private" ;; esac
if [ -n "${JOINT_BIN:-}" ]; then MARK=${JOINT_OFFICIAL_MARK:-0}; else MARK=${JOINT_OFFICIAL_MARK:-1}; fi
case "$MARK" in 0|1) ;; *) die "JOINT_OFFICIAL_MARK=$MARK: 0 or 1" ;; esac

# ── the work directory ──────────────────────────────────────────
# Deleted and re-made on every run ($J/bin, $J/run), and whatever runs from $J/bin is stopped, so it
# has to be this script's own: a fresh or empty directory, or one an earlier run marked.
case "$J" in /*) ;; *) J=$PWD/$J ;; esac
_own_path "$J" >/dev/null || die "J=$J is not a directory this script may own (pick something like /tmp/jo-x)"
own_dir "$J" || die "J=$J is not a private directory of this user (another user owns or can write to it, or to a directory above it); use another J"
J=$(cd "$J" && pwd -P); BIN=$J/bin; RUN=$J/run; ANET=$BIN/anet
_own_path "$J" >/dev/null || die "J=$J is not a directory this script may own (pick something like /tmp/jo-x)"
if [ ! -e "$J/.joint-official-dir" ] && [ -n "$(ls -A "$J" 2>/dev/null)" ]; then
  die "$J is not empty and was not made by joint-official.sh (no .joint-official-dir); use an empty or new J"
fi
: > "$J/.joint-official-dir"
# The lock is fd 9, closed in every process started below (9>&-), as in joint.sh.
if command -v flock >/dev/null; then
  exec 9>"$J/.joint-official-lock"
  flock -n 9 || die "another joint-official.sh run is using $J"
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
print("" if v is None else (json.dumps(v) if isinstance(v, bool) else v))' "$@"; }
rand(){ python3 -c 'import secrets;print(secrets.token_hex(8))'; }
now_ms(){ python3 -c 'import time;print(int(time.time()*1000))'; }

# The nodes. echo, tools and paid are the official identities (A1 anet-echo-e, B anet-tools, E
# anet-paid-demo); a and b are requesters, s a stranger, imp an impostor that registers under the
# tools agent's name. None of them is on any list of any other: an official agent is public by its
# public_capabilities, not by naming its callers.
NODES=(echo tools paid a b s imp)
OFFICIAL=(echo tools paid)
declare -A HOME_OF=() ADDR_OF=() AID_OF=() NAME_OF=() NODE_PID=()
NAME_OF=([echo]=anet-echo-e [tools]=anet-tools [paid]=anet-paid-demo [a]=requester-a [b]=requester-b
         [s]=stranger [imp]=anet-tools [o]=observer)
# o, the observer, is not in NODES: it is started in 8/8 only, from a test build that marks this run's
# tools agent official (JOINT_OFFICIAL_MARK).
# The capability the tools node serves and keeps private: its backend answers it, the daemon's
# service module mounts it, and it is not in public_capabilities. A caller must not get to it.
PRIVCAP=a2a.x402.check
# net.echo's quotas for this run, lowered from the published 60/1200 so that 2/8 can reach them.
Q_CALLER=3; Q_GLOBAL=5

# ctl <node> <path> <json> — a node's control API. The bearer token goes to curl through a file
# descriptor, not the command line: on a shared host other users can read argv.
ctl(){
  local tok; tok=$(cat "${HOME_OF[$1]}/.anet/control_token.txt" 2>/dev/null)
  curl -s -m "${CTL_WAIT:-60}" -H @<(printf 'Authorization: Bearer %s\n' "$tok") \
       -H 'Content-Type: application/json' -d "$3" "http://${ADDR_OF[$1]}$2"
}
# ctlc <node> <path> <json> — the same, printing the HTTP status; the body is left in $RUN/ctlc.out.
ctlc(){
  local tok; tok=$(cat "${HOME_OF[$1]}/.anet/control_token.txt" 2>/dev/null)
  curl -s -m "${CTL_WAIT:-60}" -o "$RUN/ctlc.out" -w '%{http_code}' \
       -H @<(printf 'Authorization: Bearer %s\n' "$tok") -H 'Content-Type: application/json' \
       -d "$3" "http://${ADDR_OF[$1]}$2"
}

up(){   curl -sf -m 2 "http://$1/ping" >/dev/null 2>&1; }
wait_up(){ local i; for ((i = 0; i < ${2:-20} * 4; i++)); do up "$1" && return 0; sleep 0.25; done; return 1; }

# start_node <node> [binary] — run a daemon as that node: its own HOME, and an XDG_RUNTIME_DIR under $RUN,
# so it writes neither this user's "current daemon" pointer nor its identity registry (see joint.sh). The
# binary is $BIN/anet unless another one under $BIN is named (stop_under $BIN stops it either way).
start_node(){
  ( cd "$RUN" && exec setsid env -u ANET_DATA_DIR -u ANET_HOME -u ANET_ID \
      HOME="${HOME_OF[$1]}" XDG_RUNTIME_DIR="$RUN/xdg" "${2:-$BIN/anet}" daemon ) >"$RUN/$1.out" 2>&1 </dev/null 9>&- &
  NODE_PID[$1]=$!
}

# send_cap <from> <to> <capability> <args-json> — a capability call; prints the interaction id.
send_cap(){
  ctl "$1" /delegate "{\"provider\":\"${AID_OF[$2]}\",\"capability\":\"$3\",\"args\":$4}" | jget interaction_id
}
# send_goal <from> <to> <text> — a task in prose; prints the interaction id.
send_goal(){
  ctl "$1" /delegate "{\"provider\":\"${AID_OF[$2]}\",\"goal\":$(json_str "$3")}" | jget interaction_id
}
# wait_task <node> <interaction_id> [seconds] — the task as the requester sees it (the A2A projection,
# /tasks/wait), once it has ended or waits on this node; as it is when the bound elapses.
wait_task(){
  [ -n "$2" ] || { echo '{}'; return; }
  CTL_WAIT=$(( ${3:-40} + 15 )) ctl "$1" /tasks/wait "{\"task_id\":\"$2\",\"timeout_ms\":$(( ${3:-40} * 1000 ))}"
}
# wait_end <node> <interaction_id> [seconds] — the task once it has ended (completed, failed, canceled or
# rejected), read with /tasks/get every second; as it is when the bound elapses.
wait_end(){
  local t="{}" i s
  for ((i = 0; i < ${3:-60}; i++)); do
    t=$(ctl "$1" /tasks/get "{\"task_id\":\"$2\"}")
    s=$(printf '%s' "$t" | tq state)
    case "$s" in completed|failed|canceled|rejected) break ;; esac
    sleep 1
  done
  printf '%s' "$t"
}
# tq FIELD — one field of the A2A task on stdin: state (lower case, without TASK_STATE_), text (the
# status message's text) or a metadata key (anet.reason, x402.payment.status, …). Empty when absent.
tq(){ python3 -c '
import sys, json
try:
    t = json.load(sys.stdin)
except Exception:
    t = {}
if not isinstance(t, dict):
    t = {}
f, st = sys.argv[1], t.get("status") or {}
if f == "state":
    s = st.get("state") or ""
    print(s[len("TASK_STATE_"):].lower() if s.startswith("TASK_STATE_") else s.lower())
elif f == "text":
    m = st.get("message") or {}
    print(" ".join(p["text"] for p in m.get("parts") or [] if isinstance(p, dict) and p.get("text")))
else:
    v = (t.get("metadata") or {}).get(f)
    print("" if v is None else (json.dumps(v, sort_keys=True) if isinstance(v, (dict, list, bool)) else v))' "$1"; }
# answer <node> <interaction_id> — "state|reason|retry_after_ms|status text" of a finished task: what the
# caller was told, as one comparable line.
answer(){
  local t; t=$(wait_task "$1" "$2")
  printf '%s|%s|%s|%s\n' "$(printf '%s' "$t" | tq state)" "$(printf '%s' "$t" | tq anet.reason)" \
    "$(printf '%s' "$t" | tq anet.retry_after_ms)" "$(printf '%s' "$t" | tq text)"
}
# result_of <node> <interaction_id> — the capability effect the requester holds for that call (/results),
# once there is one.
result_of(){
  local r i
  for ((i = 0; i < ${3:-60}; i++)); do
    r=$(ctl "$1" /results '{}' | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = {}
for x in d.get("results") or []:
    if x.get("interaction_id") == sys.argv[1]:
        print(x.get("result") or ""); break' "$2")
    [ -n "$r" ] && { echo "$r"; return 0; }
    sleep 0.5
  done
  echo '{"error":"timed out waiting for the result"}'; return 1
}
# eff FIELD — status, message, or observed (evidence.observed_state) of the effect on stdin.
eff(){ python3 -c '
import sys, json
try:
    e = json.load(sys.stdin)
except Exception:
    e = {}
f = sys.argv[1]
if f == "observed":
    print((e.get("evidence") or {}).get("observed_state") or "")
else:
    print(e.get(f) or "")' "$1"; }
# in_inbox <node> <interaction_id> — the node's inbound record of that id as its trust, "absent", or
# "unreadable" (a node that is down has an empty inbox too, and that must not pass for "nothing got in").
in_inbox(){
  ctl "$1" /inbox '{}' | python3 -c '
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
print("absent")' "${2:-none}"
}
# inbox_n <node> — how many inbound tasks the node holds; empty when unreadable.
inbox_n(){ ctl "$1" /inbox '{}' | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = None
print(len(d["inbox"] or []) if isinstance(d, dict) and "inbox" in d else "")'; }
# rx <node> <reason> — the node's receive counter for that reason (/status .receive), 0 when none.
rx(){ ctl "$1" /status '{}' | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = {}
r = d.get("receive") if isinstance(d, dict) else None
print((r or {}).get(sys.argv[1], 0))' "$2"; }
# ev_for <node> <event-type> <interaction_id> [key] — how many of the node's records of that type name
# that interaction (and, with a key, have it true); empty when the chain cannot be read.
ev_for(){ ctl "$1" /evidence "{\"event_type\":\"$2\",\"limit\":1000}" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = None
if not isinstance(d, dict) or not isinstance(d.get("records"), list):
    print(""); sys.exit()
ix, key = sys.argv[1], sys.argv[2]
print(sum(1 for r in d["records"] if (r.get("payload") or {}).get("interaction_id") == ix
          and (not key or (r.get("payload") or {}).get(key) is True)))' "$3" "${4:-}"; }
balance_of(){ ctl "$1" /balance '{}' | python3 -c '
import sys, json
try:
    print(int(json.load(sys.stdin).get("balance")))
except Exception:
    print("")'; }
# backend_line <group> <interaction_id> — the backend's log line for that call ("" when there is none).
backend_line(){ grep -F " call=$2 " "$RUN/backend-$1.log" 2>/dev/null | grep -F ' call cap=' | tail -1; }
# field <line> <name> — one name=value of a backend log line.
field(){ printf '%s\n' "$1" | tr ' ' '\n' | sed -n "s/^$2=//p" | head -1; }
# minute_now — the current UTC minute's index; fresh_minute AFTER — wait until a minute later than AFTER
# has begun and at most 15 s of it have passed, and print its index. The kernel's quota windows are fixed
# minutes (inbound.go admitAt), so 2/8 runs inside one of its own.
minute_now(){ python3 -c 'import time;print(int(time.time()//60))'; }
fresh_minute(){ python3 - "$1" <<'PY'
import sys, time
after = int(sys.argv[1])
while True:
    now = time.time()
    m, s = int(now // 60), now % 60
    if m > after and s <= 15:
        print(m)
        break
    time.sleep(60 - s + 0.2)
PY
}

hd "0/8  binaries, ports, and the stack"
if [ -n "${JOINT_BIN:-}" ]; then
  SRC=$(cd "$JOINT_BIN" 2>/dev/null && pwd -P) || die "JOINT_BIN=$JOINT_BIN is not a directory"
  for b in anet anet-official anet-hub; do
    [ -x "$SRC/$b" ] || die "JOINT_BIN has no $b"
  done
  case "$SRC" in "$BIN"|"$BIN"/*|"$RUN"|"$RUN"/*) die "JOINT_BIN must not be under $BIN or $RUN: both are replaced" ;; esac
fi
# Leftovers of an earlier run in this J — interrupted before its own cleanup — hold ports and files.
stop_under "$BIN" 10
rm -rf -- "$BIN" "$RUN"
mkdir -p "$BIN" "$RUN" "$RUN/official"

if [ -n "${JOINT_BIN:-}" ]; then
  for b in anet anet-official anet-hub anet-hub-admin; do
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
  go build -C "$ROOT" -o "$BIN/anet" ./cmd/anet                     || die "build anet failed"
  go build -C "$ROOT" -o "$BIN/anet-official" ./cmd/anet-official   || die "build anet-official failed"
  go build -C "$HUB_SRC" -o "$BIN/anet-hub" ./cmd/anet-hub          || die "build anet-hub failed"
  if [ -d "$HUB_SRC/cmd/anet-hub-admin" ]; then
    go build -C "$HUB_SRC" -o "$BIN/anet-hub-admin" ./cmd/anet-hub-admin || die "build anet-hub-admin failed"
  fi
fi

# Twenty-four loopback ports: +0 hub, +1 hub admin, +2..+4 the echo, tools and paid backends, +5..+7
# their daemons, +8 requester a, +9 requester b, +10 the stranger, +11 the impostor, +12 the impostor's
# "backend" (reserved and never bound: nothing answers there), +13 the observer (8/8), +14..+15 spare,
# +16..+23 the local A2A interfaces of the eight daemons (module/a2a is on by default; unpinned, each
# takes a port from 43811 up, outside this block: 0021 F3, lib.sh pin_a2a).
PORT_BASE=$(python3 - "${JOINT_PORT_BASE:-}" 24 <<'PY'
import random, socket, sys
want, n = sys.argv[1], int(sys.argv[2])
# "Free" means what the hub and the daemons need: a listener can be opened there. They are Go, and Go
# listens with SO_REUSEADDR, so a port whose earlier listener closed a moment ago (its accepted
# connections still in TIME_WAIT) is free for them. A bare bind() says "in use" for that port, and a
# second run on the same JOINT_PORT_BASE right after the first refused to start (joint.sh had the same,
# docs/notes/0021 F1; here docs/notes/0024).
def free(b):
    for p in range(b, b + n):
        s = socket.socket()
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            s.bind(("127.0.0.1", p))
            s.listen(1)
        except OSError:
            return False
        finally:
            s.close()
    return True
if want:
    try:
        b = int(want)
    except ValueError:
        sys.exit("JOINT_PORT_BASE=%s is not a number" % want)
    if not 1024 <= b <= 65535 - n:
        sys.exit("JOINT_PORT_BASE=%s is out of range" % want)
    # The test network's range (scripts/testnet/topology.env): in each hundred, 00-59 are its nodes'
    # ports. One that is free now may be a node stopped for a test, which must find it free again.
    taken = [p for p in range(b, b + n) if 47100 <= p <= 47499 and p % 100 < 60]
    if taken:
        sys.exit("JOINT_PORT_BASE=%d: %d-%d lie in 47x00-47x59, the test network's node ports "
                 "(scripts/testnet/topology.env); use a base in 47x60-47x76, e.g. %d"
                 % (b, taken[0], taken[-1], taken[0] // 100 * 100 + 60))
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
declare -A BACKEND_PORT=([echo]=$((PORT_BASE + 2)) [tools]=$((PORT_BASE + 3)) [paid]=$((PORT_BASE + 4)))
i=5
for n in "${NODES[@]}"; do
  ADDR_OF[$n]=127.0.0.1:$((PORT_BASE + i)); HOME_OF[$n]=$RUN/$n; i=$((i + 1))
done
DEAD_PORT=$((PORT_BASE + 12))
ADDR_OF[o]=127.0.0.1:$((PORT_BASE + 13)); HOME_OF[o]=$RUN/o
declare -A A2A_PORT_OF=()
i=16
for n in "${NODES[@]}" o; do A2A_PORT_OF[$n]=$((PORT_BASE + i)); i=$((i + 1)); done
echo "  ports:    $PORT_BASE-$((PORT_BASE + 23))   work dir: $J"
[ -n "$MUTATE" ] && note "MUTATION $MUTATE: $PRIVCAP is published on the tools node; 1/8 must go red"

# The hub, on an empty data directory.
( cd "$RUN" && exec setsid "$BIN/anet-hub" --addr "$HUB_ADDR" --data "$RUN/hub" ) >"$RUN/hub.log" 2>&1 </dev/null 9>&- &
for _ in $(seq 1 40); do curl -sf -m 2 "$HUB_URL/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
curl -sf -m 5 "$HUB_URL/healthz" >/dev/null && ok "hub up on an empty data directory" \
  || { no "hub down: $(tail -2 "$RUN/hub.log")"; exit 1; }

# The hub admin beside it, harvesting and snapshotting every 2 s, as SI-1 asks: whatever it could
# collect, it collects during this run. Its token reaches it through the environment, and curl
# through a 0600 header file.
ADMIN=0
if [ -x "$BIN/anet-hub-admin" ] && [ "${JOINT_HUB_ADMIN:-1}" != 0 ]; then
  python3 -c 'import secrets;print(secrets.token_hex(32))' > "$RUN/admin.token"
  printf 'Authorization: Bearer %s\n' "$(cat "$RUN/admin.token")" > "$RUN/admin.hdr"
  ( cd "$RUN" && ADMIN_TOKEN=$(cat "$RUN/admin.token") && export ADMIN_TOKEN \
      && exec setsid "$BIN/anet-hub-admin" --addr "$ADMIN_ADDR" --hub-data "$RUN/hub" --data "$RUN/admin" \
           --snapshot-every 2s --harvest-every 2s ) >"$RUN/admin.log" 2>&1 </dev/null 9>&- &
  for _ in $(seq 1 40); do curl -sf -m 2 "http://$ADMIN_ADDR/admin/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
  if curl -sf -m 5 "http://$ADMIN_ADDR/admin/healthz" >/dev/null; then ok "hub admin up beside it"; ADMIN=1
  else no "hub admin down: $(tail -2 "$RUN/admin.log")"; fi
else
  echo "  hub admin: not started"
fi
# adm METHOD PATH [BODY] — the admin API; prints the HTTP status, the body is left in $RUN/adm.out.
adm(){
  curl -s -m 10 -o "$RUN/adm.out" -w '%{http_code}' -X "$1" -H @"$RUN/admin.hdr" \
       -H 'Content-Type: application/json' ${3:+--data-binary "$3"} "http://$ADMIN_ADDR/admin$2"
}

# The backends: one anet-official process per identity, each serving only its own group, each with a
# token only its daemon holds (A2A-DESIGN §15; deploy/official runs them the same way under systemd).
"$BIN/anet-official" capabilities > "$RUN/official/capabilities.json" || die "anet-official capabilities failed"
for g in "${OFFICIAL[@]}"; do
  python3 -c 'import secrets;print(secrets.token_hex(32))' > "$RUN/official/$g.token"
  ( cd "$RUN" && exec setsid "$BIN/anet-official" serve -listen "127.0.0.1:${BACKEND_PORT[$g]}" \
      -token-file "$RUN/official/$g.token" -groups "$g" ) >"$RUN/backend-$g.log" 2>&1 </dev/null 9>&- &
done
for g in "${OFFICIAL[@]}"; do
  for _ in $(seq 1 40); do curl -sf -m 2 "http://127.0.0.1:${BACKEND_PORT[$g]}/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
  curl -sf -m 5 "http://127.0.0.1:${BACKEND_PORT[$g]}/healthz" >/dev/null \
    && ok "anet-official backend '$g' up on 127.0.0.1:${BACKEND_PORT[$g]}" \
    || { no "backend '$g' down: $(tail -2 "$RUN/backend-$g.log")"; exit 1; }
done
# timeout_of <capability> — its timeout_ms in the backend's own capability table, the single source
# deploy/official is generated from.
timeout_of(){ python3 -c '
import sys, json
for c in json.load(open(sys.argv[1])):
    if c["id"] == sys.argv[2]: print(c.get("timeout_ms") or ""); break' "$RUN/official/capabilities.json" "$1"; }

# The official daemons' configuration is what `anet-official service-config` prints — the generator
# deploy/official is checked against — with this run's addresses and three changes: every public
# capability's evidence in cid mode (as deploy/official has it, 0017 Q15); net.echo's quotas lowered
# to $Q_CALLER per caller and $Q_GLOBAL in all per minute, so that 2/8 can reach them; and $PRIVCAP kept
# off the tools node's public list while its service module still mounts it.
mkdir -p -m 700 "$RUN/xdg"
official_config(){
  local n=$1
  mkdir -p "${HOME_OF[$n]}/.anet"
  pin_a2a "${HOME_OF[$n]}/.anet" "${A2A_PORT_OF[$n]}" || die "cannot pin the $n node's local A2A interface"
  "$BIN/anet-official" service-config -groups "$n" -url "http://127.0.0.1:${BACKEND_PORT[$n]}" \
      -token-file "$RUN/official/$n.token" > "$RUN/official/$n.service.json" \
    || die "anet-official service-config -groups $n failed"
  python3 - "${HOME_OF[$n]}/.anet/config.json" "${ADDR_OF[$n]}" "${NAME_OF[$n]}" \
      "$RUN/official/$n.service.json" "$n" "$PRIVCAP" "$MUTATE" "$Q_CALLER" "$Q_GLOBAL" <<'PY'
import json, sys
path, addr, name, svc, group, priv, mutate, qc, qg = sys.argv[1:10]
s = json.load(open(svc))
inbound = s["inbound"]
for p in inbound["public_capabilities"]:
    p["evidence"] = "cid"
    if p["id"] == "net.echo":
        p["per_caller_per_min"], p["global_per_min"] = int(qc), int(qg)
if group == "tools":
    served = [c["id"] for c in s["modules"]["service"]["capabilities"]]
    assert priv in served, "the tools backend does not serve " + priv
    if mutate != "publish-private":
        inbound["public_capabilities"] = [p for p in inbound["public_capabilities"] if p["id"] != priv]
c = {"control_addr": addr, "name": name, "inbound": inbound,
     "modules": {"service": s["modules"]["service"]}}
if group == "paid":
    c["modules"]["x402"] = {}
json.dump(c, open(path, "w"), indent=1)
PY
  [ -s "${HOME_OF[$n]}/.anet/config.json" ] || die "could not write the $n node's configuration"
}
for n in "${OFFICIAL[@]}"; do official_config "$n"; done

# The requesters, the stranger and the impostor are fresh installs: a pinned control port, then
# `anet init` and nothing else (SI-5: closed, empty lists, no public capability, auto_max 0, an empty
# payee list).
fresh_config(){
  mkdir -p "${HOME_OF[$1]}/.anet"
  printf '{"control_addr":"%s"}\n' "${ADDR_OF[$1]}" > "${HOME_OF[$1]}/.anet/config.json"
  ( cd "$RUN" && exec env -u ANET_HOME -u ANET_ID HOME="${HOME_OF[$1]}" ANET_DATA_DIR="${HOME_OF[$1]}/.anet" \
      XDG_RUNTIME_DIR="$RUN/xdg" "$BIN/anet" init ) >"$RUN/$1-init.log" 2>&1 </dev/null 9>&- \
    || die "anet init failed for $1: $(tail -3 "$RUN/$1-init.log")"
  pin_a2a "${HOME_OF[$1]}/.anet" "${A2A_PORT_OF[$1]}" || die "cannot pin the $1 node's local A2A interface"
}
for n in a b s imp; do fresh_config "$n"; done
# The impostor offers text.digest under the tools agent's name, from a backend that does not exist:
# all it wants is to be listed as anet-tools.
python3 - "${HOME_OF[imp]}/.anet/config.json" "$DEAD_PORT" <<'PY'
import json, sys
path, port = sys.argv[1], sys.argv[2]
c = json.load(open(path))
c.setdefault("inbound", {})["public_capabilities"] = [{"id": "text.digest"}]
c["modules"] = {"service": {"capabilities": [{
    "id": "text.digest", "url": "http://127.0.0.1:%s/v1/tools/text.digest" % port,
    "name": "Text digest", "description": "Hashes text. The official anet tools agent."}]}}
json.dump(c, open(path, "w"), indent=1)
PY

for n in "${NODES[@]}"; do start_node "$n"; done
for n in "${NODES[@]}"; do
  wait_up "${ADDR_OF[$n]}" 30 || { no "$n did not come up: $(tail -3 "${HOME_OF[$n]}/.anet/daemon.log" "$RUN/$n.out" 2>/dev/null)"; exit 1; }
done
for n in "${NODES[@]}"; do
  AID_OF[$n]=$(ctl "$n" /status '{}' | jget aid)
  [ -n "${AID_OF[$n]}" ] || { no "$n reports no AID"; exit 1; }
done
ok "seven daemons up: three official, two requesters, a stranger, an impostor"
REG_ALL=1
for n in "${NODES[@]}"; do
  R=$(ctl "$n" /hub-register "{\"hub\":\"$HUB_URL\",\"name\":\"${NAME_OF[$n]}\"}")
  if [ "$(printf '%s' "$R" | jget status)" != registered ]; then
    REG_ALL=0; no "$n did not register: $(printf '%s' "$R" | head -c 200)"
  fi
  printf '  %-6s %-15s %s\n' "$n" "${NAME_OF[$n]}" "${AID_OF[$n]}"
done
[ "$REG_ALL" = 1 ] && ok "all seven registered at the hub (the impostor under the tools agent's name)"

# The hub admin registers official agents by id, AID, hub and capabilities, and by nothing else
# (§15 [C39]): no ssh runtime, no monitor, no ops.
if [ "$ADMIN" = 1 ]; then
  AREG=1
  for n in "${OFFICIAL[@]}"; do
    M=$(python3 - "${NAME_OF[$n]}" "${AID_OF[$n]}" "$HUB_URL" "${HOME_OF[$n]}/.anet/config.json" <<'PY'
import json, sys
name, aid, hub, cfg = sys.argv[1:5]
caps = [p["id"] for p in json.load(open(cfg))["inbound"]["public_capabilities"]]
print(json.dumps({"id": name, "name": name, "tier": "official", "product_line": "agentnetwork",
                  "aid": aid, "hub": hub, "caps": caps}))
PY
)
    [ "$(adm POST /api/official "$M")" = 200 ] || { AREG=0; no "admin refused the $n manifest: $(head -c 200 "$RUN/adm.out")"; }
  done
  [ "$AREG" = 1 ] && ok "the three official agents are registered in the hub admin (id, aid, hub, caps)"
  BAD=$(python3 -c 'import json;print(json.dumps({"id":"anet-echo-x","name":"x","tier":"official","product_line":"agentnetwork","monitor":{"url":"http://127.0.0.1:1/"}}))')
  C=$(adm POST /api/official "$BAD")
  [ "$C" = 400 ] && ok "a manifest that still carries a monitor section is refused (400)" \
    || no "a manifest with a monitor section answered $C: $(head -c 200 "$RUN/adm.out")"
fi

# The canary: random, so that it is in nothing public (a capability id or a profile text would be).
# Minted as joint.sh mints its canaries (lib.sh canary_new): with "~?~" in it, every base64 form of it
# differs between the standard and the URL-safe alphabet, so canary_hits's base64url search is one this
# run depends on (docs/notes/0026 §6 item 4).
CANARY=$(canary_new "$RUN/canaries.tsv" official) && [ -n "$CANARY" ] || die "no canary could be minted (canary.py mint)"
echo "  canary:   $CANARY"

hd "1/8  admission — closed, and only public capabilities get in"
# The official nodes name nobody. What lets a caller in is the public capability list and nothing else:
# a task in prose, and a capability the node serves but did not publish, are refused at the kernel's
# door (A2A-DESIGN §5.2 row 6) and leave no trace on the node.
for n in "${OFFICIAL[@]}"; do
  P=$(ctl "$n" /peers/list '{}')
  POL=$(printf '%s' "$P" | jget policy)
  NAMED=$(printf '%s' "$P" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = {}
print(sum(len(d.get(k) or []) for k in ("allow", "trust")))')
  [ "$POL" = closed ] && [ "$NAMED" = 0 ] && ok "$n: inbound policy closed, nobody on its allow or trust list" \
    || no "$n: policy '${POL:-unreadable}', ${NAMED:-?} peers named"
done
TPUB=$(ctl tools /peers/list '{}' | python3 -c '
import sys, json
try:
    print(" ".join(sorted(p["id"] for p in json.load(sys.stdin).get("public_capabilities") or [])))
except Exception:
    print("")')
echo "  tools public: $TPUB   (served but private: $PRIVCAP)"
case " $TPUB " in
  *" $PRIVCAP "*) no "$PRIVCAP is on the tools node's public list" ;;
  "  ") no "the tools node's public list is unreadable or empty" ;;
  *) ok "$PRIVCAP is served by the tools node and not on its public list" ;;
esac

# What the network is told. The card lists public capabilities only, and so does the hub's registry
# entry: 0017 Q14 decided that the registered caps are the public ones, so that a closed node does not
# publish the list of what it keeps private (implemented on wp/proj; until that is merged the second
# check below is red, as 6/8 is until B3-10).
CARD=""
for _ in $(seq 1 20); do
  CARD=$(ctl a /agents/card "{\"aid\":\"${AID_OF[tools]}\"}" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
    c = d.get("card")
    if isinstance(c, str):
        c = json.loads(c)
    print(" ".join(sorted(s.get("id", "") for s in (c or {}).get("skills") or [])))
except Exception:
    print("")')
  [ -n "$CARD" ] && break; sleep 0.5
done
case " $CARD " in
  "  ") no "the tools agent's network card is unreadable or lists no skill" ;;
  *" $PRIVCAP "*) no "the tools agent's network card lists $PRIVCAP: $CARD" ;;
  *) [ "$CARD" = "$TPUB" ] && ok "its network card lists exactly the public capabilities" \
       || no "its network card lists '$CARD', the public list is '$TPUB'" ;;
esac
HCAPS=$(curl -s -m 10 "$HUB_URL/agents/${AID_OF[tools]}" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
    # GET /agents/{aid} answers {"agent": {…, "caps": […]}, "reviews": […]}.
    print(" ".join(sorted((d.get("agent") or d).get("caps") or [])))
except Exception:
    print("")')
case " $HCAPS " in
  *" $PRIVCAP "*) no "the hub registry entry lists the private $PRIVCAP (caps: $HCAPS; 0017 Q14, wp/proj)" ;;
  "  ") no "the hub's entry for the tools agent is unreadable or lists no capability" ;;
  *) ok "the hub registry entry does not list $PRIVCAP either (caps: $HCAPS)" ;;
esac

IN0=$(inbox_n tools); NA0=$(rx tools not-accepting)
SIX=$(send_goal s tools "Summarise this for me: $CANARY")
SPX=$(send_cap s tools "$PRIVCAP" "{\"metadata\":{\"x402.payment.status\":\"payment-completed\",\"x402.payment.receipts\":[{\"success\":true,\"transaction\":\"$CANARY\",\"network\":\"base\"}]}}")
[ -n "$SIX" ] && [ -n "$SPX" ] || no "the stranger's two calls were not even sent (prose ${SIX:-none}, $PRIVCAP ${SPX:-none})"
IFS='|' read -r ST RS _ _ <<<"$(answer s "$SIX")"
[ "$ST" = rejected ] && [ "$RS" = not_accepting ] && ok "a task in prose is refused: rejected, not_accepting" \
  || no "the stranger's prose task ended '${ST:-unknown}' (${RS:-no reason}), expected rejected/not_accepting"
IFS='|' read -r ST RS _ _ <<<"$(answer s "$SPX")"
[ "$ST" = rejected ] && [ "$RS" = not_accepting ] && ok "a served but private capability ($PRIVCAP) is refused the same way" \
  || no "the call to the private $PRIVCAP ended '${ST:-unknown}' (${RS:-no reason}), expected rejected/not_accepting"
[ "$(in_inbox tools "$SIX")" = absent ] && [ "$(in_inbox tools "$SPX")" = absent ] && [ -n "$IN0" ] \
  && [ "$(inbox_n tools)" = "$IN0" ] && ok "neither left a record on the tools node ($IN0 inbound tasks, as before)" \
  || no "a refused call left a record on the tools node (inbox ${IN0:-?} → $(inbox_n tools))"
NA1=$(rx tools not-accepting)
[ "$NA1" -ge $((NA0 + 2)) ] && ok "the tools node counted both as not-accepting ($NA0 → $NA1)" \
  || no "the tools node's not-accepting counter went $NA0 → $NA1, expected +2"

# The door that is open: a public capability, for a caller nobody named.
ATX=$(send_cap a tools text.stats "{\"text\":\"one two $CANARY\"}")
T=$(wait_task a "$ATX")
[ "$(printf '%s' "$T" | tq state)" = completed ] && [ "$(printf '%s' "$T" | tq anet.effect_status)" = OK ] \
  && ok "a requester nobody named calls the public text.stats: completed, effect OK" \
  || no "text.stats from the requester: $(printf '%s' "$T" | tq state) $(printf '%s' "$T" | tq anet.effect_status) $(printf '%s' "$T" | tq anet.reason)"
[ "$(in_inbox tools "$ATX")" = public_cap ] && ok "the tools node holds it as trust=public_cap" \
  || no "the tools node's record of it: $(in_inbox tools "$ATX"), expected public_cap"
STX=$(send_cap s tools text.stats '{"text":"anyone may call this"}')
[ "$(wait_task s "$STX" | tq state)" = completed ] && ok "and so does the stranger: public means anyone" \
  || no "the stranger's text.stats did not complete"
AEX=$(send_cap a echo net.echo "{\"canary\":\"$CANARY\"}")
T=$(wait_task a "$AEX")
[ "$(printf '%s' "$T" | tq state)" = completed ] && [ "$(in_inbox echo "$AEX")" = public_cap ] \
  && ok "net.echo on the echo agent: completed, held as public_cap" \
  || no "net.echo from the requester: $(printf '%s' "$T" | tq state) $(printf '%s' "$T" | tq anet.reason), echo node: $(in_inbox echo "$AEX")"
AEFF=$(result_of a "$AEX")
printf '%s' "$AEFF" | eff observed | grep -qF "$CANARY" && ok "the echo came back to the requester with the canary in it" \
  || no "the requester's echo result does not hold the canary: $(printf '%s' "$AEFF" | head -c 200)"

hd "2/8  the kernel's admission — argument ceiling and quotas"
# Admit (§5.4) runs before anything is stored: the argument size against max_args_bytes, then the
# caller's and the capability's per-minute windows. A refusal says when to come back
# (anet.retry_after_ms, what a 429's Retry-After says over HTTP) and costs no quota.
EMAX=$(ctl echo /peers/list '{}' | python3 -c '
import sys, json
for p in json.load(sys.stdin).get("public_capabilities") or []:
    if p.get("id") == "net.echo": print(p.get("max_args_bytes") or 4096)')
PAD=$(python3 -c 'import sys;print("x"*(int(sys.argv[1])+512))' "${EMAX:-4096}")
IN0=$(inbox_n echo)
BIX=$(send_cap a echo net.echo "{\"pad\":\"$PAD\"}")
IFS='|' read -r ST RS RT _ <<<"$(answer a "$BIX")"
[ "$ST" = rejected ] && [ "$RS" = args_too_large ] && [ -z "$RT" ] \
  && ok "arguments over max_args_bytes (${EMAX:-?}) are refused: args_too_large, no retry offered" \
  || no "an oversized call ended '${ST:-unknown}' (${RS:-no reason}, retry ${RT:-none}), expected rejected/args_too_large"
[ "$(in_inbox echo "$BIX")" = absent ] && [ "$(inbox_n echo)" = "$IN0" ] && ok "and left no record" \
  || no "the oversized call left a record on the echo node"

# The quota windows are fixed minutes; start in a new one that nothing above has touched.
M0=$(fresh_minute "$(minute_now)")
IN0=$(inbox_n echo)
echo "  net.echo: $Q_CALLER per caller, $Q_GLOBAL in all per minute; window $M0 starts now"
AQ=(); for k in 1 2 3 4; do AQ+=("$(send_cap a echo net.echo "{\"n\":$k}")"); done
done_a=0; ref_a=0; rt_a=""
for x in "${AQ[@]}"; do
  IFS='|' read -r ST RS RT _ <<<"$(answer a "$x")"
  case "$ST|$RS" in
    completed\|*) done_a=$((done_a + 1)) ;;
    rejected\|quota_caller_per_min) ref_a=$((ref_a + 1)); rt_a=$RT ;;
    *) echo "    a: $x ended '$ST' ($RS)" ;;
  esac
done
[ "$done_a" = "$Q_CALLER" ] && [ "$ref_a" = 1 ] \
  && ok "requester a: $Q_CALLER calls run, the next is refused with quota_caller_per_min" \
  || no "requester a: $done_a completed and $ref_a refused for its per-caller quota, expected $Q_CALLER and 1"
[ -n "$rt_a" ] && [ "$rt_a" -gt 0 ] 2>/dev/null && [ "$rt_a" -le 60000 ] \
  && ok "the refusal says when to retry: anet.retry_after_ms=$rt_a" \
  || no "the quota refusal carries no usable anet.retry_after_ms ('$rt_a')"
BQ=(); for k in 1 2 3; do BQ+=("$(send_cap b echo net.echo "{\"n\":$k}")"); done
done_b=0; ref_b=0
for x in "${BQ[@]}"; do
  IFS='|' read -r ST RS _ _ <<<"$(answer b "$x")"
  case "$ST|$RS" in
    completed\|*) done_b=$((done_b + 1)) ;;
    rejected\|quota_global_per_min) ref_b=$((ref_b + 1)) ;;
    *) echo "    b: $x ended '$ST' ($RS)" ;;
  esac
done
[ "$done_b" = $((Q_GLOBAL - Q_CALLER)) ] && ok "requester b is not held to a's quota: its first $done_b calls run" \
  || no "requester b: $done_b calls ran, expected $((Q_GLOBAL - Q_CALLER)) (the rest of the global window)"
[ "$ref_b" = 1 ] && ok "then the global window is full, and b is refused too: quota_global_per_min" \
  || no "requester b: $ref_b refused for the global quota, expected 1"
IFS='|' read -r ST RS RT _ <<<"$(answer s "$(send_cap s echo net.echo '{"n":0}')")"
[ "$ST" = rejected ] && [ "$RS" = quota_global_per_min ] \
  && ok "a caller that never called before is held to it as well (quota_global_per_min)" \
  || no "the stranger's first net.echo ended '${ST:-unknown}' (${RS:-no reason}), expected rejected/quota_global_per_min"
[ "$(minute_now)" = "$M0" ] || note "the section ran past its quota window ($M0 → $(minute_now)); a failure above may be that, rerun"
IN1=$(inbox_n echo)
[ -n "$IN0" ] && [ "$IN1" = $((IN0 + Q_GLOBAL)) ] \
  && ok "only the $Q_GLOBAL admitted calls are on the echo node ($IN0 → $IN1); the refused left nothing" \
  || no "the echo node's inbound tasks went ${IN0:-?} → ${IN1:-?}, expected +$Q_GLOBAL"
echo "  echo node refusals: $(ctl echo /status '{}' | python3 -c '
import sys, json
r = json.load(sys.stdin).get("receive") or {}
print(", ".join("%s=%s" % (k, v) for k, v in sorted(r.items()) if k.startswith("refused-")))')"

hd "3/8  the backend — its daemon's token, checked before anything is routed"
# The backend listens on loopback, where every process on the host can reach it (§6: the auto-reply
# sandbox included). Without the token it must answer every path alike, so its routes cannot be probed,
# and it must not be told who the caller is by anything but the daemon.
EB=http://127.0.0.1:${BACKEND_PORT[echo]}
code(){ curl -s -o /dev/null -w '%{http_code}' -m 10 "$@"; }
C1=$(code -X POST -H 'Content-Type: application/json' -H "X-ANet-Caller: ${AID_OF[a]}" -d '{"x":1}' "$EB/v1/echo/net.echo")
[ "$C1" = 401 ] && ok "no token, with a caller header written by hand: 401" || no "no token: $C1, expected 401"
printf 'Authorization: Bearer %s\n' "$(rand)$(rand)" > "$RUN/wrong.hdr"
C2=$(code -X POST -H @"$RUN/wrong.hdr" -H 'Content-Type: application/json' -d '{"x":1}' "$EB/v1/echo/net.echo")
[ "$C2" = 401 ] && ok "a wrong token: 401" || no "a wrong token: $C2, expected 401"
C3=$(code -X POST -H 'Content-Type: application/json' -d '{}' "$EB/v1/echo/no.such.capability")
C4=$(code -X POST -H 'Content-Type: application/json' -d '{}' "$EB/v1/tools/text.stats")
C5=$(code "$EB/v1/")
[ "$C3" = 401 ] && [ "$C4" = 401 ] && [ "$C5" = 401 ] \
  && ok "no token, an unknown path, another group's route, a GET: all 401 — authentication comes before routing" \
  || no "without a token: unknown path $C3, another group's route $C4, GET $C5 (all must be 401)"
C6=$(code -X POST -H 'Host: anet.example' -H 'Content-Type: application/json' -d '{}' "$EB/v1/echo/net.echo")
[ "$C6" = 421 ] && ok "a non-loopback Host (DNS rebinding) is answered 421" || no "a non-loopback Host: $C6, expected 421"
# With the right token — which only the daemon and this script hold — the same paths route: the 401s
# above were the token, not a backend that refuses everything.
printf 'Authorization: Bearer %s\n' "$(cat "$RUN/official/echo.token")" > "$RUN/echo.hdr"
C7=$(code -X POST -H @"$RUN/echo.hdr" -H 'Content-Type: application/json' -d '{}' "$EB/v1/echo/no.such.capability")
C8=$(code -X POST -H @"$RUN/echo.hdr" -H 'Content-Type: application/json' -d '{"probe":true}' "$EB/v1/echo/net.echo")
[ "$C7" = 404 ] && [ "$C8" = 200 ] && ok "with the daemon's token the unknown path is 404 and the route answers 200" \
  || no "with the token: unknown path $C7 (expected 404), net.echo $C8 (expected 200)"
# What the daemon told the backend about the call it relayed in 1/8.
L=$(backend_line echo "${AEX:-none}")
echo "  backend:  ${L:-<no line for $AEX>}"
[ "$(field "$L" caller)" = "${AID_OF[a]}" ] && [ "$(field "$L" via)" = relay ] && [ "$(field "$L" status)" = 200 ] \
  && ok "the backend was told the verified caller: X-ANet-Caller = requester a's AID, via relay" \
  || no "the backend's line for $AEX names caller '$(field "$L" caller)' via '$(field "$L" via)', expected ${AID_OF[a]} via relay"
UA=$(grep -c ' status=401 ' "$RUN/backend-echo.log" 2>/dev/null)
[ "${UA:-0}" -ge 5 ] && ok "every refused knock is in its log ($UA lines with status=401)" \
  || no "the backend logged ${UA:-0} refusals, expected at least 5"
R=$(canary_hits "$CANARY" "$RUN"/backend-*.log); H=$(printf '%s\n' "$R" | grep -v '^# ')
[ -z "$H" ] && [ "$(printf '%s\n' "$R" | sed -n 's/^# searched //p')" = "${#OFFICIAL[@]}" ] \
  && ok "no backend log holds the canary, in any encoding: they log who and how much, never what" \
  || no "the backend logs: ${H:-<not all searched>}"

hd "4/8  compute — an input built to explode ends in budget, within the deadline"
# json.validate takes a schema. A few hundred bytes — a schema that recurses through three \$refs, and an
# instance forty levels deep — ask for 3^40 subschema applications. The backend counts the work and gives
# up at its budget (422 too_complex), or at its deadline (503): either way within timeout_ms, answered as
# FAILED or UNAVAILABLE, and the backend keeps serving. (The daemon's service module has the same
# timeout_ms for the call, so on a slow host its own deadline may be what the requester hears about:
# UNAVAILABLE, "context deadline exceeded". A deadline noticed between two budget checks is reported a
# few ms after it passed; up to a second of that is allowed.)
TO=$(timeout_of json.validate)
BOMB='{"schema":{"properties":{"a":{"anyOf":[{"$ref":"#"},{"$ref":"#"},{"$ref":"#"}]}},"required":["missing"]},"instance":'$(python3 -c 'print("{\"a\":"*40 + "1" + "}"*40)')'}'
T0=$(now_ms)
BVX=$(send_cap a tools json.validate "$BOMB")
BEFF=$(result_of a "${BVX:-none}")
T1=$(now_ms)
BST=$(printf '%s' "$BEFF" | eff status); BMSG=$(printf '%s' "$BEFF" | eff message)
echo "  effect:   $BST — $(printf '%s' "$BMSG" | head -c 160)   (requester waited $((T1 - T0)) ms)"
case "$BST" in
  FAILED) printf '%s' "$BMSG" | grep -q too_complex && ok "the answer is budget exhausted (FAILED, too_complex)" \
            || no "FAILED, but not for its budget: $(printf '%s' "$BMSG" | head -c 200)" ;;
  UNAVAILABLE) printf '%s' "$BMSG" | grep -Eq 'timeout|deadline exceeded' && ok "the answer is out of time (UNAVAILABLE, timeout)" \
            || no "UNAVAILABLE, but not for its deadline: $(printf '%s' "$BMSG" | head -c 200)" ;;
  *) no "the exploding schema came back '$BST': $(printf '%s' "$BEFF" | head -c 200)" ;;
esac
L=$(backend_line tools "${BVX:-none}")
MS=$(field "$L" ms); HS=$(field "$L" status)
echo "  backend:  ${L:-<no line for $BVX>}"
{ { [ "$HS" = 422 ] && [ -n "$TO" ] && [ "${MS:-x}" -le "$TO" ] 2>/dev/null; } \
  || { [ "$HS" = 503 ] && [ -n "$TO" ] && [ "${MS:-x}" -le $((TO + 1000)) ] 2>/dev/null; }; } \
  && ok "the backend stopped it after $MS ms ($HS), at or within json.validate's $TO ms" \
  || no "the backend's line says status ${HS:-?} after ${MS:-?} ms (deadline ${TO:-?} ms)"
GVX=$(send_cap a tools json.validate '{"schema":{"type":"object","required":["id"]},"instance":{"id":1}}')
GEFF=$(result_of a "${GVX:-none}")
[ "$(printf '%s' "$GEFF" | eff status)" = OK ] && printf '%s' "$GEFF" | eff observed | grep -q '"valid"' \
  && ok "the next json.validate is answered normally (valid): the backend was not held" \
  || no "a plain json.validate after it: $(printf '%s' "$GEFF" | head -c 200)"

hd "5/8  the paid demo — demo.digest.paid, paid inside the task (a2a-x402)"
# A fresh install pays nothing on its own (auto_max 0) and pays nobody it has not listed (payees.allow).
# So the quote waits for the operator; `anet pay` (/tasks/pay-manual) pays it once the payee is on the
# list; the hub settles; the result comes back in the same task with the hub's receipt; one debit.
ARGS="{\"text\":\"paid $CANARY\"}"
A0=$(balance_of a); P0=$(balance_of paid)
echo "  balances: requester a ${A0:-?}, anet-paid-demo ${P0:-?} (registration grants)"
[ -n "$A0" ] && [ "$A0" -gt 0 ] || no "requester a has no balance to pay with (${A0:-unreadable})"
PIX=$(send_cap a paid demo.digest.paid "$ARGS")
T=$(wait_task a "${PIX:-}")
PRICE=$(printf '%s' "$T" | tq x402.payment.required | python3 -c '
import sys, json
try:
    a = (json.load(sys.stdin).get("accepts") or [{}])[0]
    print("%s %s" % (a.get("amount", ""), a.get("payTo", "")))
except Exception:
    print("")')
read -r AMT PAYTO <<<"$PRICE"
[ "$(printf '%s' "$T" | tq state)" = input_required ] && [ "$(printf '%s' "$T" | tq x402.payment.status)" = payment-required ] \
  && ok "the task stops at input-required with x402.payment.status=payment-required" \
  || no "the paid call is '$(printf '%s' "$T" | tq state)' / '$(printf '%s' "$T" | tq x402.payment.status)', expected input_required / payment-required"
[ -n "$AMT" ] && [ "$PAYTO" = "${AID_OF[paid]}" ] && ok "quoted $AMT credits, payable to the paid agent" \
  || no "the quote names amount '${AMT}' and payee '${PAYTO}' (expected ${AID_OF[paid]})"
[ "$(printf '%s' "$T" | tq anet.reason)" = needs_operator_approval ] \
  && ok "and waits for the operator (anet.reason=needs_operator_approval): auto_max is 0" \
  || no "the waiting task's reason is '$(printf '%s' "$T" | tq anet.reason)', expected needs_operator_approval"
# The agent tier over its limit is not an error (0017 Q26): the task stays input-required for the
# operator, the answer names the refusal (spend_refusal) and what the operator does first. Nothing is
# signed either way; the checks below confirm it.
C=$(ctlc a /tasks/pay "{\"task_id\":\"$PIX\",\"decision\":\"submit\"}")
[ "$C" = 200 ] && [ "$(jget spend_refusal < "$RUN/ctlc.out")" = over_single_limit ] \
  && [ "$(jget anet.reason < "$RUN/ctlc.out")" = needs_operator_approval ] \
  && ok "an agent (/tasks/pay, agent_max 0) cannot pay it: the task waits for the operator (spend_refusal over_single_limit)" \
  || no "/tasks/pay on a fresh install answered $C: $(head -c 200 "$RUN/ctlc.out")"
C=$(ctlc a /tasks/pay-manual "{\"task_id\":\"$PIX\",\"decision\":\"submit\"}")
[ "$C" = 403 ] && [ "$(jget reason < "$RUN/ctlc.out")" = payee_not_allowed ] \
  && ok "nor can the operator, before the payee is on payees.allow: 403 payee_not_allowed" \
  || no "paying an unlisted payee answered $C: $(head -c 200 "$RUN/ctlc.out")"
[ "$(balance_of a)" = "$A0" ] && [ "$(ev_for a anet.payment.authorized "$PIX")" = 0 ] \
  && ok "nothing was signed and nothing moved" || no "a refused payment signed or moved something"
_peer_add "${HOME_OF[a]}/.anet/payees.allow" "${AID_OF[paid]}"
C=$(ctlc a /tasks/pay-manual "{\"task_id\":\"$PIX\",\"decision\":\"submit\"}")
[ "$C" = 200 ] && ok "with the payee listed, the operator's decision (/tasks/pay-manual) is taken: $(jget x402.payment.status < "$RUN/ctlc.out")" \
  || no "/tasks/pay-manual answered $C: $(head -c 200 "$RUN/ctlc.out")"
T=$(wait_end a "$PIX" 60)
PRC=$(printf '%s' "$T" | tq x402.payment.receipts | python3 -c '
import sys, json
try:
    rs = json.load(sys.stdin)
except Exception:
    rs = []
ok = [r for r in rs if isinstance(r, dict) and r.get("success") and r.get("transaction")]
print("%d %d" % (len(rs), len(ok)))')
[ "$(printf '%s' "$T" | tq state)" = completed ] && [ "$(printf '%s' "$T" | tq x402.payment.status)" = payment-completed ] \
  && [ "$(printf '%s' "$T" | tq anet.effect_status)" = OK ] \
  && ok "the same task completes: payment-completed, effect OK" \
  || no "after paying: '$(printf '%s' "$T" | tq state)' / '$(printf '%s' "$T" | tq x402.payment.status)' / effect '$(printf '%s' "$T" | tq anet.effect_status)'"
[ "$PRC" = "1 1" ] && ok "with one receipt: a successful settlement and its transaction" \
  || no "receipts (all, successful): ${PRC:-none}, expected one successful"
A1=$(balance_of a); P1=$(balance_of paid)
[ -n "$A1" ] && [ -n "$AMT" ] && [ $((A0 - A1)) = "$AMT" ] && [ $((P1 - P0)) = "$AMT" ] \
  && ok "one debit: requester a $A0 → $A1, anet-paid-demo $P0 → $P1" \
  || no "balances: requester a ${A0:-?} → ${A1:-?}, anet-paid-demo ${P0:-?} → ${P1:-?}, price ${AMT:-?}"
AUTH=$(ev_for a anet.payment.authorized "$PIX"); SA=$(ev_for a anet.payment.settled "$PIX")
SAV=$(ev_for a anet.payment.settled "$PIX" verified); SP=$(ev_for paid anet.payment.settled "$PIX")
[ "$AUTH" = 1 ] && [ "$SA" = 1 ] && [ "$SP" = 1 ] \
  && ok "both chains hold it once: one authorization and one settlement at the payer, one settlement at the payee" \
  || no "payment evidence for $PIX: payer authorized ${AUTH:-?}, payer settled ${SA:-?}, payee settled ${SP:-?} (each expected 1)"
[ "$SAV" = 1 ] && ok "and the payer checked the hub's signed receipt (verified)" \
  || no "the payer's settlement record is not marked verified (${SAV:-unreadable})"
C=$(ctlc a /tasks/pay-manual "{\"task_id\":\"$PIX\",\"decision\":\"submit\"}")
[ "$C" = 409 ] && [ "$(balance_of a)" = "$A1" ] && ok "paying the finished task again is refused (409) and moves nothing" \
  || no "a second payment answered $C and the balance is $(balance_of a) (was $A1)"
PAIDOBS=$(result_of a "$PIX" | eff observed)
TDX=$(send_cap a tools text.digest "$ARGS")
FREEOBS=$(result_of a "${TDX:-none}" | eff observed)
[ -n "$PAIDOBS" ] && [ "$PAIDOBS" = "$FREEOBS" ] \
  && ok "what was paid for is checkable: the same bytes as the free text.digest on the tools agent" \
  || no "paid and free digests differ: $(printf '%s' "$PAIDOBS" | head -c 120) vs $(printf '%s' "$FREEOBS" | head -c 120)"

hd "6/8  evidence — the official chain keeps result CIDs, not results"
# An official agent sees what it is asked; what it keeps on its evidence chain is another matter.
# Public capability calls are recorded with the result's CID and the metrics, and without the observed
# state — the answer itself (A2A-DESIGN §15, 0017 Q15, cid mode). net.echo is the telling case: its
# answer IS the arguments, canary included.
for n in "${OFFICIAL[@]}"; do
  R=$(ctl "$n" /evidence '{"event_type":"anet.capability.effect","limit":1000}' | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = None
if not isinstance(d, dict) or not isinstance(d.get("records"), list):
    print("unreadable"); sys.exit()
recs = [r.get("payload") or {} for r in d["records"]]
def has(o, keys):
    if isinstance(o, dict):
        return any(k in keys or has(v, keys) for k, v in o.items())
    if isinstance(o, list):
        return any(has(v, keys) for v in o)
    return False
content = sum(1 for p in recs if has(p, ("observed_state", "quirk")))
nocid = sum(1 for p in recs if p.get("status") == "OK" and not p.get("result_cid"))
print(len(recs), content, nocid)')
  read -r NREC NCONT NNOCID <<<"$R"
  if [ "$NREC" = unreadable ] || [ "${NREC:-0}" = 0 ]; then
    no "$n: no anet.capability.effect records to check (${NREC:-none})"
  elif [ "$NCONT" = 0 ] && [ "$NNOCID" = 0 ]; then
    ok "$n: $NREC effect records, each with its result CID, none with the result"
  else
    no "$n: $NCONT of $NREC effect records carry the result (observed_state), $NNOCID OK ones lack result_cid — cid mode (B3-10) is not in effect"
  fi
done
# The chain file itself: one base64 CoreDet-CBOR record per line, so a plain grep for the canary finds
# nothing whatever the records hold; canary_hits (lib.sh) looks for its base64 at every alignment too.
# Every official node got the canary in 1/8 or 5/8; net.echo answered with it.
for n in "${OFFICIAL[@]}"; do
  CH=${HOME_OF[$n]}/.anet/evidence.ael.jsonl
  R=$(canary_hits "$CANARY" "$CH"); H=$(printf '%s\n' "$R" | grep -v '^# ')
  if [ -n "$H" ]; then
    no "$n: the evidence chain holds the canary its callers sent: $H"
  elif [ -s "$CH" ] && [ "$(printf '%s\n' "$R" | sed -n 's/^# searched //p')" = 1 ]; then
    ok "$n: its evidence chain does not hold the canary, in any encoding"
  else
    no "$n: the evidence chain $CH is missing or empty"
  fi
done

hd "7/8  the hub never holds the content (SI-1, the official path)"
# Every official call above carried the canary — in arguments, in a prose task, in a paid call — through
# this hub. It relays sealed envelopes and settles payments; the admin beside it harvests and snapshots
# every 2 s. Neither may hold the canary on disk, in a log, or in any answer.
if [ "$ADMIN" = 1 ]; then
  [ "$(adm POST /api/harvest '{}')" = 200 ] || note "a forced harvest answered $(head -c 120 "$RUN/adm.out")"
fi
sleep 5
# Searched as canary_hits does (lib.sh): as it is, in hex, and in base64 at every alignment — the hub
# keeps envelopes, receipts, KELs and authorizations as base64, where a plain grep would miss a leak.
SEARCH=("$RUN/hub" "$RUN/hub.log")
[ "$ADMIN" = 1 ] && SEARCH+=("$RUN/admin" "$RUN/admin.log")
R=$(canary_hits "$CANARY" "${SEARCH[@]}"); HITS=$(printf '%s\n' "$R" | grep -v '^# ')
NF=$(printf '%s\n' "$R" | sed -n 's/^# searched //p')
NH=$(find "$RUN/hub" -type f 2>/dev/null | wc -l)
[ -z "$HITS" ] && [ "$NH" -gt 0 ] \
  && ok "no file of the hub or the admin holds the canary, in any encoding ($NF files searched, WAL included, and the logs)" \
  || no "the canary: ${HITS:-<no hub data file was searched>}"
mkdir -p "$RUN/scan"
scan(){ curl -s -m 10 "${@:2}" > "$RUN/scan/$1" 2>/dev/null; }
scan agents "$HUB_URL/agents"
for n in "${NODES[@]}"; do
  for sub in "" /card /ledger /reputation /redemptions /balance; do
    scan "agent-$n${sub//\//-}" "$HUB_URL/agents/${AID_OF[$n]}$sub"
  done
done
scan fed-reviews "$HUB_URL/fed/v1/reviews"; scan fed-cards "$HUB_URL/fed/v1/cards"
scan stats "$HUB_URL/stats"; scan graph "$HUB_URL/graph"; scan research "$HUB_URL/research"
scan x402-issuance "$HUB_URL/x402/issuance"
if [ "$ADMIN" = 1 ]; then
  for p in overview agents sessions reviews audit store discover official capabilities vision deleted; do
    scan "admin-$p" -H @"$RUN/admin.hdr" "http://$ADMIN_ADDR/admin/api/$p"
  done
  for n in "${NODES[@]}"; do
    scan "admin-agent-$n" -H @"$RUN/admin.hdr" "http://$ADMIN_ADDR/admin/api/agents/${AID_OF[$n]}"
  done
  python3 -c '
import sys, json
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    d = {}
for s in d.get("sessions") or []:
    if isinstance(s, dict) and s.get("source") and s.get("id"):
        print(s["source"], s["id"])' "$RUN/scan/admin-sessions" | while read -r src id; do
    scan "admin-session-$src-$id" -H @"$RUN/admin.hdr" "http://$ADMIN_ADDR/admin/api/sessions/$src/$id"
  done
fi
R=$(canary_hits "$CANARY" "$RUN/scan"); SHITS=$(printf '%s\n' "$R" | grep -v '^# ')
REAL=$(grep -l -F -- "${AID_OF[tools]}" "$RUN/scan/agent-tools" 2>/dev/null)
[ -z "$SHITS" ] && [ -n "$REAL" ] \
  && ok "no answer of the hub or the admin holds it ($(printf '%s\n' "$R" | sed -n 's/^# searched //p') answers read: agents, cards, ledgers, reviews, stats, graph, admin)" \
  || no "the canary is in the answers of: ${SHITS:-<none>}; the hub answered /agents/{aid}: ${REAL:+yes}${REAL:-no}"
if [ "$ADMIN" = 1 ]; then
  BADR=""
  for n in "${OFFICIAL[@]}"; do
    for sub in insights acl monitor/x ops; do
      C=$(adm GET "/api/official/${NAME_OF[$n]}/$sub")
      [ "$C" = 404 ] || BADR="$BADR ${NAME_OF[$n]}/$sub=$C"
    done
  done
  [ -z "$BADR" ] && ok "the admin has no way into an official agent: insights, acl, monitor, ops are 404 for all three" \
    || no "admin routes into official agents answered:$BADR"
else
  ns "the hub admin's half of SI-1: no hub admin in this run (JOINT_HUB_ADMIN=0 or no anet-hub-admin binary)"
fi

hd "8/8  identity and disposition — denied is not-accepting; a borrowed name is not official"
# Denying a caller on an official node takes effect on its next call, and it is told what a stranger is
# told (under closed, §5.2 row 1 answers as row 6): whether it is denied is not observable.
_peer_add "${HOME_OF[echo]}/.anet/peers.deny" "${AID_OF[a]}"
IN0=$(inbox_n echo); NA0=$(rx echo not-accepting)
DX1=$(send_cap a echo net.echo '{"after":"deny"}')
DX2=$(send_goal a echo "Please echo this back")
DX3=$(send_goal s echo "Please echo this back")
W1=$(answer a "$DX1"); W2=$(answer a "$DX2"); W3=$(answer s "$DX3")
echo "  denied a, public capability: $W1"
echo "  denied a, prose:             $W2"
echo "  stranger, prose:             $W3"
[ "${W3%%|*}" = rejected ] && [ "$W1" = "$W3" ] && [ "$W2" = "$W3" ] \
  && ok "the denied requester is told exactly what the stranger is told (state, reason, retry, text)" \
  || no "the answers differ, or are not refusals"
[ "$(in_inbox echo "$DX1")" = absent ] && [ "$(in_inbox echo "$DX2")" = absent ] && [ "$(inbox_n echo)" = "$IN0" ] \
  && ok "nothing the denied requester sent was stored" || no "the denied requester's calls left a record on the echo node"
NA1=$(rx echo not-accepting)
[ "$NA1" -ge $((NA0 + 3)) ] && ok "the echo node counted all three as not-accepting ($NA0 → $NA1)" \
  || no "the echo node's not-accepting counter went $NA0 → $NA1, expected +3"

# Official is a property of an AID in the signed manifest the client carries (§15), never of a name.
# The impostor registered as anet-tools, as this run's tools agent did. That the impostor is not marked
# says something only where marking happens at all: a build whose manifest lists none of this run's AIDs
# (every release build: its manifest names production AIDs) marks nobody here, and "the impostor is not
# marked" would hold of it even if it marked by name. So the observer: a daemon built by
# scripts/official-testbin.sh (B5-01) with a manifest, signed by a throwaway key, that lists this run's
# tools agent by AID. It must see that agent marked, and the impostor of the same name not.
#
# mark_report AID… — from the discovery answers on stdin (/find, /agents/list, /agents/card; one JSON
# document per line), for each AID "seen|unseen:marked|unmarked:flagged|clean": listed at all; carrying
# "anet.official": true where the daemon puts it (on the agent entry); carrying anything official-like
# anywhere in its entry, card included. Then "keyed|unkeyed" (does any entry carry the key at all), then
# the AIDs of entries named anet-tools that are flagged, or "-".
mark_report(){ python3 -c '
import sys, json
def flagged(o):
    if isinstance(o, dict):
        return any((k in ("anet.official", "official") and v not in (None, False, "", 0)) or flagged(v)
                   for k, v in o.items())
    if isinstance(o, list):
        return any(flagged(v) for v in o)
    return False
def haskey(o):
    if isinstance(o, dict):
        return any(k in ("anet.official", "official") or haskey(v) for k, v in o.items())
    if isinstance(o, list):
        return any(haskey(v) for v in o)
    return False
entries = []
for line in sys.stdin:
    try:
        d = json.loads(line)
    except Exception:
        continue
    if not isinstance(d, dict):
        continue
    for e in (d.get("agents") if isinstance(d.get("agents"), list) else [d] if d.get("aid") else []):
        if isinstance(e, dict):
            if isinstance(e.get("card"), str):
                try:
                    e["card"] = json.loads(e["card"])
                except Exception:
                    pass
            entries.append(e)
out = []
for aid in sys.argv[1:]:
    mine = [e for e in entries if e.get("aid") == aid]
    out.append("%s:%s:%s" % ("seen" if mine else "unseen",
                             "marked" if any(e.get("anet.official") is True for e in mine) else "unmarked",
                             "flagged" if any(flagged(e) for e in mine) else "clean"))
out.append("keyed" if any(haskey(e) for e in entries) else "unkeyed")
out.append(",".join(sorted({e.get("aid") or "?" for e in entries
                            if (e.get("name") == "anet-tools" or "anet-tools" in json.dumps(e.get("card") or {}))
                            and flagged(e)})) or "-")
print(" ".join(out))' "$@"; }
# discovery <node> — what that node is told about agents named anet-tools, and the two cards.
discovery(){
  ctl "$1" /find '{"query":"anet-tools"}'; echo
  ctl "$1" /agents/list '{"q":"anet-tools"}'; echo
  ctl "$1" /agents/card "{\"aid\":\"${AID_OF[tools]}\"}"; echo
  ctl "$1" /agents/card "{\"aid\":\"${AID_OF[imp]}\"}"; echo
}
# As requester a sees it: a build of this checkout, whose manifest lists no AID of this run.
read -r _ IMPA KEYED MARKED <<<"$(discovery a | mark_report "${AID_OF[tools]}" "${AID_OF[imp]}")"
[ "${IMPA%%:*}" = seen ] && ok "the impostor is found under the name anet-tools" \
  || no "the impostor did not show up in /find, /agents/list or /agents/card (nothing to check the mark on)"
[ "$MARKED" = "-" ] || no "requester a, whose manifest lists no agent of this run, marks by name: $MARKED"

OBS=0; OBS_WHY=""
if [ "$MARK" != 1 ]; then
  OBS_WHY="JOINT_OFFICIAL_MARK=0${JOINT_BIN:+ (the default with JOINT_BIN)}"
elif [ ! -f "$SCRIPTS/official-testbin.sh" ]; then
  OBS_WHY="scripts/official-testbin.sh is not in this checkout (B5-01 is not merged)"
elif ! command -v go >/dev/null || ! command -v ssh-keygen >/dev/null; then
  OBS_WHY="the observer is built here, and go or ssh-keygen is missing"
else
  echo "  observer: building a test anet whose manifest lists anet-tools = ${AID_OF[tools]} (log: $RUN/offtest.log)"
  # official-testbin.sh builds from this checkout and refuses a GOWORK that resolves ./cmd/anet elsewhere.
  if [ -z "${GOWORK:-}" ] && [ -f "$ROOT/../go.work" ]; then
    GOWORK=$(cd "$ROOT/.." && pwd -P)/go.work; export GOWORK
  fi
  CAPS=$(printf '%s' "$TPUB" | tr ' ' ',')
  if ( bash "$SCRIPTS/official-testbin.sh" -o "$BIN/offtest" --hub "$HUB_URL" \
         "anet-tools=${AID_OF[tools]}${CAPS:+:$CAPS}" ) >"$RUN/offtest.log" 2>&1 </dev/null 9>&-; then
    fresh_config o
    start_node o "$BIN/offtest/anet"
    if wait_up "${ADDR_OF[o]}" 30 && AID_OF[o]=$(ctl o /status '{}' | jget aid) && [ -n "${AID_OF[o]}" ] \
       && [ "$(ctl o /hub-register "{\"hub\":\"$HUB_URL\",\"name\":\"${NAME_OF[o]}\"}" | jget status)" = registered ]; then
      OBS=1
    else
      no "the observer did not come up and register: $(tail -3 "${HOME_OF[o]}/.anet/daemon.log" "$RUN/o.out" 2>/dev/null)"
    fi
  else
    no "official-testbin.sh could not build the observer: $(tail -3 "$RUN/offtest.log")"
  fi
fi
if [ "$OBS" = 1 ]; then
  read -r TOOLO IMPO _ MARKEDO <<<"$(discovery o | mark_report "${AID_OF[tools]}" "${AID_OF[imp]}")"
  echo "  observer: tools ${TOOLO:-?}, impostor ${IMPO:-?}, flagged by name: ${MARKEDO:-?}"
  [ "$TOOLO" = seen:marked:flagged ] \
    && ok "the observer, whose signed manifest lists the tools agent's AID, marks it anet.official" \
    || no "the observer's view of the tools agent is ${TOOLO:-unreadable}, expected seen:marked (the mark is not shown for a listed AID)"
  [ "${IMPO%%:*}" = seen ] && [ "${IMPO##*:}" = clean ] && [ "$MARKEDO" = "${AID_OF[tools]}" ] \
    && ok "and not the impostor of the same name: the name buys nothing, the AID is what is marked" \
    || no "the observer's view of the impostor is ${IMPO:-unreadable}; flagged by name: ${MARKEDO:-?} (only ${AID_OF[tools]} may be)"
elif [ -n "$OBS_WHY" ] && [ -n "${JOINT_OFFICIAL_MARK:-}" ] && [ "$MARK" = 1 ]; then
  no "JOINT_OFFICIAL_MARK=1, and the observer cannot be built: $OBS_WHY"
elif [ -n "$OBS_WHY" ]; then
  ns "the anet.official mark: $OBS_WHY. No build here marks an agent of this run$( [ "$KEYED" = keyed ] || printf ' (no answer carries the key at all)'), so \"the impostor is not marked\" would hold of any build"
fi

printf '\n\033[1m── %d passed, %d failed, %d not shown ──\033[0m   logs: %s\n' "$pass" "$fail" "$unshown" "$RUN"
[ "$fail" -eq 0 ]
