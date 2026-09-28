#!/usr/bin/env bash
# up.sh — a local network for the real-client interop runs (docs/notes/0035): a hub and three daemons on
# one loopback port block, left running for the clients in this directory to talk to.
#
#   requester  pays within its agent tier (agent_max 10); its local A2A interface is what the SDK clients,
#              Hermes and Claude Code (through `anet mcp`) use
#   provider   closed inbound policy, the requester and "other" on peers.allow; three public capabilities
#              behind a2aprobe's backend: FREE (price 0), PAID (1, within the agent tier), PRICEY (15, above);
#              text tasks answered by a2aprobe's responder ("echo: <text>"), or, after `up.sh backend`, by
#              the A2A server given as its §11.6 backend (the responder is then stopped)
#   other      a second requester, for the checks that need two peers (Q23 context isolation, an untrusted peer)
#
#   J=DIR INTEROP_BIN=DIR bash scripts/interop/up.sh start      start everything; writes $J/env.json
#   J=DIR bash scripts/interop/up.sh backend URL [TOKEN_FILE]    provider: modules.a2a.backends [{match:"*", url…}]
#   J=DIR bash scripts/interop/up.sh responder                   provider: back to the responder
#   J=DIR bash scripts/interop/up.sh trust AID… | untrust AID…   provider's peers.trust (read on every decision)
#   J=DIR bash scripts/interop/up.sh restart                     restart the three daemons on their data
#   J=DIR bash scripts/interop/up.sh stop                        stop what start started (by path)
#
# Ports (INTEROP_PORT_BASE, default 47400): +0 hub, +1 requester control, +2 provider control, +3 requester
# A2A, +4 provider A2A, +5 capability backend, +6 free for a §11.6 A2A backend, +7 other control,
# +8 other A2A, +9 spare. The A2A interfaces are pinned through a2a_addr.txt, as joint-a2a.sh does.
# INTEROP_BIN holds anet, anet-hub and a2aprobe (built here with go from this checkout when unset).
set -uo pipefail
umask 077
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
SCRIPTS=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ROOT=$(cd "$SCRIPTS/.." && pwd -P)
J=${J:?set J to a private work directory}
BIN=$J/bin; RUN=$J/run; ANET=$BIN/anet
# shellcheck source=../lib.sh
. "$SCRIPTS/lib.sh"
die(){ printf 'up.sh: %s\n' "$*" >&2; exit 2; }
say(){ printf '  %s\n' "$*"; }

FREE=interop.digest.free; PAID=interop.digest.paid; PRICEY=interop.digest.pricey
PRICE_PAID=1; PRICE_PRICEY=15; AGENT_MAX=10

