#!/usr/bin/env bash
# joint-admission.sh — joining a hub that admits by AID, across the two repositories (v0.3.0
# LEC J5, J6).
#
# The hub's own tests call its store and its handler; the daemon's tests call a fake hub. Neither can
# show that `anet hub-register` on a node and `anet-hub admit` on the hub's machine fit together: the
# node asks check_only, waits for admission in the background with the admit line to send, finishes on
# its own once the operator admits its AID, solves the proof of work of open registration, and says
# why at once when the address is wrong. There is no invitation anywhere: ANET_INVITE is ignored with
# one line and --token is refused before anything runs.
#
# Every hub-register here runs without a controlling terminal (setsid, stdin /dev/null), the way an
# agent runs it: it joins all the same and says first that the identity has no recovery
# passphrase yet.
#
#   JOINT_BIN        directory with prebuilt anet and anet-hub (scripts/testnet/build.sh); copied into
#                    $J, nothing is built and go is not needed. Unset: built from this checkout.
#   JOINT_PORT_BASE  the hub on 127.0.0.1:BASE, the nodes' control ports BASE+1..BASE+4 and their local
#                    A2A interfaces BASE+5..BASE+8 (on the test hosts inside 47100-47499,
#                    an internal note). Unset: the hub on 29288 and the nodes wherever the daemon
#                    allocates.
#   J                work directory (default /tmp/joint-admission); deleted and rebuilt, so it has to be
#                    a private directory of this user (lib.sh own_dir).
#
# The nodes get a private XDG_RUNTIME_DIR under $J, and everything is stopped by path at the end
# (lib.sh stop_under), never by process name.
set -uo pipefail
umask 077
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
export LC_ALL=C LANG=C
cd "$(dirname "$0")/.."
ROOT=$PWD
J=${J:-/tmp/joint-admission}
HUB_SRC=${HUB_SRC:-$ROOT/../ANetHub}
if [ -n "${JOINT_PORT_BASE:-}" ]; then
  [[ "$JOINT_PORT_BASE" =~ ^[0-9]+$ ]] && [ "$JOINT_PORT_BASE" -ge 1024 ] && [ "$JOINT_PORT_BASE" -le 65523 ] \
    || { echo "JOINT_PORT_BASE=$JOINT_PORT_BASE is not a port"; exit 2; }
  HUB=127.0.0.1:$JOINT_PORT_BASE
else
  HUB=127.0.0.1:29288
fi
# lib.sh for own_dir and stop_under. ANET is set first so sourcing it looks nothing up.
ANET=$J/anet
# shellcheck source=lib.sh
. "$ROOT/scripts/lib.sh"

pass=0; fail=0
ok(){ printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; pass=$((pass+1)); }
no(){ printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; fail=$((fail+1)); }
hd(){ printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }

KIDS=()
cleanup(){
  local p
  for p in "${KIDS[@]:-}"; do [ -n "$p" ] && kill -TERM "$p" 2>/dev/null; done
  sleep 1
  for p in "${KIDS[@]:-}"; do [ -n "$p" ] && kill -KILL "$p" 2>/dev/null; done
  # The nodes `anet up` detached: by the path they run from (lib.sh), not pkill.
  stop_under "$J" 5
}
trap cleanup EXIT INT TERM

hd "0/7  build and start a hub (a new hub admits nobody: closed)"
jdir(){ _own_path "$J" >/dev/null && own_dir "$J" || { echo "J=$J is not a private directory of this user (or is /, \$HOME…); use another J"; exit 2; }; }
jdir
stop_under "$J" 5   # leftovers of a run that was killed before its cleanup hold the ports
rm -rf "$J"; jdir; mkdir -p "$J/run"
mkdir -p -m 0700 "$J/xdg" && export XDG_RUNTIME_DIR=$J/xdg || exit 1
unset ANET_DATA_DIR ANET_HOME ANET_ID ANET_INVITE
if [ -n "${JOINT_BIN:-}" ]; then
  for b in anet anet-hub; do
    [ -x "$JOINT_BIN/$b" ] || { echo "JOINT_BIN has no $b"; exit 1; }
    cp "$JOINT_BIN/$b" "$J/$b" || { echo "cannot copy $JOINT_BIN/$b"; exit 1; }
  done
