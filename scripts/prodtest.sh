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
#   fmax 39.107.76.243     hub  http://39.107.76.243:4001         (direct, by IP)
#
# By IP, not by name. Plain HTTP to a domain resolving to an Aliyun
# address is intercepted by their ICP-filing check, which replaces the
# response with a compliance page and a 403. Measured on the live
# topology: about 8 requests in 10 to fmax.chatchat.space:4001 were
# intercepted, while the same requests to 39.107.76.243:4001 all
# succeeded. The interception is keyed on the Host header, so an IP
# avoids it.
#
# It being intermittent is what makes it worth writing down: it does not
# fail cleanly, it produces flaky behaviour that reads as a defect
# somewhere else. The real fix is TLS, which the interceptor cannot
# rewrite; until then these endpoints are addressed by IP.
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
#   RESTART=0 bash scripts/prodtest.sh    skip section 11
#
# Do not run two full passes back to back. Section 11 restarts cmax three
# times on purpose, and although it waits for the daemon to answer again
# before returning, the relay backlog it produced takes longer than that
# to drain. A second pass started immediately afterwards intermittently
# reports two failures in section 9b that belong to the first pass.
#
# Measured, not assumed: a single pass is clean, a pass immediately after
# another is not, and a pass after a few minutes is clean again. Use
# RESTART=0 for the second pass, or leave a gap. The hourly scheduled run
# uses --no-write and never reaches section 11.
#
# Shipping binaries to these hosts, learned the hard way twice:
#
#   V=$(date +%Y%m%d-%H%M); gzip -9 -c anet-hub > anet-hub-$V.gz
#   rsync -z --partial anet-hub-$V.gz root@cmax.chatchat.space:/root/   # fast
#   ssh root@cmax 'for h in emax fmax; do
#       rsync -z --partial anet-hub-'$V'.gz root@$h.chatchat.space:/root/; done'
#   ssh root@HOST 'sha256sum -c <<< "SHA  anet-hub-$V.gz"'
#
# RELAY THROUGH CMAX. The home uplink to emax and fmax runs at a few tens
# of KB/s — a 5MB binary took the better part of an hour and sometimes
# stalled. cmax has fast paths to both (all three are in the same cloud)
# and can ssh to them, so pushing once to cmax and fanning out from there
# turns half an hour into a couple of minutes.
#
# A VERSIONED filename, and NOT --append-verify. The uplink is slow enough
# that --partial is worth having, but --append-verify assumes the remote
# file is a PREFIX of the source — point it at a same-named older build
# and it appends onto a complete different file and reports success. It
# did exactly that here, and the corruption was only caught because the
# checksum was verified after. Always verify the checksum on the far end;
# a size match is not a content match.
set -uo pipefail

# 每个 ssh 都带 -n。不带的时候 ssh 会去读 stdin,而这个脚本在后台运行时 stdin
# 是它自己的输入;第一个 ssh 把它读走,bash 随即看到 EOF 并以 0 退出。表现是
# 一次"成功"的运行在第 3 节戛然而止,而摘要行根本没打印 —— 一个中途停下却报
# 成功的验证关卡,比没有关卡更坏。前台运行时看不到,因为那时 stdin 是终端。
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost

WRITE=1
[ "${1:-}" = "--no-write" ] && WRITE=0

EMAX_HUB=${EMAX_HUB:-https://hub.agentnetwork.org.cn}
FMAX_HUB=${FMAX_HUB:-http://39.107.76.243:4001}
DMAX_VOUCHER=${DMAX_VOUCHER:-http://210.45.70.176:4002/x402/redeem}

# node → ssh host : HOME : control port. ink93 is local.
CMAX_HOST=root@cmax.chatchat.space; CMAX_HOME=/root/anet4;            CMAX_PORT=29610; CMAX_BIN=/usr/local/bin/anet4
DMAX_HOST=root@dmax.chatchat.space; DMAX_HOME=/data/anet-node/home;   DMAX_PORT=29610
EMAX_HOST=root@emax.chatchat.space
INK_HOME=${INK_HOME:-/tmp/anet-prod/ink93};                           INK_PORT=29615
INK_BIN=${INK_BIN:-/tmp/deploy/anet}
# Whether the local tools exist at all.
#
# INK_BIN and FIXTURE default to paths on the workstation this script is
# usually run from, and the scheduled copy runs on cmax where neither is
# present. Seven checks then failed with "no such file" — which reads as a
# broken system and is a missing tool. A check that cannot run has to say
# so; only a check that ran and got the wrong answer is a failure.
HAVE_INK_BIN=0; [ -x "$INK_BIN" ] && HAVE_INK_BIN=1
HAVE_FIXTURE=0
FIXTURE=${FIXTURE:-/tmp/deploy/anetfixture}
[ -x "$FIXTURE" ] && HAVE_FIXTURE=1
CMAX_HOME_LOCAL=${CMAX_HOME_LOCAL:-}

pass=0; fail=0; skip=0
ok(){ printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; pass=$((pass+1)); }
no(){ printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; fail=$((fail+1)); }
sk(){ printf '\033[1;33m  ~ %s\033[0m\n' "$*"; skip=$((skip+1)); }
hd(){ printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }
info(){ printf '  %s\n' "$*"; }

# reachable <node> — can this run drive that node's control API?
#
# ink93 is a workstation, not a permanent node: it is where a person sits.
# The scheduled run on cmax cannot reach it and must not report that as a
# failure every hour — an alarm that is always ringing is one nobody
# reads, and the topology it is guarding (two hubs, two server daemons) is
# genuinely fine.
#
# So an unreachable node is skipped with its name and the reason, never
# silently dropped. "We did not check ink93" and "ink93 is healthy" are
# different statements and the summary line keeps them apart.
reachable(){
  case $1 in
    ink93) curl -sf -m 5 "http://127.0.0.1:$INK_PORT/ping" >/dev/null 2>&1 ;;
    cmax)  ssh -n -o ConnectTimeout=10 $CMAX_HOST "curl -sf -m 5 http://127.0.0.1:$CMAX_PORT/ping >/dev/null" 2>/dev/null ;;
    dmax)  ssh -n -o ConnectTimeout=10 $DMAX_HOST "curl -sf -m 5 http://127.0.0.1:$DMAX_PORT/ping >/dev/null" 2>/dev/null ;;
  esac
}

# ctl <node> <path> <json> — the control API of one node, wherever it runs.
ctl(){
  local node=$1 path=$2 body=$3
  case $node in
    ink93) curl -s -m 180 -H "Authorization: Bearer $(cat "$INK_HOME/.anet/control_token.txt")" \
             -H 'Content-Type: application/json' -d "$body" "http://127.0.0.1:$INK_PORT$path" ;;
    cmax)  ssh -n -o ConnectTimeout=20 $CMAX_HOST "curl -s -m 180 -H 'Authorization: Bearer '\$(cat $CMAX_HOME/.anet/control_token.txt) -H 'Content-Type: application/json' -d '$body' http://127.0.0.1:$CMAX_PORT$path" ;;
    dmax)  ssh -n -o ConnectTimeout=20 $DMAX_HOST "curl -s -m 180 -H 'Authorization: Bearer '\$(cat $DMAX_HOME/.anet/control_token.txt) -H 'Content-Type: application/json' -d '$body' http://127.0.0.1:$DMAX_PORT$path" ;;
  esac
}
# viafmax <path> — reach the fmax hub from a host its firewall admits.
viafmax(){ ssh -n -o ConnectTimeout=20 $EMAX_HOST "curl -s -m 30 '$FMAX_HUB$1'"; }
jq_(){ python3 -c "import sys,json
try: d=json.load(sys.stdin)
except Exception: print(''); raise SystemExit
$1" 2>/dev/null; }

# cap <node> <provider-aid> <capability> <args-json> — call and wait.
#
# The capability path rather than a prose goal: it runs deterministically
# in the provider's registry and comes back with evidence, so a check can
# assert on what happened rather than on what an agent decided to say.
cap(){
  local ix
  ix=$(ctl "$1" /delegate "{\"provider\":\"$2\",\"capability\":\"$3\",\"args\":$4}" \
       | jq_ "print(d.get('interaction_id',''))")
  [ -z "$ix" ] && { echo '{"status":"","error":"delegate refused"}'; return 1; }
  wait_result "$1" "$ix" 40
}

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
hd "0  跑的是哪一版"
# What every component reports it was built from.
#
# A stale copy of this script sat on cmax reporting failures for three
# hours against hubs that were fine: it expected an endpoint the
# deployment had moved away from, and nothing connected the deployed
# script to the repository it came from. The components now say which
# commit they are, so a disagreement is visible instead of being
# diagnosed from symptoms.
#
# Reported, not asserted. A rolling upgrade legitimately has two versions
# running at once, and failing the run for that would make the check
# useless during exactly the operation it should be watching. What it
# flags is an UNSTAMPED build, which means somebody built without
# scripts/build.sh and the comparison can no longer be made at all.
# The shipped copy carries a VERSION file written by ship-prodtest.sh.
# A copy running from the repository has no such file and reads its commit
# from git instead, so "unstamped" means neither — a copy somebody moved
# by hand, which is the case worth naming.
SELF_COMMIT=$(cat "$(dirname "$0")/VERSION" 2>/dev/null \
  || git -C "$(dirname "$0")" rev-parse --short HEAD 2>/dev/null \
  || echo unstamped)
info "本脚本: $SELF_COMMIT"
unstamped=0
for pair in "emax hub:$EMAX_HUB/healthz"; do
  n=${pair%%:*}; u=${pair#*:}
  v=$(curl -sf -m 20 "$u" | jq_ "print(d.get('commit',''))")
  info "$n: ${v:-(不报版本)}"
  { [ -z "$v" ] || [ "$v" = unknown ]; } && unstamped=$((unstamped+1))
done
fv=$(viafmax /healthz | jq_ "print(d.get('commit',''))")
info "fmax hub: ${fv:-(不报版本)}"
{ [ -z "$fv" ] || [ "$fv" = unknown ]; } && unstamped=$((unstamped+1))
for n in cmax ink93 dmax; do
  case $n in
    ink93) pv=$(curl -sf -m 5 "http://127.0.0.1:$INK_PORT/ping" | jq_ "print(d.get('commit',''))") ;;
    cmax)  pv=$(ssh -n -o ConnectTimeout=10 $CMAX_HOST "curl -sf -m 5 http://127.0.0.1:$CMAX_PORT/ping" 2>/dev/null | jq_ "print(d.get('commit',''))") ;;
    dmax)  pv=$(ssh -n -o ConnectTimeout=10 $DMAX_HOST "curl -sf -m 5 http://127.0.0.1:$DMAX_PORT/ping" 2>/dev/null | jq_ "print(d.get('commit',''))") ;;
  esac
  [ -n "$pv" ] && info "$n daemon: $pv"
  [ "$pv" = unknown ] && unstamped=$((unstamped+1))
done
[ "$unstamped" -eq 0 ] && ok "每个组件都说得出自己是哪一版" \
  || no "$unstamped 个组件报不出构建版本(未用 scripts/build.sh 构建,比对无从做起)"

# The page the hub serves is embedded in the hub binary, and building the
# binary did not rebuild it. The embedded copy sat five deployments
# behind: every change to the page reached the repository, passed its
# tests, and never reached production. Nothing noticed because nothing
# looked at the served page.
#
# Checked by a marker the current page contains and the stale one did
# not. It is a weak assertion — it proves this string is present, not
# that the bundle is current — but it is the assertion that would have
# caught the failure, and a strong one would need the hub to report the
# bundle's hash, which is worth doing only if this recurs.
page=$(curl -sf -m 30 "$EMAX_HUB/" 2>/dev/null)
if [ -z "$page" ]; then
  no "hub 不提供网页"
else
  ok "hub 提供网页($(printf '%s' "$page" | wc -c) 字节)"
  case "$page" in
    *静默*) ok "网页含当前构建的标记(联邦/静默标识已上线)";;
    *) no "网页缺当前构建的标记 —— 嵌入的 bundle 落后于仓库(build.sh 没跑前端?)";;
  esac
fi

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
  else code=$(ssh -n -o ConnectTimeout=20 $EMAX_HOST "curl -s -o /dev/null -w '%{http_code}' -m 20 '$FMAX_HUB/agents/$aid/kel'"); fi
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

# The signed supply chain, and whether it agrees with the balances.
#
# /x402/supply computes issued and outstanding from rows the hub writes.
# The chain is the same facts recorded append-only and signed, so the two
# derivations can be compared — and a hub that moved credit without
# recording it shows up here rather than in nobody noticing.
for n in emax fmax; do
  if [ "$n" = emax ]; then sup=$(curl -sf -m 20 "$EMAX_HUB/x402/supply"); else sup=$(viafmax /x402/supply); fi
  agrees=$(echo "$sup" | jq_ "print(d['supply'].get('chain_agrees'))")
  chout=$(echo "$sup"  | jq_ "print(d['supply'].get('chain_outstanding'))")
  tabout=$(echo "$sup" | jq_ "print(d['supply'].get('outstanding'))")
  hseq=$(echo "$sup"   | jq_ "print(d['supply'].get('chain_head_seq'))")
  if [ "$agrees" = True ]; then
    ok "$n:发放链与账表一致(链 $chout == 表 $tabout),链头 seq $hseq"
  else
    no "$n:发放链与账表不一致(链 $chout vs 表 $tabout)"
  fi
  [ -n "$hseq" ] && ok "$n 公布了链头,见证者有东西可钉" || no "$n 没有链头"
done

# ── 3. three daemons, registered where they should be ───────────
hd "3  三个 daemon,各自注册在该在的 hub"
NODES=""
for n in cmax ink93 dmax; do
  if reachable "$n"; then NODES="$NODES $n"; else
    sk "$n 的控制面从这台机器够不着,跳过它的检查(工作站不是常驻节点)"
  fi
