#!/usr/bin/env bash
# joint-shell.sh — the shell module across process boundaries.
#
# The module's own tests construct provider.Call directly, which means they
# assert on a CallerAID this test file wrote. That is the one field the
# whole authorization rests on, and a unit test cannot show that the value
# arriving at Invoke over the network is the caller's VERIFIED identity
# rather than something they claimed. Only a real delegation through a real
# hub can, so that is what this does.
#
# Two daemons and a hub, all built from this repo:
#   requester ──delegate──> hub relay ──> provider (built with -tags shell)
#
# Requires ANetHub checked out beside this repo (../ANetHub).
set -uo pipefail
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
cd "$(dirname "$0")/.."
ROOT=$PWD
J=${J:-/tmp/joint-shell}
HUB_SRC=${HUB_SRC:-$ROOT/../ANetHub}
HUB=127.0.0.1:29188; RC=127.0.0.1:29198; PC=127.0.0.1:29199

# Everything this script starts, killed on any exit path.
#
# The first version pkill'd by path at the end, which does not run when the
# script is interrupted or when its output pipe closes early. The leftovers
# hold the hub and control ports, so the NEXT run — or an unrelated `go
# test` binding a port — fails for a reason that has nothing to do with the
# code under test. That flake cost a debugging round already.
KIDS=()
cleanup(){
  local p
  for p in "${KIDS[@]:-}"; do [ -n "$p" ] && kill -TERM "$p" 2>/dev/null; done
  sleep 1
  for p in "${KIDS[@]:-}"; do [ -n "$p" ] && kill -KILL "$p" 2>/dev/null; done
  # Belt and braces: anything that re-execed or forked away from its
  # recorded PID is still identifiable by the path it was started from,
  # and $J is a directory this script owns.
  pkill -KILL -f "^$J/anet" 2>/dev/null
}
trap cleanup EXIT INT TERM

pass=0; fail=0
ok(){ printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; pass=$((pass+1)); }
no(){ printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; fail=$((fail+1)); }
hd(){ printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }

hd "0/6  build and bring the stack up"
rm -rf "$J"; mkdir -p "$J/run"; cd "$J"
# -tags shell: the default build does not contain the module, so a run of
# this script against a default binary would pass every deny case for the
# wrong reason.
( cd "$ROOT" && go build -tags shell -o "$J/anet" ./cmd/anet ) || { echo "build anet failed"; exit 1; }
( cd "$ROOT" && go build -o "$J/anetfixture" ./tools/anetfixture ) || exit 1
[ -d "$HUB_SRC" ] || { echo "ANetHub not found at $HUB_SRC (set HUB_SRC)"; exit 1; }
( cd "$HUB_SRC" && go build -o "$J/anet-hub" ./cmd/anet-hub ) || { echo "build hub failed"; exit 1; }
FIX=$J/anetfixture

"$J/anet-hub" --addr "$HUB" --data "$J/run/hub" >"$J/run/hub.log" 2>&1 </dev/null &
KIDS+=($!)
sleep 2
curl -sf -m 5 "http://$HUB/health" >/dev/null 2>&1 || curl -sf -m 5 "http://$HUB/" >/dev/null 2>&1 \
  || { echo "hub did not come up: $(tail -3 "$J/run/hub.log")"; exit 1; }

REQ=$J/run/req; PROV=$J/run/prov
mkdir -p "$REQ" "$PROV"

# First start creates the identities. The AIDs cannot be known before this
# — the allowlist names the requester, so the allowlist cannot be written
# before this either.
env HOME=$PROV "$J/anet" daemon >"$J/run/prov0.log" 2>&1 </dev/null & P0=$!; KIDS+=($P0)
env HOME=$REQ  "$J/anet" daemon >"$J/run/req0.log"  2>&1 </dev/null & R0=$!; KIDS+=($R0)
sleep 3
REQ_AID=$("$FIX" aid --home "$REQ/.anet")
PROV_AID=$("$FIX" aid --home "$PROV/.anet")
kill -TERM "$P0" "$R0" 2>/dev/null; sleep 2
[ -n "$REQ_AID" ] && [ -n "$PROV_AID" ] || { echo "identities were not created"; exit 1; }

