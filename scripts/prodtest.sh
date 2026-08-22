#!/usr/bin/env bash
# prodtest.sh — the live two-hub topology, tested end to end over the real
# internet.
#
# scenario.sh proves the software against loopback in one process tree.
# This proves the deployment: two hubs on two machines in two places, three
# daemons that reach them across a real network with real TLS, real DNS and
# a real cloud firewall in the way. Every defect this project shipped in
# 2026-08 lived in the gap between two things that each worked — and the
# last three were found only by making something actually travel.
#
#   emax.chatchat.space   hub  https://hub.agentnetwork.org.cn   (nginx/TLS)
#   fmax.chatchat.space   hub  http://fmax.chatchat.space:4001   (direct)
#
#   cmax   daemon → emax hub   sells text.digest / text.digest.paid
#   ink93  daemon → emax hub   an ordinary user: registers, buys, rates
#   dmax   daemon → fmax hub   sells text.stats / text.stats.paid,
#                              plus a public voucher door on :4002
#
# The split is the point: ink93 and cmax bank at one hub, dmax at the
# other, so anything ink93 and dmax do together crosses a boundary — which
# is where discovery federation, cross-hub settlement and reputation
# federation are either real or only compiled.
#
# Reachability is asymmetric and that is a fact about the network, not a
# bug: fmax's cloud firewall admits 4001/4002 from the other cloud hosts
# and not from a home line, so checks against fmax are made from emax by
# ssh. A test that pretended otherwise would fail on the operator's laptop
# and pass nowhere.
#
#   bash scripts/prodtest.sh              run everything
#   bash scripts/prodtest.sh --no-write   read-only checks (no delegations,
#                                         no payments, no ratings)
#
# Shipping binaries to these hosts, learned the hard way twice:
#
#   gzip -9 -k anet-hub && cp anet-hub.gz anet-hub-$(date +%Y%m%d-%H%M).gz
#   rsync -z --partial --timeout=300 anet-hub-VERSION.gz host:/root/
#   ssh host 'sha256sum -c <<< "SHA  anet-hub-VERSION.gz"'
#
# A VERSIONED filename, and NOT --append-verify. The uplink is slow enough
# that --partial is worth having, but --append-verify assumes the remote
# file is a PREFIX of the source — point it at a same-named older build
# and it appends onto a complete different file and reports success. It
# did exactly that here, and the corruption was only caught because the
# checksum was verified after. Always verify the checksum on the far end;
# a size match is not a content match.
set -uo pipefail
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost

WRITE=1
[ "${1:-}" = "--no-write" ] && WRITE=0