done
has(){ case " $NODES " in *" $1 "*) return 0;; *) return 1;; esac; }
CMAX_AID=$(has cmax  && ctl cmax  /status '{}' | jq_ "print(d.get('aid',''))")
DMAX_AID=$(has dmax  && ctl dmax  /status '{}' | jq_ "print(d.get('aid',''))")
INK_AID=$( has ink93 && ctl ink93 /status '{}' | jq_ "print(d.get('aid',''))")
# dmax's AID is needed by the federation, gateway and reputation sections
# even when dmax itself cannot be driven from here, so fall back to asking
# its hub rather than skipping those checks too.
if [ -z "$DMAX_AID" ]; then
  DMAX_AID=$(viafmax /agents | jq_ "
for a in (d.get('agents') or []):
    if a.get('name')=='dmax-services': print(a.get('aid','')); break")
fi
for pair in "cmax:$CMAX_AID:$EMAX_HUB" "ink93:$INK_AID:$EMAX_HUB" "dmax:$DMAX_AID:$FMAX_HUB"; do
  n=${pair%%:*}; rest=${pair#*:}; aid=${rest%%:*}; want=${rest#*:}
  has "$n" || continue
  got=$(ctl "$n" /status '{}' | jq_ "print(d.get('hub','') or d.get('hub_url',''))")
  [ -n "$aid" ] && ok "$n 有身份 ${aid:0:20}…" || no "$n 拿不到身份"
  [ "$got" = "$want" ] && ok "$n 的 hub 是 $want" || no "$n 的 hub 是 $got,期望 $want"
done
# Registered means the hub can serve your key history to a stranger.
for pair in "cmax:$CMAX_AID:e" "ink93:$INK_AID:e" "dmax:$DMAX_AID:f"; do
  n=${pair%%:*}; rest=${pair#*:}; aid=${rest%%:*}; which=${rest#*:}
  has "$n" || continue
  if [ "$which" = e ]; then code=$(curl -s -o /dev/null -w '%{http_code}' -m 20 "$EMAX_HUB/agents/$aid/kel")
  else code=$(ssh -n -o ConnectTimeout=20 $EMAX_HOST "curl -s -o /dev/null -w '%{http_code}' -m 20 '$FMAX_HUB/agents/$aid/kel'"); fi
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
if ! has ink93 || ! has cmax; then
  sk "需要 ink93 与 cmax 两侧,这台机器够不着"
else
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

fi

# ── 6. the paid loop, over the real internet ────────────────────
hd "6  付费闭环(ink93 付钱给 cmax,经 emax 账本)"
if ! has ink93 || ! has cmax; then
  sk "付费闭环需要 ink93 和 cmax 两侧的控制面,这台机器够不着"
else
bal_before=$(ctl ink93 /balance '{}' | jq_ "print(d.get('balance',''))")
cbal_before=$(ctl cmax /balance '{}' | jq_ "print(d.get('balance',''))")
info "开工前:ink93=$bal_before cmax=$cbal_before"
# Say something while there is still time to act.
#
# The section spends 25 a run and simply went quiet the day the balance
# reached zero — a skip that appeared only once the budget was already
# gone, in a run where nothing could be done about it. Ten runs of warning
# is the difference between a note an operator reads and a section that
# stopped covering the payment path without anyone deciding to stop
# covering it.
if [ -n "$bal_before" ] && [ "$bal_before" -ge 25 ] && [ "$bal_before" -lt 250 ]; then
  info "  ⚠ ink93 余额 $bal_before,约够 $((bal_before / 25)) 轮。充值:"
  info "    ssh root@emax.chatchat.space"
  info "    systemctl stop anet-hub && /data/projs/anet-hub/bin/anet-hub \\"
  info "      -data /data/projs/anet-hub/data -grant $INK_AID -amount 2000 \\"
  info "      -reason prodtest && systemctl start anet-hub"
fi
if [ -z "$bal_before" ]; then
  no "ink93 读不到余额(hub 是旧构建,或本节点无 x402 模块)"
elif [ "$bal_before" -lt 25 ]; then
  # The writing run spends 25 credits each time and the payer starts with
  # only the registration grant, so a few runs exhaust it. That is the
  # test consuming its own budget, not a defect — but reporting it as a
  # failure makes a self-inflicted shortfall look like a broken payment
  # path, which is worse than not running the section at all.
  #
  # Funding is an operator action, deliberately: who may create credit is
  # a policy question this round does not answer, so there is no remote
  # path and there should not be one yet. On the hub that holds the
  # ledger:
  #
  #   systemctl stop anet-hub
  #   anet-hub -data <dir> -grant <aid> -amount 500 -reason "prodtest"
  #   systemctl start anet-hub
  sk "ink93 只剩 $bal_before credits,不够跑付费闭环(测试把自己的赠额花完了;见脚本内的充值说明)"
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

fi

# ── 7. the gateway: pay at the hub, collect at the daemon ───────
hd "7  x402 网关:在 fmax 付钱,到 dmax 取货"
RES="/x402/resource/$DMAX_AID/text.stats.paid"
hdrs=$(ssh -n -o ConnectTimeout=20 $EMAX_HOST "curl -s -D - -o /dev/null -m 30 '$FMAX_HUB$RES'")
echo "$hdrs" | head -1 | grep -q ' 402 ' && ok "未付款时回 402" || no "回的是 $(echo "$hdrs"|head -1)"
echo "$hdrs" | grep -qi '^PAYMENT-REQUIRED:' && ok "402 带 PAYMENT-REQUIRED 头" || no "没带 PAYMENT-REQUIRED 头"
body=$(ssh -n -o ConnectTimeout=20 $EMAX_HOST "curl -s -m 30 '$FMAX_HUB$RES'")
redeem=$(echo "$body" | jq_ "print(d.get('redeem_at',''))")
[ "$redeem" = "$DMAX_VOUCHER" ] && ok "报价写明取货地址($redeem)—— hub 不代理内容" \
  || no "取货地址是 '$redeem',期望 $DMAX_VOUCHER"
price=$(echo "$body" | jq_ "print(((d.get('accepts') or [{}])[0]).get('amount',''))")
[ "$price" = 30 ] && ok "价钱来自 dmax 自己签的卡片,hub 只能拒卖不能改价" || no "网关报价 $price"

# Now actually buy it. Checking that a 402 comes back proves the quote;
# only a voucher that is paid for, carried, and spent proves the design —
# and until this ran, dmax's public redemption door had never been used
# over the real network even once. Everything that had exercised it was
# loopback in a single process tree, which is the arrangement that has hidden
# every defect this project shipped.
#
# The buyer here is dmax's own key, signing through anetfixture. That is
# not a shortcut: the gateway resolves the payer's key history from the
# hub exactly as it would a stranger's, so a signature it cannot check
# fails for the right reason.
DMAX_BAL_BEFORE=$(ctl dmax /balance '{}' | jq_ "print(d.get('balance',''))")
sig=$(ssh -n -o ConnectTimeout=20 $DMAX_HOST "/usr/local/bin/anet x402-authorize \
        --home /data/anet-node/home/.anet --pay-to '$DMAX_AID' --amount 30 \
        --network 'hub:$F_AID' --interaction 'prodtest-gw' 2>/dev/null" 2>/dev/null)
if [ -z "$sig" ]; then
  sk "网关付款跳过:节点上没有 x402-authorize(fixture 未部署)"
else
  gw=$(ssh -n -o ConnectTimeout=20 $EMAX_HOST \
        "curl -s -D /tmp/gw.hdr -m 40 -H 'PAYMENT-SIGNATURE: $sig' '$FMAX_HUB$RES'")
  gwcode=$(ssh -n -o ConnectTimeout=20 $EMAX_HOST "head -1 /tmp/gw.hdr | awk '{print \$2}'")
  voucher=$(echo "$gw" | jq_ "print(d.get('voucher',''))")
  [ "$gwcode" = 200 ] && [ -n "$voucher" ] && ok "付款后拿到的是凭证,不是结果 —— hub 见不到内容" \
    || no "网关付款失败($gwcode): ${gw:0:180}"
  ssh -n -o ConnectTimeout=20 $EMAX_HOST "grep -qi '^PAYMENT-RESPONSE:' /tmp/gw.hdr" \
    && ok "结算响应带 PAYMENT-RESPONSE 头" || no "没带 PAYMENT-RESPONSE 头"

  if [ -n "$voucher" ]; then
    # Straight to the agent, over the public internet, not through the
    # hub. This is the leg the whole design exists for.
    out=$(ssh -n -o ConnectTimeout=20 $EMAX_HOST "curl -s -m 90 -H 'Content-Type: application/json' \
          -d '{\"voucher\":\"$voucher\",\"capability\":\"text.stats.paid\",\"args\":{\"text\":\"via voucher\"}}' \
          '$DMAX_VOUCHER'")
    vs=$(echo "$out" | jq_ "print(d.get('status',''))")
    [ "$vs" = "OK" ] && ok "凭证在 dmax 上兑成了真活(hub 全程没碰请求和结果)" \
      || no "兑付失败:${out:0:200}"
    again=$(ssh -n -o ConnectTimeout=20 $EMAX_HOST "curl -s -m 60 -H 'Content-Type: application/json' \
            -d '{\"voucher\":\"$voucher\",\"capability\":\"text.stats.paid\",\"args\":{\"text\":\"via voucher\"}}' \
            '$DMAX_VOUCHER'")
    ae=$(echo "$again" | jq_ "print(d.get('error',''))")
    case "$ae" in
      *already*) ok "同一张凭证第二次被拒(一次性由 daemon 把关,hub 无从知道)";;
      *) no "凭证被重复兑付:${again:0:160}";;
    esac
    # The effect is on dmax's own chain, tagged with how it was paid for.
    # A second door to the same work must not be a door around the
    # evidence.
    viac=$(ctl dmax /evidence '{"event_type":"anet.voucher.redeemed","limit":20}' \
           | jq_ "print(len(d.get('records') or []))")
    [ "${viac:-0}" -ge 1 ] && ok "兑付记在了 dmax 自己的链上" || no "兑付没上链"
  fi
fi

# ── 8. redemption: credit can leave ─────────────────────────────
hd "8  兑付:credit 也能出去"
if ! has dmax; then
  sk "兑付要 dmax 自己签名,这台机器够不着它的控制面"
else
s1=$(viafmax /x402/supply | jq_ "print(d['supply']['outstanding'])")
rd=$(ctl dmax /redeem '{"amount":5,"reference":"prodtest"}')
rv=$(echo "$rd" | jq_ "print(d.get('verified',''))")
[ "$rv" = True ] && ok "兑付成功,且 dmax 验过 fmax 的签字" || no "兑付没有可验证的收据:${rd:0:160}"
s2=$(viafmax /x402/supply | jq_ "print(d['supply']['outstanding'])")
[ -n "$s1" ] && [ "$((s1 - s2))" = 5 ] && ok "fmax 的未清偿负债降了 5 —— credit 真的出去了" \
  || no "兑付后负债 $s1 → $s2"

fi

# ── 9. reputation across the boundary ───────────────────────────
hd "9  信誉跨 hub"
if ! has ink93; then
  sk "跨 hub 评价要 ink93 发起并评分,这台机器够不着"
else
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

fi

# ── 9b. delegation as a conversation ────────────────────────────
hd "9b 多轮对话:委派不是一次调用"
# The design says a delegation is a conversation — messages both ways, an
# end that both sides agree to, a receipt over the transcript. The live
# run only ever exercised a single capability call, so the whole
# conversational path was untested outside scenario.sh.
if ! has ink93 || ! has cmax; then
  sk "多轮对话要 ink93 与 cmax 两侧的控制面"
else
  cix=$(ctl ink93 /delegate "{\"provider\":\"$CMAX_AID\",\"goal\":\"prodtest 多轮对话\"}" \
        | jq_ "print(d.get('interaction_id',''))")
  if [ -z "$cix" ]; then
    no "散文委派没排上队"
  else
    ok "散文委派排上了($cix)"
    # It has to arrive in the provider's inbox before either side can
    # talk. Delivery is the relay, so this also proves the mailbox works
    # for something other than a capability call.
    # --pending, because the inbox is capped at 100 entries and cmax has
    # been accumulating them all week: a new delegation sorts behind the
    # backlog and never appears in the page that is returned. Scanning the
    # first hundred reported a delivery failure on a delivery that had
    # worked.
    seen=0
    for _ in $(seq 1 30); do
      seen=$(ctl cmax /inbox '{"pending":true}' | jq_ "
print(len([x for x in (d.get('inbox') or []) if x.get('interaction_id')=='$cix']))")
      [ "${seen:-0}" -ge 1 ] && break
      sleep 2
    done
    if [ "${seen:-0}" -ge 1 ]; then
      ok "对方收件箱里出现了这次委派"
    else
      # Falling back to the thread: arrival is what is being checked, and
      # the provider having the interaction at all proves it. A cap on
      # one listing is not a delivery failure.
      alt=$(ctl cmax /thread "{\"interaction_id\":\"$cix\"}" | jq_ "
print('yes' if (d.get('thread') or {}).get('interaction_id') else '')")
      [ "$alt" = yes ] && ok "委派到了对方那里(收件箱首页已被积压占满,按 id 查得到)" \
        || no "委派没到对方那里"
    fi

    # The field is "body". Sending "text" is accepted by the HTTP layer
    # and rejected by the daemon as an empty message, so the first version
    # of this silently sent nothing and reported a broken conversation.
    ctl cmax  /message "{\"interaction_id\":\"$cix\",\"body\":\"收到,正在看\"}" >/dev/null 2>&1
    ctl ink93 /message "{\"interaction_id\":\"$cix\",\"body\":\"好,谢谢\"}" >/dev/null 2>&1
    sleep 14
    # The messages hang off the thread, not off the response root. The
    # first version read the root, found nothing, and reported a broken
    # conversation on a conversation that had worked.
    turns=$(ctl ink93 /thread "{\"interaction_id\":\"$cix\"}" | jq_ "
print(len(((d.get('thread') or {}).get('messages')) or []))")
    # The goal plus both messages: three. Two would mean one direction
    # arrived and the other did not, which is the failure worth naming.
    [ "${turns:-0}" -ge 3 ] && ok "双向消息都到了($turns 条在同一个 thread 里)" \
      || no "thread 里只有 ${turns:-0} 条(期望 3:目标 + 双方各一条)"

    # Ending is mutual: one side proposes, the other accepts, and only
    # then does the provider sign a receipt over the transcript.
    ctl ink93 /end "{\"interaction_id\":\"$cix\"}" >/dev/null 2>&1
    sleep 6
    ctl cmax /end-accept "{\"interaction_id\":\"$cix\"}" >/dev/null 2>&1
    got=""
    for _ in $(seq 1 30); do
      got=$(ctl ink93 /results '{}' | jq_ "
for x in d.get('results') or []:
    if x['interaction_id']=='$cix': print('yes' if x.get('receipt') else 'norecipt'); break")
      [ -n "$got" ] && break
      sleep 2
    done
    case "$got" in
      yes) ok "双方同意结束后,提供方对整份transcript 出具了签名收据";;
      norecipt) no "结束了但没有收据";;
      *) no "结束协商没有完成";;
    esac
  fi
fi

# ── 9c. guest mode ──────────────────────────────────────────────
hd "9c 游客模式:没注册的人也能先试"
# The first thing a stranger touches. Four endpoints, and until now zero
# production coverage — a path that is broken here is broken for
# everybody who has not joined yet, which is everybody at first.
g=$(curl -sf -m 30 -X POST -H 'Content-Type: application/json' -d '{}' "$EMAX_HUB/guest/start")
gs=$(echo "$g" | jq_ "print(d.get('session',''))")
gr=$(echo "$g" | jq_ "print(d.get('remaining',''))")
gh=$(echo "$g" | jq_ "print(d.get('handler',''))")
if [ -z "$gs" ]; then
  no "开不了游客会话: ${g:0:140}"
else
  ok "陌生人开出了会话(接待方 $gh,余额 $gr 条)"
  sent=$(curl -sf -m 60 -X POST -H 'Content-Type: application/json' \
    -d "{\"session\":\"$gs\",\"body\":\"prodtest 游客消息\"}" "$EMAX_HUB/guest/send")
  left=$(echo "$sent" | jq_ "print(d.get('remaining',''))")
  if [ -n "$left" ] && [ "$left" -lt "$gr" ] 2>/dev/null; then
    ok "发一条,配额从 $gr 降到 $left —— 试聊是有限的,且限额真的在减"
  else
    no "配额没有递减($gr → ${left:-?}): ${sent:0:120}"
  fi
  # Polling returns whatever the handler has said so far, which may be
  # nothing yet — the handler is a real agent and answers when it
  # answers. What is being checked is that the endpoint works and the
  # session is live, not that a reply has arrived.
  pl=$(curl -sf -m 30 -X POST -H 'Content-Type: application/json' \
    -d "{\"session\":\"$gs\"}" "$EMAX_HUB/guest/poll" | jq_ "
print('ok' if 'messages' in d else 'bad')")
  [ "$pl" = ok ] && ok "轮询端点可用,会话仍然存在" || no "轮询失败"
  e=$(curl -s -o /dev/null -w '%{http_code}' -m 30 -X POST -H 'Content-Type: application/json' \
    -d "{\"session\":\"$gs\"}" "$EMAX_HUB/guest/end")
  [ "$e" = 200 ] && ok "会话可以结束" || no "结束会话返回 $e"
fi

# ── 9d. the read surfaces, for content rather than status ───────
hd "9d 展示面的内容对不对"
# These all answered 200 and nobody had looked at what they said. A page
# stating something untrue is the failure this project cares about most,
# and a status code cannot detect it.
g=$(curl -sf -m 30 "$EMAX_HUB/graph")
gn=$(echo "$g" | jq_ "print(len(d.get('nodes') or []))")
ge=$(echo "$g" | jq_ "print(len(d.get('edges') or []))")
dangling=$(echo "$g" | jq_ "
nodes={n['aid'] for n in (d.get('nodes') or [])}
bad={a for e in (d.get('edges') or []) for a in (e.get('source'),e.get('target')) if a and a not in nodes}
print(len(bad))")
[ "${dangling:-1}" = 0 ] \
  && ok "/graph 自洽:$gn 个节点 $ge 条边,没有悬空边" \
  || no "/graph 有 $dangling 个 AID 出现在边里却没有节点"
# Nodes reconstructed from an edge are marked, so a reader can tell a
# departed agent from one still being routed to.
unreg=$(echo "$g" | jq_ "
print(len([n for n in (d.get('nodes') or []) if not n.get('registered')]))")
info "其中 $unreg 个已不在本 hub 注册(离开了或长期未取信)"

st=$(curl -sf -m 30 "$EMAX_HUB/stats")
sa=$(echo "$st" | jq_ "print(d.get('agents',-1))")
sr=$(echo "$st" | jq_ "print(d.get('reviews',-1))")
sav=$(echo "$st" | jq_ "print(d.get('avg_rating',-1))")
sf=$(echo "$st" | jq_ "print(d.get('federated_agents',0))")
listed=$(curl -sf -m 30 "$EMAX_HUB/agents" | jq_ "print(len(d.get('agents') or []))")
# 目录里既有本 hub 注册的,也有从对等 hub 学来的,而 /stats 把两者分开报 ——
# "在我们这里注册的"与"别人告诉我们的"是两种断言,合成一个数会让 hub 报出它
# 并不拥有的覆盖面。所以要对的是两者之和,不是 agents 单独一项。
[ "$((sa + sf))" = "$listed" ] \
  && ok "/stats($sa 本地 + $sf 联邦)与目录里实际列出的 $listed 个一致" \
  || no "/stats 说 $sa 本地 + $sf 联邦 = $((sa + sf)),目录列出 $listed 个"
# The mean must lie inside the range a rating can take. A figure outside
# it means the aggregate is computed over something that is not ratings.
awk_ok=$(python3 -c "
a=$sav
print('ok' if a==0 or (1<=a<=5) else 'bad')")
[ "$awk_ok" = ok ] && ok "/stats 的均分 $sav 落在 1-5 之内(或为 0 表示无评价)" \
  || no "/stats 均分 $sav 不是一个合法评分"
[ "${sr:-0}" -ge 0 ] && ok "/stats 评价数 $sr" || no "/stats 评价数异常"

llms=$(curl -sf -m 30 "$EMAX_HUB/llms.txt")
n=$(printf '%s' "$llms" | wc -c)
[ "$n" -gt 2000 ] && ok "/llms.txt 有内容($n 字节)" || no "/llms.txt 只有 $n 字节"
# It is a CLI guide for an agent, not HTTP API documentation — it teaches
# `anet register`, not POST /register. The first version of this check
# looked for endpoints and reported a problem with the page rather than
# with the check.
#
# What matters is that the commands it teaches exist. A guide naming a
# command the binary does not have sends an agent to "unknown command",
# and this page is the only instruction most of them will read.
miss=0
# Matched as substrings, not as "anet <cmd>": the page teaches
# `anet --id <name> hub-register …`, and looking for the two words
# adjacent reported a gap in the documentation that was a gap in the
# check.
for cmd in "hub-register" " find" " delegate" "anet id new"; do
  case "$llms" in *"$cmd"*) ;; *) miss=$((miss+1)); info "llms.txt 没有教 $cmd";; esac
done
[ "$miss" = 0 ] && ok "/llms.txt 教的命令覆盖了加入与委派" || no "$miss 条关键命令未提及"
if [ -x "$INK_BIN" ]; then
  helpall=$("$INK_BIN" help --all 2>/dev/null)
  unknown=0
  # Only lines that are actually commands: fenced blocks and lines
  # beginning with `anet`. Scanning the prose matched "install / update
  # anet to the latest" as a command named `to`, and reported four
  # non-existent commands in a document that names none.
  for cmd in $(printf '%s' "$llms" \
      | grep -E '^\s*(\$ )?anet ' \
      | grep -oE '^\s*(\$ )?anet (--id [^ ]+ )?[a-z][a-z-]+' \
      | awk '{print $NF}' | sort -u); do
    case "$cmd" in daemon|help|version|up|down|stop|id|mcp|verify|logs|install|console|status) continue;; esac
    case "$helpall" in *"anet $cmd"*) ;; *) unknown=$((unknown+1)); info "llms.txt 教了 anet $cmd,help 里没有";; esac
  done
  [ "$unknown" = 0 ] && ok "llms.txt 教的每条命令这个构建都有" \
    || no "$unknown 条命令在这个构建里不存在"
fi

# ── 9e. the x402 facilitator surface ────────────────────────────
hd "9e x402 facilitator:verify 与 settle 必须给同一个答案"
# verify and settle share decoding and diverge afterwards. Nothing
# checked that they agree — a verify that says yes to a payment settle
# then refuses, or the reverse, is a facilitator giving two answers about
# the same authorization.
sup=$(viafmax /x402/supported | jq_ "
k=(d.get('kinds') or [{}])[0]
print(k.get('scheme','')+'/'+k.get('network','')[:12])")
[ -n "$sup" ] && ok "/x402/supported 公布了它结算的轨($sup…)" || no "/x402/supported 无内容"

if ! has dmax; then
  sk "verify 要 dmax 签一张授权"
else
  sig=$(ctl dmax /x402-authorize "{\"pay_to\":\"$DMAX_AID\",\"amount\":7,\"network\":\"hub:$F_AID\",\"interaction_id\":\"verify-probe\"}" \
        | jq_ "print(d.get('value',''))")
  if [ -z "$sig" ]; then
    no "签不出授权"
  else
    pp=$(printf '%s' "$sig" | base64 -d 2>/dev/null)
    vr=$(ssh -n -o ConnectTimeout=20 $EMAX_HOST "curl -s -m 30 -H 'Content-Type: application/json' \
          -d '{\"x402Version\":2,\"paymentPayload\":$pp}' '$FMAX_HUB/x402/verify'")
    valid=$(echo "$vr" | jq_ "print(d.get('isValid'))")
    [ "$valid" = True ] && ok "verify 说这张授权可以结算" \
      || no "verify 拒绝了一张好授权: $(echo "$vr" | jq_ "print(d.get('invalidReason',''))")"

    sr2=$(ssh -n -o ConnectTimeout=20 $EMAX_HOST "curl -s -m 30 -H 'Content-Type: application/json' \
          -d '{\"x402Version\":2,\"paymentPayload\":$pp}' '$FMAX_HUB/x402/settle'")
    ok2=$(echo "$sr2" | jq_ "print(d.get('success'))")
    if [ "$valid" = "$ok2" ] || { [ "$valid" = True ] && [ "$ok2" = True ]; }; then
      ok "settle 的结论与 verify 一致(都为 $ok2)"
    else
      no "verify 说 $valid,settle 说 $ok2 —— facilitator 对同一张授权给了两个答案"
    fi

    # The same authorization a second time: verify must now say it is
    # spent, or a caller would be told a settled payment is still good.
    vr2=$(ssh -n -o ConnectTimeout=20 $EMAX_HOST "curl -s -m 30 -H 'Content-Type: application/json' \
          -d '{\"x402Version\":2,\"paymentPayload\":$pp}' '$FMAX_HUB/x402/verify'")
    again=$(echo "$vr2" | jq_ "print(d.get('isValid'))")
    reason=$(echo "$vr2" | jq_ "print(d.get('invalidReason',''))")
    if [ "$again" = False ]; then
      ok "结算之后 verify 改口说不行($reason)"
    else
      no "已结算的授权 verify 仍说可以 —— 调用方会以为还能再花一次"
    fi
  fi
fi

# ── 9f. redemptions are listed where the agent can find them ────
hd "9f 兑付记录查得到"
if ! has dmax; then
  sk "要 dmax 的控制面"
else
  before=$(viafmax "/agents/$DMAX_AID/redemptions" | jq_ "print(len(d.get('redemptions') or []))")
  ctl dmax /redeem '{"amount":3,"reference":"prodtest-list"}' >/dev/null 2>&1
  # 轮询而不是睡 2 秒。兑付要从 dmax 走到 fmax 再落库,固定 2 秒在实网上不够,
  # 于是这条断言会红,而下一条("金额与 reference 对得上")却绿 —— 两条自相
  # 矛盾的结论,说明红的那条测的是等待时间,不是这件事成没成。
  after=0
  for _ in $(seq 1 30); do
    lst=$(viafmax "/agents/$DMAX_AID/redemptions")
    after=$(echo "$lst" | jq_ "print(len(d.get('redemptions') or []))")
    [ "${after:-0}" -gt "${before:-0}" ] && break
    sleep 2
  done
  [ "${after:-0}" -gt "${before:-0}" ] \
    && ok "兑付出现在列表里($before → $after 条)" || no "兑付没有出现在列表里"
  match=$(echo "$lst" | jq_ "
r=[x for x in (d.get('redemptions') or []) if x.get('reference')=='prodtest-list']
print('ok' if r and r[0].get('amount')==3 and r[0].get('aid') else 'bad')")
  [ "$match" = ok ] \
    && ok "列表里的金额与 reference 与刚才兑付的一致" || no "列表内容对不上"
fi

# ── 9g. the modules that carry caller-signed objects ────────────
hd "9g cas / blackboard / org:调用方签名的对象跨机验签"
# I previously judged these not worth production coverage because they
# open no ports and do not talk between machines. That was wrong on both
# counts that matter:
#
#   blackboard.add and org.verify take an object the CALLER signed, so
#   the provider must resolve the caller's key history through the real
#   hub over TLS. On loopback it resolves against a local fake.
#
#   cas.put carries a blob as delegation arguments, so it crosses the
#   real relay with the real body limits — nginx at 512m and the control
#   plane's own cap, both of which had been configured and never tried.
if ! has ink93 || ! has cmax; then
  sk "要 ink93 与 cmax 两侧"
else
  # cas: content addressing, and the CID must be the hash of what was put.
  # The argument is "body" and the CID comes back as observed_state
  # directly, not wrapped in JSON.
  payload="prodtest cas $(date -u +%s)"
  blob=$(printf '%s' "$payload" | base64 -w0)
  r=$(cap ink93 "$CMAX_AID" cas.put "{\"body\":\"$blob\"}")
  cid=$(echo "$r" | jq_ "print((d.get('evidence') or {}).get('observed_state',''))")
  [ -n "$cid" ] && ok "cas.put 跨机返回了 CID(${cid:0:18}…)" || no "cas.put 没有返回 CID: ${r:0:140}"
  if [ -n "$cid" ]; then
    g=$(cap ink93 "$CMAX_AID" cas.get "{\"cid\":\"$cid\"}")
    gs=$(echo "$g" | jq_ "print(d.get('status',''))")
    gb=$(echo "$g" | jq_ "print(int((d.get('metrics') or {}).get('bytes',0)))")
    want=$(printf '%s' "$payload" | wc -c)
    # The byte count is what the effect reports. Content addressing is
    # asserted the other way round below: putting the same bytes again
    # must yield the same CID, and different bytes a different one.
    { [ "$gs" = OK ] && [ "$gb" = "$want" ]; } \
      && ok "cas.get 跨机取回 $gb 字节,与放进去的长度一致" \
      || no "cas.get 状态 $gs 字节 $gb(期望 $want)"
    again=$(cap ink93 "$CMAX_AID" cas.put "{\"body\":\"$blob\"}" \
            | jq_ "print((d.get('evidence') or {}).get('observed_state',''))")
    [ "$again" = "$cid" ] && ok "同样的字节得到同一个 CID(内容寻址成立)" \
      || no "同样的字节得到了不同的 CID"
    other=$(cap ink93 "$CMAX_AID" cas.put "{\"body\":\"$(printf '%s!' "$payload" | base64 -w0)\"}" \
            | jq_ "print((d.get('evidence') or {}).get('observed_state',''))")
    [ -n "$other" ] && [ "$other" != "$cid" ] && ok "不同的字节得到不同的 CID" \
      || no "不同的内容得到了同一个 CID"
    miss=$(cap ink93 "$CMAX_AID" cas.get '{"cid":"bafyreiaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}')
    ms=$(echo "$miss" | jq_ "print(d.get('status',''))")
    [ "$ms" != OK ] && ok "取一个不存在的 CID 会诚实失败($ms),而不是返回空" \
      || no "不存在的 CID 也返回了 OK"
  fi

  # blackboard: the unit is signed by ink93 and merged by cmax, so cmax
  # has to fetch ink93's key history from the hub to check it.
  unit=$("$INK_BIN" --id x 2>/dev/null >/dev/null; \
         "$FIXTURE" cogunit --home "$INK_HOME/.anet" --body "prodtest 跨机共脑" 2>/dev/null | tail -1)
  if [ -z "$unit" ]; then
    sk "本机没有 anetfixture,跳过 blackboard"
  else
    b=$(cap ink93 "$CMAX_AID" blackboard.add "{\"unit\":\"$unit\"}")
    bs=$(echo "$b" | jq_ "print(d.get('status',''))")
    [ "$bs" = OK ] \
      && ok "共脑合并成功 —— 提供方经真 hub 解析了调用方的密钥历史并验签" \
      || no "blackboard.add 失败(跨机验签?): ${b:0:160}"
    # A tampered unit must be refused, or the signature check is theatre.
    bad=$(printf '%s' "$unit" | sed 's/.$/A/')
    br=$(cap ink93 "$CMAX_AID" blackboard.add "{\"unit\":\"$bad\"}")
    brs=$(echo "$br" | jq_ "print(d.get('status',''))")
    [ "$brs" != OK ] && ok "被改过的贡献被拒($brs)" || no "篡改过的贡献也被接受了"
  fi

  # org: a credential signed by the founder, verified by a node that
  # holds the same genesis.
  oi=$(cap ink93 "$CMAX_AID" org.info '{}')
  os=$(echo "$oi" | jq_ "print(d.get('status',''))")
  [ "$os" = OK ] && ok "org.info 报出它服务的组织" || no "org.info 失败: ${oi:0:140}"
fi

# ── 9h. a large attachment across the real relay ────────────────
hd "9h 大附件穿过真中继"
# nginx is set to 512m and the control plane has its own cap. Both were
# configured and neither had been tried: a limit nobody has crossed is a
# number in a file.
if ! has ink93 || ! has cmax; then
  sk "要 ink93 与 cmax 两侧"
else
  att=$(mktemp); head -c $((2*1024*1024)) /dev/urandom > "$att"
  aix=$(curl -s -m 300 -H "Authorization: Bearer $(cat "$INK_HOME/.anet/control_token.txt")" \
        -F "provider=$CMAX_AID" -F "goal=prodtest 附件" -F "attachment=@$att" \
        "http://127.0.0.1:$INK_PORT/delegate" | jq_ "print(d.get('interaction_id',''))")
  if [ -z "$aix" ]; then
    no "带附件的委派没排上队(控制面或 nginx 的 body 上限?)"
  else
    ok "2 MiB 附件的委派被接受了"
    seen=""
    for _ in $(seq 1 40); do
      seen=$(ctl cmax /thread "{\"interaction_id\":\"$aix\"}" | jq_ "
t=d.get('thread') or {}
n=sum(len(m.get('attachments') or []) for m in (t.get('messages') or []))
print(n if n else '')")
      [ -n "$seen" ] && break
      sleep 3
    done
    [ -n "$seen" ] && ok "附件跨过 nginx 与中继到达对方($seen 个)" \
      || no "附件没有到达对方"
  fi
  rm -f "$att"
fi

# ── 9i. a stranger verifies a receipt ───────────────────────────
hd "9i 陌生人只拿收据与 hub 地址就能验通"
# The outward promise of the whole evidence surface, and it had been
# checked only on loopback. Here the KEL comes from the production hub
# over TLS and the verifier holds nothing else.
if ! has ink93; then
  sk "要 ink93"
else
  rc=$(ctl ink93 /results '{}' | jq_ "
for x in reversed(d.get('results') or []):
    if x.get('receipt'): print(x['receipt']); break")
  if [ -z "$rc" ]; then
    no "没有可验的收据"
  else
    out=$("$INK_BIN" verify --receipt "$rc" --hub "$EMAX_HUB" 2>&1)
    # The tool prints "✓ signature verifies under <aid>" on success and
    # names what it checked. Matching on the marker rather than on a word
    # that appears in both outcomes.
    case "$out" in
      *"signature verifies"*) ok "陌生人用收据 + hub 地址验通了(无 daemon、无密钥)";;
      *) no "第三方验证失败: $(printf '%s' "$out" | head -2 | tr '\n' ' ')";;
    esac
    case "$out" in
      *"fetched from"*) ok "密钥历史是从生产 hub 现取的,验证方不预置任何东西";;
      *) no "没有从 hub 取密钥历史";;
    esac
  fi
fi

# ── 9j. one payment across two hubs creates credit once ─────────
hd "9j 跨 hub 付款:一次付款只造一份 credit,债务两侧对称"
# The invariant the whole credit design rests on, and it had never been
# exercised against two live hubs. It failed in two ways when it was.
#
# The payer's hub credited the payee whether or not the payee banked
# there, and the payee's hub credited it again on the peer's signed
# receipt — one payment, two credits, and the federation's total supply
# grew by the amount paid. Each hub stayed internally consistent, so
# neither hub's own supply check could see it.
#
# Neither side recorded the movement on its issuance chain either, so
# chain_outstanding == outstanding failed on both hubs the moment a
# cross-hub payment happened.
#
# What is checked here is the sum across both hubs, because that is the
# only place a double credit shows up.
if ! has cmax || ! reachable "$EMAX_HUB/healthz"; then
  sk "跨 hub 付款要 cmax 与两个 hub 都在"
elif [ -z "$DMAX_AID" ]; then
  sk "找不到 dmax 的 AID"
else
  esup(){ curl -s -m 30 "$EMAX_HUB/x402/supply" | jq_ "print(d['supply'].get('$1'))"; }
  fsup(){ viafmax /x402/supply | jq_ "print(d['supply'].get('$1'))"; }

  # Both hubs must offer each other's ledger, or a cross-hub buyer has
  # no rail to pay on and the clearing path cannot be reached at all.
  sup=$(curl -s -m 30 "$EMAX_HUB/x402/supported")
  n=$(echo "$sup" | jq_ "print(len(d.get('kinds') or []))")
  [ "${n:-0}" -ge 2 ] \
    && ok "emax 报出 $n 条可结算账本(自己 + 愿意清算的对端)" \
    || no "emax 只报出 ${n:-0} 条账本,跨 hub 买方无从付款"

  e0=$(esup outstanding); f0=$(fsup outstanding)
  # Cumulative, and reduced only by an operator running `anet-hub -clear`.
  # Earlier sections of this script also pay across hubs (the gateway in
  # section 7 is one), so the totals here are not this payment's amount —
  # only the delta is.
  due0=$(esup due_to_peers); owed0=$(fsup owed_by_peers)
  t0=$(( ${e0:-0} + ${f0:-0} ))
  info "付款前:emax outstanding=$e0 fmax outstanding=$f0 合计=$t0"

  out=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
    "export ANET_DATA_DIR=$CMAX_HOME/.anet; timeout 240 $CMAX_BIN delegate $DMAX_AID \
     --capability text.stats.paid --args '{\"text\":\"prodtest cross-hub\"}' --pay" 2>&1)
  net=$(echo "$out" | jq_ "print((d.get('paid') or {}).get('network',''))")
  amt=$(echo "$out" | jq_ "print((d.get('paid') or {}).get('amount',''))")
  if [ -z "$amt" ]; then
    no "跨 hub 付款没有成交:$(printf '%s' "$out" | head -3 | tr '\n' ' ')"
  else
    ok "cmax 付了 $amt,选中的账本是 ${net#hub:}"
    # The buyer must pay on its OWN hub's ledger. Taking the first
    # offered rail meant taking the seller's, which is the one a
    # cross-hub buyer holds no balance on.
    case "$net" in
      *"$(curl -s -m 30 "$EMAX_HUB/x402/supply" | jq_ "print(d.get('hub',''))")"*)
        ok "买方付在自己 hub 的账本上";;
      *) no "买方付在了别人的账本上:$net";;
    esac

    # Settlement crosses two hubs; give it room.
    for _ in 1 2 3 4 5 6; do
      e1=$(esup outstanding); f1=$(fsup outstanding)
      due=$(esup due_to_peers); owed=$(fsup owed_by_peers)
      [ "${due:-0}" != "${due0:-0}" ] && [ "${owed:-0}" != "${owed0:-0}" ] && break
      sleep 5
    done
    t1=$(( ${e1:-0} + ${f1:-0} ))
    info "付款后:emax outstanding=$e1 fmax outstanding=$f1 合计=$t1"

    [ "$t0" = "$t1" ] \
      && ok "联邦总供给不变($t0),一次付款只造了一份 credit" \
      || no "联邦总供给从 $t0 变成 $t1 —— 一次付款在两个账本上各造了一份"

    dd=$(( ${due:-0} - ${due0:-0} )); dw=$(( ${owed:-0} - ${owed0:-0} ))
    [ "$dd" = "$amt" ] \
      && ok "付款方 hub 的欠款增加了 $dd(累计 $due)" \
      || no "付款方 hub 的 due_to_peers 增加了 $dd,应为 $amt"
    [ "$dw" = "$amt" ] \
      && ok "收款方 hub 的应收增加了 $dw(累计 $owed),与对方欠款对称" \
      || no "收款方 hub 的 owed_by_peers 增加了 $dw,应为 $amt"

    # Both chains must still account for their own supply. A cross-hub
    # payment retires credit on one hub and issues it on the other; a
    # chain that recorded neither would understate one and overstate the
    # other while both hubs reported themselves consistent.
    for h in emax fmax; do
      [ $h = emax ] && ag=$(esup chain_agrees) || ag=$(fsup chain_agrees)
      [ "$ag" = True ] \
        && ok "$h 的发放链仍与账本一致(跨 hub 变动已入链)" \
        || no "$h 的发放链与账本不一致 —— 跨 hub 变动没入链"
    done

    # And the debt has to be dischargeable, not merely recordable.
    #
    # H-21 fixed "hub_owed only ever rose" on the creditor side. The
    # debtor side then grew the same shape: due_to_peers accumulated with
    # nothing that had ever reduced it in production. `-clear -payee` was
    # written and had zero live runs, so what looked like a working
    # clearing loop was half a loop.
    #
    # The discharge is signed by the debtor hub and DELIVERED to the
    # creditor: a statement sitting on the debtor's own disk discharges
    # nothing. Only after the creditor accepts does the debtor reduce its
    # own record — reducing first would leave the two hubs disagreeing
    # about a debt one of them still shows.
    dueNow=$(esup due_to_peers)
    if [ "${dueNow:-0}" -le 0 ]; then
      info "emax 当前无对外负债,跳过清偿"
    else
      out=$(ssh -n -o ConnectTimeout=30 $EMAX_HOST "
        systemctl stop anet-hub
        /data/projs/anet-hub/bin/anet-hub -data /data/projs/anet-hub/data \
          -clear $F_AID -amount $dueNow -reason prodtest-clear \
          -peer-endpoint $FMAX_HUB -payee $DMAX_AID 2>&1 | tail -3
        systemctl start anet-hub" 2>&1)
      for _ in $(seq 1 20); do reachable "$EMAX_HUB/healthz" && break; sleep 2; done
      case "$out" in
        *delivered*) ok "emax 签了 $dueNow 的清偿并投递给 fmax";;
        *) no "清偿没有投递成功:$(printf '%s' "$out" | tail -2 | tr '\n' ' ')";;
      esac
      # Both sides, because a discharge that only one hub believes in is
      # the disagreement this is meant to prevent.
      [ "$(esup due_to_peers)" = 0 ] \
        && ok "emax 的对外负债归零" || no "emax 仍欠 $(esup due_to_peers)"
      [ "$(fsup owed_by_peers)" = 0 ] \
        && ok "fmax 的应收归零 —— 两侧对同一笔债务的看法一致" \
        || no "fmax 仍记着 $(fsup owed_by_peers) 的应收"
      # Clearing must not have moved anyone's supply: it settles a claim
      # between hubs, not credit held by users.
      for h in emax fmax; do
        [ $h = emax ] && ag=$(esup chain_agrees) || ag=$(fsup chain_agrees)
        [ "$ag" = True ] \
          && ok "$h 清偿之后发放链仍与账本一致" \
          || no "$h 清偿动了供给"
      done
    fi
  fi
