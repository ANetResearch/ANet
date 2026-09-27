#!/usr/bin/env bash
# scenario.sh — a hub that knows nobody, and three nodes that join it.
#
# This is the shape of a real network rather than a test fixture:
#
#   1. a hub starts on an empty data directory. It has no relationship
#      with any daemon and no way to invite one — it can only be joined.
#   2. three daemons start with fresh identities and join the way the
#      hub's own web page tells a person to: `anet up`, then
#      `anet hub-register <hub> --name … --caps …`.
#   3. joining makes them findable. Nothing else does.
#   4. each pair proves it can actually reach the others.
#   5. A offers a capability of its own; C calls it through the hub and
#      checks the receipt.
#
# Two tiers, and the split is deliberate:
#
#   default   hermetic. No secrets, no internet, loopback only. This is
#             the tier that belongs in CI on every push.
#   --live    the same topology with real capabilities behind it: a local
#             image-caption model in a container on A, a rented frontier
#             model on B. Needs credentials and network, so it runs on
#             demand — a test that needs an API key is not a test that
#             can gate a merge.
#
# Live credentials come from $SCENARIO_ENV (default ~/.config/anet-scenario.env),
# which lives outside every repository and is never committed.
#
# Section 8 is A2A-DESIGN §17's two-hub run: encrypted delegation to an agent
# the other hub keeps to itself, a reply after the key cache time, a paid call
# across hubs with its negative cases, the three cancel orders, and the C32
# mutation. It waits out the daemon's key cache time (10 minutes) once, so the
# whole script takes about a quarter of an hour; SCENARIO_XHUB=0 leaves it out.
#
# Environment:
#   SCENARIO_ROOT       work directory (default /tmp/anet-scenario); this user's and
#                       writable by no one else (lib.sh own_dir)
#   JOINT_BIN / SCENARIO_BIN
#                       prebuilt anet, anet-hub and anetfixture (scripts/testnet/build.sh
#                       makes them); copied into $SCENARIO_ROOT/bin. Unset and
#                       $SCENARIO_ROOT/bin incomplete: built here with go (GOWORK as in
#                       joint.sh; HUB_SRC names the ANetHub checkout)
#   SCENARIO_PORT_BASE  first port of the block this run uses (default 29500): hub +0,
#                       second hub +1, daemons +10..+13, service +20, voucher door +30,
#                       section 8's requesters +40..+43. On the test hosts pick a block
#                       inside 47100-47499 (scripts/testnet/README.md), e.g. 47155. A port
#                       in use aborts the run; nothing is stopped to free it
#   HUB_PORT            the first hub alone (default: the port base)
#   SCENARIO_KEEP=1     leave everything running at the end
#   Section 8 (defaults: both hubs on this host):
#   SCENARIO_XHUB=0     skip section 8
#   XHUB_PORT_BASE2     the second side's block (default port base +50, so +50..+53 on
#                       this host when there is no XHUB_HOST2): hub +0, providers +1 and
#                       +2, their service +3
#   XHUB_HOST2          ssh target (user@host) for the second side: its hub, the two
#                       providers and their service run there. Needs non-interactive ssh
#                       and scp, python3 and curl there, the same architecture as here
#   XHUB_ADDR1 / XHUB_ADDR2
#                       the address each side's hub binds and the other side dials
#                       (default 127.0.0.1; with XHUB_HOST2 set both must be real
#                       addresses of their hosts, e.g. 10.2.2.89 and 10.2.2.90)
#   XHUB_ROOT2          the second side's work directory (default $SCENARIO_ROOT/side2, or
#                       /tmp/anet-scenario-side2 on XHUB_HOST2); same ownership rule
#   XHUB_CACHE_WAIT     seconds the key-cache cases wait (default 630: the daemon's
#                       10-minute revalidation interval, and a margin)
#
# Test hosts (docs/notes/0015, scripts/testnet/README.md): this script stops only what it
# started, by path; its daemons get a private XDG_RUNTIME_DIR; nothing binds 0.0.0.0.
#   one host:  JOINT_BIN=… SCENARIO_PORT_BASE=47155 bash scripts/scenario.sh
#   two hosts: JOINT_BIN=… SCENARIO_PORT_BASE=47155 XHUB_PORT_BASE2=47255 \
#              XHUB_HOST2=ink@10.2.2.90 XHUB_ADDR1=10.2.2.89 XHUB_ADDR2=10.2.2.90 \
#              bash scripts/scenario.sh          (run on 10.2.2.89)
set -uo pipefail
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost

LIVE=0
[ "${1:-}" = "--live" ] && LIVE=1