ALLOW=$J/run/shell-allow
# Deliberately NOT written yet. The provider comes up with allow_file
# pointing at a file that does not exist, which must mean an empty
# allowlist rather than an error or an open door.

python3 - "$PROV/.anet/config.json" "$ALLOW" "$PC" <<'PY'
import json, os, sys
p, allow, addr = sys.argv[1], sys.argv[2], sys.argv[3]
c = json.load(open(p)) if os.path.exists(p) else {}
c["control_addr"] = addr
c["modules"] = {"shell": {
    "commands": {
        "whoami": {"run": "id -un", "description": "who the daemon runs as"},
        "say":    {"run": "echo", "args": True},
        "fail":   {"run": "echo broke >&2; exit 7"},
    },
    "allow_file": allow,
}}
json.dump(c, open(p, "w"), indent=1)
PY
python3 - "$REQ/.anet/config.json" "$RC" <<'PY'
import json, os, sys
p, addr = sys.argv[1], sys.argv[2]
c = json.load(open(p)) if os.path.exists(p) else {}
c["control_addr"] = addr
json.dump(c, open(p, "w"), indent=1)
PY

env HOME=$PROV "$J/anet" daemon >"$J/run/prov.log" 2>&1 </dev/null & KIDS+=($!)
env HOME=$REQ  "$J/anet" daemon >"$J/run/req.log"  2>&1 </dev/null & KIDS+=($!)
sleep 3
rtok(){ cat "$REQ/.anet/control_token.txt"; }
ptok(){ cat "$PROV/.anet/control_token.txt"; }
rc(){ curl -s -m 30 -H "Authorization: Bearer $(rtok)" -H 'Content-Type: application/json' -d "$2" "http://$RC$1"; }
pc(){ curl -s -m 30 -H "Authorization: Bearer $(ptok)" -H 'Content-Type: application/json' -d "$2" "http://$PC$1"; }
curl -sf -m 5 "http://$RC/ping" >/dev/null && ok "requester up" || { no "requester down: $(tail -2 "$J/run/req.log")"; exit 1; }
curl -sf -m 5 "http://$PC/ping" >/dev/null && ok "provider up"  || { no "provider down: $(tail -2 "$J/run/prov.log")"; exit 1; }
# The module list goes to the daemon's own log file, not to stdout.
grep -q 'modules compiled in.*shell' "$PROV/.anet/daemon.log" && ok "provider has the shell module linked in" \
  || no "the provider binary does not contain the shell module"
rc /hub-register "{\"hub\":\"http://$HUB\",\"name\":\"Requester\"}" >/dev/null
pc /hub-register "{\"hub\":\"http://$HUB\",\"name\":\"Board\"}"     >/dev/null
printf '  requester %s\n  provider  %s\n' "$REQ_AID" "$PROV_AID"

# cap <capability> <args-json> — delegate and wait for the signed answer.
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
  echo '{"error":"timed out waiting for the result"}'; return 1
}
field(){ python3 -c "import sys,json;print(json.load(sys.stdin).get('$1',''))"; }
state(){ python3 -c "import sys,json;print((json.load(sys.stdin).get('evidence') or {}).get('observed_state',''))"; }

hd "1/7  an allowlist file that does not exist denies, over the real path"
# Deleting the file is a plausible way to revoke everything at once, so the
# absent case must deny rather than error out or open up.
EFF=$(cap 'shell.run@whoami' '{}')
[ "$(echo "$EFF" | field status)" = UNAVAILABLE ] && ok "no allowlist file, no execution" \
  || no "an absent allowlist did not deny: $EFF"
printf '# who may run commands on the provider\n%s\n' "$REQ_AID" > "$ALLOW"

hd "2/7  a listed caller runs a command on another machine"
EFF=$(cap 'shell.run@whoami' '{}')
[ "$(echo "$EFF" | field status)" = OK ] && ok "status OK" || no "expected OK, got: $EFF"
OUT=$(echo "$EFF" | state)
[ -n "$OUT" ] && ok "the output came back: $(echo "$OUT" | head -1)" || no "no output in the effect: $EFF"
# The daemon's own user, not root: the module does not raise privilege.
[ "$(echo "$OUT" | tr -d '\n')" = "$(id -un)" ] && ok "it ran as the daemon's user, not root" \
  || no "unexpected user: $OUT"