fi

# ── 9k. the MCP surface, against the real network ───────────────
hd "9k MCP 面:发现 → 委派 → 取回结果,全走实网"
# MCP is what an agent actually reaches ANet through, and it had only
# ever been exercised against a fake control plane. A tool surface that
# works against a fake and not against the network is the shape that
# fails at the moment it matters.
#
# Driven over the stdio protocol directly (scripts/mcpcall.py) rather
# than through an MCP client library, so the check has no dependency the
# daemon does not already have.
if ! has ink93; then
  sk "MCP 检查要 ink93 本机"
elif ! command -v python3 >/dev/null 2>&1; then
  sk "要 python3 来驱动 stdio 协议"
else
  MCP="python3 $(dirname "$0")/mcpcall.py $INK_BIN"

  tools=$(HOME=$INK_HOME $MCP list 2>&1)
  n=$(echo "$tools" | jq_ "print(len(d.get('tools') or []))")
  [ "${n:-0}" -ge 9 ] \
    && ok "MCP 服务器起来了,报出 $n 个工具" \
    || no "MCP 工具面不完整(${n:-0} 个):$(printf '%s' "$tools" | head -2 | tr '\n' ' ')"

  st=$(HOME=$INK_HOME $MCP call node_status '{}' 2>&1)
  case "$st" in
    *"$EMAX_HUB"*) ok "node_status 经 MCP 报出真实 hub 地址";;
    *) no "node_status 没报出 hub:$(printf '%s' "$st" | head -c 160)";;
  esac

  fnd=$(HOME=$INK_HOME $MCP call agents_find '{"capability":"text.digest"}' 2>&1)
  case "$fnd" in
    *"$CMAX_AID"*) ok "agents_find 经 MCP 在生产 hub 上找到了 cmax";;
    *) no "agents_find 没找到 cmax:$(printf '%s' "$fnd" | head -c 160)";;
  esac

  dl=$(HOME=$INK_HOME $MCP call task_delegate \
    "{\"provider\":\"$CMAX_AID\",\"capability\":\"text.digest\",\"args\":{\"text\":\"prodtest mcp\"}}" 2>&1)
  mix=$(echo "$dl" | python3 -c "
import sys,json
try: print(json.loads(json.loads(sys.stdin.read())['text']).get('interaction_id',''))
except Exception: print('')")
  if [ -z "$mix" ]; then
    no "task_delegate 经 MCP 没有排上队:$(printf '%s' "$dl" | head -c 200)"
  else
    ok "task_delegate 经 MCP 排上了 ${mix:0:14}…"
    got=""
    for _ in $(seq 1 20); do
      rr=$(HOME=$INK_HOME $MCP call task_results '{}' 2>&1)
      got=$(echo "$rr" | python3 -c "
import sys,json
try: d=json.loads(json.loads(sys.stdin.read())['text'])
except Exception: raise SystemExit
for r in d.get('results') or []:
    if r.get('interaction_id')=='$mix':
        print(json.dumps({'status':'OK' if '\"status\":\"OK\"' in (r.get('result') or '') else '?',
                          'receipt':bool(r.get('receipt_cid'))}))
        break")
      [ -n "$got" ] && break
      sleep 5
    done
    if [ -z "$got" ]; then
      no "task_results 经 MCP 没等到结果"
    else
      case "$got" in
        *'"status": "OK"'*) ok "task_results 经 MCP 取回了 OK 的结果";;
        *) no "结果状态不是 OK:$got";;
      esac
      case "$got" in
        *'"receipt": true'*) ok "MCP 取回的结果带 receipt CID —— 证据面没有在这条路上丢失";;
        *) no "MCP 取回的结果没有 receipt CID";;
      esac
    fi
  fi