else
( cd "$ROOT" && go build -o "$J/anet" ./cmd/anet ) || exit 1
[ -d "$HUB_SRC" ] || { echo "ANetHub not found at $HUB_SRC"; exit 1; }
( cd "$HUB_SRC" && go build -o "$J/anet-hub" ./cmd/anet-hub ) || exit 1
fi

"$J/anet-hub" --addr "$HUB" --data "$J/run/hub" >"$J/run/hub.log" 2>&1 & KIDS+=($!)
sleep 2
curl -sf -m 5 "http://$HUB/healthz" >/dev/null || { echo "hub down: $(tail -3 "$J/run/hub.log")"; exit 1; }
ok "hub up"

# hub-side operator commands run against the SAME data dir while it serves.
hubop(){ "$J/anet-hub" --data "$J/run/hub" "$@"; }
hubop admission | grep -qi 'admission list only' && ok "a fresh hub admits only the AIDs its operator lists" || no "a fresh hub should be closed: $(hubop admission)"

# start <n>: a separate identity, started and ready.
start(){
  local n=$1 home=$J/run/$1
  mkdir -p "$home"
  if [ -n "${JOINT_PORT_BASE:-}" ] && [ ! -f "$home/config.json" ]; then
    local off
    case "$n" in first) off=1 ;; open) off=2 ;; stray) off=3 ;; token) off=4 ;;
                 *) echo "node $n has no port in the block"; return 1 ;; esac
    printf '{"control_addr":"127.0.0.1:%s"}\n' $((JOINT_PORT_BASE + off)) > "$home/config.json"
    pin_a2a "$home" $((JOINT_PORT_BASE + 4 + off))
  fi
  ANET_DATA_DIR=$home "$J/anet" up >/dev/null 2>&1
  for _ in $(seq 1 15); do ANET_DATA_DIR=$home "$J/anet" status >/dev/null 2>&1 && break; sleep 1; done
}
# join <n> <name> [args…]: hub-register as an agent runs it, without a terminal. Sets OUT and CODE.
join(){
  local n=$1 name=$2; shift 2
  OUT=$(ANET_DATA_DIR=$J/run/$n setsid -w "$J/anet" hub-register "http://$HUB" --name "$name" "$@" </dev/null 2>&1)
  CODE=$?
}
aidof(){ ANET_DATA_DIR=$J/run/$1 "$J/anet" status --json 2>/dev/null | grep -o '"aid": *"bafyrei[a-z0-9]*"' | head -1 | grep -o 'bafyrei[a-z0-9]*'; }
hubstate(){ ANET_DATA_DIR=$J/run/$1 "$J/anet" status --json 2>/dev/null | tr -d '\n' | grep -o '"hub": *{[^}]*"state": *"[a-z_]*"' | grep -o '"state": *"[a-z_]*"' | tail -1 | grep -o '[a-z_]*"$' | tr -d '"'; }
waitstate(){ # waitstate <n> <state> <seconds>
  for _ in $(seq 1 "$3"); do [ "$(hubstate "$1")" = "$2" ] && return 0; sleep 1; done; return 1
}
stopnode(){ ANET_DATA_DIR=$J/run/$1 "$J/anet" stop >/dev/null 2>&1; }

hd "1/7  a new node joins a closed hub without a terminal: the reminder, then the admit line (J5 b)"
start first
FIRST=$(aidof first)
join first First
LAST=$(printf '%s\n' "$OUT" | tail -1)
FIRSTLINE=$(printf '%s\n' "$OUT" | head -1)
[ "$CODE" = 0 ] && ok "exit 0 while waiting for admission" || no "exit $CODE: $OUT"
case "$FIRSTLINE" in *"anet identity protect"*) ok "first, the identity has no recovery passphrase yet" ;; *) no "first line: $FIRSTLINE" ;; esac
[ "$LAST" = "  → anet-hub admit $FIRST" ] && ok "last, the admit line with the full AID" || no "last line: $LAST"
[ "$(hubstate first)" = pending_admission ] && ok "hub.state pending_admission" || no "hub.state $(hubstate first)"

