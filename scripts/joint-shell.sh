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
# Requires ANetHub checked out beside this repo (../ANetHub), or prebuilt binaries:
#
#   JOINT_BIN        directory with anet-shell (anet built with -tags shell), anetfixture and anet-hub —
#                    scripts/testnet/build.sh --variant shell=shell makes that set. They are copied into
#                    $J; nothing is built and go is not needed.
#   JOINT_PORT_BASE  hub, requester, provider on 127.0.0.1:BASE, BASE+1, BASE+2, and the two daemons'
#                    local A2A interfaces on BASE+3, BASE+4 (on the test hosts use 47100-47499,
#                    docs/notes/0015). Unset: the historical 29188, 29198, 29199, and A2A from 43811 up.
#   J                work directory (default /tmp/joint-shell); deleted and rebuilt, so it has to be a
#                    private directory of this user (lib.sh own_dir).
#
# The daemons get a private XDG_RUNTIME_DIR under $J, and everything is stopped by path at the end
# (lib.sh stop_under), never by process name.
set -uo pipefail
umask 077
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
cd "$(dirname "$0")/.."
ROOT=$PWD
J=${J:-/tmp/joint-shell}
HUB_SRC=${HUB_SRC:-$ROOT/../ANetHub}
if [ -n "${JOINT_PORT_BASE:-}" ]; then
  [[ "$JOINT_PORT_BASE" =~ ^[0-9]+$ ]] && [ "$JOINT_PORT_BASE" -ge 1024 ] && [ "$JOINT_PORT_BASE" -le 65530 ] \
    || { echo "JOINT_PORT_BASE=$JOINT_PORT_BASE is not a port"; exit 2; }
  HUB=127.0.0.1:$JOINT_PORT_BASE; RC=127.0.0.1:$((JOINT_PORT_BASE + 1)); PC=127.0.0.1:$((JOINT_PORT_BASE + 2))
else
  HUB=127.0.0.1:29188; RC=127.0.0.1:29198; PC=127.0.0.1:29199
fi
# lib.sh for own_dir and stop_under. ANET is set first so sourcing it looks nothing up.
ANET=$J/anet
# shellcheck source=lib.sh
. "$ROOT/scripts/lib.sh"

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
  # and $J is a directory this script owns. By path (lib.sh), not pkill:
  # on the test hosts a pattern is one typo away from a production daemon.
  stop_under "$J" 5
}
trap cleanup EXIT INT TERM

pass=0; fail=0
ok(){ printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; pass=$((pass+1)); }
no(){ printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; fail=$((fail+1)); }
hd(){ printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }

hd "0/8  build and bring the stack up"
jdir(){ _own_path "$J" >/dev/null && own_dir "$J" || { echo "J=$J is not a private directory of this user (or is /, \$HOME…); use another J"; exit 2; }; }
jdir
stop_under "$J" 5   # leftovers of a run that was killed before its cleanup hold the ports
rm -rf "$J"; jdir; mkdir -p "$J/run"; cd "$J"
# The daemons' "current daemon" pointer and identity registry go to $J/xdg, not this user's.
mkdir -p -m 0700 "$J/xdg" && export XDG_RUNTIME_DIR=$J/xdg || exit 1
unset ANET_DATA_DIR ANET_HOME ANET_ID
# -tags shell: the default build does not contain the module, so a run of
# this script against a default binary would pass every deny case for the
# wrong reason.
if [ -n "${JOINT_BIN:-}" ]; then
  for b in anet-shell anetfixture anet-hub; do
    [ -x "$JOINT_BIN/$b" ] || { echo "JOINT_BIN has no $b (scripts/testnet/build.sh --variant shell=shell)"; exit 1; }
  done
  cp "$JOINT_BIN/anet-shell" "$J/anet" && cp "$JOINT_BIN/anetfixture" "$J/anetfixture" \
    && cp "$JOINT_BIN/anet-hub" "$J/anet-hub" || { echo "cannot copy from JOINT_BIN"; exit 1; }
else
( cd "$ROOT" && go build -tags shell -o "$J/anet" ./cmd/anet ) || { echo "build anet failed"; exit 1; }
( cd "$ROOT" && go build -o "$J/anetfixture" ./tools/anetfixture ) || exit 1
[ -d "$HUB_SRC" ] || { echo "ANetHub not found at $HUB_SRC (set HUB_SRC)"; exit 1; }
( cd "$HUB_SRC" && go build -o "$J/anet-hub" ./cmd/anet-hub ) || { echo "build hub failed"; exit 1; }
fi
FIX=$J/anetfixture