fi

# ── 9l. the hub as a peer rendezvous ────────────────────────────
hd "9l p2p 会合:hub 兼做地址目录,两台机器上的节点才找得到对方"
# The p2p transport can carry traffic between machines. Discovery could
# not: it was a shared filesystem directory, which two hosts do not have,
# so the one module built to avoid the hub could only be used by nodes on
# one box — the case that does not need it.
#
# The hub answers "where do I dial this AID". Publishing is signed by the
# node itself, so the hub cannot list an agent that did not ask to be
# listed, and withdrawal is separate from deregistration.
if ! has ink93 || ! reachable "$EMAX_HUB/healthz"; then
  sk "会合检查要 ink93 与 emax hub"
else
  addr="tcp://198.51.100.7:39100"
  pub=$(ctl ink93 /p2p-advertise "{\"addr\":\"$addr\"}" 2>&1)
  case "$pub" in
    *published*) ok "ink93 用自己的签名把直连地址发布到了 hub";;
    *) no "发布失败:$(printf '%s' "$pub" | head -c 200)";;
  esac

  got=$(curl -s -m 30 "$EMAX_HUB/agents/$INK_AID/p2p" | jq_ "print(d.get('addr',''))")
  [ "$got" = "$addr" ] \
    && ok "任何人都能从 hub 查到这个地址($got)" \
    || no "查回来的地址是 '$got',应为 '$addr'"

  # The cost of the feature is that the hub now holds a list of
  # addresses. It is published, so whoever pays that cost can see it.
  cnt=$(curl -s -m 30 "$EMAX_HUB/p2p/peers" | jq_ "print(d.get('count'))")
  [ "${cnt:-0}" -ge 1 ] \
    && ok "hub 公开自己持有的地址目录($cnt 条)—— 这个特性的代价是可见的" \
    || no "地址目录读不到"

  # Not listed must be answerable as "not listed", not as an empty
  # address: the first falls back to the hub, the second would be dialled.
  code=$(curl -s -m 30 -o /dev/null -w '%{http_code}' "$EMAX_HUB/agents/$CMAX_AID/p2p")
  [ "$code" = 404 ] \
    && ok "没发布地址的节点返回 404,而不是一个空地址" \
    || info "cmax 也发布了地址(HTTP $code),这条检查这次不成立"

  wd=$(ctl ink93 /p2p-advertise '{"addr":""}' 2>&1)
  case "$wd" in
    *withdrawn*) ok "撤回成功 —— 停止直连与注销 hub 是两件事";;
    *) no "撤回失败:$(printf '%s' "$wd" | head -c 200)";;
  esac
  code=$(curl -s -m 30 -o /dev/null -w '%{http_code}' "$EMAX_HUB/agents/$INK_AID/p2p")
  [ "$code" = 404 ] && ok "撤回之后 hub 不再报这个地址" || no "撤回后仍能查到(HTTP $code)"