jget(){ python3 -c '
import sys, json
try: v = json.load(sys.stdin)
except Exception: v = None
for k in sys.argv[1:]:
    v = v.get(k) if isinstance(v, dict) else None
print("" if v is None else v)' "$@"; }
envget(){ jget "$@" < "$J/env.json"; }
ctl(){ # ctl <home> <addr> <path> <json>
  local tok; tok=$(cat "$1/.anet/control_token.txt" 2>/dev/null)
  curl -s -m 30 -H @<(printf 'Authorization: Bearer %s\n' "$tok") -H 'Content-Type: application/json' -d "$4" "http://$2$3"
}
up(){ curl -sf -m 2 "http://$1/ping" >/dev/null 2>&1; }
wait_up(){ local i; for ((i = 0; i < ${2:-30} * 4; i++)); do up "$1" && return 0; sleep 0.25; done; return 1; }
wait_down(){ local i; for ((i = 0; i < ${2:-30} * 4; i++)); do up "$1" || return 0; sleep 0.25; done; return 1; }
start_node(){ # start_node <home> <log>
  ( cd "$RUN" && umask 022 && exec setsid env -u ANET_DATA_DIR -u ANET_HOME -u ANET_ID \
      HOME="$1" XDG_RUNTIME_DIR="$RUN/xdg" "$BIN/anet" daemon ) >>"$2" 2>&1 </dev/null &
}
pin_a2a(){ mkdir -p "$1/.anet/modules/a2a" && printf '%s\n' "$2" > "$1/.anet/modules/a2a/a2a_addr.txt"; }
wait_a2a(){ # wait_a2a <home> <addr> — the interface answers (401 without a token)
  local i; for ((i = 0; i < 120; i++)); do
    [ -f "$1/.anet/modules/a2a/a2a_token.txt" ] && curl -s -m 2 -o /dev/null "http://$2/a2a/v1/agents" && return 0
    sleep 0.25
  done; return 1
}
start_responder(){
  ( cd "$RUN" && exec setsid "$BIN/a2aprobe" responder --ctl "$(envget prov ctl)" \
      --token-file "$(envget prov home)/.anet/control_token.txt" ) >>"$RUN/responder.log" 2>&1 </dev/null &
  echo $! > "$RUN/responder.pid"
}
stop_responder(){
  local p; p=$(cat "$RUN/responder.pid" 2>/dev/null) || return 0
  kill "$p" 2>/dev/null; rm -f "$RUN/responder.pid"
}
restart_provider(){
  local home addr; home=$(envget prov home); addr=$(envget prov ctl)
  ctl "$home" "$addr" /shutdown '{}' >/dev/null
  wait_down "$addr" 30 || die "provider did not stop"
  sleep 0.5
  start_node "$home" "$RUN/prov.log"
  wait_up "$addr" 30 || die "provider did not come back: $(tail -3 "$RUN/prov.log")"
  wait_a2a "$home" "$(envget prov a2a)" || die "provider's A2A interface did not come back"
}
# provider_config [BACKEND_URL [TOKEN_FILE]] — (re)write the provider's config.json.
provider_config(){
  python3 - "$(envget prov home)/.anet/config.json" "$(envget prov ctl)" "$(envget backend)" "$RUN/backend.token" \
    "$FREE" "$PAID" "$PRICE_PAID" "$PRICEY" "$PRICE_PRICEY" "${1:-}" "${2:-}" <<'PY'
import json, sys
path, addr, backend, token, free, paid, ppaid, pricey, ppricey, a2a_url, a2a_tok = sys.argv[1:12]
def cap(i, price, name):
    c = {"id": i, "url": "http://%s/%s" % (backend, i), "name": name,
         "description": "SHA-256 of the text you send (%s)" % ("free" if not price else "%s credits" % price),
         "tags": ["interop", "digest"]}
    if price:
        c["price"] = int(price)
    return c
# What the daemon wrote itself (the hub it registered with, the name) stays: only our keys change.
try:
    cfg = json.load(open(path))
except (OSError, ValueError):
    cfg = {}
cfg["control_addr"] = addr
cfg["modules"] = {"service": {"capabilities": [cap(free, 0, "Digest, free"), cap(paid, ppaid, "Digest"),
                                               cap(pricey, ppricey, "Digest, dearer")],
                              "token_file": token, "allow_tcp": True}}
cfg["inbound"] = {"policy": "closed", "public_capabilities": [{"id": free}, {"id": paid}, {"id": pricey}]}
if a2a_url:
    b = {"match": "*", "url": a2a_url, "allow_tcp": a2a_url.startswith("http")}
    if a2a_tok:
        b["token_file"] = a2a_tok
    cfg["modules"]["a2a"] = {"backends": [b]}
json.dump(cfg, open(path, "w"), indent=1)
PY
}

cmd=${1:-}; shift || true
case "$cmd" in
start)
  case "$J" in /*) ;; *) die "J must be absolute" ;; esac
  own_dir "$J" || die "J=$J is not a private directory of this user"
  stop_under "$BIN" 10
  rm -rf -- "$BIN" "$RUN"; mkdir -p "$BIN" "$RUN" && mkdir -p -m 700 "$RUN/xdg"
  if [ -n "${INTEROP_BIN:-}" ]; then
    for b in anet anet-hub a2aprobe; do cp "$INTEROP_BIN/$b" "$BIN/$b" || die "no $b in $INTEROP_BIN"; done
  else
    export CGO_ENABLED=0
    go build -C "$ROOT" -o "$BIN/anet" ./cmd/anet && go build -C "$ROOT" -o "$BIN/a2aprobe" ./tools/a2aprobe \
      && go build -C "$ROOT/../ANetHub" -o "$BIN/anet-hub" ./cmd/anet-hub || die "build failed"
  fi
  B=$(port_block "${INTEROP_PORT_BASE:-47400}" 10) || die "no ports"
  HUB=127.0.0.1:$B
  REQ=$RUN/req; PROV=$RUN/prov; OTH=$RUN/other
  mkdir -p "$REQ/.anet" "$PROV/.anet" "$OTH/.anet"
  ( cd "$RUN" && exec setsid "$BIN/anet-hub" --addr "$HUB" --data "$RUN/hub" ) >"$RUN/hub.log" 2>&1 </dev/null &
  for _ in $(seq 1 40); do curl -sf -m 2 "http://$HUB/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
  curl -sf -m 5 "http://$HUB/healthz" >/dev/null || die "hub down: $(tail -2 "$RUN/hub.log")"
  python3 -c 'import secrets;print(secrets.token_hex(32))' > "$RUN/backend.token"
  python3 - "$J/env.json" "$B" "$RUN" <<'PY'
import json, sys
path, b, run = sys.argv[1], int(sys.argv[2]), sys.argv[3]
a = lambda n: "127.0.0.1:%d" % (b + n)
json.dump({"hub": "http://" + a(0), "backend": a(5), "a2a_backend_addr": a(6), "run": run,
           "req": {"home": run + "/req", "ctl": a(1), "a2a": a(3)},
           "prov": {"home": run + "/prov", "ctl": a(2), "a2a": a(4)},
           "other": {"home": run + "/other", "ctl": a(7), "a2a": a(8)}}, open(path, "w"), indent=1)
PY
  for n in req other; do
    python3 - "$(envget $n home)/.anet/config.json" "$(envget $n ctl)" "$AGENT_MAX" <<'PY'
import json, sys
path, addr, amax = sys.argv[1], sys.argv[2], int(sys.argv[3])
json.dump({"control_addr": addr,
           "payments": {"auto_max": 0, "agent_max": amax, "agent_daily_max": 1000,
                        "explicit_max": 10, "daily_max": 1000, "payees_file": "payees.allow"}},
          open(path, "w"), indent=1)
PY
  done
  provider_config
  pin_a2a "$REQ" "$(envget req a2a)"; pin_a2a "$PROV" "$(envget prov a2a)"; pin_a2a "$OTH" "$(envget other a2a)"
  ( cd "$RUN" && exec setsid "$BIN/a2aprobe" backend --addr "$(envget backend)" --token-file "$RUN/backend.token" ) \
    >"$RUN/backend.log" 2>&1 </dev/null &
  for n in req prov other; do start_node "$(envget $n home)" "$RUN/$n.log"; done
  for n in req prov other; do
    wait_up "$(envget $n ctl)" 30 || die "$n down: $(tail -3 "$RUN/$n.log")"
  done
  declare -A AID
  for n in req prov other; do
    AID[$n]=$(ctl "$(envget $n home)" "$(envget $n ctl)" /status '{}' | jget aid)
    st=$(ctl "$(envget $n home)" "$(envget $n ctl)" /hub-register "{\"hub\":\"http://$HUB\",\"name\":\"Interop-$n\"}" | jget status)
    [ "$st" = registered ] || die "$n: registration $st"
  done
  peer_allow "$PROV/.anet" "${AID[req]}" "${AID[other]}"
  _peer_add "$REQ/.anet/payees.allow" "${AID[prov]}"
  _peer_add "$OTH/.anet/payees.allow" "${AID[prov]}"
  python3 - "$J/env.json" "${AID[req]}" "${AID[prov]}" "${AID[other]}" "$FREE" "$PAID" "$PRICEY" "$PRICE_PAID" "$PRICE_PRICEY" "$AGENT_MAX" <<'PY'
import json, sys
path = sys.argv[1]
e = json.load(open(path))
e["req"]["aid"], e["prov"]["aid"], e["other"]["aid"] = sys.argv[2:5]
e["skills"] = {"free": sys.argv[5], "paid": sys.argv[6], "pricey": sys.argv[7]}
e["prices"] = {"paid": int(sys.argv[8]), "pricey": int(sys.argv[9]), "agent_max": int(sys.argv[10])}
for n in ("req", "prov", "other"):
    e[n]["a2a_token_file"] = e[n]["home"] + "/.anet/modules/a2a/a2a_token.txt"
    e[n]["ctl_token_file"] = e[n]["home"] + "/.anet/control_token.txt"
    e[n]["data_dir"] = e[n]["home"] + "/.anet"
json.dump(e, open(path, "w"), indent=1)
PY
  for n in req prov other; do wait_a2a "$(envget $n home)" "$(envget $n a2a)" || die "$n: no A2A interface"; done
  start_responder
  # The provider's card at the hub (§10.5): the first registration publishes it.
  for _ in $(seq 1 40); do
    [ "$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://$HUB/a2a/v1/agents/${AID[prov]}/card")" = 200 ] && break; sleep 0.5
  done
  say "hub $HUB; requester ${AID[req]} (A2A $(envget req a2a)); provider ${AID[prov]}; other ${AID[other]}"
  say "env: $J/env.json"
  ;;
backend)
  url=${1:?backend URL}; tokf=${2:-}
  stop_responder
  provider_config "$url" "$tokf"
  restart_provider
  say "provider: §11.6 backend $url"
  ;;
responder)
  provider_config
  restart_provider
  stop_responder; start_responder
  say "provider: responder"
  ;;
restart)
  # every daemon, on its data, from what is in $BIN now (after replacing a binary there)
  for n in req prov other; do
    home=$(envget $n home); addr=$(envget $n ctl)
    ctl "$home" "$addr" /shutdown '{}' >/dev/null
    wait_down "$addr" 30 || die "$n did not stop"
    sleep 0.5
    start_node "$home" "$RUN/$n.log"
    wait_up "$addr" 30 || die "$n did not come back: $(tail -3 "$RUN/$n.log")"
    wait_a2a "$home" "$(envget $n a2a)" || die "$n: no A2A interface"
  done
  say "restarted req, prov, other"
  ;;
trust)   _peer_add "$(envget prov home)/.anet/peers.trust" "$@" ;;
untrust)
  f=$(envget prov home)/.anet/peers.trust
  for x in "$@"; do [ -f "$f" ] && { grep -vxF -- "$x" "$f" > "$f.new"; mv "$f.new" "$f"; }; done ;;
stop)
  stop_under "$BIN" 10
  ;;
*) die "usage: up.sh start|backend URL [TOKEN_FILE]|responder|restart|trust AID…|untrust AID…|stop" ;;
esac
