#!/usr/bin/env bash
# joint-invite.sh — admission control across the two repositories.
#
# The hub's own tests call its store and its handler; the daemon's tests
# call a fake hub. Neither can show that the token an operator mints with
# `anet-hub -invite-new` is the token `anet hub-register` sends (read from
# ANET_INVITE: never from the command line, F41),
# under the field name the hub reads. That is three processes and two
# repositories, and it is where a rename passes both suites and fails in
# production.
#
#   JOINT_BIN        directory with prebuilt anet and anet-hub (scripts/testnet/build.sh); copied into
#                    $J, nothing is built and go is not needed. Unset: built from this checkout.
#   JOINT_PORT_BASE  the hub on 127.0.0.1:BASE, the six nodes' control ports BASE+1..BASE+6 and their
#                    local A2A interfaces BASE+7..BASE+12 (on the test hosts inside 47100-47499,
#                    docs/notes/0015). Unset: the hub on the historical 29288 and the nodes wherever
#                    the daemon allocates (control from 39811 up, A2A from 43811 up).
#   J                work directory (default /tmp/joint-invite); deleted and rebuilt, so it has to be a
#                    private directory of this user (lib.sh own_dir).
#
# The nodes get a private XDG_RUNTIME_DIR under $J, and everything is stopped by path at the end
# (lib.sh stop_under), never by process name.
set -uo pipefail
umask 077
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
cd "$(dirname "$0")/.."
ROOT=$PWD
J=${J:-/tmp/joint-invite}
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

hd "0/6  build and start a hub that admits openly"
jdir(){ _own_path "$J" >/dev/null && own_dir "$J" || { echo "J=$J is not a private directory of this user (or is /, \$HOME…); use another J"; exit 2; }; }
jdir
stop_under "$J" 5   # leftovers of a run that was killed before its cleanup hold the ports
rm -rf "$J"; jdir; mkdir -p "$J/run"
# `anet up` writes the "current daemon" pointer and identity registry under XDG_RUNTIME_DIR: $J/xdg,
# not this user's.
mkdir -p -m 0700 "$J/xdg" && export XDG_RUNTIME_DIR=$J/xdg || exit 1
unset ANET_DATA_DIR ANET_HOME ANET_ID
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
hubop -invite-list | grep -q 'admission: OFF' && ok "a fresh hub admits openly" \
  || no "a fresh hub should admit openly"

# node <n> <name> [invite] — a separate identity per node. The invite goes in
# hub-register's environment, as install.sh passes it: never on a command line.
node(){
  local n=$1 name=$2 invite=${3:-}
  local home=$J/run/$n
  mkdir -p "$home"
  # With JOINT_PORT_BASE the control port is pinned inside the block too (otherwise the daemon picks
  # the first free one from 39811). Called in $(…), so the offset comes from the name, not a counter.
  if [ -n "${JOINT_PORT_BASE:-}" ] && [ ! -f "$home/config.json" ]; then
    local off
    case "$n" in early) off=1 ;; stranger) off=2 ;; invited) off=3 ;; second) off=4 ;; before) off=5 ;; after) off=6 ;;
                 *) echo "node $n has no port in the block"; return 1 ;; esac
    printf '{"control_addr":"127.0.0.1:%s"}\n' $((JOINT_PORT_BASE + off)) > "$home/config.json"
    pin_a2a "$home" $((JOINT_PORT_BASE + 6 + off))
  fi
  ANET_DATA_DIR=$home "$J/anet" up >/dev/null 2>&1
  for _ in $(seq 1 15); do ANET_DATA_DIR=$home "$J/anet" status >/dev/null 2>&1 && break; sleep 1; done
  ANET_INVITE=$invite ANET_DATA_DIR=$home "$J/anet" hub-register "http://$HUB" --name "$name" 2>&1
}
stopnode(){ ANET_DATA_DIR=$J/run/$1 "$J/anet" stop >/dev/null 2>&1; }
aidof(){ ANET_DATA_DIR=$J/run/$1 "$J/anet" status 2>/dev/null | grep -o 'bafyrei[a-z0-9]*' | head -1; }