fi

# ── 9m. witnessing, checked the way a reader would ──────────────
hd "9m 见证:陌生人能自己验一条见证品"
# Witnessing has been running in production since it shipped — the two
# hubs pin each other hourly — and nothing here asserted anything about
# it. The whole value of a witness is that a rewritten chain becomes
# provable, and that rests on a reader being able to check an attestation
# without trusting anybody.
#
# Two things were missing when this check was written. /agents/{aid}/kel
# served only locally registered agents, and a witness is a peer hub, so
# the key needed to check the signature could not be obtained. And `anet
# verify` knew only receipts, so no command could check one. The endpoint
# told readers to verify each signature and neither half of that was
# possible.
#
# Rewrite detection itself stays in the unit suites: producing a real
# rewrite means editing a production chain, and a test that has to
# corrupt the thing it is testing does not belong against live data.
if [ "$HAVE_INK_BIN" = 0 ]; then
  sk "见证品的独立验签需要 anet 二进制($INK_BIN 不在这台机器上)"
fi
for h in emax fmax; do
  [ $h = emax ] && W=$(curl -s -m 30 "$EMAX_HUB/x402/witnesses") || W=$(viafmax /x402/witnesses)
  n=$(echo "$W" | jq_ "print(len(d.get('attestations') or []))")
  if [ "${n:-0}" = 0 ]; then
    no "$h 上一条见证品都没有 —— 这条链只对已有旧副本的读者可验"
    continue
  fi
  ok "$h 被见证 $n 次"
  wc_=$(echo "$W" | jq_ "print((d.get('health') or {}).get('witnesses'))")
  st=$(echo "$W" | jq_ "print((d.get('health') or {}).get('stale_seconds') or 0)")
  uw=$(echo "$W" | jq_ "print((d.get('health') or {}).get('unwitnessed_records'))")
  info "$h 见证者 $wc_ 个,最近一次 ${st}s 前,未被见证的记录 $uw 条"
  # One witness is one witness. Saying so is the point of publishing the
  # list rather than only the count.
  [ "${wc_:-0}" -ge 1 ] && ok "$h 的见证者名单是公布的,不是只给一个数" \
    || no "$h 没有公布见证者名单"

  # Where to go and check, which is what makes the instruction followable.
  ep=$(echo "$W" | jq_ "print(len(d.get('witness_endpoints') or {}))")
  [ "${ep:-0}" -ge 1 ] \
    && ok "$h 公布了见证者的地址,读者能自己去问" \
    || no "$h 只给了见证者 AID,读者无从解析"

  # The check itself: take one attestation and verify it with nothing but
  # the hub URL, exactly as a stranger would.
  at=$(echo "$W" | jq_ "
xs = d.get('attestations') or []
print(xs[0]['attestation'] if xs else '')")
  wa=$(echo "$W" | jq_ "
xs = d.get('attestations') or []
print(xs[0]['witness_aid'] if xs else '')")
  # Without the command there is nothing to verify with, and the checks
  # above — that the attestations exist, that the witnesses are named and
  # reachable — have already run and are worth having on their own.
  [ "$HAVE_INK_BIN" = 0 ] && continue
  if [ $h = emax ]; then
    # A stranger holding only the hub URL: the command resolves the
    # witness itself.
    out=$("$INK_BIN" verify --attestation "$at" --hub "$EMAX_HUB" 2>&1)
  else
    # cmax and ink93 cannot reach fmax, so the key history comes back
    # through emax and the check runs offline here. That exercises the
    # other half: a reader who was handed the two objects and has no
    # network at all.
    wk=$(viafmax "/agents/$wa/kel" | jq_ "print(d.get('kel',''))")
    if [ -z "$wk" ]; then
      no "fmax 不提供见证者 $wa 的密钥历史,读者无从验签"
      continue
    fi
    ok "fmax 提供了见证者的密钥历史 —— 见证品因此可验"
    out=$("$INK_BIN" verify --attestation "$at" --kel "$wk" 2>&1)
  fi
  case "$out" in
    *"signature verifies"*) ok "$h 的见证品验通了(无 daemon、无密钥)";;
    *) no "$h 的见证品验不过: $(printf '%s' "$out" | head -2 | tr '\n' ' ')";;
  esac
  case "$out" in
    *"does not say the"*) ok "输出区分了 见证过 与 链是诚实的";;
    *) no "输出没有区分 见证过 与 链是诚实的";;
  esac
done

# ── 9n. p2p delivers between two machines, for real ─────────────
hd "9n p2p:两台机器之间真的直连投递一次"
# The transport shipped able to carry traffic between machines and was
# never configured on any production node — only the rendezvous directory
# was checked. A transport nothing has ever delivered over is a claim.
#
# Two nodes on two hubs is the case it exists for, and it needs the
# referral: an address is published to the hub that verified the
# publisher's signature, so a node asking its own hub about a peer on
# another hub gets "not mine, ask there" and follows it once.
if ! has cmax || [ -z "$DMAX_AID" ] || [ -z "$CMAX_AID" ]; then
  sk "p2p 检查要 cmax 与 dmax 的 AID"
else
  # Both sides published, under their own signatures.
  ca=$(curl -s -m 30 "$EMAX_HUB/agents/$CMAX_AID/p2p" | jq_ "print(d.get('addr',''))")
  [ -n "$ca" ] && ok "cmax 的直连地址在 emax 上:$ca" \
    || no "cmax 没有发布直连地址,p2p 不可能被用到"
  da=$(viafmax "/agents/$DMAX_AID/p2p" | jq_ "print(d.get('addr',''))")
  [ -n "$da" ] && ok "dmax 的直连地址在 fmax 上:$da" \
    || no "dmax 没有发布直连地址"

  # The referral, both ways. Without it a node can only find peers that
  # bank on the same hub it does.
  hh=$(curl -s -m 30 "$EMAX_HUB/agents/$DMAX_AID/p2p" | jq_ "print(d.get('home_hub',''))")
  [ -n "$hh" ] && ok "emax 对 dmax 报出 home hub($hh),读者知道该去哪问" \
    || no "emax 只回了个 404,跨 hub 的对端无从查起"
  hh2=$(viafmax "/agents/$CMAX_AID/p2p" | jq_ "print(d.get('home_hub',''))")
  [ -n "$hh2" ] && ok "fmax 对 cmax 报出 home hub($hh2)" \
    || no "fmax 对 cmax 没有转介 —— 它的卡片没有联邦过来"

  # And a delegation that actually goes over the wire.
  before=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
    "grep -c 'delivered delegate' $CMAX_HOME/anetpeer.log 2>/dev/null || echo 0")
  out=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
    "export ANET_DATA_DIR=$CMAX_HOME/.anet; timeout 180 $CMAX_BIN delegate $DMAX_AID \
     --capability text.stats --args '{\"text\":\"prodtest p2p\"}'" 2>&1)
  pix=$(echo "$out" | jq_ "print(d.get('interaction_id',''))")
  if [ -z "$pix" ]; then
    no "p2p 委派没有排上队:$(printf '%s' "$out" | head -2 | tr '\n' ' ')"
  else
    for _ in 1 2 3 4 5 6; do
      after=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
        "grep -c 'delivered delegate' $CMAX_HOME/anetpeer.log 2>/dev/null || echo 0")
      [ "${after:-0}" -gt "${before:-0}" ] && break
      sleep 5
    done
    if [ "${after:-0}" -gt "${before:-0}" ]; then
      ok "委派经 p2p 直接投到了 dmax,没有过 hub 中继"
    else
      # Not a failure of the system: a node that cannot be dialled falls
      # back, and that is the designed behaviour. It IS a failure of this
      # check to have proved anything, and saying which is the point.
      info "这次委派没有走 p2p(对端拨不通),已回落到 hub —— 回落本身是设计行为"
      ok "拨不通时活仍然完成,回落没有把工作丢掉"
    fi
    # The work completed either way. A transport that delivers and loses
    # the answer is worse than one that never delivered.
    got=""
    for _ in $(seq 1 24); do
      got=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
        "export ANET_DATA_DIR=$CMAX_HOME/.anet; $CMAX_BIN results" 2>/dev/null | jq_ "
for x in d.get('results') or []:
    if x.get('interaction_id') == '$pix':
        print('OK' if '\"status\":\"OK\"' in (x.get('result') or '') else '?')
        break")
      [ -n "$got" ] && break
      sleep 5
    done
    [ "$got" = OK ] && ok "结果回到了发起方,状态 OK" \
      || no "p2p 委派没有拿回结果"
  fi

  # The asymmetry, reported rather than asserted away. cmax can dial dmax
  # and dmax cannot dial cmax, so the return leg goes through the hub.
  # A peer-to-peer transport that needs BOTH sides publicly dialable
  # degrades to relay whenever one side is behind a firewall, which is
  # the ordinary case.
  info "已知限制:回程需要提供方能反向拨通发起方。一侧在防火墙后时回程走 hub"
fi

# ── 9o. the operator surface ────────────────────────────────────
hd "9o 运营面:公网可达的 admin,凭据必须不是仓库里那个"
# 23 API routes, reachable from the public internet through nginx, that
# can delete agents, change quotas, moderate and run operations. Nothing
# here asserted anything about it.
#
# The check needs no credential. What it asserts is that the credentials
# this software once shipped with do NOT work — making ADMIN_TOKEN
# mandatory closed the hole for a new deployment and did nothing for one
# already running, because the old default was copied into a unit file at
# install time and stays there.
if ! reachable "$EMAX_HUB/admin/healthz"; then
  sk "admin 面不可达(可能是刻意不对外)"