hd "3/7  the AID reaching the module is the VERIFIED one, not a claim"
# This is the check the unit tests cannot make. If the daemon passed an
# empty or attacker-controlled CallerAID, the allowlist would be decoration
# and every case below would pass for the wrong reason.
CHAIN=$(pc /evidence '{"limit":50}')
echo "$CHAIN" | grep -q "$REQ_AID" && ok "the provider's chain records the requester's AID as the caller" \
  || no "the caller AID is missing from the provider's evidence chain"

hd "4/7  arguments from the network cannot become commands"
EFF=$(cap 'shell.run@say' '{"argv":["hi; id","&& id","$(id)"]}')
OUT=$(echo "$EFF" | state)
case "$OUT" in
  *uid=*) no "an injected argument executed: $OUT" ;;
  *'hi; id'*) ok "the metacharacters came back as text" ;;
  *) no "unexpected output: $OUT" ;;
esac

hd "5/7  a failing command is FAILED, not OK with empty output"
EFF=$(cap 'shell.run@fail' '{}')
[ "$(echo "$EFF" | field status)" = FAILED ] && ok "status FAILED" || no "expected FAILED, got: $EFF"
echo "$EFF" | state | grep -q broke && ok "stderr survived the trip" || no "stderr was lost"

hd "6/7  revoking access takes effect on the next call, with no restart"
printf '# revoked\n' > "$ALLOW"
EFF=$(cap 'shell.run@whoami' '{}')
[ "$(echo "$EFF" | field status)" = UNAVAILABLE ] && ok "the revoked caller is refused" \
  || no "revocation did not take effect over the network: $EFF"
echo "$EFF" | state | grep -q "$(id -un)" && no "it ran anyway" || ok "the command did not run"
pc /evidence '{"limit":50}' | grep -q 'anet.shell.refused' && ok "the refusal is on the provider's chain" \
  || no "the refusal left no record"
# And back, so revocation is not a one-way trip needing a restart to undo.
printf '%s\n' "$REQ_AID" > "$ALLOW"
[ "$(cap 'shell.run@whoami' '{}' | field status)" = OK ] && ok "re-adding the AID also takes effect live" \
  || no "the caller could not be re-admitted without a restart"

hd "7/7  what was never enabled cannot be called"
# The node advertises the capability ids it will actually answer, so the
# hub directory is the first place a refusal has to be visible.
CARD=$(rc /find "{\"query\":\"$PROV_AID\"}")
echo "$CARD" | grep -q 'shell.run@whoami' && ok "the directory lists the commands this node serves" \
  || no "the served commands are not in the directory: $CARD"
echo "$CARD" | grep -q 'shell.exec' && no "shell.exec is advertised without the switch" \
  || ok "shell.exec is not advertised (allow_arbitrary is off)"
# Asked for by name anyway. Nothing may execute.
EFF=$(cap 'shell.exec' '{"command":"id"}')
echo "$EFF" | state | grep -q 'uid=' && no "arbitrary execution ran without the switch" \
  || ok "nothing executed"

# Known gap, in the kernel rather than this module, so it is reported
# rather than asserted:
#   位置  internal/daemon/capability.go tryCapabilityPaid
#   行为  能力解析不出时返回 false,委派落到 auto-reply;未配 auto-reply 的节点
#         对该次委派永不作答
#   影响  请求方拿到的是超时,与"节点宕了"无法区分,而不是 UNAVAILABLE
#   发现  本脚本 7/7 请求未开启的 shell.exec
case "$(echo "$EFF" | field status)" in
  UNAVAILABLE|FAILED) ok "the requester was told no" ;;
  *) printf '\033[1;33m  ! 已知缺口: 未服务的能力不作答,请求方只拿到超时(见脚本内注释)\033[0m\n' ;;
esac


printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