"$J/anet-hub" --addr "$HUB" --data "$J/run/hub" >"$J/run/hub.log" 2>&1 </dev/null &
KIDS+=($!)
sleep 2
curl -sf -m 5 "http://$HUB/health" >/dev/null 2>&1 || curl -sf -m 5 "http://$HUB/" >/dev/null 2>&1 \
  || { echo "hub did not come up: $(tail -3 "$J/run/hub.log")"; exit 1; }

REQ=$J/run/req; PROV=$J/run/prov
mkdir -p "$REQ/.anet" "$PROV/.anet"
# The control ports are pinned before the first start too: a data dir with no config.json gets the
# first free port from 39811 up, outside the block this run was given.
printf '{"control_addr":"%s"}\n' "$RC" > "$REQ/.anet/config.json"
printf '{"control_addr":"%s"}\n' "$PC" > "$PROV/.anet/config.json"
# And their local A2A interfaces (lib.sh pin_a2a): with a block, its next two ports.
if [ -n "${JOINT_PORT_BASE:-}" ]; then
  pin_a2a "$REQ/.anet" $((JOINT_PORT_BASE + 3)); pin_a2a "$PROV/.anet" $((JOINT_PORT_BASE + 4))
fi

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

# Two lists, two layers. peers.allow is the daemon's inbound policy
# (A2A-DESIGN §5, default closed): without the requester on it the
# delegation is refused before any module sees it. It is written now,
# directly — the CLI's `anet peers allow` asks for confirmation on a
# terminal. The shell module's own allowlist below is the second layer and
# is what this script exercises.
printf '%s\n' "$REQ_AID" > "$PROV/.anet/peers.allow"

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
        # Past the daemon's own 60 s default on purpose. The module used to
        # validate timeout_s up to its ceiling while the daemon killed the
        # command at sixty seconds regardless — two layers each holding a
        # timeout, only the shorter one ever visible. 90 s is the shortest
        # command that can tell the difference.
        "slow":   {"run": "sleep 75; echo finished-after-75s", "timeout_s": 300},
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
# The control tokens go to curl through a file descriptor, never on its command line: the test hosts
# have other users, and a process's arguments are theirs to read (docs/notes/0015 §4).
rc(){ curl -s -m 30 -H @<(printf 'Authorization: Bearer %s\n' "$(rtok)") -H 'Content-Type: application/json' -d "$2" "http://$RC$1"; }
pc(){ curl -s -m 30 -H @<(printf 'Authorization: Bearer %s\n' "$(ptok)") -H 'Content-Type: application/json' -d "$2" "http://$PC$1"; }
curl -sf -m 5 "http://$RC/ping" >/dev/null && ok "requester up" || { no "requester down: $(tail -2 "$J/run/req.log")"; exit 1; }
curl -sf -m 5 "http://$PC/ping" >/dev/null && ok "provider up"  || { no "provider down: $(tail -2 "$J/run/prov.log")"; exit 1; }
# The module list goes to the daemon's own log file, not to stdout.
grep -q 'modules compiled in.*shell' "$PROV/.anet/daemon.log" && ok "provider has the shell module linked in" \
  || no "the provider binary does not contain the shell module"
rc /hub-register "{\"hub\":\"http://$HUB\",\"name\":\"Requester\"}" >/dev/null
pc /hub-register "{\"hub\":\"http://$HUB\",\"name\":\"Board\"}"     >/dev/null
printf '  requester %s\n  provider  %s\n' "$REQ_AID" "$PROV_AID"