else
  ok "admin 面在 $EMAX_HUB/admin/ 上,而且是公网可达的"

  # An unauthenticated call to a read endpoint must be refused. If this
  # passes, the token is decoration.
  code=$(curl -s -m 30 -o /dev/null -w '%{http_code}' "$EMAX_HUB/admin/api/overview")
  [ "$code" = 401 ] || [ "$code" = 403 ] \
    && ok "没带凭据的调用被拒($code)" \
    || no "没带凭据就能读 admin(HTTP $code)"

  # And the published defaults must not open it.
  bad=0
  for t in anetpw2077 admin changeme; do
    r=$(curl -s -m 30 -X POST "$EMAX_HUB/admin/api/login" \
        -H 'content-type: application/json' -d "{\"token\":\"$t\"}" \
        -o /dev/null -w '%{http_code}')
    if [ "$r" = 200 ]; then
      no "仓库里公开的口令 '$t' 能登进生产 admin —— 读过源码的人都能进"
      bad=1
    fi
  done
  [ "$bad" = 0 ] && ok "仓库里公开的那几个口令都进不去"

  # Rate limiting is the only thing standing between a guess and the
  # surface, so it has to actually engage.
  #
  # 25 attempts, because the limit is 20 per minute per source. A check
  # that stops below the threshold can never fail and therefore asserts
  # nothing — the first version stopped at 8 and reported an
  # inconclusive line that read like a pass.
  #
  # The cost is that this source IP is locked out for a minute after
  # each run. That is the limiter working, and it is the point.
  last=""
  for _ in $(seq 1 25); do
    last=$(curl -s -m 30 -X POST "$EMAX_HUB/admin/api/login" \
      -H 'content-type: application/json' -d '{"token":"definitely-not-it"}' \
      -o /dev/null -w '%{http_code}')
  done
  [ "$last" = 429 ] \
    && ok "连续猜测越过阈值后被限流挡住(429)" \
    || no "25 次错误口令之后仍返回 $last —— 限流没有生效"

  # The recovery surface is the most powerful thing behind this
  # credential: it can put back an agent an operator deleted, which means
  # it can also put back one they deleted on purpose.
  #
  # Exercised without a credential, because the live token is the
  # operator's and using it here would mean a test that mutates the
  # production registry. What CAN be asserted from outside is that the
  # route exists and is closed — an open restore endpoint is a way to
  # resurrect a delisted agent.
  # GET for the listing, POST for the restore: calling each with the
  # method it actually takes, or a 405 would pass for the wrong reason.
  for spec in "GET /admin/api/deleted" "POST /admin/api/deleted/did:anet:x/restore"; do
    m=${spec%% *}; ep=${spec#* }
    code=$(curl -s -m 30 -o /dev/null -w '%{http_code}' -X "$m" "$EMAX_HUB$ep" 2>/dev/null)
    # 404/405 means this build does not have the route, which is a
    # different fact from an open one and must not be reported as a
    # breach. A 200 on an admin path is the SPA: the page is public and
    # its token gate lives inside it, so an unmatched API path falls
    # through to HTML — which is exactly what an absent route looks like.
    case "$code" in
      401|403) ok "恢复入口 $m $ep 未鉴权时被拒($code)";;
      404|405) info "恢复入口 $m $ep 不存在于线上这一版($code)";;
      200)
        if curl -s -m 30 -X "$m" "$EMAX_HUB$ep" | head -c 40 | grep -qi '<!doctype\|<html'; then
          info "恢复入口 $m $ep 未匹配,落到了 SPA —— 线上这一版没有这条路由"
        else
          no "恢复入口 $m $ep 未鉴权返回了数据 —— 它能把已注销的 agent 放回来"
        fi;;
      *) no "恢复入口 $m $ep 未鉴权返回 $code";;
    esac
  done
fi

# ── 9p. a device capability, end to end across machines ─────────
hd "9p ANetLink:设备能力从 dmax 的适配器穿到 cmax 的委派"
# ANetLink is the one repository in the suite that had never been in
# production. Its ANetCore pin sat nine minor versions behind the other
# three, and nothing checked whether the C1 wire between the daemon and
# an ANetLink runtime still matched — the pin lag turned out to be a lag
# and not an incompatibility, but that was not knowable without running
# it.
#
# What is asserted here is the whole chain: an adapter publishes a
# device, the runtime serves it on the C1 socket, the daemon folds the
# real capability id into what it registers, the hub indexes it, and a
# node on another machine and another hub delegates to it.
#
# The capability id carries the device — light.onoff@sim/lamp-1 — and the
# daemon still knows nothing about what a device is. That is C1: the
# kernel routes an id it cannot interpret.
LIGHT=light.onoff@sim/lamp-1
if [ -z "$DMAX_AID" ] || ! has cmax; then
  sk "设备检查要 cmax 与 dmax 的 AID"
else
  dcaps=$(viafmax "/agents/$DMAX_AID" | jq_ "print(','.join((d.get('agent') or {}).get('caps') or []))")
  case "$dcaps" in
    *"$LIGHT"*) ok "hub 目录里有设备能力 $LIGHT";;
    *) no "hub 目录里没有 $LIGHT —— ANetLink 的能力没有到达注册表";;
  esac
  # Findable by id, not only present on the card: precise discovery is
  # what a caller actually uses.
  found=$(viafmax "/agents?cap=$LIGHT" | jq_ "
print(','.join(a.get('aid','') for a in (d.get('agents') or [])))")
  case "$found" in
    *"$DMAX_AID"*) ok "按能力 id 精确查得到 dmax";;
    *) no "按 $LIGHT 查不到 dmax";;
  esac

  out=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
    "export ANET_DATA_DIR=$CMAX_HOME/.anet; timeout 180 $CMAX_BIN delegate $DMAX_AID \
     --capability '$LIGHT' --args '{\"on\":true}'" 2>&1)
  lix=$(echo "$out" | jq_ "print(d.get('interaction_id',''))")
  if [ -z "$lix" ]; then
    no "设备委派没有排上队:$(printf '%s' "$out" | head -2 | tr '\n' ' ')"
  else
    got=""
    for _ in $(seq 1 30); do
      got=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
        "export ANET_DATA_DIR=$CMAX_HOME/.anet; $CMAX_BIN results" 2>/dev/null | jq_ "
for x in d.get('results') or []:
    if x.get('interaction_id') == '$lix':
        print(x.get('result') or ''); break")
      [ -n "$got" ] && break
      sleep 5
    done
    [ "$(echo "$got" | jq_ "print(d.get('status',''))")" = OK ] \
      && ok "跨机点亮了 dmax 上的那盏灯,状态 OK" \
      || no "设备委派没有拿回 OK:${got:0:160}"
    # The effect carries the observed state, which is what separates
    # "the call returned" from "the device did something".
    ps=$(echo "$got" | jq_ "print((d.get('metrics') or {}).get('power_state'))")
    [ "$ps" = 1 ] \
      && ok "效果里带着观测到的状态(power_state=$ps),不只是调用成功" \
      || no "效果里没有可核对的设备状态:power_state=$ps"
  fi
fi

# ── 9q. liveness and leaving ────────────────────────────────────
hd "9q 活跃信号与离开:静默两档、注销删路由留证据"
# H-15 shipped two tiers — marked quiet after an hour without collecting
# mail, out of the browsable listing after a month — and production
# asserted nothing about either. hub-leave was verified by hand once.
#
# The hour and the month cannot be produced in a live run without waiting
# or editing production data, and they are covered by unit tests that set
# the timestamp directly. What a live run CAN prove is the part those
# tests cannot: that the signal is really being written by real polling,
# and that a hub with no record says "unknown" rather than "quiet".
for h in emax fmax; do
  [ $h = emax ] && A=$(curl -s -m 30 "$EMAX_HUB/agents?all=1") || A=$(viafmax "/agents?all=1")
  fresh=$(echo "$A" | jq_ "
import datetime
now = datetime.datetime.now(datetime.timezone.utc)
bad = []
for a in d.get('agents') or []:
    ls = a.get('last_seen') or ''
    if not ls:
        continue
    t = datetime.datetime.fromisoformat(ls.replace('Z', '+00:00'))
    if (now - t).total_seconds() > 3600:
        bad.append(a.get('name'))
print(','.join(x for x in bad if x))")
  seen=$(echo "$A" | jq_ "
print(sum(1 for a in (d.get('agents') or []) if a.get('last_seen')))")
  [ "${seen:-0}" -ge 1 ] \
    && ok "$h 记录了 ${seen} 个 agent 的取信时间 —— 活跃信号取自真实轮询,不是心跳端点" \
    || no "$h 一个取信时间都没有,静默判定没有依据"
  [ -z "$fresh" ] \
    && ok "$h 上没有超过一小时未取信的 agent" \
    || info "$h 上这些已超过一小时未取信:$fresh"
  # Quiet must be a claim the hub can support. An agent it has never seen
  # poll has no timestamp, and reporting that as quiet would be the hub
  # asserting something it does not know.
  wrong=$(echo "$A" | jq_ "
print(','.join(a.get('name','?') for a in (d.get('agents') or [])
                if not a.get('last_seen') and a.get('quiet')))")
  [ -z "$wrong" ] \
    && ok "$h 没有把 无记录 当成 已静默" \
    || no "$h 把无记录的 agent 报成静默:$wrong"
done

# Leaving: routing goes, evidence stays. Run with a throwaway identity so
# nothing in the live topology is deregistered.
# A throwaway node, not a throwaway config: hub-register goes through a
# daemon's control plane, so an identity with no daemon cannot register.
# Its own port and its own data dir, so it can never be confused with a
# live node — which is exactly the confusion that made ANET_HOME count as
# pinning in the first place.
TMPH=$(mktemp -d)
mkdir -p "$TMPH/.anet"
cat > "$TMPH/.anet/config.json" <<'JSON'
{"control_addr": "127.0.0.1:29671", "accept_delegations": false}
JSON
ANET_DATA_DIR="$TMPH/.anet" setsid "$INK_BIN" daemon >"$TMPH/daemon.log" 2>&1 </dev/null &
LEAVER_PID=$!
for _ in $(seq 1 20); do
  curl -sf -m 3 http://127.0.0.1:29671/ping >/dev/null 2>&1 && break
  sleep 1
done
if ANET_DATA_DIR="$TMPH/.anet" "$INK_BIN" hub-register "$EMAX_HUB" \
     --name prodtest-leaver --caps probe.noop >/dev/null 2>&1; then
  LA=$(ANET_DATA_DIR="$TMPH/.anet" "$INK_BIN" status 2>/dev/null | jq_ "print(d.get('aid',''))")
else
  LA=""
  info "  临时 daemon 日志:$(tail -2 "$TMPH/daemon.log" 2>/dev/null | tr '\n' ' ')"
fi
if [ -z "$LA" ]; then
  sk "临时身份注册失败,跳过 hub-leave 检查"
else
  code=$(curl -s -m 30 -o /dev/null -w '%{http_code}' "$EMAX_HUB/agents/$LA")
  [ "$code" = 200 ] && ok "临时身份注册上了 hub" || no "临时身份没能注册($code)"
  if ANET_DATA_DIR="$TMPH/.anet" "$INK_BIN" hub-leave "$EMAX_HUB" >/dev/null 2>&1; then
    ok "hub-leave 被接受(由 agent 自己签名)"
  else
    no "hub-leave 被拒"
  fi
  # Routing gone: it must no longer be findable by the capability it
  # advertised. Being listed as a bare node is fine — what must go is
  # deliverability.
  still=$(curl -s -m 30 "$EMAX_HUB/agents?cap=probe.noop" | jq_ "
print(','.join(a.get('aid','') for a in (d.get('agents') or [])))")
  case "$still" in
    *"$LA"*) no "注销之后仍能按能力查到它 —— 活还会被投进一个没人轮询的信箱";;
    *) ok "注销之后按能力查不到了,路由是真的删了";;
  esac
  # And the identity is still resolvable, because evidence about it must
  # not become unverifiable just because it left.
  # Proof outlives routing. Deregistering deleted the agent row, which
  # held the only copy of the KEL, so the hub went on serving every
  # receipt and review this agent had signed and could no longer produce
  # the key that checks them. `anet verify --receipt --hub` broke for
  # every agent that had ever left. Caught here as an info line first,
  # then fixed and promoted to an assertion.
  kc=$(curl -s -m 30 -o /dev/null -w '%{http_code}' "$EMAX_HUB/agents/$LA/kel")
  [ "$kc" = 200 ] \
    && ok "它的密钥历史仍然查得到 —— 离开删的是路由,不是证据" \
    || no "注销后密钥历史返回 $kc —— 它签过的收据在这个 hub 上再也验不了"
fi
kill "$LEAVER_PID" 2>/dev/null
rm -rf "$TMPH"

# ── 9r. the taskboard ───────────────────────────────────────────
hd "9r taskboard:一张卡片走完整个流转,越权与乱序被真的拒掉"
# Running on the production hub since it shipped, reachable, and never
# touched by a live run. The reason it was never touched is worth stating
# plainly: the board has NO CLIENT in this suite. Nine mutation endpoints,
# each gated behind a KEL-signed challenge, and nothing outside the
# package's own tests could produce one — so it could be read and not
# used. prodtest drives it with anetfixture's signer. If agents are meant
# to use the board they need real commands, and that is a separate
# decision from this check.
tbsign() {
  "$FIXTURE" relay-sign --home "$INK_HOME/.anet" --action "task.$1" 2>/dev/null
}
tbpost() {
  local action=$1 extra=$2
  local sig; sig=$(tbsign "$action")
  [ -z "$sig" ] && { echo '{"error":"could not sign"}'; return; }
  python3 - "$sig" "$extra" <<'PY' > /tmp/prodtest-tb-body.json
import json, sys
b = json.loads(sys.argv[1]); b.update(json.loads(sys.argv[2]))
print(json.dumps(b))
PY
  curl -s -m 30 -X POST "$EMAX_HUB/tasks/$action" \
    -H 'content-type: application/json' -d @/tmp/prodtest-tb-body.json
}
tbstate() { echo "$1" | jq_ "
c = d.get('card') or {}
print((c.get('state','') + '/' + c.get('column','')) if c else '')"; }

# The module client, which is what an agent would actually use.
#
# The fixture path below proves the wire; this proves an agent can reach
# it without one. Both matter: the fixture was written because there was
# no client, and a client that exists and is never exercised is how the
# board got into this state to begin with.
if [ -n "$INK_AID" ] && has cmax; then
  tbcaps=$(curl -s -m 30 "$EMAX_HUB/agents/$INK_AID" | jq_ "
print(','.join((d.get('agent') or {}).get('caps') or []))")
  case "$tbcaps" in
    *task.board*)
      ok "ink93 通过 module/taskboard 提供了看板能力"
      out=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
        "export ANET_DATA_DIR=$CMAX_HOME/.anet; timeout 180 $CMAX_BIN delegate $INK_AID \
         --capability task.board --args '{}'" 2>&1)
      bix=$(echo "$out" | jq_ "print(d.get('interaction_id',''))")
      got=""
      for _ in $(seq 1 24); do
        got=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
          "export ANET_DATA_DIR=$CMAX_HOME/.anet; $CMAX_BIN results" 2>/dev/null | jq_ "
for x in d.get('results') or []:
    if x.get('interaction_id') == '$bix':
        print(x.get('result') or ''); break")
        [ -n "$got" ] && break
        sleep 5
      done
      [ "$(echo "$got" | jq_ "print(d.get('status',''))")" = OK ] \
        && ok "另一台机器经模块读到了板子:$(echo "$got" | jq_ "print(d.get('message',''))")" \
        || no "模块读板失败:${got:0:140}"
      # The board's own answer has to come back as evidence, not a count
      # this node computed and asks to be believed.
      case "$got" in
        *observed_state*) ok "效果里带着板子自己的答复";;
        *) no "效果里没有板子的原始答复";;
      esac;;
    *) info "ink93 未配 taskboard 模块,跳过模块侧检查";;
  esac