hd "2/7  sending while waiting fails at once with the same sentence, nothing queued"
OTHER=bafyreibpaki5kknsoz7hnadmrkaczos4zyz3qyceeee7e7mandmrv7ljpe
SOUT=$(ANET_DATA_DIR=$J/run/first "$J/anet" delegate "$OTHER" "hello" 2>&1); SCODE=$?
[ "$SCODE" != 0 ] && echo "$SOUT" | grep -q "anet-hub admit $FIRST" && ok "refused with the admit line" || no "send while pending ($SCODE): $SOUT"

hd "3/7  the operator admits the AID; the node finishes on its own"
hubop admit "$FIRST" >/dev/null && ok "admitted" || no "anet-hub admit failed"
if waitstate first registered 60; then ok "registered without running anything again"; else no "still $(hubstate first) after 60 s"; fi

hd "4/7  no invitations: ANET_INVITE is ignored with one line, --token is refused before anything runs"
start token
OUT=$(ANET_DATA_DIR=$J/run/token "$J/anet" hub-register "http://$HUB" --token anetinv_x </dev/null 2>&1); CODE=$?
[ "$CODE" = 2 ] && echo "$OUT" | grep -q "0.3.0" && ok "--token: removed, exit 2" || no "--token ($CODE): $OUT"
[ -z "$(hubstate token)" ] || [ "$(hubstate token)" = none ] && ok "nothing joined" || no "hub.state $(hubstate token)"
OUT=$(ANET_INVITE=anetinv_x ANET_DATA_DIR=$J/run/token setsid -w "$J/anet" hub-register "http://$HUB" --name Token </dev/null 2>&1); CODE=$?
echo "$OUT" | grep -q "ANET_INVITE" && ok "ANET_INVITE is named as ignored" || no "no line about ANET_INVITE: $OUT"
[ "$CODE" = 0 ] && ok "and the join went on (exit 0)" || no "exit $CODE"
grep -rq 'anetinv_x' "$J/run/token" "$J/run/hub.log" 2>/dev/null && no "the invite reached a file" || ok "the invite reached no file"
stopnode token

hd "5/7  open registration: the node solves the proof of work and is on probation (J6)"
hubop admission open | grep -qi open && ok "admission open" || no "could not open admission"
start open
join open Open
[ "$CODE" = 0 ] && echo "$OUT" | grep -q "probation" && ok "registered on probation" || no "open join ($CODE): $OUT"
[ "$(hubstate open)" = registered ] && ok "hub.state registered" || no "hub.state $(hubstate open)"

hd "6/7  an address with no hub is said at once, and nothing changes"
start stray
CFG=$(cat "$J/run/stray/config.json")
OUT=$(ANET_DATA_DIR=$J/run/stray setsid -w "$J/anet" hub-register http://127.0.0.1:9 </dev/null 2>&1); CODE=$?
[ "$CODE" = 1 ] && echo "$OUT" | grep -q "cannot be reached" && ok "hub_unreachable, exit 1" || no "unreachable ($CODE): $OUT"
[ "$CFG" = "$(cat "$J/run/stray/config.json")" ] && [ ! -e "$J/run/stray/hub_join.json" ] && ok "config.json and hub_join.json untouched" \
  || no "the stray address was written"
stopnode stray

hd "7/7  revoking the admission sends the node back to waiting for it"
hubop admission closed >/dev/null
hubop admissions revoke "$FIRST" >/dev/null && ok "revoked" || no "revoke failed"
stopnode first
start first
if waitstate first pending_admission 30; then ok "after a restart: pending_admission again"; else no "after revoke: $(hubstate first)"; fi

for n in first open; do stopnode $n; done
printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