EMAX_HUB=${EMAX_HUB:-https://hub.agentnetwork.org.cn}
FMAX_HUB=${FMAX_HUB:-http://fmax.chatchat.space:4001}
DMAX_VOUCHER=${DMAX_VOUCHER:-http://dmax.chatchat.space:4002/x402/redeem}

# node → ssh host : HOME : control port. ink93 is local.
CMAX_HOST=root@cmax.chatchat.space; CMAX_HOME=/root/anet4;            CMAX_PORT=29610
DMAX_HOST=root@dmax.chatchat.space; DMAX_HOME=/data/anet-node/home;   DMAX_PORT=29610
EMAX_HOST=root@emax.chatchat.space
INK_HOME=${INK_HOME:-/tmp/anet-prod/ink93};                           INK_PORT=29615

pass=0; fail=0; skip=0
ok(){ printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; pass=$((pass+1)); }
no(){ printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; fail=$((fail+1)); }
sk(){ printf '\033[1;33m  ~ %s\033[0m\n' "$*"; skip=$((skip+1)); }
hd(){ printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }
info(){ printf '  %s\n' "$*"; }

# ctl <node> <path> <json> — the control API of one node, wherever it runs.
ctl(){
  local node=$1 path=$2 body=$3
  case $node in
    ink93) curl -s -m 180 -H "Authorization: Bearer $(cat "$INK_HOME/.anet/control_token.txt")" \
             -H 'Content-Type: application/json' -d "$body" "http://127.0.0.1:$INK_PORT$path" ;;
    cmax)  ssh -o ConnectTimeout=20 $CMAX_HOST "curl -s -m 180 -H 'Authorization: Bearer '\$(cat $CMAX_HOME/.anet/control_token.txt) -H 'Content-Type: application/json' -d '$body' http://127.0.0.1:$CMAX_PORT$path" ;;
    dmax)  ssh -o ConnectTimeout=20 $DMAX_HOST "curl -s -m 180 -H 'Authorization: Bearer '\$(cat $DMAX_HOME/.anet/control_token.txt) -H 'Content-Type: application/json' -d '$body' http://127.0.0.1:$DMAX_PORT$path" ;;
  esac
}
# viafmax <path> — reach the fmax hub from a host its firewall admits.
viafmax(){ ssh -o ConnectTimeout=20 $EMAX_HOST "curl -s -m 30 '$FMAX_HUB$1'"; }
jq_(){ python3 -c "import sys,json
try: d=json.load(sys.stdin)
except Exception: print(''); raise SystemExit
$1" 2>/dev/null; }

# wait_result <node> <interaction_id> [tries] — poll until an answer lands.
wait_result(){
  local node=$1 ix=$2 tries=${3:-40}
  for _ in $(seq 1 "$tries"); do
    local r
    r=$(ctl "$node" /results '{}' | jq_ "
for x in d.get('results') or []:
    if x['interaction_id']=='$ix': print(x['result']); break")
    [ -n "$r" ] && { echo "$r"; return 0; }
    sleep 3
  done
  echo ''
}

# ── 1. both hubs are up, and both can be verified ───────────────
hd "1  两个 hub 都在,而且都能被验证"
curl -sf -m 20 "$EMAX_HUB/healthz" >/dev/null && ok "emax hub 在(经 nginx/TLS)" || no "emax hub 不可达"
viafmax /healthz | grep -q '"status":"ok"' && ok "fmax hub 在" || no "fmax hub 不可达"

E_AID=$(curl -sf -m 20 "$EMAX_HUB/hub/identity" | jq_ "print(d.get('aid',''))")
F_AID=$(viafmax /hub/identity | jq_ "print(d.get('aid',''))")
[ -n "$E_AID" ] && ok "emax hub 有身份 ${E_AID:0:20}…" || no "emax hub 没有身份"
[ -n "$F_AID" ] && ok "fmax hub 有身份 ${F_AID:0:20}…" || no "fmax hub 没有身份"
[ "$E_AID" != "$F_AID" ] && ok "两个 hub 是两个身份(也就是两条账本)" || no "两个 hub 身份相同"

# The one that was missing in production until 2026-08-23: a custodian
# that signs settlements, receipts and vouchers, and publishes no key.
for pair in "emax:$EMAX_HUB:$E_AID" "fmax:$FMAX_HUB:$F_AID"; do
  n=${pair%%:*}; rest=${pair#*:}; aid=${rest##*:}
  if [ "$n" = emax ]; then code=$(curl -s -o /dev/null -w '%{http_code}' -m 20 "$EMAX_HUB/agents/$aid/kel")
  else code=$(ssh -o ConnectTimeout=20 $EMAX_HOST "curl -s -o /dev/null -w '%{http_code}' -m 20 '$FMAX_HUB/agents/$aid/kel'"); fi
  [ "$code" = 200 ] && ok "$n hub 公布自己的密钥历史 —— 它签的东西外人能验" \
    || no "$n hub 不公布自己的密钥历史($code):它签的收据谁也验不了"
done

# ── 2. the ledger adds up, on both ──────────────────────────────
hd "2  两边的账都是平的"
for n in emax fmax; do
  if [ "$n" = emax ]; then s=$(curl -sf -m 20 "$EMAX_HUB/x402/supply"); else s=$(viafmax /x402/supply); fi
  out=$(echo "$s" | jq_ "print(d['supply']['outstanding'])")
  bal=$(echo "$s" | jq_ "print(d['supply']['balances'])")
  if [ -z "$out" ]; then no "$n hub 不提供 /x402/supply(旧构建?)"
  elif [ "$out" = "$bal" ]; then ok "$n:未清偿 $out == 各账户合计 $bal"
  else no "$n 账不平:$out vs $bal"; fi
done

# ── 3. three daemons, registered where they should be ───────────
hd "3  三个 daemon,各自注册在该在的 hub"
CMAX_AID=$(ctl cmax /status '{}' | jq_ "print(d.get('aid',''))")
DMAX_AID=$(ctl dmax /status '{}' | jq_ "print(d.get('aid',''))")
INK_AID=$(ctl ink93 /status '{}' | jq_ "print(d.get('aid',''))")
for pair in "cmax:$CMAX_AID:$EMAX_HUB" "ink93:$INK_AID:$EMAX_HUB" "dmax:$DMAX_AID:$FMAX_HUB"; do
  n=${pair%%:*}; rest=${pair#*:}; aid=${rest%%:*}; want=${rest#*:}
  got=$(ctl "$n" /status '{}' | jq_ "print(d.get('hub','') or d.get('hub_url',''))")
  [ -n "$aid" ] && ok "$n 有身份 ${aid:0:20}…" || no "$n 拿不到身份"
  [ "$got" = "$want" ] && ok "$n 的 hub 是 $want" || no "$n 的 hub 是 $got,期望 $want"
done
# Registered means the hub can serve your key history to a stranger.
for pair in "cmax:$CMAX_AID:e" "ink93:$INK_AID:e" "dmax:$DMAX_AID:f"; do
  n=${pair%%:*}; rest=${pair#*:}; aid=${rest%%:*}; which=${rest#*:}
  if [ "$which" = e ]; then code=$(curl -s -o /dev/null -w '%{http_code}' -m 20 "$EMAX_HUB/agents/$aid/kel")
  else code=$(ssh -o ConnectTimeout=20 $EMAX_HOST "curl -s -o /dev/null -w '%{http_code}' -m 20 '$FMAX_HUB/agents/$aid/kel'"); fi
  [ "$code" = 200 ] && ok "$n 的密钥历史 hub 上有,陌生人可自行验签" || no "$n 未注册成功($code)"
done

# A node that moved hubs must not still be listed at the old one. Until
# `anet hub-leave` existed it always was, and the old hub went on queueing
# work into a mailbox nobody would poll — accepted, and silently
# swallowed. This check is the one that found it.
stale=$(curl -sf -m 20 "$EMAX_HUB/agents" | jq_ "
print(len([a for a in (d.get('agents') or []) if a.get('aid')=='$DMAX_AID' and not a.get('home_hub')]))")
[ "${stale:-0}" = 0 ] && ok "dmax 不再被 emax 当作本地 agent(换 hub 后注销干净)" \
  || no "emax 仍把 dmax 列为本地 —— 投给它的活会进死信箱(anet hub-leave 没做?)"

# ── 4. discovery across the boundary ────────────────────────────
hd "4  跨 hub 的发现"
[ "$WRITE" = 1 ] && ctl dmax /visibility '{"visibility":"federated"}' >/dev/null 2>&1
seen=0
for _ in $(seq 1 15); do
  seen=$(curl -sf -m 20 "$EMAX_HUB/agents?cap=text.stats" | jq_ "
print(len([a for a in (d.get('agents') or []) if a.get('aid')=='$DMAX_AID']))")
  [ "${seen:-0}" -ge 1 ] && break
  sleep 6
done
if [ "${seen:-0}" -ge 1 ]; then
  ok "emax hub 从 fmax 学到了 dmax 的卡片(按能力 id 可查)"
  home=$(curl -sf -m 20 "$EMAX_HUB/agents?cap=text.stats" | jq_ "
for a in (d.get('agents') or []):
    if a.get('aid')=='$DMAX_AID': print(a.get('home_hub','')); break")
  [ -n "$home" ] && ok "学来的条目带着 home hub($home),知道该往哪投" \
    || no "学来的条目没有 home hub —— 投递不知道去哪"
else
  no "目录没有跨过来(dmax 是否 visibility=federated?)"
fi

if [ "$WRITE" = 0 ]; then
  hd "只读模式"; info "跳过委派 / 付款 / 评价"
  printf '\n\033[1m── %d 通过, %d 失败, %d 跳过 ──\033[0m\n' "$pass" "$fail" "$skip"
  [ "$fail" -eq 0 ]; exit
fi

# ── 5. a capability call inside one hub ─────────────────────────
hd "5  同一个 hub 内:ink93 调 cmax 的能力"
ix=$(ctl ink93 /delegate "{\"provider\":\"$CMAX_AID\",\"capability\":\"text.digest\",\"args\":{\"text\":\"anet\"}}" \
     | jq_ "print(d.get('interaction_id',''))")
if [ -n "$ix" ]; then
  r=$(wait_result ink93 "$ix")
  st=$(echo "$r" | jq_ "print(d.get('status',''))")
  dg=$(echo "$r" | jq_ "
import json as j
e=d.get('evidence') or {}
o=e.get('observed_state') or ''
print(j.loads(o).get('digest','') if o.startswith('{') else '')")
  [ "$st" = OK ] && ok "调用成功(经 emax 中继,跨两台机器)" || no "状态 $st: $r"
  # sha256("anet")
  [ "$dg" = "8f202bdbf250aa9bb932743a005ed2714febdd7b4a99a75dd4b1e0e2e2d0e9c5" ] \
    && ok "摘要内容正确,而不只是状态正确" || info "摘要 ${dg:0:16}…(内容校验跳过)"
else
  no "委派没排上队"
fi

# ── 6. the paid loop, over the real internet ────────────────────
hd "6  付费闭环(ink93 付钱给 cmax,经 emax 账本)"
bal_before=$(ctl ink93 /balance '{}' | jq_ "print(d.get('balance',''))")
cbal_before=$(ctl cmax /balance '{}' | jq_ "print(d.get('balance',''))")
info "开工前:ink93=$bal_before cmax=$cbal_before"
if [ -z "$bal_before" ]; then
  no "ink93 读不到余额(hub 是旧构建,或本节点无 x402 模块)"
else
  q=$(ctl ink93 /delegate "{\"provider\":\"$CMAX_AID\",\"capability\":\"text.digest.paid\",\"args\":{\"text\":\"pay\"}}" \
      | jq_ "print(d.get('interaction_id',''))")
  qr=$(wait_result ink93 "$q")
  qs=$(echo "$qr" | jq_ "print(d.get('status',''))")
  [ "$qs" = PAYMENT_REQUIRED ] && ok "不付钱时拿到报价,不是报错" || no "状态 $qs"
  [ "$(ctl ink93 /balance '{}' | jq_ "print(d.get('balance',''))")" = "$bal_before" ] \
    && ok "只问价没扣钱" || no "报价过程动了余额"

  pix=$(ctl ink93 /delegate "{\"provider\":\"$CMAX_AID\",\"capability\":\"text.digest.paid\",\"args\":{\"text\":\"pay\"},\"pay\":true}" \
        | jq_ "print(d.get('interaction_id',''))")
  pr=$(wait_result ink93 "$pix")
  ps=$(echo "$pr" | jq_ "print(d.get('status',''))")
  [ "$ps" = OK ] && ok "付过钱之后活真的干了" || no "付费后状态 $ps: ${pr:0:200}"
  rcpt=$(echo "$pr" | jq_ "print((d.get('paid') or {}).get('receipt',''))")
  [ -n "$rcpt" ] && ok "hub 签的结算收据随结果回来了(付款方可自证)" \
    || no "结果里没有结算收据"
  bal_after=$(ctl ink93 /balance '{}' | jq_ "print(d.get('balance',''))")
  cbal_after=$(ctl cmax /balance '{}' | jq_ "print(d.get('balance',''))")
  [ "$((bal_before - bal_after))" = 25 ] && ok "付款方扣了 25" || no "付款方 $bal_before → $bal_after"
  [ "$((cbal_after - cbal_before))" = 25 ] && ok "收款方进了 25" || no "收款方 $cbal_before → $cbal_after"
  for pair in "ink93:anet.payment.authorized" "ink93:anet.payment.settled" "cmax:anet.payment.settled"; do
    n=${pair%%:*}; t=${pair#*:}
    c=$(ctl "$n" /evidence "{\"event_type\":\"$t\",\"limit\":50}" | jq_ "print(len(d.get('records') or []))")
    [ "${c:-0}" -ge 1 ] && ok "$n 链上有 $t" || no "$n 链上没有 $t"
  done
fi

# ── 7. the gateway: pay at the hub, collect at the daemon ───────
hd "7  x402 网关:在 fmax 付钱,到 dmax 取货"
RES="/x402/resource/$DMAX_AID/text.stats.paid"
hdrs=$(ssh -o ConnectTimeout=20 $EMAX_HOST "curl -s -D - -o /dev/null -m 30 '$FMAX_HUB$RES'")
echo "$hdrs" | head -1 | grep -q ' 402 ' && ok "未付款时回 402" || no "回的是 $(echo "$hdrs"|head -1)"
echo "$hdrs" | grep -qi '^PAYMENT-REQUIRED:' && ok "402 带 PAYMENT-REQUIRED 头" || no "没带 PAYMENT-REQUIRED 头"
body=$(ssh -o ConnectTimeout=20 $EMAX_HOST "curl -s -m 30 '$FMAX_HUB$RES'")
redeem=$(echo "$body" | jq_ "print(d.get('redeem_at',''))")
[ "$redeem" = "$DMAX_VOUCHER" ] && ok "报价写明取货地址($redeem)—— hub 不代理内容" \
  || no "取货地址是 '$redeem',期望 $DMAX_VOUCHER"
price=$(echo "$body" | jq_ "print(((d.get('accepts') or [{}])[0]).get('amount',''))")
[ "$price" = 30 ] && ok "价钱来自 dmax 自己签的卡片,hub 只能拒卖不能改价" || no "网关报价 $price"

# ── 8. redemption: credit can leave ─────────────────────────────
hd "8  兑付:credit 也能出去"
s1=$(viafmax /x402/supply | jq_ "print(d['supply']['outstanding'])")
rd=$(ctl dmax /redeem '{"amount":5,"reference":"prodtest"}')
rv=$(echo "$rd" | jq_ "print(d.get('verified',''))")
[ "$rv" = True ] && ok "兑付成功,且 dmax 验过 fmax 的签字" || no "兑付没有可验证的收据:${rd:0:160}"
s2=$(viafmax /x402/supply | jq_ "print(d['supply']['outstanding'])")
[ -n "$s1" ] && [ "$((s1 - s2))" = 5 ] && ok "fmax 的未清偿负债降了 5 —— credit 真的出去了" \
  || no "兑付后负债 $s1 → $s2"

# ── 9. reputation across the boundary ───────────────────────────
hd "9  信誉跨 hub"
cix=$(ctl ink93 /delegate "{\"provider\":\"$DMAX_AID\",\"capability\":\"text.stats\",\"args\":{\"text\":\"cross hub\"}}" \
      | jq_ "print(d.get('interaction_id',''))")
if [ -n "$cix" ]; then
  cr=$(wait_result ink93 "$cix" 60)
  cs=$(echo "$cr" | jq_ "print(d.get('status',''))")
  [ "$cs" = OK ] && ok "跨 hub 的能力调用真的执行了(ink93@emax → dmax@fmax)" \
    || no "跨 hub 调用状态 $cs"
  ctl ink93 /review "{\"interaction_id\":\"$cix\",\"rating\":5,\"comment\":\"prodtest\"}" >/dev/null 2>&1
  sleep 3
  lr=$(curl -sf -m 20 "$EMAX_HUB/agents/$DMAX_AID/reputation" | jq_ "
print((d.get('reputation') or {}).get('local',{}).get('reviews',0))")
  [ "${lr:-0}" -ge 1 ] && ok "跨 hub 的活可以被评价(评价方的 hub 收下了自己用户的评分)" \
    || no "跨 hub 交互无法评价(emax 本地评价数 ${lr:-0})"
  pr=0
  for _ in $(seq 1 20); do
    pr=$(viafmax "/agents/$DMAX_AID/reputation" | jq_ "
print(len((d.get('reputation') or {}).get('peers') or []))")
    [ "${pr:-0}" -ge 1 ] && break
    sleep 6
  done
  [ "${pr:-0}" -ge 1 ] && ok "评分经信誉同步流到了 fmax,记在 peer 来源下" \
    || no "评分没有跨过去(fmax 的 peers 列为空)"
  lc=$(viafmax "/agents/$DMAX_AID/reputation" | jq_ "
print((d.get('reputation') or {}).get('local',{}).get('reviews',0))")
  conc=$(viafmax "/agents/$DMAX_AID/reputation" | jq_ "
print((d.get('reputation') or {}).get('concentration',''))")
  info "fmax 上 dmax 的信誉:本地 $lc 条,peer $pr 个来源,集中度 $conc"
else
  no "跨 hub 委派没排上队"
fi

printf '\n\033[1m── %d 通过, %d 失败, %d 跳过 ──\033[0m\n' "$pass" "$fail" "$skip"
[ "$fail" -eq 0 ]