# cap <capability> <args-json> — delegate and wait for the signed answer.
# cap <capability> <args-json> [轮询次数] — 派活并等结果。
#
# 默认 40 次 × 0.5s = 20 秒,对几乎所有命令都够。第三个参数给真正的长命令用:
# 不给的话,脚本会先于 daemon 放弃,报出来的是"超时"而实际是脚本没等够 ——
# 一个把自己的耐心当成被测对象的超时。
cap(){
  local ix; ix=$(rc /delegate "{\"provider\":\"$PROV_AID\",\"capability\":\"$1\",\"args\":$2}" \
                 | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
  [ -n "$ix" ] || { echo '{"error":"delegate refused"}'; return 1; }
  result_of "$ix" "${3:-40}"
}
# result_of <interaction_id> [轮询次数] — 等那一次调用的结果并打印 effect。
result_of(){
  for _ in $(seq 1 "${2:-40}"); do
    local r; r=$(rc /results '{}' | python3 -c "
import sys,json
for x in json.load(sys.stdin).get('results') or []:
    if x['interaction_id']=='$1': print(x['result']); break
")
    [ -n "$r" ] && { echo "$r"; return 0; }
    sleep 0.5
  done
  echo '{"error":"timed out waiting for the result"}'; return 1
}
field(){ python3 -c "import sys,json;print(json.load(sys.stdin).get('$1',''))"; }
state(){ python3 -c "import sys,json;print((json.load(sys.stdin).get('evidence') or {}).get('observed_state',''))"; }

hd "1/8  an allowlist file that does not exist denies, over the real path"
# Deleting the file is a plausible way to revoke everything at once, so the
# absent case must deny rather than error out or open up.
EFF=$(cap 'shell.run@whoami' '{}')
[ "$(echo "$EFF" | field status)" = UNAVAILABLE ] && ok "no allowlist file, no execution" \
  || no "an absent allowlist did not deny: $EFF"
printf '# who may run commands on the provider\n%s\n' "$REQ_AID" > "$ALLOW"

hd "2/8  a listed caller runs a command on another machine"
EFF=$(cap 'shell.run@whoami' '{}')
[ "$(echo "$EFF" | field status)" = OK ] && ok "status OK" || no "expected OK, got: $EFF"
OUT=$(echo "$EFF" | state)
[ -n "$OUT" ] && ok "the output came back: $(echo "$OUT" | head -1)" || no "no output in the effect: $EFF"
# The daemon's own user, not root: the module does not raise privilege.
[ "$(echo "$OUT" | tr -d '\n')" = "$(id -un)" ] && ok "it ran as the daemon's user, not root" \
  || no "unexpected user: $OUT"

hd "3/8  the AID reaching the module is the VERIFIED one, not a claim"
# This is the check the unit tests cannot make. If the daemon passed an
# empty or attacker-controlled CallerAID, the allowlist would be decoration
# and every case below would pass for the wrong reason.
CHAIN=$(pc /evidence '{"limit":50}')
echo "$CHAIN" | grep -q "$REQ_AID" && ok "the provider's chain records the requester's AID as the caller" \
  || no "the caller AID is missing from the provider's evidence chain"

hd "4/8  arguments from the network cannot become commands"
EFF=$(cap 'shell.run@say' '{"argv":["hi; id","&& id","$(id)"]}')
OUT=$(echo "$EFF" | state)
case "$OUT" in
  *uid=*) no "an injected argument executed: $OUT" ;;
  *'hi; id'*) ok "the metacharacters came back as text" ;;
  *) no "unexpected output: $OUT" ;;
esac

hd "5/8  a failing command is FAILED, not OK with empty output"
EFF=$(cap 'shell.run@fail' '{}')
[ "$(echo "$EFF" | field status)" = FAILED ] && ok "status FAILED" || no "expected FAILED, got: $EFF"
echo "$EFF" | state | grep -q broke && ok "stderr survived the trip" || no "stderr was lost"

hd "6/8  revoking access takes effect on the next call, with no restart"
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

hd "7/8  what was never enabled cannot be called"
# A registration publishes public capabilities only (0017 Q14, A2A-DESIGN §10.2): these commands are
# served to the callers on the module's allow list, so /register names none of them and the hub
# directory has nothing to list for this node. (Before Q14 this step expected shell.run@whoami in the
# directory; docs/notes/0024.)
CARD=$(rc /find "{\"query\":\"$PROV_AID\"}")
if ! printf '%s' "$CARD" | python3 -c 'import sys,json;assert isinstance(json.load(sys.stdin).get("agents"),list)' 2>/dev/null; then
  no "the directory did not answer: $CARD"
elif printf '%s' "$CARD" | grep -q 'shell\.run@'; then
  no "a command served to the allow list only is in the public directory: $CARD"
else
  ok "the commands, served to the allow list only, are not in the public directory (0017 Q14)"
fi
echo "$CARD" | grep -q 'shell.exec' && no "shell.exec is advertised without the switch" \
  || ok "shell.exec is not advertised (allow_arbitrary is off)"