fi

if [ ! -x "$FIXTURE" ]; then
  sk "taskboard 检查需要 anetfixture"
else
  r=$(tbpost create '{"title":"prodtest card","column":"backlog","taskdoc_cid":"bafyreiez5ziuzobff7qdlcklemjevbwu43sxakol3gydk7ifushu7t4i3u"}')
  CARD=$(echo "$r" | jq_ "print((d.get('card') or {}).get('id',''))")
  if [ -z "$CARD" ]; then
    no "建卡失败:$(printf '%s' "$r" | head -c 160)"
  else
    ok "在生产 hub 上建了一张卡($CARD)"
    # A card must carry the document it is about. A board of titles is a
    # board of intentions.
    [ "$(tbstate "$r")" = created/backlog ] \
      && ok "新卡状态 created/backlog" || no "新卡状态是 $(tbstate "$r")"

    # Out of order must be refused. This is the half that matters: a
    # board that accepts any transition records a story rather than a
    # process.
    bad=$(tbpost accept "{\"card_id\":\"$CARD\"}")
    case "$(echo "$bad" | jq_ "print(d.get('error',''))")" in
      *"only submitted cards are accepted"*) ok "未提交就验收被拒";;
      *) no "未提交的卡片被验收了:$(printf '%s' "$bad" | head -c 140)";;
    esac

    for step in "move ready" "claim -" "submit -" "accept -"; do
      a=${step%% *}; extra=${step#* }
      if [ "$extra" = - ]; then body="{\"card_id\":\"$CARD\"}"
      else body="{\"card_id\":\"$CARD\",\"column\":\"$extra\"}"; fi
      [ "$a" = submit ] && body="{\"card_id\":\"$CARD\",\"note\":\"prodtest\"}"
      st=$(tbstate "$(tbpost "$a" "$body")")
      [ -n "$st" ] && info "  $a → $st" || no "$a 这一步失败了"
    done
    final=$(curl -s -m 30 "$EMAX_HUB/tasks/cards/$CARD" | jq_ "
c = d.get('card') or {}
print(c.get('state','') + '/' + c.get('column',''))")
    [ "$final" = accepted/done ] \
      && ok "卡片走完 created→ready→claimed→submitted→accepted,落在 done" \
      || no "卡片停在 $final"

    # And an unsigned mutation must be refused, or the signature is
    # decoration.
    uc=$(curl -s -m 30 -o /dev/null -w '%{http_code}' -X POST "$EMAX_HUB/tasks/claim" \
      -H 'content-type: application/json' -d "{\"card_id\":\"$CARD\"}")
    [ "$uc" = 401 ] || [ "$uc" = 403 ] \
      && ok "没签名的改动被拒($uc)" || no "没签名就能改板子(HTTP $uc)"
  fi
fi

# ── 9s. auto-reply ──────────────────────────────────────────────
hd "9s 自动回复:委派到达 → 调模型 → 答复经 hub 回来"
# Auto-reply had only ever run in `scenario --live`. No production node
# was configured for it, so the path that makes a node answer without a
# human — the one an unattended agent actually depends on — had never
# crossed a real network.
#
# Skipped rather than failed when cmax has no backend configured: this
# needs a model credential, and a check that fails for want of a key
# teaches an operator to ignore red.
ar=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
  "export ANET_DATA_DIR=$CMAX_HOME/.anet; $CMAX_BIN autoreply show" 2>/dev/null)
case "$ar" in
  *backend*) ok "cmax 配了自动回复后端";;
  *) sk "cmax 未配自动回复(需要模型凭据)"; ar="";;
esac
if [ -n "$ar" ]; then
  # The backend answers at all, checked without touching the hub. A
  # failure here is a credential or endpoint problem, and separating it
  # from the network path is what makes the next assertion readable.
  t=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
    "export ANET_DATA_DIR=$CMAX_HOME/.anet; timeout 120 $CMAX_BIN autoreply test '一句话回答:1+1 等于几'" 2>&1)
  [ "$(echo "$t" | jq_ "print(d.get('status',''))")" = ok ] \
    && ok "后端本地自检通过(不经 hub)" \
    || no "后端自检失败:$(printf '%s' "$t" | head -c 160)"

  # And the whole path: ink93 delegates a conversational task, cmax's
  # loop picks it up, calls the model, and the answer comes back through
  # the real hub.
  #
  # Needs a requester this machine can drive. The scheduled copy runs on
  # cmax and cannot reach ink93's control port, so without this the whole
  # section reported "the delegation did not queue" — a missing requester
  # dressed as a broken auto-reply.
  if ! has ink93; then
    sk "自动回复的整条路径需要一个本机够得着的发起方(ink93)"
    aix=""
  else
    aix=$(ctl ink93 /delegate \
      "{\"provider\":\"$CMAX_AID\",\"goal\":\"用一句话说明什么是默克尔树\"}" \
      | jq_ "print(d.get('interaction_id',''))")
  fi
  if [ -z "$aix" ]; then
    [ "$(has ink93 && echo y)" = y ] && no "对话委派没有排上队"
  else
    reply=""
    for _ in $(seq 1 30); do
      reply=$(ctl ink93 /thread "{\"interaction_id\":\"$aix\"}" | jq_ "
t = d.get('thread') or {}
ms = [m for m in (t.get('messages') or []) if m.get('from') != 'me']
print(ms[-1].get('body','') if ms else '')")
      [ -n "$reply" ] && break
      sleep 6
    done
    if [ -n "$reply" ]; then
      ok "cmax 自动答复了,经真 hub 回到 ink93(${#reply} 字节)"
      info "  ${reply:0:80}"
    else
      no "等了三分钟没有自动答复"
    fi
    # The runaway guard has to exist, or an auto-replying pair can talk
    # to each other until somebody notices the bill.
    mx=$(echo "$ar" | jq_ "print(d.get('max_auto_replies') or 0)")
    info "  失控护栏 max_auto_replies=${mx:-默认 30}"
  fi
fi

# ── 9t. the two invariants, on a node that can actually break them ──
hd "9t 两条不变式:org 数据不进公开发布"
# Both invariants are enforced at one chokepoint — what this node
# publishes to its hub — and both were, at one time or another, mechanisms
# with nothing behind them. INV-1's runtime guard had no call site at all
# until it was wired here. INV-2 has a producer (the org module declares
# its org id confidential) and had never been exercised against a live
# node.
#
# cmax is the node that can break them: it runs the org module, so it
# holds a real confidential value, and it publishes a profile to a real
# hub that federates and serves a browsable directory.
if ! has cmax; then
  sk "不变式检查要 cmax(它是配了 org 的那个节点)"
else
  orgcaps=$(ctl cmax /status '{}' | jq_ "print(','.join(d.get('caps') or []))")
  case "$orgcaps" in
    *org.info*) ok "cmax 配了 org 模块 —— 它持有一个真的机密值";;
    *) sk "cmax 未配 org,不变式无从检验"; orgcaps="";;
  esac
  if [ -n "$orgcaps" ]; then
    # The org id, derived the way anyone would: from the genesis the node
    # is configured with.
    # The value the node must never disclose, derived the way anyone
    # holding the genesis would derive it.
    GEN=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST "python3 - <<'PY'
import json
print(json.load(open('$CMAX_HOME/.anet/config.json'))['modules']['org']['genesis'])
PY" 2>/dev/null)
    OID=$([ -n "$GEN" ] && [ "$HAVE_FIXTURE" = 1 ] && "$FIXTURE" org-id --genesis "$GEN" 2>/dev/null)
    if [ -z "$OID" ] && [ "$HAVE_FIXTURE" = 0 ]; then
      sk "推导 org id 需要 anetfixture($FIXTURE 不在这台机器上)"
    elif [ -z "$OID" ]; then
      no "取不到 cmax 的 org id"
    else
      # Publishing something that carries the org id must be refused. The
      # realistic leak is not a field somebody added on purpose — it is
      # prose an agent wrote about itself.
      before=$(ctl cmax /status '{}' | jq_ "print(d.get('summary',''))")
      out=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
        "export ANET_DATA_DIR=$CMAX_HOME/.anet; timeout 60 $CMAX_BIN profile set \
         --summary 'prodtest $OID'" 2>&1)
      case "$out" in
        *"confidential token"*|*inv2*)
          ok "含机密值的 profile 被拒 —— INV-2 在生产上真的在跑";;
        *) no "含机密值的 profile 发出去了:$(printf '%s' "$out" | head -2 | tr '\n' ' ')";;
      esac
      # And an ordinary profile still publishes. A guard that refuses
      # everything protects nothing and gets switched off.
      ok2=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
        "export ANET_DATA_DIR=$CMAX_HOME/.anet; timeout 60 $CMAX_BIN profile set \
         --summary '$before'" 2>&1)
      case "$ok2" in
        *error*) no "普通 profile 也被拒了:$(printf '%s' "$ok2" | head -2 | tr '\n' ' ')";;
        *) ok "普通 profile 照常发布,档案已还原";;
      esac
    fi
  fi
fi

# ── 9u. a real protocol, end to end ─────────────────────────────
hd "9u ANetMock:真 ONVIF 设备穿过适配器、daemon、hub 到另一台机器"
# ANetMock exists to test ANetLink's adapters against real wire formats —
# real SOAP, real ISAPI, real Dahua CGI — and joint.sh names the chain in
# a comment while never running it. So the adapters were tested against
# fakes written by the same people who wrote the adapters, which is the
# arrangement every defect this month has been found hiding in.
#
# It also covers what L-1 says has no simulator: PTZ and snapshot. Those
# are not "no simulator exists" any more; they are "no REAL camera has
# been tested", which is a different and smaller gap.
PTZ=ptz.move@onvif/camera-006
RTSP=stream.rtsp@onvif/camera-007
if [ -z "$DMAX_AID" ] || ! has cmax; then
  sk "ANetMock 检查要 cmax 与 dmax"
else
  dcaps=$(viafmax "/agents/$DMAX_AID" | jq_ "print(','.join((d.get('agent') or {}).get('caps') or []))")
  case "$dcaps" in
    *"$PTZ"*) ok "hub 目录里有 mock 摄像头的 PTZ 能力";;
    *) sk "dmax 未接 ANetMock,跳过"; dcaps="";;
  esac
  if [ -n "$dcaps" ]; then
    # A device that can be read back reports OK and carries the readback.
    # "I sent it" and "I confirmed it" are different claims and the
    # adapter has to make the right one.
    out=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
      "export ANET_DATA_DIR=$CMAX_HOME/.anet; timeout 180 $CMAX_BIN delegate $DMAX_AID \
       --capability '$PTZ' --args '{\"pan\":0.3,\"tilt\":0.1}'" 2>&1)
    pix=$(echo "$out" | jq_ "print(d.get('interaction_id',''))")
    got=""
    for _ in $(seq 1 30); do
      got=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
        "export ANET_DATA_DIR=$CMAX_HOME/.anet; $CMAX_BIN results" 2>/dev/null | jq_ "
for x in d.get('results') or []:
    if x.get('interaction_id') == '$pix':
        print(x.get('result') or ''); break")
      [ -n "$got" ] && break
      sleep 5
    done
    [ "$(echo "$got" | jq_ "print(d.get('status',''))")" = OK ] \
      && ok "跨机把 mock 摄像头转过去了,状态 OK" \
      || no "PTZ 委派没有拿回 OK:${got:0:160}"
    # A readback, not a particular one. The first version asserted
    # "pan 0→", which held only on a camera that had never been moved —
    # a PTZ head accumulates, so every run after the first started
    # somewhere else and the check failed on a correct result. An
    # assertion that encodes the first run's state tests the run, not the
    # system.
    obs=$(echo "$got" | jq_ "print((d.get('evidence') or {}).get('observed_state',''))")
    case "$obs" in
      *"pan "*"→"*) ok "效果里带着真实读回($obs)";;
      *) no "效果里没有可核对的读回:$obs";;
    esac

    # And one that cannot be verified must say so, however cleanly the
    # call succeeded. A stream URI is what the camera claims until
    # somebody probes it.
    out2=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
      "export ANET_DATA_DIR=$CMAX_HOME/.anet; timeout 180 $CMAX_BIN delegate $DMAX_AID \
       --capability '$RTSP' --args '{}'" 2>&1)
    rix=$(echo "$out2" | jq_ "print(d.get('interaction_id',''))")
    got2=""
    for _ in $(seq 1 30); do
      got2=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
        "export ANET_DATA_DIR=$CMAX_HOME/.anet; $CMAX_BIN results" 2>/dev/null | jq_ "
for x in d.get('results') or []:
    if x.get('interaction_id') == '$rix':
        print(x.get('result') or ''); break")
      [ -n "$got2" ] && break
      sleep 5
    done
    [ "$(echo "$got2" | jq_ "print(d.get('status',''))")" = UNVERIFIED ] \
      && ok "未探测的流报 UNVERIFIED,而不是把 发出去了 说成 确认了" \
      || no "stream.rtsp 状态是 $(echo "$got2" | jq_ "print(d.get('status',''))"),应为 UNVERIFIED"

    # The two vendor wire formats L-1 says have no simulator. They do
    # now, and these are the adapters that had only ever been tested
    # against fakes written by the same people who wrote them.
    #
    # system.info because it reads something only the device knows — its
    # model and firmware — so a wrong answer cannot come from the
    # adapter's own assumptions.
    for vend in "hikvision/camera-107 isapi DS-2CD2143G0-I" \
                "dahua/camera-108 dahua-cgi IPC-HFW4431R-Z"; do
      set -- $vend
      dev=$1; proto=$2; model=$3
      vout=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
        "export ANET_DATA_DIR=$CMAX_HOME/.anet; timeout 180 $CMAX_BIN delegate $DMAX_AID \
         --capability 'system.info@$dev' --args '{}'" 2>&1)
      vix=$(echo "$vout" | jq_ "print(d.get('interaction_id',''))")
      vgot=""
      for _ in $(seq 1 24); do
        vgot=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
          "export ANET_DATA_DIR=$CMAX_HOME/.anet; $CMAX_BIN results" 2>/dev/null | jq_ "
