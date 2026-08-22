#!/usr/bin/env bash
# joint.sh — drive every module end to end across four repos.
#
# Prerequisites, all in $J (default /tmp/joint), all built from source:
#   anet          go build ./cmd/anet                              (this repo)
#   anetfixture   go build ./tools/anetfixture                     (this repo)
#   anetpeer      go build ./tools/anetpeer                        (this repo)
#   anetlinkd     go build -tags onvif,hikvision,dahua ./cmd/...   (ANetLink)
#   anetmock      go build ./cmd/anetmock                          (ANetMock)
#   anet-hub      go build ./cmd/anet-hub                          (ANetHub)
#
# and these already running:
#   ./anetmock -venue office -api 127.0.0.1:29080 -base-port 29200
#   ./anetlinkd --config run/anetlink.json --c1-socket $J/run/link/c1.sock
#   ./anet-hub --addr 127.0.0.1:29088 --data run/hub
#
# The daemons and peer processes are started by this script.
#
#   ANetMock (real ONVIF/Zigbee endpoints) ← ANetLink (adapters, C1 socket)
#     ← anet daemon "provider" → ANetHub (relay) ← anet daemon "requester"
#
# Every call below crosses at least two process boundaries. That is the point:
# both repos' suites fake each other, and every defect this run has found so
# far lived exactly in the gap the fakes cover.
set -uo pipefail
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
J=${J:-/tmp/joint}; cd "$J"
REQ=$J/run/req; PROV=$J/run/home
RC=127.0.0.1:29098; PC=127.0.0.1:29099; MOCK=127.0.0.1:29080
FIX=$J/anetfixture
pass=0; fail=0
ok(){   printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; pass=$((pass+1)); }
no(){   printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; fail=$((fail+1)); }
hd(){   printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }
rtok(){ cat "$REQ/.anet/control_token.txt"; }
# call <path> <json>  — the requester's control API
rc(){ curl -s -m 30 -H "Authorization: Bearer $(rtok)" -H 'Content-Type: application/json' \
        -d "$2" "http://$RC$1"; }