# Asked for by name anyway. Nothing may execute, and the requester is told so rather than left to
# time out: the kernel answers a capability it has no provider for with UNAVAILABLE and
# anet.reason=capability_not_served (internal/daemon/capability.go tryCapabilityPaid), and the
# task ends rejected (A2A-DESIGN §4.3).
XIX=$(rc /delegate "{\"provider\":\"$PROV_AID\",\"capability\":\"shell.exec\",\"args\":{\"command\":\"id\"}}" | field interaction_id)
EFF=$(result_of "$XIX")
echo "$EFF" | state | grep -q 'uid=' && no "arbitrary execution ran without the switch" \
  || ok "nothing executed"
[ "$(echo "$EFF" | field status)" = UNAVAILABLE ] && ok "the requester was told UNAVAILABLE" \
  || no "an unserved capability was not answered UNAVAILABLE: $(echo "$EFF" | head -c 200)"
XST=$(rc /thread "{\"interaction_id\":\"$XIX\"}" | python3 -c 'import sys,json;print((json.load(sys.stdin).get("thread") or {}).get("state",""))' 2>/dev/null)
[ "$XST" = rejected ] && ok "and the task ended rejected, not failed or open" \
  || no "the unserved call ended '${XST:-unknown}', expected rejected"
# The reason code travels in the result's metadata, which the control plane shows through the task
# view (/tasks/get, the A2A projection). A build without that route has no surface for it.
XCODE=$(curl -s -m 30 -o "$J/xget.json" -w '%{http_code}' -H @<(printf 'Authorization: Bearer %s\n' "$(rtok)") \
          -H 'Content-Type: application/json' -d "{\"task_id\":\"$XIX\"}" "http://$RC/tasks/get")
if [ "$XCODE" = 404 ] && ! python3 -c 'import sys,json;json.load(open(sys.argv[1]))' "$J/xget.json" 2>/dev/null; then
  printf '\033[1;33m  ! 此构建的控制面没有 /tasks/get,anet.reason 无处可查(B1-07 合入后本条为硬断言)\033[0m\n'
else
  python3 - "$J/xget.json" <<'PY' && ok "anet.reason=capability_not_served" || no "the reason is not capability_not_served: $(head -c 300 "$J/xget.json")"
import json, sys
def reasons(v):
    if isinstance(v, dict):
        for k, x in v.items():
            if k == "anet.reason":
                yield x
            yield from reasons(x)
    elif isinstance(v, list):
        for x in v:
            yield from reasons(x)
sys.exit(0 if "capability_not_served" in reasons(json.load(open(sys.argv[1]))) else 1)
PY
fi

hd "8/8  一条真的跑过一分钟的命令"
# The bound the operator configured has to be the bound that applies. The
# module validated timeout_s up to its ceiling while the daemon killed
# every invocation at 60 s, so a 20-minute command died at one and the
# only visible sign was a FAILED with no explanation.
#
# Timed, because the failure mode is "it came back, just wrong": a killed
# command also returns, at 60 s, with FAILED.
START=$(date +%s)
EFF=$(cap 'shell.run@slow' '{}' 400)
ELAPSED=$(( $(date +%s) - START ))
[ "$(echo "$EFF" | field status)" = OK ] \
  && ok "75 秒的命令跑完了(用时 ${ELAPSED}s)" \
  || no "75 秒的命令没跑完(用时 ${ELAPSED}s):$(echo "$EFF" | head -c 200)"
echo "$EFF" | state | grep -q 'finished-after-75s' \
  && ok "回显是命令跑到最后才有的那一行" \
  || no "回显里没有命令末尾的输出"
[ "$ELAPSED" -ge 70 ] && ok "确实等了 ${ELAPSED}s —— 不是被 60 秒截断后凑出来的" \
  || no "只用了 ${ELAPSED}s,不可能真的跑完 sleep 75"

# 长命令在跑的时候,节点还得能答别的活。轮询循环是同步分发的,所以这一条
# 验的是长调用确实被移出了那个循环 —— 否则 75 秒里这个节点对谁都不应答。
( sleep 3; cap 'shell.run@whoami' '{}' > "$J/during.json" ) &
PARALLEL=$!
cap 'shell.run@slow' '{}' 400 > "$J/slow2.json"
wait $PARALLEL
[ "$(field status < "$J/during.json")" = OK ] \
  && ok "长命令在跑的同时,另一个调用照常应答" \
  || no "长命令把节点堵住了:$(head -c 160 "$J/during.json")"


printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
