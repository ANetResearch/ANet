#!/usr/bin/env bash
# joint-a2a.sh — the local A2A interface end to end (A2A-DESIGN §17, the joint-a2a row; plan 0014 B5-03).
#
# An A2A client that knows nothing about anet — a2a-go's own client, unmodified (tools/a2aprobe) — talks
# to a requester daemon's local A2A interface (127.0.0.1, its own bearer token), and through it, the hub
# and end-to-end encryption, to a provider daemon:
#
#   a2aprobe (a2a-go client) → requester :a2a → requester daemon → hub (relay) → provider daemon
#                                                                   ← responder (the provider's agent)
#                                                                   ← backend (its priced capabilities)
#
# The probe covers SendMessage (blocking and returnImmediately), SendStreamingMessage, GetTask, ListTasks
# by contextId, CancelTask, a client whose card lost its securityRequirements (401), another agent's path
# (TaskNotFound), the a2a-x402 same-task flow with the payment message of §8.7 (payment-submitted without a
# payload, anet.payment.accept) and its three refusals — above the agent tier, an option not offered, a
# payload of the client's own. The script then checks what the probe cannot see: the provider's side of
# the cancel, what the backend ran, what the requester signed, what moved at the hub, and that the hub's
# data holds none of the text. The requester is restarted and the probe's saved configuration (URL and
# token, as Hermes keeps them) must still work. The raw JSON-RPC responses of the blocking sends are read
# by the port of Hermes' A2A client (internal/a2ashape TestHermesReadsJointRecord). a2a-tck is run on
# request, as a record only.
#
# Self-contained, like joint.sh: its own binaries, hub, two daemons, backend and responder, on a loopback
# port block of its own; at the end it stops exactly what it started — by path, never by process name.
#
#   J=/tmp/ja bash scripts/joint-a2a.sh                  build from this checkout (needs go)
#   JOINT_BIN=DIR J=/tmp/ja bash scripts/joint-a2a.sh    prebuilt binaries, no go on the host
#
# Environment:
#   J                 work directory (default /tmp/joint-a2a-<uid>). It must be this user's and writable by no
#                     one else (lib.sh own_dir). An existing non-empty directory is used only if an earlier run
#                     of this script made it (.joint-a2a-dir); each run replaces $J/bin and $J/run.
#   JOINT_BIN         directory with prebuilt anet, anet-hub, a2aprobe and optionally a2ashape-hermes.test
#                     (scripts/testnet/build.sh makes this set). Copied into $J/bin. Unset: built here with go
#                     from this checkout and HUB_SRC.
#   HUB_SRC           ANetHub checkout to build from (default: ../ANetHub beside this repository)
#   JOINT_PORT_BASE   first of 10 consecutive loopback ports; unset = a random free block in 20000-32000. On the
#                     test hosts use a block inside 47100-47499 that the deployed test network does not use
#                     (docs/notes/0015, scripts/testnet/topology.env). A port in use aborts the run.
#   JOINT_A2A_ALLOC   1 = let the requester's A2A interface choose its own port on first start (43811 and up)
#                     instead of one from the block. Not on the test hosts: that range is where their production
#                     and test-network daemons keep their own interfaces, and this daemon's private runtime
#                     directory hides their records from its scan. Refused with a JOINT_PORT_BASE in 47100-47499.
#   JOINT_TIMEOUT     bound of one probe operation that waits for the provider (default 90s)
#   JOINT_A2A_TCK     1 = also run scripts/a2a-tck.sh through the requester's interface (never gates; needs the
#                     a2a-tck checkout and a Python venv, see docs/notes/0019; A2A_TCK_DIR/A2A_TCK_VENV apply)
#   JOINT_A2A_TCK_TIMEOUT  wall-clock bound of that run in seconds (default 1800)
#   JOINT_KEEP        1 = leave everything running at the end (default: stop it)
#
# Ports of the block: +0 hub, +1 requester control, +2 provider control, +3 requester A2A, +4 provider A2A,
# +5 the provider's capability backend, +6..+9 spare.
#
# Building: as joint.sh — ANet and ANetHub from one workspace (GOWORK; ../go.work in the work-tree layout).
set -uo pipefail
umask 077
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
SCRIPTS=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPTS/.." && pwd -P)