ROOT=${SCENARIO_ROOT:-/tmp/anet-scenario}
BIN=${SCENARIO_BIN:-${JOINT_BIN:-$ROOT/bin}}
PORT_BASE=${SCENARIO_PORT_BASE:-29500}
case "$PORT_BASE" in ''|*[!0-9]*) echo "SCENARIO_PORT_BASE=$PORT_BASE 不是端口号"; exit 1 ;; esac
HUB_PORT=${HUB_PORT:-$PORT_BASE}
HUB=http://127.0.0.1:$HUB_PORT
SVC_PORT=$((PORT_BASE+20))
VOUCHER_PORT=$((PORT_BASE+30))
SCENARIO_ENV=${SCENARIO_ENV:-$HOME/.config/anet-scenario.env}
CAPTION_URL=${CAPTION_URL:-http://127.0.0.1:8099/caption}

# lib.sh for stop_under: this script stops what it started by path, never by process name.
ANET=$BIN/anet
# shellcheck source=lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

pass=0; fail=0
ok(){ printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; pass=$((pass+1)); }
no(){ printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; fail=$((fail+1)); }
hd(){ printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }
info(){ printf '  %s\n' "$*"; }

# ── node helpers ────────────────────────────────────────────────
# Each daemon is a separate HOME, which is how one machine hosts several
# identities: one runtime, one agent, one AID.
home_of(){ echo "$ROOT/$1"; }
# The control token goes to curl through a file descriptor, never on its command line: the test hosts
# have other users, and a process's arguments are theirs to read (docs/notes/0015 §4).
ctl(){ # ctl <node> <path> <json>
  local h; h=$(home_of "$1")
  local addr; addr=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["control_addr"])' "$h/.anet/config.json")
  curl -s -m 300 -H @<(printf 'Authorization: Bearer %s\n' "$(cat "$h/.anet/control_token.txt")") \
       -H 'Content-Type: application/json' -d "$3" "http://$addr$2"
}
# ports_free ADDR PORT… — nothing listens on any of these ports at ADDR, and ADDR is an address of this
# host. Tested the way Go binds (SO_REUSEADDR), so this script's own last run in TIME_WAIT does not count.
ports_free(){ python3 -c '
import socket, sys
addr, busy = sys.argv[1], []
for p in sys.argv[2:]:
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        s.bind((addr, int(p)))
    except (OSError, ValueError, OverflowError) as e:
        busy.append("%s:%s (%s)" % (addr, p, getattr(e, "strerror", None) or e))
    finally:
        s.close()
if busy:
    sys.exit("in use, or not an address of this host: " + ", ".join(busy))' "$@"; }
aid_of(){ "$BIN/anetfixture" aid --home "$(home_of "$1")/.anet"; }

# cap <from> <to-aid> <capability> <args-json> — delegate and wait
cap(){
  local ix
  ix=$(ctl "$1" /delegate "{\"provider\":\"$2\",\"capability\":\"$3\",\"args\":$4}" \
       | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
  [ -n "$ix" ] || { echo '{"error":"delegate refused"}'; return 1; }
  for _ in $(seq 1 60); do
    local r
    r=$(ctl "$1" /results '{}' | python3 -c "
import sys,json
for x in json.load(sys.stdin).get('results') or []:
    if x['interaction_id']=='$ix': print(x['result']); break
")
    [ -n "$r" ] && { echo "$r"; return 0; }
    sleep 1
  done
  echo '{"error":"timed out"}'; return 1
}

# ── 0. a hub that knows nobody ──────────────────────────────────
hd "0  一个谁也不认识的 hub"
# 只停本脚本自己起的进程:可执行文件或脚本参数在 $ROOT 之下的(lib.sh stop_under)。按进程名杀
# 会停掉整台机器上的 anet 与 anet-hub —— 别的工作树的联调,以及测试主机上以同一用户跑着的生产
# daemon 与 hub(docs/notes/0015 §4)。为此二进制必须从 $ROOT 下运行:SCENARIO_BIN 指向别处时
# 先拷进 $ROOT/bin。$ROOT 也必须是本用户的、别人写不了的目录(lib.sh own_dir):二进制从这里运行,
# 测试主机上以 root 跑时,别的用户先建好的 /tmp/anet-scenario 能让他换掉要运行的东西。
_own_path "$ROOT" >/dev/null && own_dir "$ROOT" \
  || { echo "SCENARIO_ROOT=$ROOT 不能用:不是本用户的私有目录(或是 /、\$HOME 之类)"; exit 1; }
# 本次起的 daemon 把"当前 daemon"指针与身份注册表写在 $ROOT/xdg,不写本用户的 /tmp/anet-<uid> 或
# $XDG_RUNTIME_DIR/anet:否则同一用户的真 daemon(测试主机上 root 跑的生产节点,0015 §4)之后的 anet
# 命令会连到这里的测试节点。身份只由各自的 HOME 决定。
mkdir -p "$ROOT/xdg" && chmod 700 "$ROOT/xdg" && export XDG_RUNTIME_DIR=$ROOT/xdg || exit 1
unset ANET_DATA_DIR ANET_HOME ANET_ID
mkdir -p "$ROOT/bin"
if [ "$(cd "$BIN" 2>/dev/null && pwd -P)" != "$(cd "$ROOT/bin" && pwd -P)" ]; then
  for b in anet anet-hub anetfixture; do
    [ -x "$BIN/$b" ] || { echo "JOINT_BIN/SCENARIO_BIN=$BIN 里没有 $b"; exit 1; }
  done
  stop_under "$ROOT/bin"
  for b in anet anet-hub anetfixture; do cp "$BIN/$b" "$ROOT/bin/$b" || exit 1; done
  BIN=$ROOT/bin
elif ! { [ -x "$BIN/anet" ] && [ -x "$BIN/anet-hub" ] && [ -x "$BIN/anetfixture" ]; }; then
  # 自包含:没给二进制、$ROOT/bin 也不全,就从本检出与 HUB_SRC 构建(与 joint.sh 同一套 GOWORK 规则:
  # 两仓必须解析到同一份 ANetCore,wire 2 的 daemon 与 hub 要同代)。
  command -v go >/dev/null || { echo "$BIN 里没有 anet/anet-hub/anetfixture,本机也没有 go:用 JOINT_BIN 给预编译二进制"; exit 1; }
  SRC_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
  HUB_SRC=${HUB_SRC:-$SRC_ROOT/../ANetHub}
  [ -d "$HUB_SRC/cmd/anet-hub" ] || { echo "找不到 ANetHub($HUB_SRC),设 HUB_SRC"; exit 1; }
  if [ -z "${GOWORK:-}" ] && [ -f "$SRC_ROOT/../go.work" ]; then
    GOWORK=$(cd "$SRC_ROOT/.." && pwd -P)/go.work; export GOWORK
  fi
  core_of(){ d=$(go -C "$1" list -m -f '{{.Dir}}' github.com/ANetResearch/ANetCore 2>/dev/null) && [ -n "$d" ] && (cd "$d" && pwd -P); }
  [ -n "$(core_of "$SRC_ROOT")" ] && [ "$(core_of "$SRC_ROOT")" = "$(core_of "$HUB_SRC")" ] \
    || { echo "ANet 与 ANetHub 解析到不同的 ANetCore;把 GOWORK 设成含三仓的工作区"; exit 1; }
  stop_under "$ROOT/bin"
  echo "  构建:ANet $SRC_ROOT,ANetHub $HUB_SRC(GOWORK=$(go env GOWORK))"
  CGO_ENABLED=0 go build -C "$SRC_ROOT" -o "$ROOT/bin/anet" ./cmd/anet \
    && CGO_ENABLED=0 go build -C "$SRC_ROOT" -o "$ROOT/bin/anetfixture" ./tools/anetfixture \
    && CGO_ENABLED=0 go build -C "$HUB_SRC" -o "$ROOT/bin/anet-hub" ./cmd/anet-hub \
    || { echo "构建失败"; exit 1; }
fi
stop_under "$ROOT"
# 收尾:任何退出路径都停掉本次起的全部进程(日志与数据目录留着看)。SCENARIO_KEEP=1 则留着进程。
# 第 8 节在另一台主机上起了东西时,xhub_side2_down 也停那边(见第 8 节;在那之前它什么也不做)。
xhub_side2_down(){ :; }
trap '[ "${SCENARIO_KEEP:-0}" = 1 ] || { xhub_side2_down; stop_under "$ROOT" 10; }' EXIT
trap 'exit 130' INT TERM
# Every port this run binds on this host, checked before anything starts (an earlier run's processes were
# stopped just above). A busy port aborts the run and nothing is stopped to free it: on the test hosts it
# can be a node deploy.sh put there (47100-47499 is the test net's range) or another checkout's run, and a
# hub or daemon of this run that failed to bind would leave its health check to be answered by that one.
SCN_PORTS="$HUB_PORT $((HUB_PORT+1)) $((PORT_BASE+10)) $((PORT_BASE+11)) $((PORT_BASE+12)) $((PORT_BASE+13)) $SVC_PORT $VOUCHER_PORT"
[ "${SCENARIO_XHUB:-1}" = 1 ] && SCN_PORTS="$SCN_PORTS $((PORT_BASE+40)) $((PORT_BASE+41)) $((PORT_BASE+42)) $((PORT_BASE+43))"
# shellcheck disable=SC2086
ports_free 127.0.0.1 $SCN_PORTS || { echo "SCENARIO_PORT_BASE=$PORT_BASE 这一段有端口被占:换一段(本脚本不去停占用者)"; exit 1; }
# Section 8 moves hub1 to XHUB_ADDR1, where a deployed hub may listen without holding the loopback port.
if [ "${SCENARIO_XHUB:-1}" = 1 ] && [ -n "${XHUB_ADDR1:-}" ] && [ "$XHUB_ADDR1" != 127.0.0.1 ]; then
  ports_free "$XHUB_ADDR1" "$HUB_PORT" || { echo "XHUB_ADDR1=$XHUB_ADDR1 上 $HUB_PORT 用不了(被占,或不是本机地址)"; exit 1; }
fi
rm -rf "$ROOT/hub" "$ROOT/A" "$ROOT/B" "$ROOT/C"
mkdir -p "$ROOT/hub"
setsid "$BIN/anet-hub" --addr "127.0.0.1:$HUB_PORT" --data "$ROOT/hub" >"$ROOT/hub.log" 2>&1 </dev/null &
for _ in $(seq 1 30); do curl -sf -m 2 "$HUB/healthz" >/dev/null && break; sleep 1; done
curl -sf -m 5 "$HUB/healthz" >/dev/null && ok "hub 起来了,数据目录全新" || { no "hub 起不来: $(tail -2 "$ROOT/hub.log")"; exit 1; }
n=$(curl -s -m 5 "$HUB/agents" | python3 -c 'import sys,json;print(len(json.load(sys.stdin).get("agents") or []))')
[ "$n" = "0" ] && ok "它认识 0 个 agent —— 只能被加入,不能去邀请" || no "全新 hub 上已有 $n 个 agent"

# ── 1. three nodes join the documented way ──────────────────────
hd "1  三个节点按网页上教的方式加入"
port=$((PORT_BASE+10))
for node in A B C; do
  h=$(home_of "$node"); mkdir -p "$h/.anet"
  cat > "$h/.anet/config.json" <<CFG
{
 "control_addr": "127.0.0.1:$port",
 "hub_url": "$HUB"
}
CFG
  port=$((port+1))
done

# A's own capability. Hermetic and deterministic: it hashes what it is
# given. In --live mode A also fronts the caption model, and the contrast
# is the point — one capability that needs nothing, one that owns a model.
mkdir -p "$ROOT/svc"
cat > "$ROOT/svc/scenario-svc.py" <<'PY'
import hashlib, json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        try:
            req = json.loads(self.rfile.read(n) or b"{}")
        except Exception:
            req = {}
        text = str(req.get("text", ""))
        body = json.dumps({
            "digest": hashlib.sha256(text.encode()).hexdigest(),
            "length": len(text),
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass

HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
setsid python3 "$ROOT/svc/scenario-svc.py" "$SVC_PORT" >"$ROOT/svc.log" 2>&1 </dev/null &
sleep 1

# A declares it. This is the whole of "putting your own service on the
# network": an id, and a URL behind it.
python3 - "$(home_of A)/.anet/config.json" "http://127.0.0.1:$SVC_PORT" "$VOUCHER_PORT" <<'PY'
import json, sys
p, svc, door = sys.argv[1], sys.argv[2], sys.argv[3]
c = json.load(open(p))
c["name"] = "NodeA"
c["caps"] = ["digest"]
c["modules"] = {"service": {"capabilities": [
    {"id": "text.digest", "url": svc,
     "description": "sha256 of the text you send"},
    # The same work, priced. Two capabilities behind one service so the
    # free and paid paths are compared against identical output — a paid
    # path that quietly did something else would otherwise look like it
    # was working.
    {"id": "text.digest.paid", "url": svc, "price": 25,
     "description": "sha256, 25 credits"}]}}
# A's public voucher face: where a buyer who paid at the hub brings the
# voucher. The hub never sees this address's traffic, which is the whole
# point of selling access rather than proxying it.
c["modules"]["x402"] = {"voucher_addr": "127.0.0.1:" + door,
                        "voucher_url": "http://127.0.0.1:%s/x402/redeem" % door}
# The voucher door serves only public capabilities, through the kernel's
# admission check (A2A-DESIGN §5.4). text.digest stays reachable only by
# the peers A allows; the priced one is public.
c["inbound"] = {"policy": "closed", "public_capabilities": [{"id": "text.digest.paid"}]}
json.dump(c, open(p, "w"), indent=1)
PY
python3 - "$(home_of B)/.anet/config.json" <<'PY'
import json, sys
p = sys.argv[1]; c = json.load(open(p)); c["name"] = "NodeB"; c["caps"] = ["chat"]
json.dump(c, open(p, "w"), indent=1)
PY
python3 - "$(home_of C)/.anet/config.json" <<'PY'
import json, sys
p = sys.argv[1]; c = json.load(open(p)); c["name"] = "NodeC"
json.dump(c, open(p, "w"), indent=1)
PY

for node in A B C; do
  setsid env HOME="$(home_of "$node")" "$BIN/anet" daemon >"$ROOT/$node.log" 2>&1 </dev/null &
done
sleep 5
up=0
for node in A B C; do
  h=$(home_of "$node")
  addr=$(python3 -c "import json;print(json.load(open('$h/.anet/config.json'))['control_addr'])")
  curl -sf -m 3 "http://$addr/ping" >/dev/null && up=$((up+1))
done
[ "$up" = 3 ] && ok "三个 daemon 各自起来了(三个身份,三个 HOME)" || no "只有 $up/3 起来"

# The documented join: `anet hub-register <hub> --name … --caps …`.
ctl A /hub-register "{\"hub\":\"$HUB\",\"name\":\"NodeA\",\"caps\":[\"digest\",\"text.digest.paid\"]}" >/dev/null
ctl B /hub-register "{\"hub\":\"$HUB\",\"name\":\"NodeB\",\"caps\":[\"chat\"]}" >/dev/null
ctl C /hub-register "{\"hub\":\"$HUB\",\"name\":\"NodeC\",\"caps\":[]}" >/dev/null
sleep 2
A=$(aid_of A); B=$(aid_of B); C=$(aid_of C)
# Every node runs the default closed inbound policy (A2A-DESIGN §5) and
# allows the other two by name. Written directly: the CLI's `anet peers
# allow` asks for confirmation on a terminal. The daemons read the files on
# every decision, so no restart is needed.
for node in A B C; do
  : > "$(home_of "$node")/.anet/peers.allow"
  for other in "$A" "$B" "$C"; do printf '%s\n' "$other" >> "$(home_of "$node")/.anet/peers.allow"; done
done
info "A $A"
info "B $B"
info "C $C"

# ── 2. joining is what makes you findable ───────────────────────
hd "2  加入之后才可被发现"
# Registered and listed are different states, and the difference is the
# hub's own rule: it lists what advertises a service. C joined with no
# capabilities, so it is reachable and not in the directory — which is
# correct, and a test that demanded three listings would have been
# demanding a bug.
reg=0
for x in "$A" "$B" "$C"; do
  curl -sf -m 10 "$HUB/agents/$x/kel" >/dev/null 2>&1 && reg=$((reg+1))
done
[ "$reg" = 3 ] && ok "三个都注册上了,而 hub 从未主动联系过任何一个" || no "只有 $reg/3 注册成功"
listed=$(curl -s -m 10 "$HUB/agents" | python3 -c "
import sys,json
ags={a['aid'] for a in json.load(sys.stdin).get('agents') or []}
print(sum(1 for x in ['$A','$B','$C'] if x in ags))")
[ "$listed" = 2 ] && ok "目录里只有 A 和 B —— 加入 ≠ 上架,C 没有声明能力" \
  || no "目录里有 $listed 个,期望 2(只有声明了能力的会被列出)"
found=$(ctl C /find '{"query":"digest"}' | python3 -c "
import sys,json
print(next((a['name'] for a in json.load(sys.stdin).get('agents') or [] if a['aid']=='$A'), ''))")
[ "$found" = "NodeA" ] && ok "C 用散文找到了 A" || no "C 没找到 A(得到 '$found')"
# The exact question, which the prose search cannot express.
byid=$(ctl C /find '{"capability":"text.digest"}' | python3 -c "
import sys,json
print(next((a['name'] for a in json.load(sys.stdin).get('agents') or [] if a['aid']=='$A'), ''))")
[ "$byid" = "NodeA" ] && ok "C 按能力 id 精确找到了 A" || no "按 id 没找到 A(得到 '$byid')"
none=$(ctl C /find '{"capability":"nobody.serves.this"}' | python3 -c "
import sys,json;print(len(json.load(sys.stdin).get('agents') or []))")
[ "$none" = "0" ] && ok "无人提供的能力返回空,而不是退回散文搜索" || no "无人提供的能力返回了 $none 个"

# Third-party verifiability, from the hub alone.
kel=$(curl -s -m 10 "$HUB/agents/$A/kel" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("kel",""))')
[ -n "$kel" ] && ok "hub 公布了 A 的密钥历史,陌生人可以自行验签" || no "hub 不发布密钥历史"
# The directory entry must be the agent's own words, not the hub's.
card=$(curl -s -m 10 "$HUB/agents/$A/card")
echo "$card" | python3 -c "
import sys,json
c=json.load(sys.stdin)
assert c['subject_did']=='$A', 'subject mismatch'
assert 'text.digest' in c['capabilities'], c['capabilities']
assert c['envelope']['signer_aid']=='$A'
" 2>/dev/null && ok "目录里 A 的条目是 A 自己签的(能力在签名内,hub 改不动)" \
  || no "A 没有已签名的卡片: $(echo "$card" | head -c 160)"

# ── 3. every pair can actually reach the others ─────────────────
hd "3  三者两两连通"
# reach delivers a delegation and confirms it landed in the target's
# inbox — delivery is the thing the network exists to do, so it is the
# honest reachability test.
#
# It then closes the interaction, and that matters more than it looks. A
# probe that leaves an open prose task behind is work: on a node running
# auto-reply, six probes become six model calls queued ahead of whatever
# the test actually wanted to measure. The first version of this measured
# its own backlog.
reach(){ # reach <from> <to-node> <to-aid>
  local ix
  ix=$(ctl "$1" /delegate "{\"provider\":\"$3\",\"goal\":\"reachability check from $1\"}" \
       | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
  [ -n "$ix" ] || return 1
  local landed=""
  for _ in $(seq 1 20); do
    landed=$(ctl "$2" /inbox '{}' | python3 -c "
import sys,json
print(next((x['interaction_id'] for x in (json.load(sys.stdin).get('inbox') or [])
            if x['interaction_id']=='$ix'), ''))" 2>/dev/null)
    [ -n "$landed" ] && break
    sleep 1
  done
  # Close it either way; an undelivered probe still leaves a local record.
  # The requester's end asks the provider to complete, and the provider's
  # daemon completes on its own (A2A-DESIGN §4.2).
  ctl "$1" /end "{\"interaction_id\":\"$ix\"}" >/dev/null 2>&1
  [ -n "$landed" ]
}
for pair in "A B $B" "A C $C" "B A $A" "B C $C" "C A $A" "C B $B"; do
  set -- $pair
  if reach "$1" "$2" "$3"; then ok "$1 → $2 送达(对端 inbox 里确认)"; else no "$1 → $2 未送达"; fi
done

# ── 4. A's own capability, called by C through the hub ──────────
hd "4  C 通过 hub 调用 A 自己的服务"
EFF=$(cap C "$A" text.digest '{"text":"anet"}')
info "effect: $(echo "$EFF" | head -c 220)"
echo "$EFF" | grep -q '"status":"OK"' && ok "调用成功" || no "调用失败: $(echo "$EFF" | head -c 200)"
# sha256("anet") — computed independently of the service under test.
WANT=$(printf 'anet' | sha256sum | cut -d' ' -f1)
echo "$EFF" | grep -q "$WANT" && ok "返回的摘要就是 sha256(\"anet\"),内容正确而不只是状态正确" \
  || no "摘要不对,期望 $WANT"
echo "$EFF" | grep -q '"verify_trust": *1' && ok "信任等级 V1 —— 服务应答了,但 daemon 无从判断答案对不对" \
  || no "信任等级不是 V1: $(echo "$EFF" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("evidence"))' 2>/dev/null)"

# The receipt, checked the way a stranger checks it.
hd "5  收据"
R=$(ctl C /results '{}' | python3 -c "
import sys,json
rs=[x for x in json.load(sys.stdin)['results'] if x['provider']=='$A' and 'digest' in x['goal']]
print(json.dumps(rs[-1]) if rs else '')")
[ -n "$R" ] && ok "C 拿到了带签名收据的结果" || no "C 没有收到结果"
if [ -n "$R" ]; then
  echo "$R" | python3 -c "import sys,json;d=json.load(sys.stdin);open('$ROOT/receipt.b64','w').write(d['receipt']);open('$ROOT/result.bin','w').write(d['result'])"
  if HOME=/nonexistent "$BIN/anet" verify --receipt "$(cat "$ROOT/receipt.b64")" --hub "$HUB" --result "$ROOT/result.bin" >"$ROOT/verify.out" 2>&1; then
    ok "陌生人只用收据 + hub 地址就验通了(无 daemon、无密钥)"
  else
    no "第三方验证失败: $(tail -3 "$ROOT/verify.out")"
  fi
fi

# Both sides recorded it.
for node in A C; do
  n=$(ctl "$node" /evidence '{"limit":200}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["head"]["length"])')
  [ "${n:-0}" -gt 0 ] && ok "$node 的证据链上有 $n 条记录" || no "$node 的证据链是空的"
done

# ── 6. live tier ────────────────────────────────────────────────
if [ "$LIVE" = 1 ]; then
  hd "6  实况层:A 本地模型 · B 租来的前沿模型"
  if [ -f "$SCENARIO_ENV" ]; then . "$SCENARIO_ENV"; fi
  : "${OPENROUTER_API_KEY:=}"
  : "${OPENROUTER_MODEL:=nvidia/nemotron-nano-12b-v2-vl:free}"

  # A gains a second capability, backed by a model that runs on this
  # machine and talks to nothing.
  if curl -sf -m 5 "${CAPTION_URL%/caption}/health" >/dev/null 2>&1; then
    python3 - "$(home_of A)/.anet/config.json" "$CAPTION_URL" <<'PY'
import json, sys
p, url = sys.argv[1], sys.argv[2]
c = json.load(open(p))
caps = c["modules"]["service"]["capabilities"]
if not any(x["id"] == "image.caption" for x in caps):
    caps.append({"id": "image.caption", "url": url,
                 "description": "caption an image; args: image_b64",
                 "protocol": "local-model"})
c["caps"] = ["digest", "image.caption"]
json.dump(c, open(p, "w"), indent=1)
PY
    ctl A /shutdown '{}' >/dev/null 2>&1; sleep 2
    setsid env HOME="$(home_of A)" "$BIN/anet" daemon >"$ROOT/A.log" 2>&1 </dev/null &
    sleep 4
    ctl A /hub-register "{\"hub\":\"$HUB\",\"name\":\"NodeA\",\"caps\":[\"digest\",\"image.caption\"]}" >/dev/null
    ok "A 挂上了本地 caption 模型(容器内,不出网)"
  else
    no "caption 服务没在 $CAPTION_URL —— 跳过 A 的本地模型"
  fi

  # B rents one. Same network, entirely different terms.
  if [ -n "$OPENROUTER_API_KEY" ]; then
    ctl B /autoreply "{\"on\":true,\"backend\":\"openai\",\"api_base\":\"https://openrouter.ai/api/v1\",\"api_key\":\"$OPENROUTER_API_KEY\",\"model\":\"$OPENROUTER_MODEL\",\"system_prompt\":\"You are an agent on the ANet network. Answer the request directly and briefly.\"}" >/dev/null
    sleep 1
    st=$(ctl B /status '{}' | python3 -c 'import sys,json;print(json.load(sys.stdin).get("auto_reply",""))' 2>/dev/null)
    ok "B 接上了 OpenRouter($OPENROUTER_MODEL)${st:+ · $st}"
  else
    no "没有 OPENROUTER_API_KEY —— 跳过 B(把它放进 $SCENARIO_ENV)"
  fi

  # C asks A to caption a real image, through the hub.
  if curl -sf -m 5 "${CAPTION_URL%/caption}/health" >/dev/null 2>&1; then
    IMG=${SCENARIO_IMAGE:-$ROOT/scene.png}
    [ -f "$IMG" ] || python3 - "$IMG" <<'PY'
import struct, zlib, random, sys
random.seed(7); W = H = 96; px = bytearray()
for y in range(H):
    px.append(0)
    for x in range(W):
        if y < H * 0.55:
            r, g, b = 110 + y // 3, 160 + y // 4, 230
            if (x - 70) ** 2 + (y - 22) ** 2 < 130: r, g, b = 255, 240, 150
        else:
            r, g, b = 70 + random.randint(0, 25), 130 + random.randint(0, 40), 60
        px += bytes((r, g, b))
def chunk(t, d):
    c = t + d
    return struct.pack('>I', len(d)) + c + struct.pack('>I', zlib.crc32(c) & 0xffffffff)
png = (b'\x89PNG\r\n\x1a\n'
       + chunk(b'IHDR', struct.pack('>IIBBBBB', W, H, 8, 2, 0, 0, 0))
       + chunk(b'IDAT', zlib.compress(bytes(px))) + chunk(b'IEND', b''))
open(sys.argv[1], 'wb').write(png)
PY
    B64=$(base64 -w0 < "$IMG")
    info "图片 $IMG · $(stat -c%s "$IMG") 字节 · base64 $(echo -n "$B64" | wc -c) 字节"
    CAPEFF=$(cap C "$A" image.caption "{\"image_b64\":\"$B64\"}")
    CAPTION=$(echo "$CAPEFF" | python3 -c "
import sys,json
try:
    e=json.load(sys.stdin).get('evidence') or {}
    print(json.loads(e.get('observed_state','{}')).get('caption',''))
except Exception: print('')")
    if [ -n "$CAPTION" ]; then
      ok "C → hub → A → 本地模型 → 回到 C:「$CAPTION」"
    else
      no "caption 没回来: $(echo "$CAPEFF" | head -c 220)"
    fi
  fi

  # C asks B in prose. A capability call and a conversation are different
  # things and the network carries both.
  if [ -n "$OPENROUTER_API_KEY" ]; then
    IX=$(ctl C /delegate "{\"provider\":\"$B\",\"goal\":\"In one short sentence: what is an agent network for?\"}" \
         | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
    REPLY=""
    for _ in $(seq 1 60); do
      # The thread is nested under "thread", and a message's author is
      # "me" or "them" rather than an AID — the store already knows which
      # side it is on, so it does not repeat the peer's identity per line.
      REPLY=$(ctl C /thread "{\"interaction_id\":\"$IX\"}" | python3 -c "
import sys,json
th=(json.load(sys.stdin) or {}).get('thread') or {}
ms=[m for m in (th.get('messages') or []) if m.get('from')=='them' and m.get('kind')=='text']
print(ms[-1].get('body','') if ms else '')" 2>/dev/null)
      [ -n "$REPLY" ] && break
      sleep 3
    done
    [ -n "$REPLY" ] && ok "B 用租来的模型回答了 C:「$(echo "$REPLY" | head -c 100)」" \
      || no "B 没有回复(检查 $ROOT/B.log)"
  fi
fi

# ── 7. two hubs ────────────────────────────────────────────────
hd "6.5  付费闭环:报价 → 授权 → 结算 → 干活"
# The loop every piece of which existed and none of which had ever run in
# a line. Asserted end to end because that is exactly the shape of the
# gap: each step passed on its own for a month.
balance_of(){ ctl "$1" /balance '{}' | python3 -c 'import sys,json;print(json.load(sys.stdin).get("balance",-1))'; }
ev_count(){ # ev_count <node> <event-type>
  ctl "$1" /evidence "{\"event_type\":\"$2\",\"limit\":200}" \
    | python3 -c 'import sys,json;print(len(json.load(sys.stdin).get("records") or []))'
}

c_before=$(balance_of C); a_before=$(balance_of A)
info "开工前:C=$c_before A=$a_before(注册赠额)"
[ "${c_before:-0}" -gt 0 ] 2>/dev/null && ok "注册赠额到账,新节点不必等人来充值就能试" \
  || no "C 没有余额($c_before),付费路径无从测起"

# 1. Ask without paying. A quote is an answer, not a failure.
quote=$(cap C "$A" text.digest.paid '{"text":"pay me"}')
qs=$(echo "$quote" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status",""))')
qa=$(echo "$quote" | python3 -c '
import sys,json
p=json.load(sys.stdin).get("payment_required") or {}
a=(p.get("accepts") or [{}])[0]
print(a.get("amount",""))')
[ "$qs" = "PAYMENT_REQUIRED" ] && ok "不付钱时拿到的是报价(PAYMENT_REQUIRED),不是报错" \
  || no "状态是 $qs,期望 PAYMENT_REQUIRED"
[ "$qa" = "25" ] && ok "报价说明了价钱(25 credits),且带着可付的 rail" || no "报价里没有价钱:$quote"
[ "$(balance_of C)" = "$c_before" ] && ok "只问价没扣钱" || no "报价过程动了余额"

# 2. Pay it. One call, and the quote's interaction stays on both chains.
paid=$(ctl C /delegate "{\"provider\":\"$A\",\"capability\":\"text.digest.paid\",\"args\":{\"text\":\"pay me\"},\"pay\":true}")
pix=$(echo "$paid" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
[ -n "$pix" ] && ok "--pay 走通了报价→付款→重投的整条路" || no "付费投递失败:$paid"
res=""
for _ in $(seq 1 60); do
  res=$(ctl C /results '{}' | python3 -c "
import sys,json
for x in json.load(sys.stdin).get('results') or []:
    if x['interaction_id']=='$pix': print(x['result']); break
")
  [ -n "$res" ] && break
  sleep 1
done
ps=$(echo "$res" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status",""))' 2>/dev/null)
[ "$ps" = "OK" ] && ok "付过钱之后,活真的干了(OK)" || no "付费后状态是 $ps:$res"

# 3. The result carries the hub's own signature over the settlement —
#    not the provider's word that the provider was paid.
prcpt=$(echo "$res" | python3 -c '
import sys,json
p=json.load(sys.stdin).get("paid") or {}
print(p.get("receipt",""))' 2>/dev/null)
[ -n "$prcpt" ] && ok "结算收据随结果回来了(hub 签的,付款方可自证)" \
  || no "结果里没有结算收据,付款方只有一句'对方说收到了'"

# 4. The credit moved, both ways.
c_after=$(balance_of C); a_after=$(balance_of A)
[ "$((c_before - c_after))" = "25" ] && ok "付款方扣了 25" || no "付款方 $c_before → $c_after"
[ "$((a_after - a_before))" = "25" ] && ok "收款方进了 25" || no "收款方 $a_before → $a_after"

# 5. Both chains carry the event. This is the custody bargain: the
#    balance is the hub's, the record is the parties'.
[ "$(ev_count C anet.payment.authorized)" -gt 0 ] && ok "付款方链上有 anet.payment.authorized" \
  || no "付款方链上没有授权记录"
[ "$(ev_count C anet.payment.settled)" -gt 0 ] && ok "付款方链上有 anet.payment.settled(且验过 hub 签名)" \
  || no "付款方链上没有结算记录"
[ "$(ev_count A anet.payment.settled)" -gt 0 ] && ok "收款方链上有 anet.payment.settled" \
  || no "收款方链上没有结算记录"

# 6. The free twin returns the same bytes. A paid path that quietly did
#    something else would otherwise look like it was working.
freeres=$(cap C "$A" text.digest '{"text":"pay me"}')
fd=$(echo "$freeres" | python3 -c 'import sys,json;print((json.load(sys.stdin).get("metrics") or {}).get("length",""))' 2>/dev/null)
pd=$(echo "$res"     | python3 -c 'import sys,json;print((json.load(sys.stdin).get("metrics") or {}).get("length",""))' 2>/dev/null)
[ -n "$pd" ] && [ "$fd" = "$pd" ] && ok "付费与免费路径给出同一份结果,钱买的是执行不是别的东西"   || no "付费路径的结果与免费路径不一致($fd vs $pd)"

hd "6.6  x402 网关:在 hub 付钱,到 daemon 取货"
# The hub sells access and stops. It never sees the request or the
# result — the buyer takes a voucher to the agent directly.
HUBAID=$(curl -s -m 10 "$HUB/hub/identity" | python3 -c 'import sys,json;print(json.load(sys.stdin)["aid"])')
RES_URL="$HUB/x402/resource/$A/text.digest.paid"
q=$(curl -s -m 10 -D "$ROOT/402.hdr" "$RES_URL")
code=$(head -1 "$ROOT/402.hdr" | awk '{print $2}')
[ "$code" = "402" ] && ok "未付款时 hub 回 402(这是 x402 的第一句话)" || no "hub 回了 $code"
grep -qi '^PAYMENT-REQUIRED:' "$ROOT/402.hdr" && ok "402 带 PAYMENT-REQUIRED 头" || no "402 没带 PAYMENT-REQUIRED 头"
redeem=$(echo "$q" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("redeem_at",""))')
[ "$redeem" = "http://127.0.0.1:$VOUCHER_PORT/x402/redeem" ] \
  && ok "报价里写明了取货地址($redeem)—— hub 不代理内容" || no "报价没给取货地址:$q"
gprice=$(echo "$q" | python3 -c '
import sys,json
print(((json.load(sys.stdin).get("accepts") or [{}])[0]).get("amount",""))')
[ "$gprice" = "25" ] && ok "价钱来自 A 自己签的卡片,hub 只能拒卖不能改价" || no "网关报价 $gprice"

# Pay at the gateway, using C's key. anetfixture signs the authorization
# with the same code path the daemon uses.
sig=$("$BIN/anetfixture" x402-authorize --home "$(home_of C)/.anet" \
        --pay-to "$A" --amount 25 --network "hub:$HUBAID" --interaction "gw-1" 2>/dev/null)
if [ -n "$sig" ]; then
  vres=$(curl -s -m 20 -H "PAYMENT-SIGNATURE: $sig" -D "$ROOT/gw.hdr" "$RES_URL")
  vcode=$(head -1 "$ROOT/gw.hdr" | awk '{print $2}')
  voucher=$(echo "$vres" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("voucher",""))' 2>/dev/null)
  [ "$vcode" = "200" ] && [ -n "$voucher" ] && ok "付款后拿到的是凭证,不是结果 —— hub 见不到内容" \
    || no "网关付款失败($vcode): $vres"
  grep -qi '^PAYMENT-RESPONSE:' "$ROOT/gw.hdr" && ok "结算响应带 PAYMENT-RESPONSE 头" || no "没带 PAYMENT-RESPONSE 头"
  if [ -n "$voucher" ]; then
    out=$(curl -s -m 60 -H 'Content-Type: application/json' \
          -d "{\"voucher\":\"$voucher\",\"capability\":\"text.digest.paid\",\"args\":{\"text\":\"via voucher\"}}" \
          "$redeem")
    vs=$(echo "$out" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status",""))' 2>/dev/null)
    [ "$vs" = "OK" ] && ok "凭证在 daemon 上兑成了真活(hub 全程没碰请求和结果)" || no "兑付失败:$out"
    again=$(curl -s -m 30 -H 'Content-Type: application/json' \
            -d "{\"voucher\":\"$voucher\",\"capability\":\"text.digest.paid\",\"args\":{\"text\":\"via voucher\"}}" \
            "$redeem")
    ae=$(echo "$again" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("error",""))' 2>/dev/null)
    case "$ae" in *already*) ok "同一张凭证第二次被拒(一次性由 daemon 把关)";; *) no "凭证被重复兑付:$again";; esac
  fi
else
  info "anetfixture 不支持 x402-authorize,跳过网关付款(仅验了 402 面)"
fi

hd "6.7  兑付:credit 也能出去,且 hub 的负债可被数出来"
sup(){ curl -s -m 10 "$HUB/x402/supply" | python3 -c "import sys,json;print(json.load(sys.stdin)['supply']['$1'])"; }
out1=$(sup outstanding); bal1=$(sup balances)
[ "$out1" = "$bal1" ] && ok "账是平的:未清偿 $out1 == 各账户合计 $bal1" || no "账不平:$out1 vs $bal1"
rd=$(ctl A /redeem '{"amount":10,"reference":"scenario-inv-1"}')
rv=$(echo "$rd" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("verified",""))' 2>/dev/null)
[ "$rv" = "True" ] && ok "兑付成功,且 hub 为取走的额度签了字(节点已验签)" || no "兑付没有可验证的收据:$rd"
out2=$(sup outstanding); bal2=$(sup balances)
[ "$((out1 - out2))" = "10" ] && ok "hub 的未清偿负债确实降了 10 —— credit 真的出去了" \
  || no "兑付后负债 $out1 → $out2"
[ "$out2" = "$bal2" ] && ok "兑付之后账仍然是平的" || no "兑付把账做歪了:$out2 vs $bal2"
[ "$(ev_count A anet.credit.redeemed)" -gt 0 ] && ok "兑付记在了本节点自己的链上" || no "兑付没上链"

hd "7  第二个 hub:目录联邦与跨 hub 结算"
HUB2_PORT=$((HUB_PORT+1))
HUB2=http://127.0.0.1:$HUB2_PORT
rm -rf "$ROOT/hub2"; mkdir -p "$ROOT/hub2"

# Hub 1's identity, so hub 2 can name it as a peer.
H1=$(curl -s -m 10 "$HUB/hub/identity" | python3 -c 'import sys,json;print(json.load(sys.stdin)["aid"])')
setsid "$BIN/anet-hub" --addr "127.0.0.1:$HUB2_PORT" --data "$ROOT/hub2" >"$ROOT/hub2.log" 2>&1 </dev/null &
for _ in $(seq 1 30); do curl -sf -m 2 "$HUB2/healthz" >/dev/null && break; sleep 1; done
H2=$(curl -s -m 10 "$HUB2/hub/identity" | python3 -c 'import sys,json;print(json.load(sys.stdin)["aid"])')
[ -n "$H2" ] && ok "第二个 hub 起来了" || no "第二个 hub 起不来: $(tail -2 "$ROOT/hub2.log")"

# Both hubs peer with each other, discovery on. Written as config and the
# hubs restarted, because that is how an operator actually turns this on.
for pair in "hub:$H2:$HUB2" "hub2:$H1:$HUB"; do
  d=${pair%%:*}; rest=${pair#*:}; aid=${rest%%:*}; ep=${rest#*:}
  cat > "$ROOT/$d/federation.json" <<CFG
{"delivery":"allowlist","discovery":"allowlist","home":"$([ "$d" = hub ] && echo "$HUB" || echo "$HUB2")",
 "peers":[{"aid":"$aid","endpoint":"$ep"}]}
CFG
done
# 只停本脚本的两个 hub(从 $BIN/anet-hub 起的),不碰机器上别的 hub。
stop_under "$BIN/anet-hub" 10
setsid "$BIN/anet-hub" --addr "127.0.0.1:$HUB_PORT"  --data "$ROOT/hub"  >>"$ROOT/hub.log"  2>&1 </dev/null &
setsid "$BIN/anet-hub" --addr "127.0.0.1:$HUB2_PORT" --data "$ROOT/hub2" >>"$ROOT/hub2.log" 2>&1 </dev/null &
for _ in $(seq 1 30); do curl -sf -m 2 "$HUB2/healthz" >/dev/null && curl -sf -m 2 "$HUB/healthz" >/dev/null && break; sleep 1; done
grep -q "discovery=" "$ROOT/hub.log" && ok "两个 hub 互为 peer,discovery 已开" \
  || no "discovery 没开:$(grep federation "$ROOT/hub.log" | tail -2)"

# An agent on hub 2, visible to the federation.
mkdir -p "$ROOT/D/.anet"
cat > "$ROOT/D/.anet/config.json" <<CFG
{"control_addr":"127.0.0.1:$((PORT_BASE+13))","hub_url":"$HUB2","name":"NodeD","caps":["remote.digest"],
 "modules":{"service":{"capabilities":[
   {"id":"remote.digest","url":"http://127.0.0.1:$SVC_PORT","description":"sha256, on the other hub"}]}}}
CFG
setsid env HOME="$ROOT/D" "$BIN/anet" daemon >"$ROOT/D.log" 2>&1 </dev/null &
sleep 4
ctl D /hub-register "{\"hub\":\"$HUB2\",\"name\":\"NodeD\",\"caps\":[\"remote.digest\"]}" >/dev/null
D=$(aid_of D)
# D accepts the cross-hub caller C by name.
printf '%s\n' "$C" > "$ROOT/D/.anet/peers.allow"
# Visibility is opt-in and hub-local by default — a card federates only
# when its own agent says so, signed, because a setting anyone else could
# change is not a setting.
ctl D /visibility '{"visibility":"federated"}' >/dev/null
sleep 3

# Hub 1 pulls hub 2's directory.
for _ in $(seq 1 20); do
  seen=$(curl -s -m 10 "$HUB/agents?cap=remote.digest" | python3 -c "
import sys,json;print(len(json.load(sys.stdin).get('agents') or []))")
  [ "$seen" = "1" ] && break
  sleep 3
done
[ "${seen:-0}" = "1" ] && ok "hub1 从 hub2 学到了 NodeD 的卡片(按能力 id 可查)" \
  || no "目录没有跨过去(hub1 看到 ${seen:-0} 个)"
home=$(curl -s -m 10 "$HUB/agents?cap=remote.digest" | python3 -c "
import sys,json
a=(json.load(sys.stdin).get('agents') or [{}])[0]
print(a.get('home_hub',''))")
[ -n "$home" ] && ok "学来的条目带着 home hub($home),知道该往哪投" || no "学来的条目没有 home hub"

# Reputation across the boundary. The ratings are signed evidence, so a
# peer can withhold them but cannot invent them — and the receiving hub
# keeps them per source rather than folding them into one number, because
# a hub can mint accounts and review its own agents.
D_IX="ix-fed-$(date +%s)"
r=$(cap C "$D" remote.digest '{"text":"cross-hub"}' 2>/dev/null)
rs=$(echo "$r" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status",""))' 2>/dev/null)
if [ "$rs" = "OK" ]; then
  ok "跨 hub 的能力调用真的执行了(C 在 hub1,D 在 hub2)"
else
  info "跨 hub 调用未完成($rs),信誉联邦仍按已有评价检查"
fi
# Now actually review it, and watch the rating cross. Asserting the shape
# of the endpoint proves the endpoint; only a review that travels proves
# the federation. The half that was missing until this ran: a cross-hub
# interaction could not be reviewed AT ALL, because both hubs asked "are
# both parties registered here" and neither could answer yes.
if [ "$rs" = "OK" ]; then
  cix=$(ctl C /results '{}' | python3 -c "
import sys,json
for x in json.load(sys.stdin).get('results') or []:
    if x.get('provider')=='$D': print(x['interaction_id']); break")
  if [ -n "$cix" ]; then
    ctl C /review "{\"interaction_id\":\"$cix\",\"rating\":5,\"comment\":\"cross-hub\"}" >/dev/null 2>&1
    sleep 2
    lr=$(curl -s -m 10 "$HUB/agents/$D/reputation" | python3 -c "
import sys,json;print((json.load(sys.stdin).get('reputation') or {}).get('local',{}).get('reviews',0))")
    [ "${lr:-0}" -ge 1 ] && ok "跨 hub 的活可以被评价了(评价方的 hub 收下了它自己用户的评分)" \
      || no "跨 hub 交互仍然无法评价(hub1 本地评价数 ${lr:-0})"
    # hub2 拉过去,并且落在 peer 那一列而不是本地列
    for _ in $(seq 1 20); do
      pr=$(curl -s -m 10 "$HUB2/agents/$D/reputation" | python3 -c "
import sys,json
r=json.load(sys.stdin).get('reputation') or {}
print(len(r.get('peers') or []))" 2>/dev/null)
      [ "${pr:-0}" -ge 1 ] && break
      sleep 3
    done
    [ "${pr:-0}" -ge 1 ] && ok "评分经信誉同步流真的到了 hub2,并记在 peer 来源下" \
      || no "评分没有跨过去(hub2 的 peers 列为空)"
    lc=$(curl -s -m 10 "$HUB2/agents/$D/reputation" | python3 -c "
import sys,json
r=json.load(sys.stdin).get('reputation') or {}
print(r.get('local',{}).get('reviews',0))" 2>/dev/null)
    [ "${lc:-0}" = "0" ] && ok "它没有被并进 hub2 的本地计数(来源分得清)" \
      || no "peer 的评分混进了本地列(${lc})"
  else
    info "找不到 C→D 的交互 id,跳过跨 hub 评价"
  fi
fi

# hub2 serves its reviews; hub1 pulls them. Even with no review present,
# the stream and the per-source shape must be right — a reputation
# endpoint that 404s is one nobody can build on.
rep=$(curl -s -m 10 "$HUB/agents/$D/reputation")
haspeers=$(echo "$rep" | python3 -c '
import sys,json
r=json.load(sys.stdin).get("reputation") or {}
print("yes" if "peers" in r and "combined" in r and "concentration" in r else "no")' 2>/dev/null)
[ "$haspeers" = "yes" ] && ok "信誉按来源分开发布(local / peers / combined + 集中度)" \
  || no "信誉端点没有按来源拆分:$rep"
note=$(echo "$rep" | python3 -c 'import sys,json;print(len(json.load(sys.stdin).get("note","")))' 2>/dev/null)
[ "${note:-0}" -gt 40 ] && ok "合并值旁边写明了它不能证明什么(peer 可自建账号刷评价)" \
  || no "合并评分没有附带告诫"
strm=$(curl -sf -m 10 "$HUB2/fed/v1/reviews?cursor=0" | python3 -c '
import sys,json;d=json.load(sys.stdin);print("ok" if "reviews" in d and "cursor" in d else "bad")' 2>/dev/null)
[ "$strm" = "ok" ] && ok "hub2 开着信誉同步流(带游标,拉取式)" || no "信誉同步流不可用"

# ── 8. two hubs: the other hub's agent, the key cache, money, cancels ──
# A2A-DESIGN §17:联调行 scenario.sh 与补充用例 C2、C25、C32、C34;支付负面用例对应 SI-9 / §8.4 / §8.5。
#
# 两边(side),各有一个 hub,两个 hub 互为联邦对端(投递与目录都开):
#   side 1  hub1(第 0 节那个 hub,本节以新的 federation.json 重启,绑 XHUB_ADDR1),和四个纯请求方
#           R、R2、R3、R4:没有能力、不进任何目录,只在 hub1 注册。R 的支出档位能付 20 credit 的单笔。
#   side 2  自己的 hub(X2,空数据目录)与两个 provider:
#           P  可见性 hub-local(默认),入站 closed,名单上只有 R、R2、R3、R4;两个标价 20 的能力
#              (xhub.digest.paid;xhub.slow.paid 按 args.sleep 拖时间)。
#           Q  入站 approve,名单为空;可见性 federated —— 对照:它的卡片连同密钥跨 hub 同步。
#   side 2 默认也在本机(另一端口段,另一目录);给了 XHUB_HOST2 就经 ssh 在那台主机上起。那边的控制面只绑
#   那台主机的回环,本脚本经 ssh 在那边调用(x2),令牌不出那台主机、不上命令行。两种布局走同一套 x2 代码。
#
# 8.1 hub-local provider:hub1 的目录与注册表里没有 P,却能经 /fed/v2/keys 取到它的密钥;R 的委派加密
#     送达 P,P 的回复加密回到 R;两个 hub 的数据目录与日志里搜不到任务文字。
# 8.2 开三个要跨过缓存时限的任务(R4→P、R2→P、R→Q 待批),重启 P 与 Q,开始计时。R4、R2 此后什么也不做,
#     P 对它们密钥的记录因此真的与等待一样旧(R 在 8.3/8.4 付款,P 发给它的每条状态都可能先行复核)。
# 8.3 跨 hub 付费,同一 ix:入口 hub(X2)与账本 hub(hub1)各拒少付与错收款方;P 的商户核对拒少付与
#     错收款方(付款消息由 R 自己的密钥签、经 anetfixture 发出 —— R 的 daemon 不会签这种条款);然后
#     正确付款结算一次、活只干一次;重放同一授权、同一绑定另签一张、重发付款消息,都不再多出 credit。
# 8.4 C34 取消三种顺序:付款已提交而结算未定(账本 hub 停着)、已结算而结果未到、本地已取消而结果才到;
#     付款方证据链上都有这一笔结算,且各只一笔。第一种顺序同时是 C25 的"结算未知":入口 hub 答
#     settlement_pending,provider 用同一授权重试到账本 hub 回来,恰好扣一次、干一次;其间 R 不签第二张授权
#     (8.3 里账本 hub 也拒同一任务的第二张授权:duplicate_binding)。
# 8.5 超过缓存时限之后:P(重启过)回复 R4 送达;这次回复触发的密钥复核经 /fed/v2/keys 成功(P 库里
#     peer_identity.keys_checked_at 回复前超过 10 分钟、回复后前进);Q 批准待批项并回复,送达跨 hub 的
#     纯请求方 R(C2)。
# 8.6 C32 mutation:两个 hub 以 -test-no-fed-key-lookup 重启。首次联系 hub-local provider 失败
#     (R3→P 封不了信封,hub1 答 404);超时限回复所需的密钥复核失败(P 的日志,keys_checked_at 不前进),
#     回复仍以已存密钥送达 —— §3.5 [C2]
#     的"hub 失败或 404 时继续使用已存"。去掉开关、hub 再重启后 R3→P 恢复,说明差别只来自这条查询。
#     设计 §17 C32 行写"两例均失败";第二例若真要失败,daemon 就得在 hub 404 时丢掉仍然有效的已存密钥,与
#     §3.5 [C2] 相反。这里按 §3.5 与实现断言(失败的是复核,不是投递),设计行留待 B6-02 追认。
#
# "缓存时限"是 daemon 对已存对端密钥的复核间隔(internal/daemon/seal_send.go keysRevalidateMS,10 分钟,
# A2A-DESIGN §3.5 第 1 步)。它是常量,所以本节真等 XHUB_CACHE_WAIT 秒;8.3、8.4 在等待期间做完。
if [ "${SCENARIO_XHUB:-1}" != 1 ]; then
  hd "8  两个 hub 之间(SCENARIO_XHUB=${SCENARIO_XHUB},跳过)"
else
hd "8  两个 hub 之间:加密委派、缓存时限、跨 hub 付费与取消(A2A-DESIGN §17)"

XB1=$((PORT_BASE+40))                          # side 1: R +0, R2 +1, R3 +2, R4 +3
XB2=${XHUB_PORT_BASE2:-$((PORT_BASE+50))}      # side 2: hub +0, P +1, Q +2, service +3
X2_SSH=${XHUB_HOST2:-}
ADDR1=${XHUB_ADDR1:-127.0.0.1}
ADDR2=${XHUB_ADDR2:-127.0.0.1}
CACHE_WAIT=${XHUB_CACHE_WAIT:-630}
PRICE=20
XR=$ROOT/xhub
if [ -n "$X2_SSH" ]; then S2=${XHUB_ROOT2:-/tmp/anet-scenario-side2}; else S2=${XHUB_ROOT2:-$ROOT/side2}; fi
HUB1=http://$ADDR1:$HUB_PORT
HUB2X=http://$ADDR2:$XB2
FIX=$BIN/anetfixture
declare -A XPORT=([R]=$XB1 [R2]=$((XB1+1)) [R3]=$((XB1+2)) [R4]=$((XB1+3)))
declare -A XPID=()
# Filled in as the section goes. Declared here because the script runs under set -u, and a step that
# failed early must fail its own checks, not end the run.
H1= H2X= P= Q= R= R2= R3= R4= TTL_R4= TTL_R2= C2_IX= TTL_R4_TEXT= TTL_R2_TEXT= C2_TEXT= PAY_IX=
T0=$(date +%s)
# 跨主机时 hub 地址不是回环:别让系统代理截走本脚本、daemon 与 hub 之间的请求。
export NO_PROXY="$NO_PROXY,$ADDR1,$ADDR2" no_proxy="$no_proxy,$ADDR1,$ADDR2"
X2_SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=4
             -o ControlMaster=auto -o "ControlPath=$ROOT/ssh-%C" -o ControlPersist=120)

# ── helpers ─────────────────────────────────────────────────────
q(){ printf '%q' "$1"; }
# jget KEY… — one value out of the JSON on stdin; objects, lists and booleans as JSON; "" when absent.
jget(){ python3 -c '
import sys, json
try:
    v = json.load(sys.stdin)
except Exception:
    v = None
for k in sys.argv[1:]:
    v = v.get(k) if isinstance(v, dict) else None
print("" if v is None else json.dumps(v) if isinstance(v, (dict, list, bool)) else v)' "$@"; }
# waitfor SECONDS CMD… — run CMD every half second until it succeeds; fail after SECONDS.
waitfor(){ local n=$1 i; shift; for ((i = 0; i < n * 2; i++)); do "$@" && return 0; sleep 0.5; done; return 1; }
http_up(){ curl -sf -m 2 --noproxy '*' "$1" >/dev/null 2>&1; }
canary(){ python3 -c 'import secrets;print(secrets.token_hex(6))'; }

# side 2's library. x2 sends it as text, with the lib.sh helpers it uses, ahead of every snippet it runs
# there: the same code runs on this host (bash) and on XHUB_HOST2 (ssh … bash -s), so a single-host run
# exercises exactly what a two-host run sends. Nothing in it reads stdin, which carries the script.
s2_ctl(){ # s2_ctl <node> <path> <json>: that node's control API, token through a file descriptor
  local h=$S2/$1/.anet addr
  addr=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["control_addr"])' "$h/config.json" 2>/dev/null) || return 1
  curl -s -m 300 --noproxy '*' -H @<(printf 'Authorization: Bearer %s\n' "$(cat "$h/control_token.txt" 2>/dev/null)") \
       -H 'Content-Type: application/json' -d "$3" "http://$addr$2"
}
s2_up(){ local i; for ((i = 0; i < ${2:-30} * 4; i++)); do curl -sf -m 2 --noproxy '*' "$1" >/dev/null 2>&1 && return 0; sleep 0.25; done; return 1; }
s2_pid(){ # s2_pid <name> <binary>: the pid in $S2/<name>.pid when that process runs $S2/bin/<binary>
  local p; p=$(cat "$S2/$1.pid" 2>/dev/null)
  [ -n "$p" ] && [ "$(readlink "/proc/$p/exe" 2>/dev/null)" = "$S2/bin/$2" ] && printf '%s' "$p"
}
s2_wait_gone(){ local i; for ((i = 0; i < ${2:-15} * 4; i++)); do kill -0 "$1" 2>/dev/null || return 0; sleep 0.25; done; return 1; }
s2_start_daemon(){ # s2_start_daemon <node>: its own HOME, a private XDG_RUNTIME_DIR, the pid recorded
  ( cd "$S2" && exec setsid env -u ANET_DATA_DIR -u ANET_HOME -u ANET_ID HOME="$S2/$1" XDG_RUNTIME_DIR="$S2/xdg" \
      "$S2/bin/anet" daemon ) >>"$S2/$1.log" 2>&1 </dev/null &
  echo $! >"$S2/$1.pid"
  local addr; addr=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["control_addr"])' "$S2/$1/.anet/config.json")
  s2_up "http://$addr/ping" 30
}
s2_stop_daemon(){ # s2_stop_daemon <node>: shutdown through the control API, then wait for the process
  local p; p=$(s2_pid "$1" anet)
  s2_ctl "$1" /shutdown '{}' >/dev/null 2>&1
  [ -n "$p" ] || return 0
  s2_wait_gone "$p" 20 && return 0
  kill -TERM "$p" 2>/dev/null; s2_wait_gone "$p" 5 || kill -KILL "$p" 2>/dev/null; return 0
}
s2_start_hub(){ # s2_start_hub <port> [flag…]
  local port=$1; shift
  ( cd "$S2" && exec setsid "$S2/bin/anet-hub" --addr "$BIND2:$port" --data "$S2/hub" "$@" ) >>"$S2/hub.log" 2>&1 </dev/null &
  echo $! >"$S2/hub.pid"
  s2_up "http://$BIND2:$port/healthz" 30
}
s2_stop_hub(){
  local p; p=$(s2_pid hub anet-hub)
  [ -n "$p" ] || return 0
  kill -TERM "$p" 2>/dev/null; s2_wait_gone "$p" 15 || kill -KILL "$p" 2>/dev/null; return 0
}
S2_LIB="_own_path own_dir pids_under stop_under ports_free s2_ctl s2_up s2_pid s2_wait_gone s2_start_daemon s2_stop_daemon s2_start_hub s2_stop_hub"
# x2 SNIPPET — run a bash snippet on side 2 (here, or on XHUB_HOST2 over ssh), after the library.
x2(){
  local script
  script=$(printf 'set -uo pipefail\nS2=%q\nBIND2=%q\nexport NO_PROXY=%q no_proxy=%q\n' "$S2" "$ADDR2" "$NO_PROXY" "$NO_PROXY"
           printf 'unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY all_proxy\n'
           declare -f $S2_LIB
           printf '%s\n' "$1")
  if [ -z "$X2_SSH" ]; then printf '%s\n' "$script" | bash -s
  else printf '%s\n' "$script" | ssh "${X2_SSH_OPTS[@]}" "$X2_SSH" 'bash -s'; fi
}
ctl2(){ x2 "s2_ctl $(q "$1") $(q "$2") $(q "$3")"; }

# side 1: R, R2, R3 under $XR, run from $BIN like every other daemon here.
xctl(){ # xctl <node> <path> <json>
  curl -s -m 300 --noproxy '*' -H @<(printf 'Authorization: Bearer %s\n' "$(cat "$XR/$1/.anet/control_token.txt" 2>/dev/null)") \
       -H 'Content-Type: application/json' -d "$3" "http://127.0.0.1:${XPORT[$1]}$2"
}
x1_start(){ # x1_start <node>
  ( cd "$XR" && exec setsid env -u ANET_DATA_DIR -u ANET_HOME -u ANET_ID HOME="$XR/$1" XDG_RUNTIME_DIR="$ROOT/xdg" \
      "$BIN/anet" daemon ) >>"$XR/$1.log" 2>&1 </dev/null &
  XPID[$1]=$!
  waitfor 30 http_up "http://127.0.0.1:${XPORT[$1]}/ping"
}
x1_stop(){ # x1_stop <node>: shutdown, then wait for the port and the process (the store closes after the port)
  local i p=${XPID[$1]:-}
  xctl "$1" /shutdown '{}' >/dev/null 2>&1
  for ((i = 0; i < 80; i++)); do http_up "http://127.0.0.1:${XPORT[$1]}/ping" || break; sleep 0.25; done
  [ -n "$p" ] || return 0
  for ((i = 0; i < 80; i++)); do kill -0 "$p" 2>/dev/null || return 0; sleep 0.25; done
  # Gone before the caller goes on: 8.4 (c) writes this node's store next.
  kill -TERM "$p" 2>/dev/null
  for ((i = 0; i < 40; i++)); do kill -0 "$p" 2>/dev/null || return 0; sleep 0.25; done
  kill -KILL "$p" 2>/dev/null; return 0
}
# hub_is <url> <aid> — the hub answering at url is that one. A hub of this run that failed to bind leaves
# its health check to whatever else listens there; only the identity tells them apart.
hub_is(){ [ -n "$2" ] && [ "$(curl -s -m 10 --noproxy '*' "$1/hub/identity" | jget aid)" = "$2" ]; }
# hub1 runs from $BIN/anet-hub and nothing else does once the setup below has run (side 2's hub runs from
# its own copy), so stopping it by that path stops exactly hub1.
hub1_start(){ # hub1_start [flag…]
  ( cd "$ROOT" && exec setsid "$BIN/anet-hub" --addr "$ADDR1:$HUB_PORT" --data "$ROOT/hub" "$@" ) >>"$ROOT/hub.log" 2>&1 </dev/null &
  waitfor 30 http_up "$HUB1/healthz" && hub_is "$HUB1" "$H1"
}
hub1_stop(){ stop_under "$BIN/anet-hub" 10; }
hubs_restart(){ # hubs_restart [flag…]: both hubs, the same flags; each must come back as itself
  local f flags=""
  for f in "$@"; do flags="$flags $(q "$f")"; done
  hub1_stop; x2 "s2_stop_hub" >/dev/null
  hub1_start "$@" && x2 "s2_start_hub $XB2$flags" && hub_is "$HUB2X" "$H2X"
}

# nctl <node> <path> <json> — any node of this section, whichever side it is on.
nctl(){ case $1 in P|Q) ctl2 "$@" ;; *) xctl "$@" ;; esac; }
aid_of_x(){ nctl "$1" /status '{}' | jget aid; }
thread_of(){ nctl "$1" /thread "{\"interaction_id\":\"$2\"}"; }
tstate(){ thread_of "$1" "$2" | jget thread state; }
state_is(){ [ "$(tstate "$1" "$2")" = "$3" ]; }
# tmeta <node> <ix> <key> — one key of the task's A2A metadata (/tasks/get).
tmeta(){ nctl "$1" /tasks/get "{\"task_id\":\"$2\"}" | jget metadata "$3"; }
tmeta_is(){ [ "$(tmeta "$1" "$2" "$3")" = "$4" ]; }
# heard <node> <ix> <text> — how many text messages from the peer say exactly <text>.
heard(){ thread_of "$1" "$2" | python3 -c '
import sys, json
try:
    t = json.load(sys.stdin).get("thread") or {}
except Exception:
    t = {}
print(sum(1 for m in t.get("messages") or [] if m.get("from") == "them" and m.get("kind") == "text" and m.get("body") == sys.argv[1]))' "$3"; }
heard_it(){ [ "$(heard "$@")" -gt 0 ] 2>/dev/null; }
# said_meta <node> <ix> <key> <value> — how many messages from the peer carry metadata key == value.
said_meta(){ thread_of "$1" "$2" | python3 -c '
import sys, json
try:
    t = json.load(sys.stdin).get("thread") or {}
except Exception:
    t = {}
n = 0
for m in t.get("messages") or []:
    md = m.get("metadata")
    if m.get("from") == "them" and isinstance(md, dict) and str(md.get(sys.argv[1])) == sys.argv[2]:
        n += 1
print(n)' "$3" "$4"; }
said_meta_n(){ [ "$(said_meta "$1" "$2" "$3" "$4")" -ge "${5:-1}" ] 2>/dev/null; }
# kind_from_them <node> <ix> <kind> — how many messages of that kind the peer sent.
kind_from_them(){ thread_of "$1" "$2" | python3 -c '
import sys, json
try:
    t = json.load(sys.stdin).get("thread") or {}
except Exception:
    t = {}
print(sum(1 for m in t.get("messages") or [] if m.get("from") == "them" and m.get("kind") == sys.argv[1]))' "$3"; }
got_kind(){ [ "$(kind_from_them "$@")" -gt 0 ] 2>/dev/null; }
# inbox_trust <node> <ix> — the provider's inbox entry for that id as its trust, "" when absent.
inbox_trust(){ nctl "$1" /inbox '{}' | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = {}
print(next((x.get("trust") or "none" for x in (d.get("inbox") or []) if x.get("interaction_id") == sys.argv[1]), ""))' "$2"; }
in_inbox(){ [ -n "$(inbox_trust "$1" "$2")" ]; }
# held_by <node> <ix> — the requester of that held (pending) delegation, "" when none is held.
held_by(){ nctl "$1" /inbound/pending '{}' | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = {}
print(next((x.get("requester") or "" for x in (d.get("pending") or []) if x.get("interaction_id") == sys.argv[1]), ""))' "$2"; }
is_held(){ [ -n "$(held_by "$1" "$2")" ]; }
# settled <node> <ix> — "records verified after_terminal": the node's anet.payment.settled records for
# that ix, how many say verified, how many arrived after the task had ended. Empty when unreadable.
settled(){ nctl "$1" /evidence '{"event_type":"anet.payment.settled","limit":1000}' | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = None
if not isinstance(d, dict) or not isinstance(d.get("records"), list):
    print(""); sys.exit()
ps = [r.get("payload") or {} for r in d["records"] if (r.get("payload") or {}).get("interaction_id") == sys.argv[1]]
print(len(ps), sum(1 for p in ps if p.get("verified") is True), sum(1 for p in ps if p.get("after_terminal") is True))' "$2"; }
settled_verified(){ local s; s=$(settled "$1" "$2"); set -- $s; [ "${2:-0}" -ge 1 ]; }
settled_any(){ local s; s=$(settled "$1" "$2"); set -- $s; [ "${1:-0}" -ge 1 ]; }
bal(){ nctl "$1" /balance '{}' | jget balance; }
# delegate_cap <node> <provider-aid> <capability> <args-json> — prints the interaction id.
delegate_cap(){ nctl "$1" /delegate "{\"provider\":\"$2\",\"capability\":\"$3\",\"args\":$4}" | jget interaction_id; }
delegate_goal(){ nctl "$1" /delegate "{\"provider\":\"$2\",\"goal\":\"$3\"}"; }
quoted(){ tmeta_is "$1" "$2" x402.payment.status payment-required; }
# svc_calls <text> — how many times P's service was asked to work on exactly <text>.
svc_calls(){
  local n
  n=$(x2 "if [ -f \"\$S2/svc-calls.log\" ]; then grep -c -x -F -- $(q "$1") \"\$S2/svc-calls.log\"; else echo 0; fi" | tail -1)
  echo "${n:-?}"
}
svc_ran(){ [ "$(svc_calls "$1")" -ge 1 ] 2>/dev/null; }
# p_log_mark — the length of P's daemon log now; p_logged <mark> <text> — whether P logged <text> since.
p_log_mark(){
  local n; n=$(x2 "if [ -f \"\$S2/P/.anet/daemon.log\" ]; then wc -l <\"\$S2/P/.anet/daemon.log\"; else echo 0; fi" | tail -1)
  case "$n" in ''|*[!0-9]*) n=0 ;; esac
  echo "$n"
}
p_logged(){ x2 "tail -n +\$(( $(q "${1:-0}") + 1 )) \"\$S2/P/.anet/daemon.log\" 2>/dev/null | grep -q -F -- $(q "$2")"; }
# p_keys_checked <aid> — "<keys_checked_at> <age ms>" of P's stored record of aid's key set, both by side 2's
# clock; empty when P holds none. keys_checked_at is when P last fetched or re-verified that set at its hub
# (internal/daemon/peerkel.go notePeer); a message P seals to aid re-checks it once it is older than the
# cache time (seal_send.go keysRevalidateMS), and a successful re-check moves it forward.
p_keys_checked(){
  x2 "python3 -c $(q 'import os, sqlite3, sys, time
if not os.path.exists(sys.argv[1]):
    sys.exit()
db = sqlite3.connect(sys.argv[1], timeout=15)
row = db.execute("SELECT keys_checked_at FROM peer_identity WHERE aid=?", (sys.argv[2],)).fetchone()
if row:
    print(row[0], int(time.time() * 1000) - row[0])') \"\$S2/P/.anet/interactions/interactions.db\" $(q "$1")" | tail -1
}
# keys_rechecked <aid> <since> — P's keys_checked_at for aid is now later than <since>.
keys_rechecked(){ local v; v=$(p_keys_checked "$1"); v=${v%% *}; [ -n "$v" ] && [ "$v" -gt "${2:-0}" ] 2>/dev/null; }
# keys_stale <aid> — prints the age in seconds and succeeds when P's record of aid is past the cache time.
keys_stale(){ local v; v=$(p_keys_checked "$1"); set -- $v; echo "$(( ${2:-0} / 1000 ))"; [ "${2:-0}" -gt 600000 ] 2>/dev/null; }
# nonce_of <node> <ix> — the task nonce the requester put in its TaskDoc, read from its store (one SELECT;
# SQLite's own locking makes that safe next to the running daemon).
# pay_bind = hex(SHA-256("anet/x402-bind/v1" 0 ix 0 nonce)) is what an authorization for that task binds
# (A2A-DESIGN X4); the daemon exposes neither, so a payment signed outside it for the same task needs this.
nonce_of(){ python3 - "$XR/$1/.anet/interactions/interactions.db" "$2" <<'PY'
import os, sqlite3, sys
if not os.path.exists(sys.argv[1]):
    sys.exit()
db = sqlite3.connect(sys.argv[1], timeout=15)
row = db.execute("SELECT task_nonce FROM interaction WHERE id=?", (sys.argv[2],)).fetchone()
print(row[0] if row else "")
PY
}
pay_bind(){ python3 -c 'import hashlib,sys;print(hashlib.sha256(b"anet/x402-bind/v1\0"+sys.argv[1].encode()+b"\0"+sys.argv[2].encode()).hexdigest())' "$1" "$2"; }
# authorize <amount> <pay-to> <bind> — a PaymentPayload signed with R's own key on hub1's ledger, as JSON.
authorize(){ "$FIX" x402-authorize --home "$XR/R/.anet" --pay-to "$2" --amount "$1" --network "hub:$H1" \
               --interaction "$3" 2>/dev/null | python3 -c 'import sys,base64,json;print(json.dumps(json.loads(base64.b64decode(sys.stdin.read()))))'; }
# settle_at <hub-url> <payload-json> <requirements-json> — POST /x402/settle; prints "success errorReason".
settle_at(){ python3 -c 'import json,sys;print(json.dumps({"x402Version":2,"paymentPayload":json.loads(sys.argv[1]),"paymentRequirements":json.loads(sys.argv[2])}))' "$2" "$3" \
  | curl -s -m 60 --noproxy '*' -H 'Content-Type: application/json' --data-binary @- "$1/x402/settle" \
  | python3 -c 'import sys,json
try:
    d = json.load(sys.stdin)
except Exception:
    d = {}
print("%s %s" % (str(d.get("success")).lower(), d.get("errorReason") or "-"))'; }
# pay_as_r <ix> <payload-json> — send P a payment-submitted for that task as R, with that payload: sealed with
# R's key (anetfixture seal --metadata) and sent through hub1 as R. Prints the hub's HTTP code.
pay_as_r(){
  local meta env
  meta=$(python3 -c 'import json,sys;print(json.dumps({"x402.payment.status":"payment-submitted","x402.payment.payload":json.loads(sys.argv[1])}))' "$2")
  env=$("$FIX" seal --home "$XR/R/.anet" --hub "$HUB1" --to "$P" --type message --ix "$1" --metadata "$meta" 2>>"$XR/fixture.log") || { echo 0; return; }
  "$FIX" relay-send --home "$XR/R/.anet" --hub "$HUB1" --to "$P" --envelope "$env" 2>>"$XR/fixture.log" | head -1 | jget hub code
}
# listed_at <list-url> <aid> — how many entries of a hub's agent list (/agents, /a2a/v1/agents) name aid;
# "?" when the list cannot be read (an unreadable list names nobody, and that must not pass for "not listed").
listed_at(){ curl -s -m 10 --noproxy '*' "$1" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = None
if not isinstance(d, dict) or not isinstance(d.get("agents"), list):
    print("?"); sys.exit()
print(sum(1 for a in d["agents"] if isinstance(a, dict) and a.get("aid") == sys.argv[1]))' "$2"; }
# grep_hub_dirs <text…> — files under either hub's data directory, or its log, that contain any text.
grep_hub_dirs(){
  local t n1 n2 total=0
  for t in "$@"; do
    n1=$(grep -a -r -l -F -- "$t" "$ROOT/hub" "$ROOT/hub.log" 2>/dev/null | wc -l)
    n2=$(x2 "grep -a -r -l -F -- $(q "$t") \"\$S2/hub\" \"\$S2/hub.log\" 2>/dev/null | wc -l" | tail -1)
    total=$((total + n1 + ${n2:-0}))
  done
  echo "$total"
}

xhub_setup(){
  if [ -n "$X2_SSH" ]; then
    case "$ADDR1$ADDR2" in *127.0.0.1*|*localhost*)
      no "XHUB_HOST2 给了,XHUB_ADDR1/XHUB_ADDR2 却是回环:两台主机上的 hub 互相连不上"; return 1 ;; esac
    command -v ssh >/dev/null && command -v scp >/dev/null || { no "XHUB_HOST2 需要 ssh 与 scp"; return 1; }
  fi
  case "$S2" in /*) ;; *) no "XHUB_ROOT2=$S2 不是绝对路径"; return 1 ;; esac
  case "$XB2" in ''|*[!0-9]*) no "XHUB_PORT_BASE2=$XB2 不是端口号"; return 1 ;; esac
  case "$CACHE_WAIT" in ''|*[!0-9]*) no "XHUB_CACHE_WAIT=$CACHE_WAIT 不是秒数"; return 1 ;; esac
  [ "$CACHE_WAIT" -ge 610 ] || info "XHUB_CACHE_WAIT=$CACHE_WAIT 短于 daemon 的 10 分钟复核间隔:8.5、8.6 的超时限前提会判红"
  if [ -z "$X2_SSH" ]; then
    local p2
    for p2 in "$XB2" $((XB2+1)) $((XB2+2)) $((XB2+3)); do
      case " $SCN_PORTS " in *" $p2 "*) no "XHUB_PORT_BASE2=$XB2 与主段的端口重叠($p2)"; return 1 ;; esac
    done
  fi

  # side 2's directory: this user's, writable by no one else, and empty or made by an earlier run; and its
  # four ports free once this script's own earlier run there is stopped (a busy one is not ours to stop).
  local s2real
  s2real=$(x2 "XB2=$XB2
$(cat <<'SNIP'
_own_path "$S2" >/dev/null && own_dir "$S2" || { echo "side 2: $S2 is not a private directory of this user" >&2; exit 1; }
S2=$(cd "$S2" && pwd -P)
if [ ! -e "$S2/.scenario-side2" ] && [ -n "$(ls -A "$S2")" ]; then
  echo "side 2: $S2 is not empty and was not made by scenario.sh" >&2; exit 1
fi
: >"$S2/.scenario-side2"
stop_under "$S2" 10
ports_free "$BIND2" "$XB2" && ports_free 127.0.0.1 $((XB2+1)) $((XB2+2)) $((XB2+3)) || exit 1
rm -rf -- "$S2/bin" "$S2/hub" "$S2/P" "$S2/Q" "$S2/xdg" "$S2/xhub-svc.py"
rm -f -- "$S2"/*.log "$S2"/*.pid
mkdir -p "$S2/bin" "$S2/xdg" "$S2/hub" "$S2/P/.anet" "$S2/Q/.anet" && chmod 700 "$S2/xdg" && printf '%s\n' "$S2"
SNIP
)" | tail -1)
  [ -n "$s2real" ] || { no "side 2 的目录 $S2 或端口 $XB2..$((XB2+3)) 用不了(见上)"; return 1; }
  S2=$s2real
  # From here on the EXIT trap stops side 2 as well (on this host it is under $ROOT anyway).
  xhub_side2_down(){
    x2 "stop_under \"\$S2\" 10" >/dev/null 2>&1 \
      || echo "  !! 没能停掉 ${X2_SSH:-本机}:$S2 下的进程(ssh 断了?):到那台主机上按路径停 $S2 下的进程" >&2
    [ -z "$X2_SSH" ] || ssh "${X2_SSH_OPTS[@]}" -O exit "$X2_SSH" >/dev/null 2>&1
  }
  if [ -z "$X2_SSH" ]; then
    cp "$BIN/anet" "$BIN/anet-hub" "$S2/bin/" || { no "拷不进 $S2/bin"; return 1; }
  else
    scp -q "${X2_SSH_OPTS[@]}" "$BIN/anet" "$BIN/anet-hub" "$X2_SSH:$S2/bin/" || { no "scp 到 $X2_SSH:$S2/bin 失败"; return 1; }
  fi
  info "side 1: hub1 $HUB1,请求方 $XR"
  info "side 2: ${X2_SSH:-本机} $S2,hub $HUB2X"

  # P's service: sha256 of args.text, after args.sleep seconds; every call is logged, so a paid task can
  # be shown to have run exactly once.
  x2 "$(cat <<'SNIP'
cat >"$S2/xhub-svc.py" <<'PY'
import hashlib, json, sys, time
from http.server import BaseHTTPRequestHandler, HTTPServer
LOG = sys.argv[2]
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        try:
            req = json.loads(self.rfile.read(n) or b"{}")
        except Exception:
            req = {}
        text = str(req.get("text", ""))
        with open(LOG, "a") as f:
            f.write(text + "\n")
        try:
            pause = min(float(req.get("sleep", 0)), 30.0)
        except Exception:
            pause = 0
        if pause > 0:
            time.sleep(pause)
        body = json.dumps({"digest": hashlib.sha256(text.encode()).hexdigest(), "length": len(text)}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
SNIP
)"
  x2 "( cd \"\$S2\" && exec setsid python3 \"\$S2/xhub-svc.py\" $((XB2+3)) \"\$S2/svc-calls.log\" ) >\"\$S2/svc.log\" 2>&1 </dev/null &
      for i in \$(seq 1 40); do curl -s -o /dev/null -m 2 --noproxy '*' http://127.0.0.1:$((XB2+3))/ && exit 0; sleep 0.25; done; exit 1" \
    || { no "P 的服务起不来: $(x2 'tail -3 "$S2/svc.log"')"; return 1; }

  # The two hubs, federated with each other. Side 2's hub starts once without federation to mint its
  # identity; hub1 keeps the identity it has had since section 0.
  H1=$(curl -s -m 10 --noproxy '*' "$HUB/hub/identity" | jget aid)
  [ -n "$H1" ] || H1=$(curl -s -m 10 --noproxy '*' "$HUB1/hub/identity" | jget aid)
  [ -n "$H1" ] || { no "hub1 没在跑(前面几节失败了?),第 8 节无从做起"; return 1; }
  x2 "s2_start_hub $XB2" || { no "side 2 的 hub 起不来: $(x2 'tail -2 "$S2/hub.log"')"; return 1; }
  H2X=$(curl -s -m 10 --noproxy '*' "$HUB2X/hub/identity" | jget aid)
  [ -n "$H2X" ] || { no "从本机连不上 side 2 的 hub $HUB2X"; return 1; }
  printf '{"delivery":"allowlist","discovery":"allowlist","home":"%s",\n "peers":[{"aid":"%s","endpoint":"%s"}]}\n' \
    "$HUB1" "$H2X" "$HUB2X" >"$ROOT/hub/federation.json"
  x2 "printf '%s\n' $(q "{\"delivery\":\"allowlist\",\"discovery\":\"allowlist\",\"home\":\"$HUB2X\",\"peers\":[{\"aid\":\"$H1\",\"endpoint\":\"$HUB1\"}]}") >\"\$S2/hub/federation.json\""
  # Section 7's two hubs both run from $BIN/anet-hub; stopping that path stops them and nothing else.
  stop_under "$BIN/anet-hub" 10
  local mark; mark=$(wc -l <"$ROOT/hub.log")
  hubs_restart || { no "两个 hub 以联邦配置重启失败: $(tail -2 "$ROOT/hub.log")"; return 1; }
  tail -n +$((mark + 1)) "$ROOT/hub.log" | grep -q -F "federation: delivery=allowlist peers=1" \
    && x2 "grep -q -F 'federation: delivery=allowlist peers=1' \"\$S2/hub.log\"" \
    && ok "两个 hub 互为联邦对端(hub1 $H1 · X2 $H2X)" || { no "联邦没开起来"; return 1; }

  # Side 2's providers.
  local pcfg qcfg
  pcfg=$(cat <<CFG
{"control_addr":"127.0.0.1:$((XB2+1))","hub_url":"$HUB2X","name":"NodeP",
 "caps":["xhub.digest.paid","xhub.slow.paid"],
 "modules":{"service":{"capabilities":[
   {"id":"xhub.digest.paid","url":"http://127.0.0.1:$((XB2+3))","price":$PRICE,"description":"sha256 of args.text, on the other hub"},
   {"id":"xhub.slow.paid","url":"http://127.0.0.1:$((XB2+3))","price":$PRICE,"description":"the same, after args.sleep seconds"}]}},
 "inbound":{"policy":"closed"}}
CFG
)
  qcfg=$(cat <<CFG
{"control_addr":"127.0.0.1:$((XB2+2))","hub_url":"$HUB2X","name":"NodeQ","caps":["xhub.review"],
 "inbound":{"policy":"approve"}}
CFG
)
  x2 "umask 077; printf '%s\n' $(q "$pcfg") >\"\$S2/P/.anet/config.json\"; printf '%s\n' $(q "$qcfg") >\"\$S2/Q/.anet/config.json\"
      s2_start_daemon P && s2_start_daemon Q" || { no "side 2 的 provider 起不来: $(x2 'tail -3 "$S2/P.log" "$S2/Q.log"')"; return 1; }
  ctl2 P /hub-register "{\"hub\":\"$HUB2X\",\"name\":\"NodeP\",\"caps\":[\"xhub.digest.paid\",\"xhub.slow.paid\"]}" >/dev/null
  ctl2 Q /hub-register "{\"hub\":\"$HUB2X\",\"name\":\"NodeQ\",\"caps\":[\"xhub.review\"]}" >/dev/null
  ctl2 Q /visibility '{"visibility":"federated"}' >/dev/null
  P=$(aid_of_x P); Q=$(aid_of_x Q)

  # Side 1's pure requesters: no capabilities, registered at hub1 only.
  rm -rf -- "$XR"; mkdir -p "$XR"
  local n
  for n in R R2 R3 R4; do
    mkdir -p "$XR/$n/.anet"
    cat >"$XR/$n/.anet/config.json" <<CFG
{"control_addr":"127.0.0.1:${XPORT[$n]}","hub_url":"$HUB1","name":"Node$n",
 "payments":{"auto_max":0,"agent_max":50,"agent_daily_max":500,"explicit_max":50,"daily_max":500,"payees_file":"payees.allow"}}
CFG
    x1_start "$n" || { no "$n 起不来: $(tail -3 "$XR/$n/.anet/daemon.log" 2>/dev/null)"; return 1; }
    xctl "$n" /hub-register "{\"hub\":\"$HUB1\",\"name\":\"Node$n\",\"caps\":[]}" >/dev/null
  done
  R=$(aid_of_x R); R2=$(aid_of_x R2); R3=$(aid_of_x R3); R4=$(aid_of_x R4)
  [ -n "$P" ] && [ -n "$Q" ] && [ -n "$R" ] && [ -n "$R2" ] && [ -n "$R3" ] && [ -n "$R4" ] \
    || { no "有节点没有 AID(P=$P Q=$Q R=$R R2=$R2 R3=$R3 R4=$R4)"; return 1; }
  # P takes delegations only from the four requesters, by name (written directly: the CLI asks on a TTY;
  # the daemon reads the file on every decision). R may pay P and nobody else.
  x2 "umask 077; printf '%s\n' $(q "$R") $(q "$R2") $(q "$R3") $(q "$R4") >\"\$S2/P/.anet/peers.allow\""
  _peer_add "$XR/R/.anet/payees.allow" "$P"
  local reg=0 x
  for x in "$R" "$R2" "$R3" "$R4"; do curl -sf -m 10 --noproxy '*' "$HUB1/agents/$x/kel" >/dev/null && reg=$((reg+1)); done
  for x in "$P" "$Q"; do curl -sf -m 10 --noproxy '*' "$HUB2X/agents/$x/kel" >/dev/null && reg=$((reg+1)); done
  [ "$reg" = 6 ] && ok "六个节点各自注册在自己的 hub 上(R、R2、R3、R4 在 hub1;P、Q 在 X2)" \
    || { no "只有 $reg/6 注册成功"; return 1; }
  info "P $P(hub-local,closed)"
  info "Q $Q(federated,approve)"
  info "R $R · R2 $R2 · R3 $R3 · R4 $R4(纯请求方)"
}

# ── 8.1 ─────────────────────────────────────────────────────────
xhub_local_provider(){
  hd "8.1  hub-local provider:经 /fed/v2/keys 取钥,加密委派与回复"
  local listed card keys ix word reply
  listed="$(listed_at "$HUB1/agents" "$P")/$(listed_at "$HUB1/a2a/v1/agents" "$P")"
  card=$(curl -s -o /dev/null -w '%{http_code}' -m 10 --noproxy '*' "$HUB1/a2a/v1/agents/$P/card")
  [ "$listed" = 0/0 ] && [ "$card" = 404 ] && ok "hub1 的目录与注册表里没有 P(hub-local 可见性,卡片 404)" \
    || no "hub1 看得到 P(或读不了列表):/agents 与 /a2a/v1/agents 各 $listed 处,卡片 $card"
  keys=$(curl -s -m 20 --noproxy '*' "$HUB1/agents/$P/keys" | jget aid)
  [ "$keys" = "$P" ] && ok "hub1 却能给出 P 的加密密钥 —— 只能来自向 X2 的 /fed/v2/keys 查询" \
    || no "hub1 给不出 P 的密钥"

  word="xhello-$(canary)"; reply="xreply-$(canary)"
  ix=$(delegate_goal R "$P" "$word" | jget interaction_id)
  [ -n "$ix" ] && ok "R(hub1)把任务委派给了 P(X2):$ix" || { no "R 委派 P 失败"; return; }
  if waitfor 40 in_inbox P "$ix"; then
    [ "$(inbox_trust P "$ix")" = peer ] && ok "P 收到了,按名单接受(trust=peer)" || no "P 收到了,trust=$(inbox_trust P "$ix")"
  else
    no "P 的 inbox 里没有这个任务"
  fi
  ctl2 P /tasks/reply "{\"task_id\":\"$ix\",\"text\":\"$reply\",\"state\":\"completed\"}" >/dev/null
  waitfor 40 heard_it R "$ix" "$reply" && ok "P 的回复加密回到了 R(跨 hub 纯请求方)" || no "R 没收到 P 的回复"
  waitfor 20 state_is R "$ix" completed && ok "R 那边任务 completed" || no "R 那边任务是 $(tstate R "$ix")"
  local hits; hits=$(grep_hub_dirs "$word" "$reply")
  [ "$hits" = 0 ] && ok "两个 hub 的数据目录与日志里都搜不到任务与回复的文字(hub 只搬密文)" \
    || no "hub 上有 $hits 个文件含任务文字"
}

# ── 8.2 ─────────────────────────────────────────────────────────
xhub_ttl_open(){
  hd "8.2  开三个要跨过缓存时限的任务,然后重启 provider"
  # R4 and R2 do nothing else until their task is answered: P then has sent them nothing since it first
  # recorded their keys, so its record really is as old as the wait. (R pays in 8.3/8.4, and every status
  # P sends R there can re-check R's keys once ten minutes have passed — R's reply would prove nothing.)
  TTL_R4_TEXT="xttl-$(canary)"; TTL_R2_TEXT="xttl2-$(canary)"; C2_TEXT="xc2-$(canary)"
  TTL_R4=$(delegate_goal R4 "$P" "$TTL_R4_TEXT" | jget interaction_id)
  TTL_R2=$(delegate_goal R2 "$P" "$TTL_R2_TEXT" | jget interaction_id)
  C2_IX=$(delegate_goal R "$Q" "$C2_TEXT" | jget interaction_id)
  if [ -n "$TTL_R4" ] && [ -n "$TTL_R2" ] && waitfor 40 in_inbox P "$TTL_R4" && waitfor 40 in_inbox P "$TTL_R2"; then
    ok "R4→P 与 R2→P 两个任务到了 P,等缓存时限之后再答"
  else
    no "R4→P / R2→P 的任务没到 P(${TTL_R4:-无} ${TTL_R2:-无})"
  fi
  if [ -n "$C2_IX" ] && waitfor 40 is_held Q "$C2_IX"; then
    [ "$(held_by Q "$C2_IX")" = "$R" ] && ok "R→Q 进了 Q 的待批队列(approve)" || no "Q 的待批项请求方不是 R"
    if in_inbox Q "$C2_IX"; then no "待批的任务不该出现在 Q 的 inbox 里"; else ok "待批期间 Q 的 inbox 里没有它"; fi
  else
    no "R→Q 没进 Q 的待批队列"
  fi
  waitfor 30 said_meta_n R "$C2_IX" anet.inbound pending_approval \
    && ok "R 被告知在等 Q 的运营者批准(anet.inbound=pending_approval)" || no "R 没收到待批通知"
  x2 "s2_stop_daemon P; s2_stop_daemon Q; s2_start_daemon P && s2_start_daemon Q" \
    && ok "P 与 Q 重启了(进程内缓存全没了)" || no "P/Q 重启失败"
  waitfor 20 is_held Q "$C2_IX" && ok "重启后待批项还在" || no "重启后待批项没了"
  T0=$(date +%s)
  info "缓存时限计时开始;$CACHE_WAIT 秒后答这三个任务,其间做 8.3、8.4"
}

# ── 8.3 ─────────────────────────────────────────────────────────
xhub_pay(){
  hd "8.3  跨 hub 付费:负面用例在前,正确付款只产生一份 credit(同一 ix)"
  local text ix req rb0 pb0 rb pb bind under wrongpayee r n s good
  text="xpay-$(canary)"
  ix=$(delegate_cap R "$P" xhub.digest.paid "{\"text\":\"$text\"}")
  PAY_IX=$ix
  [ -n "$ix" ] || { no "R 委派 P 的标价能力失败"; return; }
  waitfor 40 quoted R "$ix" && ok "P 报了价,R 的任务停在 payment-required(支出档位不自动付)" \
    || { no "R 没收到报价($(tmeta R "$ix" x402.payment.status))"; return; }
  req=$(tmeta R "$ix" x402.payment.required | python3 -c '
import sys, json
pr = json.load(sys.stdin)
for o in pr.get("accepts") or []:
    if o.get("network") == sys.argv[1]:
        print(json.dumps({k: o[k] for k in ("scheme", "network", "amount", "asset", "payTo", "maxTimeoutSeconds") if k in o}))
        break' "hub:$H1")
  if [ -n "$req" ] && [ "$(printf '%s' "$req" | jget payTo)" = "$P" ] && [ "$(printf '%s' "$req" | jget amount)" = "$PRICE" ]; then
    ok "报价里有 R 的账本 hub:$H1 这条 rail(收款方 P,$PRICE credit)—— 跨 hub 才付得了"
  else
    no "报价里没有 R 的账本可付:$(tmeta R "$ix" x402.payment.required | head -c 300)"; return
  fi
  rb0=$(bal R); pb0=$(bal P)
  info "付款前:R=$rb0(hub1 账上)P=$pb0(X2 账上)"

  # The hubs, each on its own (SI-9, §8.5), with authorizations signed by R's own key and bound to this
  # task. The ledger hub hub1 is asked directly. The entry hub X2 is asked while hub1 is stopped: X2 hands a
  # refusal from the ledger hub back as its answer, so with hub1 up an X2 that skipped its own check would
  # answer exactly the same; with hub1 down, forwarding can only end in settlement_pending, and
  # invalid_amount / payee_mismatch can only be X2's own.
  n=$(nonce_of R "$ix")
  [ -n "$n" ] || { no "读不到 R 这个任务的 task nonce(interactions.db)"; return; }
  bind=$(pay_bind "$ix" "$n")
  under=$(authorize $((PRICE - 15)) "$P" "$bind")
  wrongpayee=$(authorize "$PRICE" "$Q" "$bind")
  [ -n "$under" ] && [ -n "$wrongpayee" ] || { no "anetfixture x402-authorize 签不出授权"; return; }
  r=$(settle_at "$HUB1" "$under" "$req");       [ "$r" = "false invalid_amount" ] && ok "账本 hub hub1 拒了少付(invalid_amount)" || no "hub1 对少付答 $r"
  r=$(settle_at "$HUB1" "$wrongpayee" "$req");  [ "$r" = "false payee_mismatch" ] && ok "账本 hub hub1 拒了错收款方(payee_mismatch)" || no "hub1 对错收款方答 $r"
  hub1_stop
  if http_up "$HUB1/healthz"; then
    no "hub1 停不下来:入口 hub 自己的核对无从单独验证"
  else
    r=$(settle_at "$HUB2X" "$under" "$req")
    [ "$r" = "false invalid_amount" ] && ok "入口 hub X2 自己拒了少付(账本 hub 停着,这个拒绝不是转来的)" || no "账本 hub 停着时 X2 对少付答 $r"
    r=$(settle_at "$HUB2X" "$wrongpayee" "$req")
    [ "$r" = "false payee_mismatch" ] && ok "入口 hub X2 自己拒了错收款方(payee_mismatch)" || no "账本 hub 停着时 X2 对错收款方答 $r"
  fi
  hub1_start || { no "hub1 重启失败"; return; }

  # The provider's merchant check (§8.4), on the same task: payment messages from R that its daemon would
  # never sign. Each is refused with its code and the quote stands. The hub would refuse the first two as
  # well, and P would pass that refusal on with the same code; that P refused them itself, before
  # presenting anything, is in its log ("refused before settlement"). The third pays the right payee the
  # right amount but is bound to other work: a hub settles that one, only the payee can tell.
  local mark stolen
  mark=$(p_log_mark)
  [ "$(pay_as_r "$ix" "$under")" = 200 ] || no "少付的付款消息没送进 hub1"
  waitfor 40 said_meta_n R "$ix" x402.payment.error INVALID_AMOUNT \
    && p_logged "$mark" "$ix: payment refused before settlement: invalid_amount" \
    && ok "P 的商户核对自己拒了少付(没交给 hub):payment-failed / INVALID_AMOUNT,报价仍在" \
    || no "少付:R 没收到 INVALID_AMOUNT,或 P 不是自己拒的"
  [ "$(pay_as_r "$ix" "$wrongpayee")" = 200 ] || no "错收款方的付款消息没送进 hub1"
  waitfor 40 said_meta_n R "$ix" anet.reason payee_mismatch \
    && [ "$(said_meta R "$ix" x402.payment.error SETTLEMENT_FAILED)" -ge 1 ] \
    && p_logged "$mark" "$ix: payment refused before settlement: payee_mismatch" \
    && ok "P 自己拒了错收款方:SETTLEMENT_FAILED / anet.reason=payee_mismatch" \
    || no "错收款方:R 没收到 payee_mismatch,或 P 不是自己拒的"
  stolen=$(authorize "$PRICE" "$P" "$(pay_bind "$ix" "other-work-$n")")
  [ -n "$stolen" ] && [ "$(pay_as_r "$ix" "$stolen")" = 200 ] || no "绑定到别的活的付款消息没送进 hub1"
  waitfor 40 said_meta_n R "$ix" anet.reason binding_mismatch \
    && p_logged "$mark" "$ix: payment refused before settlement: binding_mismatch" \
    && ok "P 拒了绑定到别的活的授权(收款方、金额都对,hub 会收):SETTLEMENT_FAILED / binding_mismatch" \
    || no "挪用:R 没收到 binding_mismatch,或 P 不是自己拒的"
  rb=$(bal R); pb=$(bal P)
  [ "$rb" = "$rb0" ] && [ "$pb" = "$pb0" ] && [ "$(svc_calls "$text")" = 0 ] \
    && ok "七次被拒之后谁的余额都没动,活也没干" || no "被拒之后余额 R $rb0→$rb P $pb0→$pb,服务调用 $(svc_calls "$text") 次"

  # Now the real payment, from R's daemon (task-agent tier), on the same task.
  s=$(nctl R /tasks/pay "{\"task_id\":\"$ix\",\"decision\":\"submit\"}" | jget x402.payment.status)
  [ "$s" = payment-submitted ] && ok "R 按 agent 档签了授权,同一任务上发出 payment-submitted" || no "R 付款答 $s"
  waitfor 90 state_is R "$ix" completed && ok "结算后活干完了,结果回到 R(completed)" || no "R 的任务是 $(tstate R "$ix")"
  rb=$(bal R); pb=$(bal P)
  [ "$((rb0 - rb))" = "$PRICE" ] && ok "R 在 hub1 上扣了 $PRICE" || no "R $rb0 → $rb"
  [ "$((pb - pb0))" = "$PRICE" ] && ok "P 在 X2 上进了 $PRICE(X2 与 hub1 之间清算)" || no "P $pb0 → $pb"
  set -- $(settled R "$ix"); [ "${1:-}" = 1 ] && [ "${2:-}" = 1 ] && ok "R 的证据链上恰好一笔结算,且验过 hub 签名" || no "R 的结算记录:$*"
  set -- $(settled P "$ix"); [ "${1:-}" = 1 ] && ok "P 的证据链上恰好一笔结算" || no "P 的结算记录:$*"
  [ "$(svc_calls "$text")" = 1 ] && ok "活只干了一次" || no "服务被调用 $(svc_calls "$text") 次"

  # Only one credit, however it is asked for again.
  good=$(thread_of R "$ix" | python3 -c '
import sys, json
t = json.load(sys.stdin).get("thread") or {}
for m in reversed(t.get("messages") or []):
    md = m.get("metadata")
    if m.get("from") == "me" and isinstance(md, dict) and md.get("x402.payment.status") == "payment-submitted":
        print(json.dumps(md.get("x402.payment.payload"))); break')
  if [ -n "$good" ] && [ "$good" != null ]; then
    r=$(settle_at "$HUB2X" "$good" "$req")
    [ "${r%% *}" = true ] && ok "同一授权再交一次:X2 给回原结算(幂等)" || no "重放同一授权答 $r"
    r=$(settle_at "$HUB2X" "$(authorize "$PRICE" "$P" "$bind")" "$req")
    [ "$r" = "false duplicate_binding" ] && ok "同一任务另签一张授权:账本 hub 拒绝(duplicate_binding)" || no "同绑定第二张授权答 $r"
    [ "$(pay_as_r "$ix" "$good")" = 200 ] || no "重发的付款消息没送进 hub1"
    sleep 5
  else
    no "R 的记录里找不到它发出的付款"
  fi
  rb=$(bal R); pb=$(bal P)
  [ "$((rb0 - rb))" = "$PRICE" ] && [ "$((pb - pb0))" = "$PRICE" ] && [ "$(svc_calls "$text")" = 1 ] \
    && ok "重放、同绑定第二张、重发付款消息之后,仍只有一份 credit、一次执行" \
    || no "之后 R $rb0→$rb P $pb0→$pb,服务 $(svc_calls "$text") 次"
  set -- $(settled P "$ix"); [ "${1:-}" = 1 ] || no "P 的证据链上多出了结算:$*"
}

# ── 8.4 ─────────────────────────────────────────────────────────
# C34: the payer's evidence chain holds the settlement, whichever way a cancel and a payment cross.
xhub_cancel_orders(){
  hd "8.4  取消与付款的三种顺序(C34)"
  local rb0 pb0 rb pb a b c s e ta tc
  rb0=$(bal R); pb0=$(bal P)

  # (a) Cancel after payment-submitted, before the settlement. P is offline while R pays and cancels, so
  #     both wait in P's mailbox in that order; the ledger hub (hub1) is stopped when P takes the payment,
  #     so the settlement is still unknown when P reads the cancel.
  ta="c34a-$(canary)"
  a=$(delegate_cap R "$P" xhub.digest.paid "{\"text\":\"$ta\"}")
  if [ -n "$a" ] && waitfor 40 quoted R "$a"; then
    x2 "s2_stop_daemon P"
    s=$(nctl R /tasks/pay "{\"task_id\":\"$a\",\"decision\":\"submit\"}" | jget x402.payment.status)
    # C25: one authorization in flight per task; the requester does not sign a second before the first has
    # a definite outcome.
    e=$(nctl R /tasks/pay "{\"task_id\":\"$a\",\"decision\":\"submit\"}" | jget error)
    case "$e" in *"no outcome yet"*) ok "(a) 付款未定时 R 拒绝再签一张授权(C25)" ;; *) no "(a) 第二次付款答:${e:-成功了}" ;; esac
    nctl R /tasks/cancel "{\"task_id\":\"$a\"}" >/dev/null
    [ "$s" = payment-submitted ] && [ "$(tmeta R "$a" anet.cancel_requested)" = true ] && state_is R "$a" working \
      && ok "(a) 付款已提交后取消:R 的任务不变(working),记下 anet.cancel_requested" \
      || no "(a) R:付款 $s,状态 $(tstate R "$a"),cancel_requested=$(tmeta R "$a" anet.cancel_requested)"
    grep -q "$a: .*queued for delivery" "$XR/R/.anet/daemon.log" 2>/dev/null \
      && no "(a) R 的付款或取消没能当场送到 X2(顺序不再确定)"
    hub1_stop
    local amark; amark=$(p_log_mark)
    x2 "s2_start_daemon P" >/dev/null
    if waitfor 60 got_kind P "$a" cancel; then
      [ "$(tmeta P "$a" x402.payment.status)" = payment-submitted ] && ! state_is P "$a" canceled \
        && ok "(a) P 先收付款、结算未定(账本 hub 停着),再收取消:任务不取消" \
        || no "(a) P:状态 $(tstate P "$a"),付款 $(tmeta P "$a" x402.payment.status)"
      # Without this the case would pass just the same if hub1 had not really been down: the settlement
      # would simply have gone through at once, and C25's unknown outcome would never have happened.
      p_logged "$amark" "$a: settlement outcome not known yet" \
        && ok "(a) P 的第一次结算没有结果(入口 hub 连不上账本 hub),之后用同一授权重试(C25)" \
        || no "(a) P 的日志里没有'结算结果未知':结算未知这条路没走到"
    else
      no "(a) P 没收到取消"
    fi
    hub1_start || no "hub1 重启失败"
    waitfor 240 state_is R "$a" completed && ok "(a) hub1 回来后结算完成,结果送到 R" || no "(a) R 的任务是 $(tstate R "$a")"
    set -- $(settled R "$a"); [ "${1:-}" = 1 ] && [ "${2:-}" = 1 ] && ok "(a) R 的证据链上有这笔结算(一笔,验过)" || no "(a) R 的结算记录:$*"
    set -- $(settled P "$a"); [ "${1:-}" = 1 ] && [ "$(svc_calls "$ta")" = 1 ] \
      && ok "(a) 结算未知后重试:P 只结算一笔、只干一次(C25)" || no "(a) P 的结算记录 $*,服务调用 $(svc_calls "$ta") 次"
  else
    no "(a) 没拿到报价"
  fi

  # (b) Cancel after the settlement, before the result: the capability takes 15 s after payment-verified.
  b=$(delegate_cap R "$P" xhub.slow.paid "{\"text\":\"c34b-$(canary)\",\"sleep\":15}")
  if [ -n "$b" ] && waitfor 40 quoted R "$b"; then
    nctl R /tasks/pay "{\"task_id\":\"$b\",\"decision\":\"submit\"}" >/dev/null
    if waitfor 40 settled_verified R "$b"; then
      nctl R /tasks/cancel "{\"task_id\":\"$b\"}" >/dev/null
      [ "$(tmeta R "$b" anet.cancel_requested)" = true ] && state_is R "$b" working \
        && ok "(b) 结算后、结果前取消:R 的任务不变,记下 anet.cancel_requested" \
        || no "(b) R:状态 $(tstate R "$b"),cancel_requested=$(tmeta R "$b" anet.cancel_requested)"
    else
      no "(b) R 没在结果之前看到结算"
    fi
    waitfor 90 state_is R "$b" completed && ok "(b) 结果照常送到(completed)" || no "(b) R 的任务是 $(tstate R "$b")"
    set -- $(settled R "$b"); [ "${1:-}" = 1 ] && [ "${2:-}" = 1 ] && ok "(b) R 的证据链上有这笔结算(一笔,验过)" || no "(b) R 的结算记录:$*"
  else
    no "(b) 没拿到报价"
  fi

  # (c) Canceled here, then the result arrives. The daemon has no path to this state once a payment is
  #     submitted (§4.2 keeps a paid task open, and a deny sweep skips it), so the harness writes it the way
  #     the daemon's unit test does (x402task_test.go TestCancelAndPaymentRaces), with R stopped: the task
  #     "ended here while its payment was on its way". What is under test is R's receive path.
  tc="c34c-$(canary)"
  c=$(delegate_cap R "$P" xhub.digest.paid "{\"text\":\"$tc\"}")
  if [ -n "$c" ] && waitfor 40 quoted R "$c"; then
    x2 "s2_stop_daemon P"
    nctl R /tasks/pay "{\"task_id\":\"$c\",\"decision\":\"submit\"}" >/dev/null
    x1_stop R
    python3 - "$XR/R/.anet/interactions/interactions.db" "$c" <<'PY'
import sqlite3, sys, time, datetime
db = sqlite3.connect(sys.argv[1], timeout=15)
now = int(time.time() * 1000)
stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")
db.execute("UPDATE interaction SET state='canceled', state_at=?, state_seq=state_seq+1, updated_at=? "
           "WHERE id=? AND state NOT IN ('completed','failed','canceled','rejected')", (now, stamp, sys.argv[2]))
db.commit()
PY
    x1_start R || no "(c) R 重启失败"
    state_is R "$c" canceled && ok "(c) 付款在路上时,R 这边任务已经是 canceled" || no "(c) R 的任务是 $(tstate R "$c")"
    x2 "s2_start_daemon P" >/dev/null
    if waitfor 120 settled_any R "$c"; then
      set -- $(settled R "$c")
      [ "${1:-}" = 1 ] && [ "${2:-}" = 1 ] && [ "${3:-}" = 1 ] \
        && ok "(c) 结果到了已取消的任务:R 仍验证并记下这笔结算(after_terminal)" || no "(c) R 的结算记录:$*"
    else
      no "(c) R 的证据链上没有这笔结算"
    fi
    state_is R "$c" canceled && ok "(c) 任务没有因此重开(仍是 canceled)" || no "(c) R 的任务变成了 $(tstate R "$c")"
    waitfor 30 svc_ran "$tc"; [ "$(svc_calls "$tc")" = 1 ] && ok "(c) 付过的活照常干了一次" || no "(c) 服务调用 $(svc_calls "$tc") 次"
  else
    no "(c) 没拿到报价"
  fi
  rb=$(bal R); pb=$(bal P)
  [ "$((rb0 - rb))" = "$((3 * PRICE))" ] && [ "$((pb - pb0))" = "$((3 * PRICE))" ] \
    && ok "三种顺序各结算一次:R 共扣 $((3 * PRICE)),P 共进 $((3 * PRICE))" || no "三种顺序之后 R $rb0→$rb P $pb0→$pb"
}

# ── 8.5 ─────────────────────────────────────────────────────────
xhub_ttl_close(){
  hd "8.5  超过缓存时限之后回复(provider 重启过)与待批项批准(C2)"
  local left reply c2reply fedq mark
  left=$((T0 + CACHE_WAIT - $(date +%s)))
  if [ "$left" -gt 0 ]; then info "等缓存时限:还有 $left 秒"; sleep "$left"; fi
  # Visibility, now that the directories have long had time to sync: Q (federated) crossed, P did not.
  fedq=$(curl -s -m 10 --noproxy '*' "$HUB1/agents" | python3 -c '
import sys, json
ags = {a.get("aid") for a in json.load(sys.stdin).get("agents") or []}
print(("Q" if sys.argv[1] in ags else "") + ("P" if sys.argv[2] in ags else ""))' "$Q" "$P")
  [ "$fedq" = Q ] && ok "hub1 的目录里有 Q(federated),没有 P(hub-local)" || no "hub1 的目录:'$fedq'(期望只有 Q)"
  # The reply after the cache time. That it arrives is the case; that it really came after the cache time,
  # and that the re-check it set off went through /fed/v2/keys, are read from P's own record of R4's keys:
  # older than ten minutes before, moved forward after. (A "no failure in the log" check passed just as
  # well when no re-check happened at all.)
  local kc age
  kc=$(p_keys_checked "$R4"); kc=${kc%% *}
  age=$(keys_stale "$R4") && ok "P 上次核实 R4 的密钥是 $age 秒前,超过 10 分钟的缓存时限(其间 P 重启过)" \
    || no "P 对 R4 密钥的记录只有 ${age:-?} 秒(keys_checked_at=${kc:-无}):这次回复不在缓存时限之后"
  reply="xttl-reply-$(canary)"
  mark=$(p_log_mark)
  ctl2 P /tasks/reply "{\"task_id\":\"$TTL_R4\",\"text\":\"$reply\",\"state\":\"completed\"}" >/dev/null
  waitfor 60 heard_it R4 "$TTL_R4" "$reply" && ok "P 重启后、超过缓存时限才回复,仍送到了跨 hub 的纯请求方 R4" \
    || no "R4 没收到超时限的回复"
  if waitfor 30 keys_rechecked R4 "${kc:-0}"; then
    ok "这次回复触发的密钥复核成功了:X2 经 /fed/v2/keys 向 hub1 取到 R4 的密钥(keys_checked_at 前进)"
  else
    no "P 没有重新核实 R4 的密钥(keys_checked_at 未前进)$(p_logged "$mark" "revalidating $R4's keys" && echo ',日志里复核失败')"
  fi
  [ "$(curl -s -m 20 --noproxy '*' "$HUB2X/agents/$R2/keys" | jget aid)" = "$R2" ] \
    && ok "反方向也通:X2 给得出 hub1 上纯请求方 R2 的密钥" || no "X2 给不出 R2 的密钥"

  c2reply="xc2-reply-$(canary)"
  # The held item came in before T0 (8.2), so it has waited at least this long.
  [ $(( $(date +%s) - T0 )) -ge 600 ] && ok "待批项已挂了 $(( $(date +%s) - T0 )) 秒以上(超过缓存时限),其间 Q 重启过" \
    || no "待批项只挂了 $(( $(date +%s) - T0 )) 秒:不在缓存时限之后"
  [ "$(ctl2 Q /inbound/approve "{\"interaction_id\":\"$C2_IX\"}" | jget status)" = approved ] \
    && ok "Q 的运营者在缓存时限之后批准了待批项" || no "Q 批准失败"
  ctl2 Q /tasks/reply "{\"task_id\":\"$C2_IX\",\"text\":\"$c2reply\",\"state\":\"completed\"}" >/dev/null
  waitfor 60 heard_it R "$C2_IX" "$c2reply" && ok "Q 的回复送到了跨 hub 的纯请求方 R(C2)" || no "R 没收到 Q 的回复"
  waitfor 20 state_is R "$C2_IX" completed && ok "R 那边这个任务 completed" || no "R 那边是 $(tstate R "$C2_IX")"
}

# ── 8.6 ─────────────────────────────────────────────────────────
xhub_c32(){
  hd "8.6  C32 mutation:两个 hub 关掉 /fed/v2/keys 查询(-test-no-fed-key-lookup)"
  local reply out ix mark hmark kc age
  hmark=$(wc -l <"$ROOT/hub.log")
  hubs_restart -test-no-fed-key-lookup || { no "两个 hub 带开关重启失败: $(tail -2 "$ROOT/hub.log")"; return; }
  tail -n +$((hmark + 1)) "$ROOT/hub.log" | grep -q -F "TEST MODE (-test-no-fed-key-lookup)" \
    && x2 "grep -q -F 'TEST MODE (-test-no-fed-key-lookup)' \"\$S2/hub.log\"" \
    && ok "两个 hub 以测试开关重启(日志写明 TEST MODE)" || { no "hub 没认这个开关(二进制太旧?)"; return; }
  # The reply after the cache time: its key refresh goes through the lookup, and fails; the reply is sealed
  # with the stored set and still arrives (§3.5 step 1 [C2]). Only a reply past the cache time asks the hub
  # at all, so that is checked first, on P's own record of R2's keys.
  kc=$(p_keys_checked "$R2"); kc=${kc%% *}
  age=$(keys_stale "$R2") && ok "P 上次核实 R2 的密钥是 $age 秒前,超过缓存时限" \
    || no "P 对 R2 密钥的记录只有 ${age:-?} 秒(keys_checked_at=${kc:-无}):这次回复不会去问 hub,mutation 测不到"
  reply="xttl2-reply-$(canary)"
  mark=$(p_log_mark)
  ctl2 P /tasks/reply "{\"task_id\":\"$TTL_R2\",\"text\":\"$reply\",\"state\":\"completed\"}" >/dev/null
  if waitfor 30 p_logged "$mark" "revalidating $R2's keys"; then
    ok "超时限回复:P 复核 R2 密钥的那次查询失败了(mutation 生效)"
  else
    no "超时限回复:P 没有因为关掉查询而复核失败"
  fi
  keys_rechecked R2 "${kc:-0}" && no "关掉查询后 P 仍刷新了 R2 的密钥记录(密钥从哪来的?)" \
    || ok "……P 对 R2 的记录没有刷新(8.5 里同样的复核在查询开着时刷新了 R4 的)"
  waitfor 60 heard_it R2 "$TTL_R2" "$reply" && ok "……回复仍以已存密钥送到 R2(§3.5 [C2]:hub 404 时继续用已存)" \
    || no "关掉查询后 R2 没收到回复"
  # First contact with the hub-local provider: no keys, nothing sent.
  [ "$(curl -s -o /dev/null -w '%{http_code}' -m 20 --noproxy '*' "$HUB1/agents/$P/keys")" = 404 ] \
    && ok "hub1 再给不出 P 的密钥(404)" || no "关掉查询后 hub1 仍给出 P 的密钥"
  out=$(delegate_goal R3 "$P" "xc32-$(canary)")
  # Refused for that reason and no other: the daemon names the hub's answer ("… keys rejected (404): …"),
  # and a 429 or a hub that is down would fail the delegation too.
  case "$(printf '%s' "$out" | jget error)" in
    *"encryption keys"*"(404)"*) ok "首次联系 hub-local 的 P:hub1 答 404,R3 封不了信封,委派失败" ;;
    *) no "关掉查询后 R3→P 答:$(printf '%s' "$out" | head -c 200)" ;;
  esac
  # The mutation is that one lookup: Q, whose card and keys federate, is still reachable.
  ix=$(delegate_goal R3 "$Q" "xc32q-$(canary)" | jget interaction_id)
  [ -n "$ix" ] && waitfor 40 is_held Q "$ix" && ok "对照:federated 的 Q 仍可达(密钥随联邦卡片来)" \
    || no "关掉查询后连 Q 也不可达"
  hubs_restart || { no "两个 hub 去掉开关重启失败"; return; }
  ix=$(delegate_goal R3 "$P" "xc32p-$(canary)" | jget interaction_id)
  [ -n "$ix" ] && waitfor 40 in_inbox P "$ix" && ok "去掉开关后 R3→P 立即恢复:差别只来自 /fed/v2/keys 查询" \
    || no "去掉开关后 R3→P 仍不通"
}

if xhub_setup; then
  xhub_local_provider
  xhub_ttl_open
  xhub_pay
  xhub_cancel_orders
  xhub_ttl_close
  xhub_c32
fi
fi

printf '\n\033[1m── %d 通过, %d 失败 ──\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