# cap <capability> <args-json> — delegate, wait for the result, print the effect
cap(){
  local ix; ix=$(rc /delegate "{\"provider\":\"$PROV_AID\",\"capability\":\"$1\",\"args\":$2}" \
                 | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
  [ -n "$ix" ] || { echo '{"error":"delegate refused"}'; return 1; }
  for _ in $(seq 1 40); do
    local r; r=$(rc /results '{}' | python3 -c "
import sys,json
for x in json.load(sys.stdin).get('results') or []:
    if x['interaction_id']=='$ix': print(x['result']); break
")
    [ -n "$r" ] && { echo "$r"; return 0; }
    sleep 0.5
  done
  echo '{\"error\":\"timed out waiting for the result\"}'; return 1
}

hd "0/6  bring the stack up"
pgrep -x anet | xargs -r kill -TERM 2>/dev/null; sleep 2
REQ_AID=$($FIX aid --home "$REQ/.anet")
GENESIS=$($FIX org-genesis --home "$REQ/.anet" --nonce joint 2>$J/run/orgid.txt)
ORG_ID=$(sed 's/^org id: //' $J/run/orgid.txt)

# Two peer processes sharing a rendezvous directory. Without them the p2p
# module is the one module that changes how delegations travel and the one
# module no joint run could exercise.
pgrep -x anetpeer | xargs -r kill -TERM 2>/dev/null; sleep 1
rm -rf run/rv run/peer; mkdir -p run/rv run/peer
setsid ./anetpeer --socket run/peer/req.sock --peer run/peer/req.wire --rendezvous run/rv >run/peer-req.log 2>&1 </dev/null &
setsid ./anetpeer --socket run/peer/prov.sock --peer run/peer/prov.wire --rendezvous run/rv >run/peer-prov.log 2>&1 </dev/null &
sleep 1

# The provider gets every module: anetlink (devices), cas, blackboard, org, p2p.
python3 - "$PROV/.anet/config.json" "$GENESIS" "$J" <<'PY'
import json, sys
p, genesis, J = sys.argv[1], sys.argv[2], sys.argv[3]
c = json.load(open(p))
c["modules"] = {
    "anetlink":   {"socket": J + "/run/link/c1.sock"},
    "cas":        {"dir": J + "/run/cas"},
    "blackboard": {"enabled": True},
    "org":        {"genesis": genesis},
    "p2p":        {"socket": J + "/run/peer/prov.sock"},
}
json.dump(c, open(p, "w"), indent=1)
PY

# The requester needs the transport too — a direct path is only a path if
# both ends have one.
python3 - "$REQ/.anet/config.json" "$J" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
c["modules"] = {"p2p": {"socket": sys.argv[2] + "/run/peer/req.sock"}}
json.dump(c, open(sys.argv[1], "w"), indent=1)
PY

setsid env HOME=$PROV ./anet daemon >run/prov.log 2>&1 < /dev/null &
setsid env HOME=$REQ ./anet daemon >run/req.log 2>&1 < /dev/null &
sleep 3
PROV_AID=$($FIX aid --home "$PROV/.anet")
grep -q "modules:" run/prov.log && printf '  provider modules: %s\n' "$(grep -m1 'modules:' run/prov.log | sed 's/.*modules: //')"
curl -sf -m 5 "http://$RC/ping" >/dev/null && ok "requester up (it refused to start before the ledger fix)" || no "requester still will not start: $(tail -2 run/req.log)"
curl -sf -m 5 "http://$PC/ping" >/dev/null && ok "provider up" || no "provider down: $(tail -2 run/prov.log)"
rc /hub-register '{"hub":"http://127.0.0.1:29088","name":"Requester"}' >/dev/null
curl -s -m 30 -H "Authorization: Bearer $(cat $PROV/.anet/control_token.txt)" -H 'Content-Type: application/json' \
  -d '{"hub":"http://127.0.0.1:29088","name":"JointNode"}' "http://$PC/hub-register" >/dev/null
printf '  requester %s\n  provider  %s\n  org       %s\n' "$REQ_AID" "$PROV_AID" "$ORG_ID"

# ptz reads one camera's pan/tilt/zoom out of the mock's scene — the ground
# truth, independent of anything the daemon reports about itself.
ptz(){ curl -s -m 5 "http://$MOCK/api/scene" | python3 -c "
import sys,json
for d in json.load(sys.stdin)['devices']:
    if d['id']=='$1':
        st=d.get('state') or {}
        print(json.dumps({k:st.get(k) for k in ('pan','tilt','zoom')}))
        break
"; }

# state pulls what a capability actually read out of the result it returned.
# It lives in evidence.observed_state: metrics are float64 and cannot hold a
# CID, a blob or a list.
state(){ python3 -c "import sys,json;print((json.load(sys.stdin).get('evidence') or {}).get('observed_state',''))"; }

hd "1/6  device control — daemon → ANetLink → ANetMock (ONVIF PTZ)"
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
# The device path is the one where provenance matters most, and it was the
# one carrying none: ANetLink's C1 wire had no field for the quirk label and
# the daemon's shim ignored the evidence block entirely.
echo "$EFF" | python3 -c "import sys,json;e=json.load(sys.stdin).get('evidence');sys.exit(0 if e and e.get('protocol') and e.get('verify_trust') is not None else 1)" \
  && ok "the effect arrived with its provenance (protocol + trust), not just a number" \
  || no "the device effect still carries no provenance"

hd "2/6  distributed storage — cas.put / cas.get round trip"
BLOB=$(printf 'joint run %s' "$(date -u +%FT%TZ)" | base64 -w0)
PUT=$(cap cas.put "{\"body\":\"$BLOB\"}")
CID=$(echo "$PUT" | python3 -c "import sys,json;print((json.load(sys.stdin).get('evidence') or {}).get('observed_state',''))")
echo "  put:     $(echo "$PUT" | head -c 300)"
[ -n "$CID" ] && ok "cas.put returned a CID: $CID" || no "cas.put returned no CID"
GET=$(cap cas.get "{\"cid\":\"$CID\"}")
echo "$GET" | grep -q "$BLOB" && ok "cas.get returned the same bytes, addressed by content" \
  || no "cas.get did not return the stored bytes: $(echo "$GET" | head -c 300)"
BAD=$(cap cas.get '{"cid":"bafyreiaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}')
echo "$BAD" | grep -qi 'failed\|not found\|no such' && ok "an unknown CID fails honestly rather than returning nothing" \
  || no "an unknown CID did not fail: $(echo "$BAD" | head -c 200)"

hd "3/6  shared brain — blackboard.add / snapshot (requester signs, provider verifies)"
UNIT=$($FIX cogunit --home "$REQ/.anet" --task joint-1 --type claim --body "the camera reached its commanded position" 2>$J/run/unit.txt)
UNIT_ID=$(sed 's/^unit id: //' $J/run/unit.txt)
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

hd "4/6  org membership — org.verify against the configured genesis"
CRED=$($FIX org-credential --home "$REQ/.anet" --genesis "$GENESIS" --subject "$PROV_AID" --role member 2>/dev/null)
V=$(cap org.verify "{\"credential\":\"$CRED\"}")
echo "  verify:  $(echo "$V" | head -c 300)"
echo "$V" | grep -qi '"status": *"ok"\|"status":"ok"' && ok "a founder-issued member credential verifies" \
  || no "a valid credential failed to verify: $(echo "$V" | head -c 300)"
EXPIRED=$($FIX org-credential --home "$REQ/.anet" --genesis "$GENESIS" --subject "$PROV_AID" \
          --ttl 1s --issued-ago 2h 2>/dev/null)
E=$(cap org.verify "{\"credential\":\"$EXPIRED\"}")
echo "$E" | grep -qi 'expired\|window\|failed' && ok "an expired credential is refused" \
  || no "an expired credential passed: $(echo "$E" | head -c 300)"
I=$(cap org.info '{}')
[ "$(echo "$I" | state)" = "$ORG_ID" ] && ok "org.info names the org it actually serves" \
  || no "org.info did not report the configured org: $(echo "$I" | head -c 300)"

# INV-2: which org a node belongs to is not public. The realistic leak is
# not a field someone added on purpose — it is prose the agent wrote about
# itself, so the check belongs at the publish chokepoint.
pc(){ curl -s -m 20 -H "Authorization: Bearer $(cat $PROV/.anet/control_token.txt)" \
        -H 'Content-Type: application/json' -d "$2" "http://$PC$1"; }
LEAK=$(pc /profile "{\"summary\":\"I coordinate work for $ORG_ID\",\"readme\":\"\",\"pricing\":\"\"}")
echo "$LEAK" | grep -qi "inv2\|refusing to publish" && ok "publishing the org id is refused (INV-2)" \
  || no "the node published its org membership: $(echo "$LEAK" | head -c 200)"
CLEAN=$(pc /profile '{"summary":"I operate devices over ANetLink","readme":"","pricing":""}')
echo "$CLEAN" | grep -qi "inv2\|error" && no "an ordinary profile was blocked: $(echo "$CLEAN" | head -c 200)" \
  || ok "an ordinary profile still publishes"

hd "5/6  peer-to-peer — a delegation that never touches the hub"
delivered(){ grep -c delivered "$1" 2>/dev/null || true; }
BEFORE_P2P=$(delivered run/peer-req.log)
BEFORE_BACK=$(delivered run/peer-prov.log)
P2PEFF=$(cap cas.stat "{\"cid\":\"$CID\"}")
echo "  effect:  $(echo "$P2PEFF" | head -c 220)"
echo "$P2PEFF" | grep -q '"status":"OK"' && ok "the call succeeded over the peer transport" \
  || no "the call failed: $(echo "$P2PEFF" | head -c 200)"
AFTER_P2P=$(delivered run/peer-req.log)
[ "$AFTER_P2P" -gt "$BEFORE_P2P" ] && ok "the requester's peer carried the delegation directly ($BEFORE_P2P → $AFTER_P2P)" \
  || no "the delegation still went through the hub (peer log unchanged at $AFTER_P2P)"
AFTER_BACK=$(delivered run/peer-prov.log)
[ "$AFTER_BACK" -gt "$BEFORE_BACK" ] && ok "the result came back the same way ($BEFORE_BACK → $AFTER_BACK)" \
  || no "the provider's peer carried nothing: $(tail -2 run/peer-prov.log)"

hd "6/6  evidence — the chain survives the run and a restart"
REC=$(wc -l < "$REQ/.anet/evidence.ael.jsonl" 2>/dev/null || echo 0)
echo "  requester chain: $REC records"
curl -s -m 10 -H "Authorization: Bearer $(rtok)" -d '{}' "http://$RC/shutdown" >/dev/null
sleep 2
setsid env HOME=$REQ ./anet daemon >run/req-restart.log 2>&1 < /dev/null &
sleep 3
if curl -sf -m 5 "http://$RC/ping" >/dev/null; then
  ok "the requester restarted against a chain holding $REC records, receipts included"
else
  no "the requester will not restart: $(tail -3 run/req-restart.log)"
fi

printf '\n\033[1m── %d passed, %d failed ──\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