for x in d.get('results') or []:
    if x.get('interaction_id') == '$vix':
        print(x.get('result') or ''); break")
        [ -n "$vgot" ] && break
        sleep 5
      done
      vobs=$(echo "$vgot" | jq_ "print((d.get('evidence') or {}).get('observed_state',''))")
      vproto=$(echo "$vgot" | jq_ "print((d.get('evidence') or {}).get('protocol',''))")
      case "$vobs" in
        *"$model"*) ok "$proto 从真协议读回了型号($vobs)";;
        *) no "$proto 没有读回设备型号:$vobs";;
      esac
      [ "$vproto" = "$proto" ] \
        && ok "效果记名的协议是 $proto,不是笼统的一句 device" \
        || no "效果记的协议是 $vproto,应为 $proto"
    done

    # The two frontends that were empty directories until they were
    # written. A suite that counted six and could start four is why these
    # are asserted rather than assumed.
    #
    # Modbus reads a register and Zigbee flips a switch, because the two
    # exercise opposite halves: one is a value only the device knows, the
    # other is a state this call changed and read back.
    mb=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
      "export ANET_DATA_DIR=$CMAX_HOME/.anet; timeout 180 $CMAX_BIN delegate $DMAX_AID \
       --capability 'sensor.temperature@modbus/climate-004' --args '{}'" 2>&1)
    mix=$(echo "$mb" | jq_ "print(d.get('interaction_id',''))")
    mgot=""
    for _ in $(seq 1 24); do
      mgot=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
        "export ANET_DATA_DIR=$CMAX_HOME/.anet; $CMAX_BIN results" 2>/dev/null | jq_ "
for x in d.get('results') or []:
    if x.get('interaction_id') == '$mix':
        print(x.get('result') or ''); break")
      [ -n "$mgot" ] && break
      sleep 5
    done
    [ "$(echo "$mgot" | jq_ "print(d.get('status',''))")" = OK ] \
      && ok "Modbus 从真寄存器读到了温度" \
      || no "Modbus 读取失败:${mgot:0:140}"
    [ "$(echo "$mgot" | jq_ "print((d.get('evidence') or {}).get('protocol',''))")" = modbus ] \
      && ok "效果记名的协议是 modbus" || no "Modbus 效果没有记名协议"

    zb=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
      "export ANET_DATA_DIR=$CMAX_HOME/.anet; timeout 180 $CMAX_BIN delegate $DMAX_AID \
       --capability 'switch.onoff@mqttbridge/light-083' --args '{\"on\":true}'" 2>&1)
    zix=$(echo "$zb" | jq_ "print(d.get('interaction_id',''))")
    zgot=""
    for _ in $(seq 1 24); do
      zgot=$(ssh -n -o ConnectTimeout=20 $CMAX_HOST \
        "export ANET_DATA_DIR=$CMAX_HOME/.anet; $CMAX_BIN results" 2>/dev/null | jq_ "
for x in d.get('results') or []:
    if x.get('interaction_id') == '$zix':
        print(x.get('result') or ''); break")
      [ -n "$zgot" ] && break
      sleep 5
    done
    [ "$(echo "$zgot" | jq_ "print(d.get('status',''))")" = OK ] \
      && ok "Zigbee 开关翻转并读回成功" \
      || no "Zigbee 调用失败:${zgot:0:140}"
    # ON, not 1: a client reading the boolean the way zigbee2mqtt
    # actually publishes it got an empty string until the mock's two
    # halves were made to agree.
    [ "$(echo "$zgot" | jq_ "print((d.get('evidence') or {}).get('observed_state',''))")" = ON ] \
      && ok "读回是 ON —— 线上是厂商编码,不是内部数值" \
      || no "Zigbee 读回不是 ON"
  fi
fi

# ── 9v. ANetLink's own MCP surface ──────────────────────────────
hd "9v ANetLink MCP:列设备 → 用列表给的名字直接调用"
# Four tools, never driven outside the package's own tests. The one thing
# a model client does — read a tool's output, fill the next tool's input
# from it — is the thing this checks, because it is the thing that was
# broken: the listing published "key" while the invoke took "device", so
# following the obvious path failed with "unexpected additional
# properties".
if [ -z "$DMAX_AID" ]; then
  sk "MCP 面检查要 dmax"
else
  mcpout=$(ssh -n -o ConnectTimeout=30 $CMAX_HOST \
    "ssh -n -o ConnectTimeout=20 root@dmax.chatchat.space 'bash /root/ship/mcp-probe.sh'" 2>/dev/null)
  if [ -z "$mcpout" ]; then
    sk "ANetLink MCP 探针不可用(/root/ship/mcp-probe.sh 未部署)"
  else
    case "$mcpout" in
      *devices_list*) ok "ANetLink MCP 起来了,报出了工具";;
      *) no "MCP 没有列出工具";;
    esac
    # The listing has to publish the field the invoke takes, under that
    # name. Anything else makes a client guess.
    case "$mcpout" in
      *'\"device\":\"'*) ok "devices_list 报出的字段就叫 device";;
      *) no "devices_list 没有报出 device 字段 —— 客户端得靠猜";;
    esac
    # And the invoke, called with exactly that value, has to work.
    case "$mcpout" in
      *'\"id\":4'*|*'"id":4'*) : ;;
      *) no "MCP invoke 没有回应";;
    esac
    case "$mcpout" in
      *"DS-2CD2143G0-I"*)
        ok "经 MCP 从真 ISAPI 读回了型号 —— 列表给的名字可以直接拿去调用";;
      *"unexpected additional properties"*)
        no "MCP 列表与调用对同一个东西用了两个名字";;
      *) no "MCP invoke 没有拿回设备信息";;
    esac
  fi
fi

# ── 10. what a node can check for itself ────────────────────────
hd "10  节点自查:审计发放链 + 对账"
if ! has dmax; then
  sk "自查要 dmax 的控制面,这台机器够不着"
else
  aud=$(ctl dmax /audit-hub '{}')
  av=$(echo "$aud" | jq_ "print(d.get('verified'))")
  ae=$(echo "$aud" | jq_ "print(d.get('entries'))")
  [ "$av" = True ] && ok "dmax 独立验通了 fmax 的发放链($ae 条,只用 hub 公布的密钥历史)" \
    || no "发放链验证失败:$(echo "$aud" | jq_ "print(d.get('problems') or d.get('error'))")"

  rec=$(ctl dmax /reconcile '{}')
  ra=$(echo "$rec" | jq_ "print(d.get('agrees'))")
  rb=$(echo "$rec" | jq_ "print(d.get('balance'))")
  rd=$(echo "$rec" | jq_ "print(d.get('derived_from_entries'))")
  if [ "$ra" = True ]; then
    ok "dmax 的账与 hub 的流水对得上(余额 $rb == 流水合计 $rd)"
  else
    # Not a hard failure: entries predating the fix that made settlement
    # write them will never have counterparts. What matters is that the
    # discrepancy is reported rather than invisible.
    miss=$(echo "$rec" | jq_ "print(len(d.get('missing_from_hub') or []))")
    # Two different facts, and merging them hides the one that matters.
    # The sums agreeing means the hub's own ledger is internally
    # consistent for this account; items not matching means this node
    # holds payment records the hub has no entry for. A run can have the
    # first and not the second.
    if [ "$rb" = "$rd" ]; then
      ok "hub 的账本对这个账户自洽(余额 $rb == 流水合计 $rd)"
      info "另有 $miss 项本机记录在 hub 上找不到对应条目 —— 早于结算写流水那次修复的历史"
    else
      info "余额 $rb 与流水合计 $rd 不等,另有 $miss 项对不上"
    fi
    ok "对账把差异报了出来,而不是让它不可见"
  fi
fi

# ── 11. restarts, with work in flight ───────────────────────────
hd "11  重启:活还在路上的时候"
# joint.sh proves the evidence chain survives a restart. Nothing checked
# what happens to work that is in flight, which is where the interesting
# failures are — and where D-7 already found two: a redelivered
# delegation executed a second time, and a redelivered result was
# recorded on the chain twice.
#
# Guarded, because it restarts a production hub. RESTART=0 skips it.
if [ "${RESTART:-1}" = 0 ]; then
  sk "重启测试已按 RESTART=0 跳过"
elif ! has ink93 || ! has cmax; then
  sk "重启测试要 ink93 与 cmax 两侧"
else
  # 11a — the hub restarts while a message is queued for a node that is
  # not collecting it. The mailbox is SQLite, so it should survive; what
  # is being checked is that it does.
  info "11a hub 重启,消息还在队列里"
  ssh -n -o ConnectTimeout=20 $CMAX_HOST "systemctl stop anet4" >/dev/null 2>&1
  sleep 2
  qix=$(ctl ink93 /delegate "{\"provider\":\"$CMAX_AID\",\"capability\":\"text.digest\",\"args\":{\"text\":\"restart-a\"}}" \
        | jq_ "print(d.get('interaction_id',''))")
  if [ -z "$qix" ]; then
    no "投递没排上队"
  else
    ssh -n -o ConnectTimeout=20 $EMAX_HOST "systemctl restart anet-hub" >/dev/null 2>&1
    for _ in $(seq 1 20); do curl -sf -m 5 "$EMAX_HUB/healthz" >/dev/null && break; sleep 2; done
    ok "hub 重启后恢复服务"
    ssh -n -o ConnectTimeout=20 $CMAX_HOST "systemctl start anet4" >/dev/null 2>&1
    got=""
    for _ in $(seq 1 45); do
      got=$(ctl ink93 /results '{}' | jq_ "
for x in d.get('results') or []:
    if x['interaction_id']=='$qix': print(x['result']); break")
      [ -n "$got" ] && break
      sleep 2
    done
    [ "$(echo "$got" | jq_ "print(d.get('status',''))")" = OK ] \
      && ok "排队中的活跨过了 hub 重启,仍然被送到并完成" \
      || no "hub 重启后这条投递丢了: ${got:0:120}"
  fi

  # 11b — the provider restarts between receiving and answering. The
  # interaction is in its own store, so it should pick up where it left
  # off rather than losing the request.
  info "11b 提供方收到之后、回答之前重启"
  bix=$(ctl ink93 /delegate "{\"provider\":\"$CMAX_AID\",\"capability\":\"text.digest\",\"args\":{\"text\":\"restart-b\"}}" \
        | jq_ "print(d.get('interaction_id',''))")
  sleep 3   # long enough for it to arrive, short enough to interrupt
  ssh -n -o ConnectTimeout=20 $CMAX_HOST "systemctl restart anet4" >/dev/null 2>&1
  got=""
  for _ in $(seq 1 45); do
    got=$(ctl ink93 /results '{}' | jq_ "
for x in d.get('results') or []:
    if x['interaction_id']=='$bix': print(x['result']); break")
    [ -n "$got" ] && break
    sleep 2
  done
  [ "$(echo "$got" | jq_ "print(d.get('status',''))")" = OK ] \
    && ok "提供方重启后仍然答复了这次委派" \
    || no "提供方重启丢了这次委派: ${got:0:120}"

  # 11c — the one that has actually gone wrong. Delivery is at-least-once
  # (the relay acks after processing, so a crash in between replays), and
  # execution must not be. A redelivered delegation must not run the work
  # a second time, issue a second receipt, or add a second chain record.
  info "11c 答复之后、ack 之前重启:重投不得重复执行"
  before=$(ctl cmax /evidence '{"event_type":"anet.capability.effect","limit":500}' \
           | jq_ "print(len(d.get('records') or []))")
  cix2=$(ctl ink93 /delegate "{\"provider\":\"$CMAX_AID\",\"capability\":\"text.digest\",\"args\":{\"text\":\"restart-c\"}}" \
         | jq_ "print(d.get('interaction_id',''))")
  # Restart repeatedly while it is being handled, so at least one restart
  # lands in the window between answering and the ack.
  for _ in 1 2 3; do
    sleep 2
    ssh -n -o ConnectTimeout=20 $CMAX_HOST "systemctl restart anet4" >/dev/null 2>&1
  done
  got=""
  for _ in $(seq 1 60); do
    got=$(ctl ink93 /results '{}' | jq_ "
for x in d.get('results') or []:
    if x['interaction_id']=='$cix2': print(x['result']); break")
    [ -n "$got" ] && break
    sleep 2
  done
  [ "$(echo "$got" | jq_ "print(d.get('status',''))")" = OK ] \
    && ok "反复重启之后这次委派仍然完成了" || no "反复重启后没有结果"
  after=$(ctl cmax /evidence '{"event_type":"anet.capability.effect","limit":500}' \
          | jq_ "print(len(d.get('records') or []))")
  grew=$(( ${after:-0} - ${before:-0} ))
  # One delegation, one execution. More than one means a redelivery was
  # executed again — the failure D-7 fixed, which had never been checked
  # against a real restart.
  [ "$grew" -le 1 ] \
    && ok "链上只多了 $grew 条能力效果 —— 重投没有导致重复执行" \
    || no "链上多了 $grew 条能力效果,一次委派被执行了多次"
  # And the requester must not have recorded two acceptances for one
  # interaction.
  acc=$(ctl ink93 /evidence '{"event_type":"anet.result.accepted","limit":500}' \
        | jq_ "
print(len([r for r in (d.get('records') or []) if (r.get('payload') or {}).get('interaction_id')=='$cix2']))")
  [ "${acc:-0}" -le 1 ] \
    && ok "请求方对这次交互只记了 ${acc:-0} 条接受" \
    || no "请求方记了 $acc 条接受,一次交互被当成多次完成"

  # Leave the topology settled. This section restarts cmax three times,
  # and a run started immediately afterwards catches it mid-recovery and
  # reports failures that belong to this section rather than to what it
  # is testing. Waiting here is cheaper than a run that is intermittently
  # and inexplicably red.
  for _ in $(seq 1 30); do
    ssh -n -o ConnectTimeout=10 $CMAX_HOST "curl -sf -m 5 http://127.0.0.1:$CMAX_PORT/ping >/dev/null" 2>/dev/null && break
    sleep 2
  done
  info "重启测试结束,节点已恢复"
fi


printf '\n\033[1m── %d 通过, %d 失败, %d 跳过 ──\033[0m\n' "$pass" "$fail" "$skip"
[ "$fail" -eq 0 ]
