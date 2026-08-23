#!/usr/bin/env bash
# prodperf.sh — what the live topology actually costs, in time.
#
# prodtest answers "does it work". This answers "how long does it take and
# where does the time go", which nothing measured. The numbers matter for
# a specific reason: several parts of this system poll, and a poll
# interval that is short relative to a round trip produces load without
# producing responsiveness. Guessing at that is how a five-second poll
# ends up hammering a hub that takes eight seconds to answer.
#
# Every figure is measured across the real internet against the real
# deployment, so they include TLS, DNS, a cloud firewall and two clocks.
# They are not micro-benchmarks and should not be compared against any.
#
#   bash scripts/prodperf.sh            the full set
#   N=20 bash scripts/prodperf.sh       more samples per figure
set -uo pipefail
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost

EMAX_HUB=${EMAX_HUB:-https://hub.agentnetwork.org.cn}
FMAX_HUB=${FMAX_HUB:-http://39.107.76.243:4001}
EMAX_HOST=${EMAX_HOST:-root@emax.chatchat.space}
DMAX_HOST=${DMAX_HOST:-root@dmax.chatchat.space}
CMAX_HOST=${CMAX_HOST:-root@cmax.chatchat.space}
INK_HOME=${INK_HOME:-/tmp/anet-prod/ink93}
INK_PORT=${INK_PORT:-29615}
CMAX_PORT=${CMAX_PORT:-29610}
DMAX_PORT=${DMAX_PORT:-29610}
CMAX_HOME=${CMAX_HOME:-/root/anet4}
DMAX_HOME=${DMAX_HOME:-/data/anet-node/home}
N=${N:-10}

hd(){ printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }
row(){ printf '  %-46s %s\n' "$1" "$2"; }

# stats <file-of-milliseconds> — min / median / p90 / max.
#
# Median and p90 rather than a mean: these are network measurements, and
# one retransmit makes a mean say something that never happened.
stats(){
  python3 - "$1" <<'PY'
import sys
xs=[float(l) for l in open(sys.argv[1]) if l.strip()]
if not xs:
    print("(no samples)"); raise SystemExit
xs.sort()
def q(p):
    if len(xs)==1: return xs[0]
    i=min(int(p*len(xs)), len(xs)-1)
    return xs[i]
print(f"n={len(xs)}  min={xs[0]:.0f}ms  med={q(0.5):.0f}ms  p90={q(0.9):.0f}ms  max={xs[-1]:.0f}ms")
PY
}

# timeit <n> <shell-command> — run it n times, print the stats.
timeit(){
  local n=$1; shift
  local f; f=$(mktemp)
  for _ in $(seq 1 "$n"); do
    local s e
    s=$(date +%s%3N)
    eval "$*" >/dev/null 2>&1
    e=$(date +%s%3N)
    echo $((e - s)) >> "$f"
  done
  stats "$f"
  rm -f "$f"
}

ctl(){
  local node=$1 path=$2 body=$3
  case $node in
    ink93) curl -s -m 300 -H "Authorization: Bearer $(cat "$INK_HOME/.anet/control_token.txt")" \
             -H 'Content-Type: application/json' -d "$body" "http://127.0.0.1:$INK_PORT$path" ;;
    cmax)  ssh -o ConnectTimeout=20 $CMAX_HOST "curl -s -m 300 -H 'Authorization: Bearer '\$(cat $CMAX_HOME/.anet/control_token.txt) -H 'Content-Type: application/json' -d '$body' http://127.0.0.1:$CMAX_PORT$path" ;;
    dmax)  ssh -o ConnectTimeout=20 $DMAX_HOST "curl -s -m 300 -H 'Authorization: Bearer '\$(cat $DMAX_HOME/.anet/control_token.txt) -H 'Content-Type: application/json' -d '$body' http://127.0.0.1:$DMAX_PORT$path" ;;
  esac
}
jq_(){ python3 -c "import sys,json
try: d=json.load(sys.stdin)
except Exception: print(''); raise SystemExit
$1" 2>/dev/null; }

hd "1  hub 端点的往返时间(从 ink93,家宽 + TLS)"
# curl's own timing rather than wall-clock around the process, so the
# figure is the request and not the fork.
local_timeit(){
  local n=$1 url=$2
  for _ in $(seq 1 "$n"); do
    curl -sf -o /dev/null -w '%{time_total}\n' -m 30 "$url" || echo 0
  done | python3 -c "
import sys
xs=sorted(float(l)*1000 for l in sys.stdin if l.strip() and float(l)>0)
if not xs: print('(no samples)'); raise SystemExit
q=lambda p: xs[min(int(p*len(xs)), len(xs)-1)]
print(f'n={len(xs)}  min={xs[0]:.1f}ms  med={q(0.5):.1f}ms  p90={q(0.9):.1f}ms  max={xs[-1]:.1f}ms')"
}
row "GET /healthz (emax, TLS)"        "$(local_timeit "$N" "$EMAX_HUB/healthz")"
row "GET /agents (emax)"              "$(local_timeit "$N" "$EMAX_HUB/agents")"
row "GET /x402/supply (emax)"         "$(local_timeit "$N" "$EMAX_HUB/x402/supply")"
row "GET /x402/issuance (emax)"       "$(local_timeit "$N" "$EMAX_HUB/x402/issuance?from=0")"

hd "2  hub 端点(在 emax 上打 fmax,机房内)"
# The timing loop runs ON emax. Wrapping each iteration in its own ssh
# would measure the ssh handshake from here, which is most of the time and
# none of the thing being measured — the first version of this reported
# 3-5 seconds for a request that takes 2 milliseconds, which is worse than
# having no number.
remote_timeit(){
  local host=$1 n=$2 url=$3
  ssh -o ConnectTimeout=20 "$host" "
    for _ in \$(seq 1 $n); do
      curl -sf -o /dev/null -w '%{time_total}\n' -m 20 '$url' || echo 0
    done" 2>/dev/null | python3 -c "
import sys
xs=sorted(float(l)*1000 for l in sys.stdin if l.strip() and float(l)>0)
if not xs: print('(no samples)'); raise SystemExit
q=lambda p: xs[min(int(p*len(xs)), len(xs)-1)]
print(f'n={len(xs)}  min={xs[0]:.1f}ms  med={q(0.5):.1f}ms  p90={q(0.9):.1f}ms  max={xs[-1]:.1f}ms')"
}
row "GET /healthz (fmax)"             "$(remote_timeit "$EMAX_HOST" "$N" "$FMAX_HUB/healthz")"
row "GET /x402/issuance/head (fmax)"  "$(remote_timeit "$EMAX_HOST" "$N" "$FMAX_HUB/x402/issuance/head")"
row "GET /healthz (emax 打自己)"      "$(remote_timeit "$EMAX_HOST" "$N" "http://127.0.0.1:8088/healthz")"

hd "3  daemon 控制面(本机回环)"
row "POST /status (ink93)"            "$(timeit "$N" "ctl ink93 /status '{}'")"
row "POST /balance (ink93, 要打 hub)" "$(timeit "$N" "ctl ink93 /balance '{}'")"
row "POST /evidence (ink93, 读本地链)" "$(timeit "$N" "ctl ink93 /evidence '{\"limit\":50}'")"

hd "4  一次能力调用的端到端时间"
# The whole path: sign a task, relay through the hub, the provider polls,
# executes, signs a receipt, relays back, the requester verifies. The poll
# interval dominates and that is the point of measuring it.
CMAX_AID=$(ctl cmax /status '{}' | jq_ "print(d.get('aid',''))")
if [ -n "$CMAX_AID" ]; then
  f=$(mktemp)
  for i in $(seq 1 "$((N < 5 ? N : 5))"); do
    s=$(date +%s%3N)
    ix=$(ctl ink93 /delegate "{\"provider\":\"$CMAX_AID\",\"capability\":\"text.digest\",\"args\":{\"text\":\"perf-$i\"}}" \
         | jq_ "print(d.get('interaction_id',''))")
    [ -z "$ix" ] && continue
    for _ in $(seq 1 90); do
      r=$(ctl ink93 /results '{}' | jq_ "
for x in d.get('results') or []:
    if x['interaction_id']=='$ix': print('done'); break")
      [ -n "$r" ] && break
      sleep 1
    done
    e=$(date +%s%3N)
    echo $((e - s)) >> "$f"
  done
  row "delegate → result (ink93→emax→cmax)" "$(stats "$f")"
  rm -f "$f"
else
  row "delegate → result" "(cmax 够不着,跳过)"
fi

hd "5  联邦与见证的节奏"
# Not latency: how long a fact takes to cross. A directory entry that
# takes half an hour to appear is not broken, but somebody choosing a
# poll interval needs to know it.
row "目录同步稳态间隔"   "2 分钟(eager 5 秒直到收敛)"
row "全量重读间隔"       "每 15 轮 ≈ 30 分钟"
row "见证间隔"           "1 小时"
row "daemon 轮询间隔"    "$(ctl ink93 /status '{}' | jq_ "print(d.get('poll_interval_seconds') or 5)") 秒"

hd "6  账本规模与查询代价"
sup=$(curl -sf -m 30 "$EMAX_HUB/x402/supply")
row "emax 发放链条数"     "$(curl -sf -m 30 "$EMAX_HUB/x402/issuance?from=0" | jq_ "print(len(d.get('entries') or []))")"
row "emax 未清偿"         "$(echo "$sup" | jq_ "print(d['supply']['outstanding'])")"
row "emax 注册 agent"     "$(curl -sf -m 30 "$EMAX_HUB/agents" | jq_ "print(len(d.get('agents') or []))")"
row "GET /x402/witnesses" "$(local_timeit "$N" "$EMAX_HUB/x402/witnesses")"

hd "7  二进制体积(裁剪的效果)"
if command -v go >/dev/null 2>&1; then
  cd "$(dirname "$0")/.."
  go build -o /tmp/perf-full ./cmd/anet 2>/dev/null
  go build -tags "no_x402,no_service,no_mcp,no_cas,no_org,no_blackboard,no_p2p,no_anetlink" \
     -o /tmp/perf-min ./cmd/anet 2>/dev/null
  row "完整构建" "$(du -h /tmp/perf-full 2>/dev/null | cut -f1)"
  row "全部模块去掉" "$(du -h /tmp/perf-min 2>/dev/null | cut -f1)"
  rm -f /tmp/perf-full /tmp/perf-min
fi

printf '\n\033[1m── 以上为实网测量,含 TLS/DNS/防火墙,不可与本机 benchmark 比较 ──\033[0m\n'