J=${J:-/tmp/joint-a2a-$(id -u)}
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
die(){  printf '\033[1;31mjoint-a2a.sh: %s\033[0m\n' "$*" >&2; exit 2; }
for c in curl python3 setsid; do command -v "$c" >/dev/null || die "$c is required"; done

# ── the work directory ──────────────────────────────────────────
case "$J" in /*) ;; *) J=$PWD/$J ;; esac
_own_path "$J" >/dev/null || die "J=$J is not a directory this script may own (pick something like /tmp/joint-a2a-x)"
own_dir "$J" || die "J=$J is not a private directory of this user (another user owns or can write to it, or to a directory above it); use another J"
J=$(cd "$J" && pwd -P); BIN=$J/bin; RUN=$J/run; ANET=$BIN/anet
_own_path "$J" >/dev/null || die "J=$J is not a directory this script may own (pick something like /tmp/joint-a2a-x)"
if [ ! -e "$J/.joint-a2a-dir" ] && [ -n "$(ls -A "$J" 2>/dev/null)" ]; then
  die "$J is not empty and was not made by joint-a2a.sh (no .joint-a2a-dir); use an empty or new J"
fi
: > "$J/.joint-a2a-dir"
# The lock is fd 9, closed in every process started below (9>&-).
if command -v flock >/dev/null; then
  exec 9>"$J/.joint-a2a-lock"
  flock -n 9 || die "another joint-a2a.sh run is using $J"
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

home_of(){ case $1 in req) echo "$REQ" ;; prov) echo "$PROV" ;; esac; }
addr_of(){ case $1 in req) echo "$RC" ;; prov) echo "$PC" ;; esac; }
# ctl <node> <path> <json> — a node's control API; the token goes to curl through a file descriptor.
ctl(){
  local tok; tok=$(cat "$(home_of "$1")/.anet/control_token.txt" 2>/dev/null)
  curl -s -m 30 -H @<(printf 'Authorization: Bearer %s\n' "$tok") -H 'Content-Type: application/json' \
       -d "$3" "http://$(addr_of "$1")$2"
}
rc(){ ctl req "$@"; }
pc(){ ctl prov "$@"; }

up(){   curl -sf -m 2 "http://$1/ping" >/dev/null 2>&1; }
wait_up(){   local i; for ((i = 0; i < ${2:-20} * 4; i++)); do up "$1" && return 0; sleep 0.25; done; return 1; }
wait_down(){ local i; for ((i = 0; i < ${2:-20} * 4; i++)); do up "$1" || return 0; sleep 0.25; done; return 1; }

# start_node <node> <log> — a daemon with its own HOME and a private XDG_RUNTIME_DIR (no pointer of this
# user's real daemon is touched). Under umask 022, a user's usual one, not this script's 077: the modes the
# daemon gives its own files are then its doing (the a2a_token.txt check below), and all of it is inside
# $J, which no one else can enter.
declare -A NODE_PID=()
start_node(){
  ( cd "$RUN" && umask 022 && exec setsid env -u ANET_DATA_DIR -u ANET_HOME -u ANET_ID \
      HOME="$(home_of "$1")" XDG_RUNTIME_DIR="$RUN/xdg" "$BIN/anet" daemon ) >"$2" 2>&1 </dev/null 9>&- &
  NODE_PID[$1]=$!
}
# stop_node <node> — shutdown through the control API; wait for the port, then the process.
stop_node(){
  local i p=${NODE_PID[$1]:-}
  ctl "$1" /shutdown '{}' >/dev/null
  wait_down "$(addr_of "$1")" 15 || return 1
  [ -n "$p" ] || return 0
  for ((i = 0; i < 60; i++)); do kill -0 "$p" 2>/dev/null || return 0; sleep 0.25; done
  return 1
}

# report <file> — the probe's lines as this script's checks: PASS/FAIL/NOTE, anything else indented.
report(){
  local line
  while IFS= read -r line; do
    case "$line" in
      "PASS "*) ok "${line#PASS }" ;;
      "FAIL "*) no "${line#FAIL }" ;;
      "NOTE "*) note "${line#NOTE }" ;;
      "") ;;
      *) printf '    %s\n' "$line" ;;
    esac
  done < "$1"
}

# probe_ran <exit status> <what> — the probe's own verdict is in its lines; a status other than 0 (all
# passed) or 1 (a check failed) means it did not get through: it could not run (2), or it died.
probe_ran(){ case "$1" in 0|1) ;; *) no "$2 did not finish (exit $1)" ;; esac; }

# sha <file> — its sha256, or "missing".
sha(){ if [ -f "$1" ]; then sha256sum "$1" | cut -d' ' -f1; else echo missing; fi; }
mode(){ stat -c %a "$1" 2>/dev/null; }

hd "0/6  binaries, ports, the hub"
if [ -n "${JOINT_BIN:-}" ]; then
  SRC=$(cd "$JOINT_BIN" 2>/dev/null && pwd -P) || die "JOINT_BIN=$JOINT_BIN is not a directory"
  for b in anet anet-hub a2aprobe; do
    [ -x "$SRC/$b" ] || die "JOINT_BIN has no $b (scripts/testnet/build.sh builds it)"
  done
  case "$SRC" in "$BIN"|"$BIN"/*|"$RUN"|"$RUN"/*) die "JOINT_BIN must not be under $BIN or $RUN: both are replaced" ;; esac
fi
if [ "${JOINT_A2A_ALLOC:-0}" = 1 ] && [[ "${JOINT_PORT_BASE:-}" =~ ^[0-9]+$ ]] \
   && [ "$JOINT_PORT_BASE" -ge 47100 ] && [ "$JOINT_PORT_BASE" -le 47499 ]; then
  die "JOINT_A2A_ALLOC=1 with a JOINT_PORT_BASE of the test network: on a test host the scan from 43811 can take a port a production daemon recorded for its own A2A interface; leave JOINT_A2A_ALLOC unset there"
fi
stop_under "$BIN" 10
rm -rf -- "$BIN" "$RUN"
mkdir -p "$BIN" "$RUN"

if [ -n "${JOINT_BIN:-}" ]; then
  for b in anet anet-hub a2aprobe a2ashape-hermes.test; do
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
  go build -C "$ROOT" -o "$BIN/anet" ./cmd/anet                 || die "build anet failed"
  go build -C "$ROOT" -o "$BIN/a2aprobe" ./tools/a2aprobe       || die "build a2aprobe failed"
  go build -C "$HUB_SRC" -o "$BIN/anet-hub" ./cmd/anet-hub      || die "build anet-hub failed"
  # The Hermes contract, as a binary: it reads the record the probe writes (section 5).
  go test -C "$ROOT" -c -o "$BIN/a2ashape-hermes.test" ./internal/a2ashape || die "build the a2ashape test binary failed"
fi
PROBE=$BIN/a2aprobe

PORT_BASE=$(port_block "${JOINT_PORT_BASE:-}" 10) || die "no ports"   # lib.sh
HUB_ADDR=127.0.0.1:$PORT_BASE; HUB_URL=http://$HUB_ADDR
RC=127.0.0.1:$((PORT_BASE + 1)); PC=127.0.0.1:$((PORT_BASE + 2))
REQ_A2A_PIN=127.0.0.1:$((PORT_BASE + 3)); PROV_A2A_PIN=127.0.0.1:$((PORT_BASE + 4))
BACKEND=127.0.0.1:$((PORT_BASE + 5))
REQ=$RUN/req; PROV=$RUN/prov
NONCE=joint$(python3 -c 'import secrets;print(secrets.token_hex(10))')
echo "  ports:    $PORT_BASE-$((PORT_BASE + 9))   work dir: $J"

( cd "$RUN" && exec setsid "$BIN/anet-hub" --addr "$HUB_ADDR" --data "$RUN/hub" ) >"$RUN/hub.log" 2>&1 </dev/null 9>&- &
for _ in $(seq 1 40); do curl -sf -m 2 "$HUB_URL/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
curl -sf -m 5 "$HUB_URL/healthz" >/dev/null && ok "hub up on an empty data directory" \
  || { no "hub down: $(tail -2 "$RUN/hub.log")"; exit 1; }

hd "1/6  a requester that pays within its agent tier, a provider with priced public skills"
# The limits the probe's payment cases are built on: PAID within the agent tier, PRICEY above it.
PAID=joint.digest.paid; PRICEY=joint.digest.pricey
PRICE_PAID=5; PRICE_PRICEY=15; AGENT_MAX=10; AGENT_DAILY_MAX=20
[ "$PRICE_PAID" -le "$AGENT_MAX" ] && [ "$PRICE_PRICEY" -gt "$AGENT_MAX" ] || die "prices and limits do not frame the cases"
mkdir -p "$REQ/.anet" "$PROV/.anet"
mkdir -p -m 700 "$RUN/xdg"
# The requester: nothing automatic (auto_max 0, the default), the agent tier — the tier of a local A2A
# client (§8.6) — open to PRICE_PAID, the provider on the payee list (written below, once its AID exists).
python3 - "$REQ/.anet/config.json" "$RC" "$AGENT_MAX" "$AGENT_DAILY_MAX" <<'PY'
import json, sys
path, addr, amax, adaily = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
json.dump({"control_addr": addr,
           "payments": {"auto_max": 0, "agent_max": amax, "agent_daily_max": adaily,
                        "explicit_max": 10, "daily_max": 50, "payees_file": "payees.allow"}},
          open(path, "w"), indent=1)
PY
# The provider: the closed default (text tasks only from peers.allow), two priced capabilities behind the
# backend, both public (public_capabilities), so they are on its network card. The daemon proves itself
# to the backend with a token of their own (token_file, 0600).
python3 -c 'import secrets;print(secrets.token_hex(32))' > "$RUN/backend.token"
python3 - "$PROV/.anet/config.json" "$PC" "$BACKEND" "$PAID" "$PRICE_PAID" "$PRICEY" "$PRICE_PRICEY" "$RUN/backend.token" <<'PY'
import json, sys
path, addr, backend, paid, ppaid, pricey, ppricey, token = sys.argv[1:9]
caps = [
    {"id": paid, "url": "http://%s/paid" % backend, "price": int(ppaid),
     "name": "Digest", "description": "SHA-256 of the text you send, %s credits" % ppaid, "tags": ["joint", "digest"]},
    {"id": pricey, "url": "http://%s/pricey" % backend, "price": int(ppricey),
     "name": "Digest, dearer", "description": "SHA-256 of the text you send, %s credits" % ppricey, "tags": ["joint", "digest"]},
]
json.dump({"control_addr": addr,
           "modules": {"service": {"capabilities": caps, "token_file": token, "allow_tcp": True}},
           "inbound": {"policy": "closed", "public_capabilities": [{"id": paid}, {"id": pricey}]}},
          open(path, "w"), indent=1)
PY
# Each local A2A interface on a port of this block: written where the module keeps its address
# (<data dir>/modules/a2a/a2a_addr.txt, §11.1), which it binds again on every start. JOINT_A2A_ALLOC=1
# leaves the requester to choose its own. The directories are made 0755 on purpose: the daemon must
# narrow its module state directory to 0700 itself, and the check below sees whether it did.
pin_a2a(){
  mkdir -p "$1/.anet/modules/a2a" && chmod 755 "$1/.anet/modules" "$1/.anet/modules/a2a" \
    && printf '%s\n' "$2" > "$1/.anet/modules/a2a/a2a_addr.txt" || die "cannot write $1/.anet/modules/a2a/a2a_addr.txt"
}
[ "${JOINT_A2A_ALLOC:-0}" = 1 ] || pin_a2a "$REQ" "$REQ_A2A_PIN"
pin_a2a "$PROV" "$PROV_A2A_PIN"

( cd "$RUN" && exec setsid "$PROBE" backend --addr "$BACKEND" --token-file "$RUN/backend.token" ) \
  >"$RUN/backend.log" 2>&1 </dev/null 9>&- &
for n in req prov; do start_node $n "$RUN/$n.log"; done
wait_up "$RC" 30 && ok "requester up" || { no "requester down: $(tail -3 "$RUN/req.log")"; exit 1; }
wait_up "$PC" 30 && ok "provider up"  || { no "provider down: $(tail -3 "$RUN/prov.log")"; exit 1; }
REQ_AID=$(rc /status '{}' | jget aid); PROV_AID=$(pc /status '{}' | jget aid)
[ -n "$REQ_AID" ] && [ -n "$PROV_AID" ] || { no "no AIDs: req=$REQ_AID prov=$PROV_AID"; exit 1; }
REG_R=$(rc /hub-register "{\"hub\":\"$HUB_URL\",\"name\":\"JointA2ARequester\"}" | jget status)
REG_P=$(pc /hub-register "{\"hub\":\"$HUB_URL\",\"name\":\"JointA2AProvider\"}" | jget status)
[ "$REG_R" = registered ] && [ "$REG_P" = registered ] && ok "both registered at the hub" \
  || { no "registration failed: req=$REG_R prov=$REG_P"; exit 1; }
# Written directly (the CLI asks on a terminal); both files are read on every decision. payees.allow has
# the format of the peer lists, so lib.sh's writer for them serves.
peer_allow "$PROV/.anet" "$REQ_AID"
_peer_add "$REQ/.anet/payees.allow" "$PROV_AID"
printf '  requester %s\n  provider  %s\n' "$REQ_AID" "$PROV_AID"

A2A_DIR=$REQ/.anet/modules/a2a
REQ_A2A=""
for _ in $(seq 1 20); do REQ_A2A=$(tr -d '[:space:]' < "$A2A_DIR/a2a_addr.txt" 2>/dev/null); [ -n "$REQ_A2A" ] && [ -f "$A2A_DIR/a2a_token.txt" ] && break; sleep 0.25; done
if [ "${JOINT_A2A_ALLOC:-0}" = 1 ]; then
  [ -n "$REQ_A2A" ] && ok "the requester's A2A interface chose $REQ_A2A and recorded it" || no "no a2a_addr.txt in $A2A_DIR"
else
  [ "$REQ_A2A" = "$REQ_A2A_PIN" ] && ok "the requester's A2A interface is on $REQ_A2A, from a2a_addr.txt" \
    || no "a2a_addr.txt says '$REQ_A2A', not $REQ_A2A_PIN"
fi
[ "$(mode "$A2A_DIR/a2a_token.txt")" = 600 ] && [ "$(mode "$A2A_DIR")" = 700 ] \
  && ok "a2a_token.txt is 0600 in a 0700 directory (the daemon's doing: it runs under umask 022)" \
  || no "token $(mode "$A2A_DIR/a2a_token.txt"), directory $(mode "$A2A_DIR")"
[ -n "$REQ_A2A" ] || exit 1
ADDR_SHA=$(sha "$A2A_DIR/a2a_addr.txt"); TOKEN_SHA=$(sha "$A2A_DIR/a2a_token.txt")

# Each credential only where it belongs (X5), on the real listeners: the A2A token is not the control
# token and opens nothing on the control plane, and the other way round; and the interface answers only
# to a loopback Host (421) and never to a web page (403) (SI-7). Tokens go to curl through a descriptor.
status_with(){ # status_with <token-file> <url> [curl args…] — the HTTP status of a GET with that bearer
  local f=$1 u=$2; shift 2
  curl -s -m 10 -o /dev/null -w '%{http_code}' -H @<(printf 'Authorization: Bearer %s\n' "$(cat "$f" 2>/dev/null)") "$@" "$u"
}
A2A_TOK=$A2A_DIR/a2a_token.txt; CTL_TOK=$REQ/.anet/control_token.txt
A2A_CARD=http://$REQ_A2A/a2a/v1/agents/$PROV_AID/.well-known/agent-card.json
c_ok=$(status_with "$A2A_TOK" "$A2A_CARD"); c_ctl=$(status_with "$CTL_TOK" "$A2A_CARD")
p_a2a=$(status_with "$A2A_TOK" "http://$RC/status")
[ "$c_ok" = 200 ] && [ "$c_ctl" = 401 ] && [ "$p_a2a" = 401 ] \
  && ok "the A2A token opens the interface and not the control plane; the control token does not open the interface" \
  || no "card with the A2A token $c_ok (want 200), with the control token $c_ctl (want 401); control /status with the A2A token $p_a2a (want 401)"
c_host=$(status_with "$A2A_TOK" "$A2A_CARD" -H "Host: example.com:${REQ_A2A##*:}")
c_origin=$(status_with "$A2A_TOK" "$A2A_CARD" -H 'Origin: http://example.com')
[ "$c_host" = 421 ] && [ "$c_origin" = 403 ] \
  && ok "the interface refuses a foreign Host (421) and a web page's Origin (403), token or not" \
  || no "foreign Host $c_host (want 421), Origin $c_origin (want 403)"

# The provider's agent: answers every text task "echo: <text>" (not those saying [hold]).
( cd "$RUN" && exec setsid "$PROBE" responder --ctl "$PC" --token-file "$PROV/.anet/control_token.txt" ) \
  >"$RUN/responder.log" 2>&1 </dev/null 9>&- &

# Does this hub publish A2A network cards (§10.5)? If it does, the provider's must be there and verify.
REGISTRY=""
code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' "$HUB_URL/a2a/v1/agents")
if [ "$code" = 200 ]; then
  for _ in $(seq 1 20); do
    [ "$(curl -s -m 5 -o /dev/null -w '%{http_code}' "$HUB_URL/a2a/v1/agents/$PROV_AID/card")" = 200 ] && { REGISTRY=1; break; }
    sleep 0.5
  done
  [ -n "$REGISTRY" ] && ok "the hub publishes the provider's network card" \
    || no "the hub has the A2A registry but no card for the provider (public skills: $PAID, $PRICEY)"
else
  note "the hub has no A2A registry (GET /a2a/v1/agents: $code): proxy cards are made from the AID, UNVERIFIED"
fi
BAL_R0=$(rc /balance '{}' | jget balance); BAL_P0=$(pc /balance '{}' | jget balance)

hd "2/6  a2a-go's client → the requester's A2A interface → hub → provider"
"$PROBE" run --a2a-addr "$REQ_A2A" --token-file "$A2A_DIR/a2a_token.txt" --agent "$PROV_AID" --nonce "$NONCE" \
  --paid "$PAID" --pricey "$PRICEY" ${REGISTRY:+--registry} \
  --client-out "$RUN/a2a-client.json" --state "$RUN/probe-state.json" --record "$RUN/hermes-record.json" \
  --timeout "${JOINT_TIMEOUT:-90s}" >"$RUN/probe-run.txt" 2>&1 9>&-
prc=$?
report "$RUN/probe-run.txt"
probe_ran "$prc" "the probe (see $RUN/probe-run.txt)"

hd "3/6  what the client cannot see: the provider, the backend, the money, the hub"
state_of(){ jget "$1" < "$RUN/probe-state.json" 2>/dev/null; }
CANCEL=$(state_of cancel_task)
if [ -n "$CANCEL" ]; then
  st=""
  for _ in $(seq 1 60); do
    st=$(pc /tasks/get "{\"task_id\":\"$CANCEL\"}" | jget status state)
    [ "$st" = TASK_STATE_CANCELED ] && break
    sleep 0.5
  done
  [ "$st" = TASK_STATE_CANCELED ] && ok "the provider's side of the canceled task is canceled too" \
    || no "the provider's side of the canceled task is '$st'"
else
  no "the probe made no task to cancel"
fi
# The quote the client declined (payment-rejected): canceled at the provider too (§8.3).
OVER=$(state_of over_task)
if [ -n "$OVER" ]; then
  st=""
  for _ in $(seq 1 60); do
    st=$(pc /tasks/get "{\"task_id\":\"$OVER\"}" | jget status state)
    [ "$st" = TASK_STATE_CANCELED ] && break
    sleep 0.5
  done
  [ "$st" = TASK_STATE_CANCELED ] && ok "the provider's side of the declined quote is canceled too" \
    || no "the provider's side of the declined quote is '$st'"
else
  no "the probe made no quote above the agent tier"
fi
code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d '{"text":"x"}' "http://$BACKEND/paid")
[ "$code" = 401 ] && ok "the backend refuses a caller without the daemon's token" || no "the backend answered $code without a token"
CALLS=$(curl -s -m 5 "http://$BACKEND/calls")
n_paid=$(printf '%s' "$CALLS" | jget "$PAID"); n_pricey=$(printf '%s' "$CALLS" | jget "$PRICEY")
[ "${n_paid:-0}" = 1 ] && [ "${n_pricey:-0}" = 0 ] \
  && ok "the backend ran the paid skill once and the unpaid one never" || no "backend calls: $CALLS"
auth=$(rc /evidence '{"event_type":"anet.payment.authorized","limit":100}' | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = None
if not isinstance(d, dict) or not isinstance(d.get("records"), list):
    print("unreadable"); sys.exit()
print(" ".join(str((r.get("payload") or {}).get("purpose")) for r in d["records"]) or "none")')
[ "$auth" = task-agent ] && ok "the requester signed one authorization, at the agent tier (task-agent)" \
  || no "the requester's anet.payment.authorized purposes: $auth (want exactly one task-agent)"
BAL_R1=$(rc /balance '{}' | jget balance); BAL_P1=$(pc /balance '{}' | jget balance)
moved=$(python3 - "$BAL_R0" "$BAL_R1" "$BAL_P0" "$BAL_P1" "$PRICE_PAID" <<'PY'
import sys
try:
    r0, r1, p0, p1, price = (float(x) for x in sys.argv[1:6])
except ValueError:
    print("unreadable"); sys.exit()
print("yes" if r0 - r1 == price and p1 - p0 == price else "no")
PY
)
case "$moved" in
  yes) ok "at the hub, $PRICE_PAID credits moved from the requester to the provider, once" ;;
  no)  no "balances: requester $BAL_R0 → $BAL_R1, provider $BAL_P0 → $BAL_P1 (want ∓$PRICE_PAID)" ;;
  *)   no "balances unreadable: requester '$BAL_R0' → '$BAL_R1', provider '$BAL_P0' → '$BAL_P1'" ;;
esac
# The hub carried every message of this run and holds none of their text (SI-1): the nonce is in every
# text and every capability argument the probe sent. The data directory is read raw, WAL included.
# Run again after the restart, whose messages carry the nonce as well.
hub_holds_no_text(){
  local hits; hits=$(grep -rlaF -- "$NONCE" "$RUN/hub" "$RUN/hub.log" 2>/dev/null | wc -l)
  [ "$hits" = 0 ] && ok "the hub's data directory and log hold none of the text sent$1" \
    || no "the text sent is in $hits file(s) of the hub$1: $(grep -rlaF -- "$NONCE" "$RUN/hub" "$RUN/hub.log" 2>/dev/null | tr '\n' ' ')"
}
hub_holds_no_text ""
got=$(grep -rlaF -- "$NONCE" "$PROV/.anet" 2>/dev/null | wc -l)
[ "$got" -gt 0 ] && ok "and the provider has it (the search finds what is there)" || no "the text is nowhere at the provider either: the search proves nothing"

hd "4/6  the requester restarts: the client's configuration still works"
if stop_node req; then
  ok "requester stopped"
  start_node req "$RUN/req-2.log"
  wait_up "$RC" 30 && ok "requester up again" || no "requester did not come back: $(tail -3 "$RUN/req-2.log")"
  # The control plane answers before every module has started: wait for the interface itself.
  for _ in $(seq 1 80); do [ -f "$A2A_DIR/a2a_addr.txt" ] && curl -s -m 2 -o /dev/null "http://$REQ_A2A/" && break; sleep 0.25; done
  [ "$(sha "$A2A_DIR/a2a_addr.txt")" = "$ADDR_SHA" ] && [ "$(sha "$A2A_DIR/a2a_token.txt")" = "$TOKEN_SHA" ] \
    && ok "a2a_addr.txt and a2a_token.txt are what they were" || no "a2a_addr.txt or a2a_token.txt changed across the restart"
  "$PROBE" again --client "$RUN/a2a-client.json" --state "$RUN/probe-state.json" --record "$RUN/hermes-record.json" \
    --timeout "${JOINT_TIMEOUT:-90s}" >"$RUN/probe-again.txt" 2>&1 9>&-
  arc=$?
  report "$RUN/probe-again.txt"
  probe_ran "$arc" "the probe after the restart (see $RUN/probe-again.txt)"
  hub_holds_no_text ", after the restart as well"
else
  no "the requester did not stop"
fi

hd "5/6  Hermes' A2A client (the port) reads the recorded responses"
REC=$RUN/hermes-record.json
if [ ! -s "$REC" ]; then
  no "the probe recorded no response"
else
  hrc=3
  if [ -x "$BIN/a2ashape-hermes.test" ]; then
    ( cd "$RUN" && ANET_HERMES_JOINT="$REC" "$BIN/a2ashape-hermes.test" -test.run '^TestHermesReadsJointRecord$' \
        -test.v -test.count=1 ) >"$RUN/hermes.txt" 2>&1 9>&-
    hrc=$?
  elif command -v go >/dev/null && [ -d "$ROOT/internal/a2ashape" ]; then
    ANET_HERMES_JOINT="$REC" go test -C "$ROOT" ./internal/a2ashape -run '^TestHermesReadsJointRecord$' -count=1 -v \
      >"$RUN/hermes.txt" 2>&1 9>&-
    hrc=$?
  fi
  if [ "$hrc" = 3 ]; then
    note "no a2ashape-hermes.test in JOINT_BIN and no go here: the Hermes contract was not run on $REC"
  else
    while IFS= read -r line; do
      case "$line" in
        *"--- PASS: TestHermesReadsJointRecord/"*) ok "Hermes reads ${line##*TestHermesReadsJointRecord/}" ;;
        *"--- FAIL: TestHermesReadsJointRecord/"*) no "Hermes reads ${line##*TestHermesReadsJointRecord/}" ;;
        *"--- SKIP: TestHermesReadsJointRecord"*) no "the Hermes contract skipped (ANET_HERMES_JOINT not seen)" ;;
        *"gap"*) note "Hermes: ${line#*: }" ;;
      esac
    done < "$RUN/hermes.txt"
    [ "$hrc" = 0 ] || grep -q -- '--- FAIL' "$RUN/hermes.txt" || no "the Hermes contract did not run: $(tail -3 "$RUN/hermes.txt")"
    # The acceptance item itself (U1): the answer of a blocking SendMessage, read by the port. A binary
    # without the test, or a record without the case, runs nothing and exits 0 — that is not a pass.
    grep -q -- '--- PASS: TestHermesReadsJointRecord/text-blocking (' "$RUN/hermes.txt" \
      || no "the Hermes contract did not read the blocking SendMessage answer (no passed text-blocking case in $RUN/hermes.txt)"
    echo "    what Hermes' model reads, case by case: $RUN/hermes.txt"
  fi
fi

hd "6/6  a2a-tck (a record, not a gate)"
if [ "${JOINT_A2A_TCK:-0}" = 1 ]; then
  label="joint-a2a $(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  bash "$SCRIPTS/a2a-tck.sh" --aid "$PROV_AID" --data-dir "$REQ/.anet" --out "$RUN/tck" --label "$label" \
    --timeout "${JOINT_A2A_TCK_TIMEOUT:-1800}" >"$RUN/tck.log" 2>&1 9>&-
  trc=$?
  if [ "$trc" = 0 ] && [ -f "$RUN/tck/summary.md" ]; then
    note "a2a-tck ran; record in $RUN/tck (summary.md, run.json, reports/) — for docs/notes, not a gate"
    sed -n '1,25p' "$RUN/tck/summary.md" | sed 's/^/    /'
  else
    note "a2a-tck did not run (exit $trc, see $RUN/tck.log); not a gate"
  fi
else
  note "skipped (JOINT_A2A_TCK=1 runs scripts/a2a-tck.sh here and keeps its record)"
fi

hd "result"
printf '  %d passed, %d failed   (logs: %s)\n' "$pass" "$fail" "$RUN"
[ "$fail" = 0 ]