hd "1/6  with admission off, a node joins with no token"
OUT=$(node early Early)
echo "$OUT" | grep -q '"status": *"registered"' && ok "joined without a token" || no "could not join: $OUT"
EARLY_AID=$(aidof early)

hd "2/6  turn admission on"
hubop -invite-required true | grep -q 'admission: ON' && ok "admission is on" || no "could not turn admission on"

hd "3/6  an uninvited node is refused, and says why"
OUT=$(node stranger Stranger)
if echo "$OUT" | grep -qi 'invite'; then ok "refused, and the reason names the invite"; else no "unhelpful refusal: $OUT"; fi
echo "$OUT" | grep -q '"status": *"registered"' && no "the uninvited node registered anyway" || ok "it did not register"
stopnode stranger

hd "4/6  the node already registered keeps registering, with no token"
OUT=$(ANET_DATA_DIR=$J/run/early "$J/anet" hub-register "http://$HUB" --name Early 2>&1)
echo "$OUT" | grep -q '"status": *"registered"' && ok "turning admission on did not lock out an existing node" \
  || no "an existing node was locked out: $(echo "$OUT" | tr -d "\n" | head -c 300)"

hd "5/6  a minted token admits exactly one node"
MINT=$(hubop -invite-new -label "one bench board" -invite-uses 1)
TOKEN=$(echo "$MINT" | grep -o 'anetinv_[a-z0-9]*' | head -1)
[ -n "$TOKEN" ] && ok "minted: ${TOKEN:0:16}…" || { no "could not mint: $MINT"; exit 1; }
echo "$MINT" | grep -q 'only time the token is shown' && ok "the mint output says the token is shown once" \
  || no "the mint output does not warn that the token is unrecoverable"

OUT=$(node invited Invited "$TOKEN")
echo "$OUT" | grep -q '"status": *"registered"' && ok "the invited node joined" || no "a valid token was refused: $OUT"
INVITED_AID=$(aidof invited)

OUT=$(node second Second "$TOKEN")
echo "$OUT" | grep -q '"status": *"registered"' && no "a second node reused a single-use token" \
  || ok "the token is used up and the second node is refused"
stopnode second

hubop -invite-list | grep -q "$INVITED_AID" && ok "the hub records which node came in on the token" \
  || no "the redeemer was not recorded"

hd "6/6  revoking closes the gate without evicting"
MINT2=$(hubop -invite-new -label "leaked" -invite-uses 0)
TOKEN2=$(echo "$MINT2" | grep -o 'anetinv_[a-z0-9]*' | head -1)
ID2=$(echo "$MINT2" | head -1 | awk '{print $2}')
OUT=$(node before Before "$TOKEN2")
echo "$OUT" | grep -q '"status": *"registered"' && ok "joined on the standing invite" || no "unlimited invite refused: $OUT"
hubop -invite-revoke "$ID2" >/dev/null
OUT=$(node after After "$TOKEN2")
echo "$OUT" | grep -q '"status": *"registered"' && no "a revoked token still admitted" || ok "the revoked token admits nobody new"
stopnode after
# GET /agents/{aid}, not the browsable listing: /agents lists only agents
# that advertise something (caps or a profile), so a node registered with
# neither is legitimately absent from it. Asking about the identity
# directly is the question this step actually has.
BEFORE_AID=$(aidof before)
if [ -z "$BEFORE_AID" ]; then
  no "could not read the AID of the node that joined before the revoke"
else
  CODE=$(curl -s -o "$J/run/before-agent.json" -w '%{http_code}' -m 10 "http://$HUB/agents/$BEFORE_AID")
  if [ "$CODE" = 200 ]; then
    ok "the node admitted before the revoke is still registered"
  else
    no "revoking evicted somebody: GET /agents/$BEFORE_AID → $CODE $(head -c 200 "$J/run/before-agent.json")"
  fi
fi

for n in early invited before; do stopnode $n; done
printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
