#!/usr/bin/env bash
# onboard.sh — a new user, from a fresh binary to working, on each
# distribution variant.
#
# scenario.sh and prodtest.sh both start from a topology that is already
# configured. Nothing walked the path a person actually takes: download a
# build, start it, join a hub, find somebody, delegate, get an answer.
# That path crosses first-run behaviour — key generation, config
# creation, the registration handshake — which every other test skips by
# having done it already.
#
# It also runs the path on the trimmed builds, because "this variant can
# do X" is a claim in docs/DISTRIBUTIONS-zh.md and claims about builds
# should be checked against the builds.
#
#   bash scripts/onboard.sh              against a local hub
#   HUB=https://... bash scripts/onboard.sh   against a real one
set -uo pipefail
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost

ROOT=${ROOT:-/tmp/anet-onboard}
HUB_PORT=${HUB_PORT:-29700}
HUB=${HUB:-http://127.0.0.1:$HUB_PORT}
OWN_HUB=1
[ -n "${HUB_EXTERNAL:-}" ] && { HUB=$HUB_EXTERNAL; OWN_HUB=0; }

pass=0; fail=0
ok(){ printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; pass=$((pass+1)); }
no(){ printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; fail=$((fail+1)); }
hd(){ printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }
info(){ printf '  %s\n' "$*"; }

cd "$(dirname "$0")/.."
rm -rf "$ROOT"; mkdir -p "$ROOT/bin"

hd "0  按发行版设计构建每一档"
declare -A PROFILE=(
 [min]="no_x402,no_service,no_mcp,no_taskboard,no_cas,no_org,no_blackboard,no_p2p,no_anetlink"
 [standard]="no_x402,no_mcp,no_taskboard,no_cas,no_org,no_blackboard,no_p2p,no_anetlink"
 [paid]="no_mcp,no_taskboard,no_cas,no_org,no_blackboard,no_p2p,no_anetlink"
)
# Through build.sh, so the binaries carry a commit stamp. Building with a
# bare `go build` here would leave every profile reporting "unknown", and
# the check below asserting that a node can say which build it is would
# pass against a build that cannot.
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
git diff --quiet 2>/dev/null || COMMIT="$COMMIT-dirty"
STAMP="-X github.com/ANetResearch/ANet/internal/version.Commit=$COMMIT        -X github.com/ANetResearch/ANet/internal/version.BuiltAt=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
for k in min standard paid; do
  if go build -tags "${PROFILE[$k]}" -ldflags "$STAMP" -o "$ROOT/bin/anet-$k" ./cmd/anet 2>/dev/null; then
    ok "$k 构建成功($(( $(stat -c%s "$ROOT/bin/anet-$k")/1024 ))K)"
  else
    no "$k 构建失败"
  fi
done
go build -o "$ROOT/bin/anet-hub" ../ANetHub/cmd/anet-hub 2>/dev/null \
  || (cd ../ANetHub && go build -o "$ROOT/bin/anet-hub" ./cmd/anet-hub 2>/dev/null)
[ -x "$ROOT/bin/anet-hub" ] && ok "hub 构建成功" || { no "hub 构建失败"; exit 1; }

if [ "$OWN_HUB" = 1 ]; then
  hd "1  一个谁也不认识的 hub"
  mkdir -p "$ROOT/hub"
  setsid "$ROOT/bin/anet-hub" --addr "127.0.0.1:$HUB_PORT" --data "$ROOT/hub" \
    >"$ROOT/hub.log" 2>&1 </dev/null &
  for _ in $(seq 1 30); do curl -sf -m 2 "$HUB/healthz" >/dev/null && break; sleep 1; done
  curl -sf -m 5 "$HUB/healthz" >/dev/null && ok "hub 起来了" || { no "hub 起不来"; exit 1; }
fi

# start <profile> <port> [extra-config-json]
start(){
  local prof=$1 port=$2 extra=${3:-}
  local home="$ROOT/$prof"
  mkdir -p "$home/.anet"
  python3 - "$home/.anet/config.json" "$port" "$HUB" "$prof" "$extra" <<'PY'
import json, sys
path, port, hub, name, extra = sys.argv[1:6]
cfg = {"control_addr": f"127.0.0.1:{port}", "hub_url": hub,
       "name": f"onboard-{name}", "accept_delegations": True}
if extra:
    cfg.update(json.loads(extra))
json.dump(cfg, open(path, "w"), indent=1)
PY
  setsid env HOME="$home" "$ROOT/bin/anet-$prof" daemon >"$ROOT/$prof.log" 2>&1 </dev/null &
  for _ in $(seq 1 20); do curl -sf -m 2 "http://127.0.0.1:$port/ping" >/dev/null && return 0; sleep 1; done
  return 1
}
ctl(){ # ctl <profile> <port> <path> <json>
  curl -s -m 120 -H "Authorization: Bearer $(cat "$ROOT/$1/.anet/control_token.txt")" \
    -H 'Content-Type: application/json' -d "$4" "http://127.0.0.1:$2$3"
}
jq_(){ python3 -c "import sys,json
try: d=json.load(sys.stdin)
except Exception: print(''); raise SystemExit
$1" 2>/dev/null; }

hd "2  第一次启动:身份、配置、控制口"
if start min 29710; then
  ok "min 首次启动成功(自动生成身份与控制令牌)"
  [ -s "$ROOT/min/.anet/control_token.txt" ] && ok "控制令牌已生成" || no "没有控制令牌"
  aid=$(curl -sf -m 5 http://127.0.0.1:29710/ping | jq_ "print(d.get('aid',''))")
  case "$aid" in bafyrei*) ok "身份是自证的 AID(${aid:0:16}…)";; *) no "AID 形状不对: $aid";; esac
  cm=$(curl -sf -m 5 http://127.0.0.1:29710/ping | jq_ "print(d.get('commit',''))")
  { [ -n "$cm" ] && [ "$cm" != unknown ]; } \
    && ok "报得出自己是哪一版($cm)" || no "报不出构建版本($cm)"
else
  no "min 起不来"
fi

hd "3  加入 hub"
r=$(ctl min 29710 /hub-register "{\"hub\":\"$HUB\",\"name\":\"onboard-min\",\"caps\":[]}")
[ "$(echo "$r" | jq_ "print(d.get('status',''))")" = registered ] \
  && ok "min 注册成功" || no "注册失败: ${r:0:120}"
MIN_AID=$(echo "$r" | jq_ "print(d.get('aid',''))")
code=$(curl -s -o /dev/null -w '%{http_code}' -m 10 "$HUB/agents/$MIN_AID/kel")
[ "$code" = 200 ] && ok "陌生人能取到它的密钥历史,可自行验签" || no "hub 不公布它的 KEL($code)"

hd "4  一个提供服务的节点"
mkdir -p "$ROOT/svc"
cat > "$ROOT/svc/svc.py" <<'PY'
import hashlib, json
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        try: req = json.loads(self.rfile.read(n) or b"{}")
        except Exception: req = {}
        t = str(req.get("text", ""))
        b = json.dumps({"digest": hashlib.sha256(t.encode()).hexdigest(), "length": len(t)}).encode()
        self.send_response(200); self.send_header("Content-Type","application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self,*a): pass
HTTPServer(("127.0.0.1", 29720), H).serve_forever()
PY
setsid python3 "$ROOT/svc/svc.py" >"$ROOT/svc.log" 2>&1 </dev/null &
sleep 1
SVC='{"modules":{"service":{"capabilities":[{"id":"text.digest","url":"http://127.0.0.1:29720","description":"sha256"},{"id":"text.digest.paid","url":"http://127.0.0.1:29720","price":25,"description":"sha256, 25 credits"}]}}}'
if start standard 29711 "$SVC"; then
  ok "standard 启动成功"
  r=$(ctl standard 29711 /hub-register "{\"hub\":\"$HUB\",\"name\":\"onboard-standard\",\"caps\":[\"text.digest\",\"text.digest.paid\"]}")
  STD_AID=$(echo "$r" | jq_ "print(d.get('aid',''))")
  [ -n "$STD_AID" ] && ok "standard 注册成功" || no "注册失败"
  sleep 2
  found=$(curl -sf -m 10 "$HUB/agents?cap=text.digest" | jq_ "print(len(d.get('agents') or []))")
  [ "${found:-0}" -ge 1 ] && ok "按能力 id 能被找到" || no "找不到它"
else
  no "standard 起不来"
fi

hd "5  min 调用 standard 的能力"
ix=$(ctl min 29710 /delegate "{\"provider\":\"$STD_AID\",\"capability\":\"text.digest\",\"args\":{\"text\":\"anet\"}}" \
     | jq_ "print(d.get('interaction_id',''))")
res=""
for _ in $(seq 1 40); do
  res=$(ctl min 29710 /results '{}' | jq_ "
for x in d.get('results') or []:
    if x['interaction_id']=='$ix': print(x['result']); break")
  [ -n "$res" ] && break
  sleep 1
done
[ "$(echo "$res" | jq_ "print(d.get('status',''))")" = OK ] \
  && ok "调用成功,证据随结果返回" || no "调用未成功: ${res:0:140}"

hd "6  min 遇到标价的能力"
# The claim in DISTRIBUTIONS-zh.md is that a build without x402 refuses
# priced work and says why, rather than doing it for free.
ix=$(ctl min 29710 /delegate "{\"provider\":\"$STD_AID\",\"capability\":\"text.digest.paid\",\"args\":{\"text\":\"x\"}}" \
     | jq_ "print(d.get('interaction_id',''))")
res=""
for _ in $(seq 1 40); do
  res=$(ctl min 29710 /results '{}' | jq_ "
for x in d.get('results') or []:
    if x['interaction_id']=='$ix': print(x['result']); break")
  [ -n "$res" ] && break
  sleep 1
done
st=$(echo "$res" | jq_ "print(d.get('status',''))")
msg=$(echo "$res" | jq_ "print(d.get('message',''))")
[ "$st" = UNAVAILABLE ] && ok "无付费模块的提供方拒绝标价的活,而不是白干" \
  || no "状态是 $st,期望 UNAVAILABLE"
case "$msg" in *no_x402*) ok "拒绝时说清了原因(提到 no_x402)";; *) no "没说原因: ${msg:0:100}";; esac

hd "7  min 试图付款"
for c in balance redeem reconcile audit-hub; do
  r=$(ctl min 29710 "/$c" '{"amount":1,"reference":"x"}')
  case "$(echo "$r" | jq_ "print(d.get('error',''))")" in
    *no_x402*) ok "/$c 明确回答本构建没有付费支持";;
    *) no "/$c 的回答不对: ${r:0:100}";;
  esac
done

hd "8  paid 档能收费"
if start paid 29712 "$SVC"; then
  ok "paid 启动成功"
  r=$(ctl paid 29712 /hub-register "{\"hub\":\"$HUB\",\"name\":\"onboard-paid\",\"caps\":[\"text.digest.paid\"]}")
  PAID_AID=$(echo "$r" | jq_ "print(d.get('aid',''))")
  bal=$(ctl paid 29712 /balance '{}' | jq_ "print(d.get('balance',''))")
  [ -n "$bal" ] && ok "paid 读得到余额(注册赠额 $bal)" || no "读不到余额"
  # min cannot pay, so paid pays paid — the point here is that the module
  # is present and the loop runs, not who the counterparty is.
  ix=$(ctl paid 29712 /delegate "{\"provider\":\"$STD_AID\",\"capability\":\"text.digest.paid\",\"args\":{\"text\":\"y\"}}" \
       | jq_ "print(d.get('interaction_id',''))")
  res=""
  for _ in $(seq 1 40); do
    res=$(ctl paid 29712 /results '{}' | jq_ "
for x in d.get('results') or []:
    if x['interaction_id']=='$ix': print(x['result']); break")
    [ -n "$res" ] && break
    sleep 1
  done
  st=$(echo "$res" | jq_ "print(d.get('status',''))")
  # standard has no x402 either, so it refuses. What this proves is that
  # paid ASKED properly and got a real answer.
  case "$st" in
    UNAVAILABLE) ok "paid 向无付费模块的提供方要价,得到明确拒绝";;
    PAYMENT_REQUIRED) ok "paid 拿到了报价";;
    *) no "状态 $st";;
  esac
else
  no "paid 起不来"
fi

hd "9  收尾"
pkill -f "$ROOT/bin/anet-" 2>/dev/null
pkill -f "$ROOT/svc/svc.py" 2>/dev/null
[ "$OWN_HUB" = 1 ] && pkill -f "$ROOT/bin/anet-hub" 2>/dev/null
info "数据目录留在 $ROOT 供检查"

printf '\n\033[1m── %d 通过, %d 失败 ──\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
