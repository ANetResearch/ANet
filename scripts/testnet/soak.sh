#!/usr/bin/env bash
# soak.sh — 实验室测试网上的长稳测试(docs/notes/0036)。在编排机(ink88)上运行,经 ssh 驱动 lab 岛两台主机。
#
#   bash scripts/testnet/soak.sh setup              部署(hub1@Ink89 ⇄ hub2@Ink90、7 个节点)并配置、起辅助进程、冒烟
#   bash scripts/testnet/soak.sh smoke              每种负载各跑一次(不起常驻负载)
#   bash scripts/testnet/soak.sh run [小时]         常驻混合负载 + 每 5 分钟采样 + 按时刻表重启,到时停负载、排空、
#                                                   末次采样、check(默认 4 小时;SOAK_CHAOS 改时刻表)
#   bash scripts/testnet/soak.sh sample             采一次样(追加到 $SOAK_STATE/samples.jsonl)
#   bash scripts/testnet/soak.sh stop               让负载停止并排空(不停节点)
#   bash scripts/testnet/soak.sh check              拉回事件与账目、canary 扫描、出报告($SOAK_STATE/report.md)
#   bash scripts/testnet/soak.sh gdump <名>…        给测试网 Go 进程发 SIGQUIT,取运行时的全部 goroutine 栈(进程随之
#                                                   退出;deploy.sh restart 拉起)。没有 pprof 端点时唯一能数 goroutine 的办法
#   bash scripts/testnet/soak.sh down               停掉本次运行的全部进程(节点按 deploy.sh stop,辅助进程按 pid
#                                                   文件并核对命令行),保留全部文件与日志
#   bash scripts/testnet/soak.sh status
#
# 拓扑在 scripts/testnet/soak/topology.env(运行标识 soak,目录 ~/anet-testnet/soak/)。主机侧逻辑在
# soak/soakhost.py(拷到 <run>/soak/ 下,经 ssh 执行):模拟模型(auto_reply 的 OpenAI 兼容端点)、能力后端
# (module/service 的 JSON 约定,Unix socket)、负载进程、采样、终态导出、canary 扫描。令牌只在主机上读。
#
# 负载(每个请求方一个负载进程,见 plan_of):
#   跨 hub 文本任务(附件 0 / 1–16 KiB / 64–256 KiB / ~1 MiB / 3–5 MiB,一半是 image/png 由模型回显其 sha256;
#   15% 两轮)、能力调用(短:soak.short、cas.put→get→stat 往返、net.echo、text.digest;长:soak.long 15–120 s)、x402 付费调用
#   (d1b auto 档、d2c agent 档;d1a 由 a2aprobe 付)、取消(投递前、等待中、长调用执行中)、本机 A2A 接口流式
#   调用(JSON-RPC 与 HTTP+JSON 交替)与 ListTasks、a2a-go 客户端的完整场景(a2aprobe run,每 30 分钟)、
#   MCP send_message(每 10 分钟)。
# 付款上限(payments,写进节点 config.json;每个 x402 调用的金额 1 或 2):
#   d1a auto 0 / agent 5 / agent 日 300 / 人工 5 / 日 300,收款方 {d2b}
#   d1b auto 2 / agent 2 / agent 日 1500 / 人工 5 / 日 1500,收款方 {d2b, off2}
#   d2c auto 0 / agent 2 / agent 日 1500 / 人工 5 / 日 1500,收款方 {d1c, off2}
#   发放:d1a、d1b 在 hub1,d2c 在 hub2,各 anet-hub -grant 2000(另有注册赠送 100)。
# 泄漏检查:每 5 分钟采样 RSS/线程/fd/SQLite(含 WAL)/信箱/outbox/未终态任务;每次按时刻表重启前先对该进程
# SIGQUIT 取 goroutine 栈(SOAK_GDUMP=0 关掉),负载停后空闲 5、10 分钟各采一次样(看 RSS 是否回落),
# check 之后对全部 Go 进程取一次 goroutine 栈。
# 安全边界同 scripts/testnet/README.md:只用 ink@10.2.2.89/90、只写 ~/anet-testnet/soak/、端口 47100–47499、
# 私有 XDG_RUNTIME_DIR、只按 pid 文件 + 路径停进程、不 sudo。
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export TESTNET_TOPOLOGY=${TESTNET_TOPOLOGY:-$HERE/soak/topology.env}
# shellcheck source=common.sh
. "$HERE/common.sh"
SOAK_STATE=${SOAK_STATE:-$TESTNET_STATE}
mkdir -p "$SOAK_STATE"
RB='~/anet-testnet/'"$TESTNET_RUN_ID"          # the run directory, as the remote shell spells it
SH="python3 $RB/soak/soakhost.py"
SSHO=("${TN_SSH_OPTS[@]}")
# Restart timetable, minutes after the load starts: "<min> <what>" (restart NODE | hubdown HUB SECONDS).
SOAK_CHAOS=${SOAK_CHAOS:-"
35  restart d2b
60  hubdown hub2 45
80  restart d1a
95  restart d2b
120 hubdown hub2 60
140 hubdown hub1 40
155 restart d2b
180 hubdown hub2 30
200 restart d1a
215 restart d1c
"}
SAMPLE_EVERY=${SAMPLE_EVERY:-300}

log(){ printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$SOAK_STATE/soak.log" >&2; }
host_of(){ tn_node_host "$1"; }
on(){ local h=$1; shift; ssh "${SSHO[@]}" "$(tn_host_ssh "$h")" "$@"; }
dep(){ bash "$HERE/deploy.sh" "$@" 2>&1 | tee -a "$SOAK_STATE/deploy.log" >&2; }
ctl(){ printf '%s' "${3:-{\}}" | on "$(host_of "$1")" "$SH ctl $1 $2"; }
jget(){ python3 -c '
import sys, json
try: v = json.load(sys.stdin)
except Exception: v = None
for k in sys.argv[1:]:
    v = v.get(k) if isinstance(v, dict) else None
print("" if v is None else json.dumps(v) if isinstance(v, (dict, list, bool)) else v)' "$@"; }
aid(){ cat "$SOAK_STATE/aid-$1" 2>/dev/null || { ctl "$1" /status | jget aid | tee "$SOAK_STATE/aid-$1"; }; }
daemons(){ tn_nodes daemon; tn_nodes official; }
a2a_port(){ printf '%s\n' "$SOAK_A2A_PORTS" | awk -v n="$1" '$1==n {print $2}'; }

# ── setup ───────────────────────────────────────────────────────
push_helpers(){
  local h
  for h in $(tn_hosts); do
    on "$h" "mkdir -p $RB/soak/logs $RB/soak/run && chmod 0700 $RB/soak"
    scp -q "${SSHO[@]}" "$HERE/soak/soakhost.py" "$TN_ANET_ROOT/scripts/mcpcall.py" \
      "$(tn_host_ssh "$h"):anet-testnet/$TESTNET_RUN_ID/soak/"
    # a2aprobe (a2a-go's own client) is not one of the node binaries deploy.sh installs
    on "$h" "mkdir -p $RB/soak/bin"
    scp -q "${SSHO[@]}" "$(tn_bin_dir "$(tn_host_arch "$h")")/a2aprobe" "$(tn_host_ssh "$h"):anet-testnet/$TESTNET_RUN_ID/soak/bin/"
  done
}

pin_a2a(){ # the local A2A interface in the 47400 block, written before the node's first start
  local n p
  for n in $(daemons); do
    p=$(a2a_port "$n"); [ -n "$p" ] || continue
    # umask 027 as remote.sh: a group-writable node directory makes module/service refuse the node's
    # Unix-socket backends (internal/backendconn walks the socket's ancestors).
    on "$(host_of "$n")" "umask 027; d=$RB/nodes/$n/home/.anet; mkdir -p \$d/modules/a2a && chmod 0700 \$d \$d/modules \$d/modules/a2a \
      && { [ -s \$d/modules/a2a/a2a_addr.txt ] || (umask 077; echo 127.0.0.1:$p > \$d/modules/a2a/a2a_addr.txt); }"
  done
}

# merge NODE JSON: merge into the (stopped) node's config.json on its host ("modules"/"inbound" one level down).
merge(){
  local b; b=$(printf '%s' "$2" | base64 -w0)
  on "$(host_of "$1")" "python3 - $1 $b $TESTNET_RUN_ID" <<'PY'
import base64, json, os, sys
n, patch, run = sys.argv[1], sys.argv[2], sys.argv[3]
d = os.path.expanduser("~/anet-testnet/%s/nodes/%s" % (run, n))
p = os.path.join(d, "home/.anet/config.json")
c = json.load(open(p))
patch = json.loads(base64.b64decode(patch).decode().replace("{NODE_DIR}", d))
for k, v in patch.items():
    if k in ("modules", "inbound") and isinstance(v, dict):
        c.setdefault(k, {}).update(v)
    else:
        c[k] = v
fd = os.open(p + ".tmp", os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
os.write(fd, json.dumps(c, indent=1).encode()); os.close(fd); os.replace(p + ".tmp", p)
print("%s: %s" % (n, sorted(c)))
PY
}
# list NODE FILE AID...: a peer or payee list (0600), replaced.
list(){
  local n=$1 f=$2; shift 2
  on "$(host_of "$n")" "umask 077; printf '%s\n' $* > $RB/nodes/$n/home/.anet/$f; chmod 600 $RB/nodes/$n/home/.anet/$f"
}

svc_caps(){ # svc_caps NODE [pricey]: the service module of a provider node, on its Unix-socket backend
  local s="unix://{NODE_DIR}/svc/backend.sock"
  printf '{"token_file":"{NODE_DIR}/svc/token","capabilities":['
  printf '{"id":"soak.short","url":"%s:/short","name":"Digest","description":"SHA-256 of text","tags":["soak"]},' "$s"
  printf '{"id":"soak.long","url":"%s:/long","timeout_ms":300000,"name":"Slow digest","description":"SHA-256 of text after args.seconds","tags":["soak"]},' "$s"
  printf '{"id":"soak.paid","url":"%s:/paid","price":1,"name":"Paid digest","description":"SHA-256 of text, 1 credit","tags":["soak"]}' "$s"
  [ "${2:-}" = pricey ] && printf ',{"id":"soak.pricey","url":"%s:/pricey","price":50,"name":"Dear digest","description":"SHA-256 of text, 50 credits","tags":["soak"]}' "$s"
  printf ']}'
}
autoreply(){ printf '{"backend":"openai","api_base":"http://127.0.0.1:%s/v1","model":"soak","error_reply":"soak: held","poll_interval_seconds":3,"max_auto_replies":30}' "$1"; }

start_helper(){ # start_helper HOST NAME ARGS…: a soakhost.py process, pid in <run>/soak/run/NAME.pid
  local h=$1 n=$2; shift 2
  stop_helper "$h" "$n"
  on "$h" "cd $RB/soak && (setsid env -i PATH=/usr/local/bin:/usr/bin:/bin HOME=\$HOME NO_PROXY='*' no_proxy='*' \
    nice -n 5 python3 $RB/soak/soakhost.py $* >> logs/$n.log 2>&1 < /dev/null & echo \$! > run/$n.pid); sleep 1; \
    p=\$(cat run/$n.pid); [ -d /proc/\$p ] && echo \"$n pid \$p\" || { tail -5 logs/$n.log; exit 1; }"
}
stop_helper(){ # by pid file, and only a python3 running this run's soakhost.py
  on "$1" "f=$RB/soak/run/$2.pid; [ -f \$f ] || exit 0; p=\$(cat \$f); \
    if [ -d /proc/\$p ] && tr '\0' ' ' < /proc/\$p/cmdline | grep -q \"\$HOME/anet-testnet/$TESTNET_RUN_ID/soak/soakhost.py\"; then \
      kill \$p; for i in 1 2 3 4 5 6 7 8 9 10; do [ -d /proc/\$p ] || break; sleep 1; done; \
      [ -d /proc/\$p ] && kill -9 \$p; fi; rm -f \$f"
}

cmd_setup(){
  tn_validate
  log "setup: run $TESTNET_RUN_ID, hosts $(tn_hosts | tr '\n' ' ')"
  dep ink89 hub1
  dep ink90 hub2
  push_helpers
  pin_a2a
  dep federate
  dep ink89 daemon
  dep ink90 daemon
  dep ink90 official
  local n; for n in $(daemons); do rm -f "$SOAK_STATE/aid-$n"; log "$n $(aid "$n")"; done
  local D1A D1B D1C D2A D2B D2C OFF2
  D1A=$(aid d1a) D1B=$(aid d1b) D1C=$(aid d1c) D2A=$(aid d2a) D2B=$(aid d2b) D2C=$(aid d2c) OFF2=$(aid off2)
  for n in d1a d1b d1c d2a d2b d2c; do dep stop "$n"; done
  # backend tokens (0600, made on the host, never leave it)
  for n in d1c d2b; do
    on "$(host_of "$n")" "d=$RB/nodes/$n/svc; mkdir -p \$d && chmod 700 \$d; [ -s \$d/token ] || (umask 077; head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > \$d/token)"
  done
  merge d1c "{\"modules\":{\"service\":$(svc_caps d1c),\"x402\":{}},\"auto_reply\":$(autoreply 47180),
    \"inbound\":{\"public_capabilities\":[{\"id\":\"soak.short\"},{\"id\":\"soak.long\"},{\"id\":\"soak.paid\"}]}}"
  merge d2b "{\"modules\":{\"cas\":{\"dir\":\"{NODE_DIR}/cas\"},\"service\":$(svc_caps d2b pricey),\"x402\":{}},\"auto_reply\":$(autoreply 47280),
    \"inbound\":{\"public_capabilities\":[{\"id\":\"cas.stat\"},{\"id\":\"soak.short\"},{\"id\":\"soak.long\"},{\"id\":\"soak.paid\"},{\"id\":\"soak.pricey\"}]}}"
  merge d2a "{\"modules\":{\"cas\":{\"dir\":\"{NODE_DIR}/cas\"}},\"auto_reply\":$(autoreply 47280)}"
  merge d1a '{"payments":{"auto_max":0,"agent_max":5,"agent_daily_max":300,"explicit_max":5,"daily_max":300,"payees_file":"payees.allow"}}'
  merge d1b '{"payments":{"auto_max":2,"agent_max":2,"agent_daily_max":1500,"explicit_max":5,"daily_max":1500,"payees_file":"payees.allow"}}'
  merge d2c '{"payments":{"auto_max":0,"agent_max":2,"agent_daily_max":1500,"explicit_max":5,"daily_max":1500,"payees_file":"payees.allow"}}'
  list d1c peers.allow "$D2C" "$D1B"
  list d2b peers.allow "$D1B" "$D1A"
  list d2a peers.allow "$D1A"
  list d1a payees.allow "$D2B"
  list d1b payees.allow "$D2B" "$OFF2"
  list d2c payees.allow "$D1C" "$OFF2"
  # credit: the hub binary's own -grant, with the hub stopped (docs: an operator's way in)
  dep stop hub1; dep stop hub2
  on ink89 "cd $RB/nodes/hub1 && for a in $D1A $D1B; do $RB/bin/anet-hub -data data -grant \$a -amount 2000 -reason 'soak grant' </dev/null; done"
  on ink90 "cd $RB/nodes/hub2 && for a in $D2C; do $RB/bin/anet-hub -data data -grant \$a -amount 2000 -reason 'soak grant' </dev/null; done"
  dep restart hub1; dep restart hub2
  helpers_up
  for n in d1a d1b d1c d2a d2b d2c; do dep restart "$n"; done
  sleep 5
  for n in $(daemons); do log "$n visibility: $(ctl "$n" /visibility '{"visibility":"federated"}' | head -c 120)"; done
  log "waiting for the discovery federation (d2b at hub1, d1c at hub2)"
  local i
  for ((i = 0; i < 60; i++)); do
    if curl -s -m 10 --noproxy '*' "$(tn_hub_url hub1)/agents" | grep -q "$D2B" \
       && curl -s -m 10 --noproxy '*' "$(tn_hub_url hub2)/agents" | grep -q "$D1C"; then break; fi
    sleep 5
  done
  [ "$i" -lt 60 ] && log "federated directories ready" || log "WARNING: directories not federated after 5 min"
  write_plans
  cmd_sample baseline
  log "setup done"
}

helpers_up(){
  start_helper ink89 llm89 mock-llm --addr 127.0.0.1:47180
  start_helper ink90 llm90 mock-llm --addr 127.0.0.1:47280
  start_helper ink89 backend-d1c backend --node d1c --socket "$RB/nodes/d1c/svc/backend.sock" --token-file "$RB/nodes/d1c/svc/token"
  start_helper ink90 backend-d2b backend --node d2b --socket "$RB/nodes/d2b/svc/backend.sock" --token-file "$RB/nodes/d2b/svc/token"
}

# ── load plans ──────────────────────────────────────────────────
plan_of(){ # plan_of NODE: the load of one requester (JSON)
  local D1C D2A D2B OFF2
  D1C=$(aid d1c) D2A=$(aid d2a) D2B=$(aid d2b) OFF2=$(aid off2)
  case $1 in
  d1a) cat <<J ;;
{"node":"d1a","drain":900,"ops":[
 {"op":"text","name":"d2a","peer":"$D2A","every":20,"multi":0.15},
 {"op":"cas","name":"d2a","peer":"$D2A","every":40},
 {"op":"cas","name":"d2b","peer":"$D2B","every":60},
 {"op":"cap","name":"d2b","peer":"$D2B","every":20,"skills":["soak.short"]},
 {"op":"cap","name":"d2b-long","peer":"$D2B","every":120,"skills":["soak.long"],"max_inflight":2},
 {"op":"cap","name":"off2","peer":"$OFF2","every":30,"skills":["net.echo","text.digest"]},
 {"op":"a2a","name":"d2b","peer":"$D2B","every":30},
 {"op":"a2a","name":"d2a","peer":"$D2A","every":60},
 {"op":"a2a-list","name":"d2b","peer":"$D2B","every":120},
 {"op":"cancel","name":"d2b","peer":"$D2B","mode":"long","every":300,"max_inflight":1},
 {"op":"a2aprobe","name":"d2b","peer":"$D2B","every":1800,"paid":"soak.paid","pricey":"soak.pricey","registry":true,"max_inflight":1}
]}
J
  d1b) cat <<J ;;
{"node":"d1b","drain":900,"ops":[
 {"op":"text","name":"d2b","peer":"$D2B","every":20,"multi":0.15},
 {"op":"paid","name":"off2","peer":"$OFF2","skill":"demo.digest.paid","every":90,"tier":"auto"},
 {"op":"paid","name":"d2b","peer":"$D2B","skill":"soak.paid","every":90,"tier":"auto"},
 {"op":"cap","name":"d1c","peer":"$D1C","every":60,"skills":["soak.short"]},
 {"op":"cancel","name":"d2b","peer":"$D2B","mode":"quick","every":240,"max_inflight":1},
 {"op":"cancel","name":"d2b","peer":"$D2B","mode":"late","every":240,"max_inflight":1}
]}
J
  d2c) cat <<J ;;
{"node":"d2c","drain":900,"ops":[
 {"op":"text","name":"d1c","peer":"$D1C","every":20,"multi":0.15},
 {"op":"cap","name":"d1c","peer":"$D1C","every":30,"skills":["soak.short"]},
 {"op":"cap","name":"d1c-long","peer":"$D1C","every":150,"skills":["soak.long"],"max_inflight":2},
 {"op":"paid","name":"d1c","peer":"$D1C","skill":"soak.paid","every":90,"tier":"agent"},
 {"op":"paid","name":"off2","peer":"$OFF2","skill":"demo.digest.paid","every":120,"tier":"agent"},
 {"op":"cancel","name":"d1c","peer":"$D1C","mode":"late","every":300,"max_inflight":1},
 {"op":"cancel","name":"d1c","peer":"$D1C","mode":"long","every":360,"max_inflight":1},
 {"op":"mcp","name":"off2","peer":"$OFF2","skill":"text.digest","every":600,"max_inflight":1}
]}
J
  esac
}
REQUESTERS="d1a d1b d2c"
write_plans(){
  local n
  for n in $REQUESTERS; do
    plan_of "$n" | python3 -c 'import json,sys; json.dump(json.load(sys.stdin), sys.stdout, indent=1)' > "$SOAK_STATE/plan-$n.json"
    scp -q "${SSHO[@]}" "$SOAK_STATE/plan-$n.json" "$(tn_host_ssh "$(host_of "$n")"):anet-testnet/$TESTNET_RUN_ID/soak/plan-$n.json"
  done
}

cmd_smoke(){ # each op of each plan once, concurrently, with the load worker's own code
  write_plans
  local n
  for n in $REQUESTERS; do
    log "smoke $n"
    on "$(host_of "$n")" "cd $RB/soak && $SH once --plan plan-$n.json $([ "${SMOKE_PROBE:-0}" = 1 ] && echo --probe)" \
      | tee -a "$SOAK_STATE/smoke.log" || true
  done
}

# ── sampling ────────────────────────────────────────────────────
cmd_sample(){ # sample [LABEL]
  local h out
  for h in $(tn_hosts); do
    out=$(on "$h" "$SH sample" 2>&1) || { log "sample $h failed: ${out:0:200}"; continue; }
    printf '%s' "$out" | python3 -c 'import json,sys; d=json.load(sys.stdin); d["label"]=sys.argv[1]; print(json.dumps(d))' "${1:-}" \
      >> "$SOAK_STATE/samples.jsonl"
  done
}

# ── the run ─────────────────────────────────────────────────────
load_up(){
  local n
  for h in $(tn_hosts); do on "$h" "rm -f $RB/soak/run/stop"; done
  for n in $REQUESTERS; do start_helper "$(host_of "$n")" "load-$n" load --plan "$RB/soak/plan-$n.json"; done
}
SOAK_GDUMP=${SOAK_GDUMP:-1}
gdump(){ # gdump NAME…: goroutine dumps (the processes exit), summaries appended to $SOAK_STATE/gdump.jsonl
  local n base
  for n in "$@"; do
    base=${n%-peer}; base=${base%-backend}
    on "$(host_of "$base")" "$SH gdump $n" | tee -a "$SOAK_STATE/gdump.jsonl" | python3 -c '
import json, sys
for l in sys.stdin:
    r = json.loads(l); print("gdump %s: %s goroutines after %s s" % (r["name"], r.get("goroutines"), r.get("uptime_s")), file=sys.stderr)'
  done
}
chaos(){ # chaos WHAT ARGS
  [ "$SOAK_GDUMP" = 1 ] && gdump "$2"   # the stack of every goroutine after its uptime, before the stop
  case $1 in
    restart) log "chaos: restart $2"; dep stop "$2"; sleep 10; dep restart "$2" ;;
    hubdown) log "chaos: $2 down for ${3}s"; dep stop "$2"; sleep "$3"; dep restart "$2" ;;
  esac
  printf '%s %s\n' "$(date -u +%s)" "$*" >> "$SOAK_STATE/chaos.log"
}
cmd_run(){
  local hours=${1:-4} t0 end next_sample line m what
  write_plans
  t0=$(date -u +%s); end=$((t0 + $(python3 -c "print(int(float('$hours')*3600))")))
  printf '%s\n' "$t0" > "$SOAK_STATE/t0"
  log "run: ${hours} h, until $(date -u -d @$end +%H:%M:%S)"
  cmd_sample start
  load_up
  next_sample=$((t0 + SAMPLE_EVERY))
  local -a sched=(); mapfile -t sched < <(printf '%s\n' "$SOAK_CHAOS" | awk 'NF')
  local i=0
  while [ "$(date -u +%s)" -lt "$end" ]; do
    if [ "$i" -lt "${#sched[@]}" ]; then
      line=${sched[$i]}; m=${line%% *}; what=${line#* }
      if [ "$(date -u +%s)" -ge $((t0 + m * 60)) ]; then
        # shellcheck disable=SC2086
        chaos $what || log "chaos $what failed"
        i=$((i + 1)); continue
      fi
    fi
    if [ "$(date -u +%s)" -ge "$next_sample" ]; then
      cmd_sample "t+$(( ($(date -u +%s) - t0) / 60 ))m" || true
      next_sample=$((next_sample + SAMPLE_EVERY))
    fi
    sleep 10
  done
  cmd_stop
  cmd_sample drained
  # no load: does RSS come back down (the Go scavenger returns freed heap within minutes)?
  sleep 300; cmd_sample idle+5
  sleep 300; cmd_sample idle+10
  GDUMP_END=$SOAK_GDUMP cmd_check
}
cmd_stop(){
  local h n i alive
  log "stopping the load (stop file); draining"
  for h in $(tn_hosts); do on "$h" "touch $RB/soak/run/stop"; done
  for ((i = 0; i < 120; i++)); do
    alive=0
    for n in $REQUESTERS; do
      on "$(host_of "$n")" "p=\$(cat $RB/soak/run/load-$n.pid 2>/dev/null); [ -n \"\$p\" ] && [ -d /proc/\$p ]" && alive=$((alive + 1))
    done
    [ "$alive" = 0 ] && break
    [ $((i % 6)) = 0 ] && log "  $alive load workers still draining"
    sleep 10
  done
  for n in $REQUESTERS; do stop_helper "$(host_of "$n")" "load-$n"; done
  log "load stopped"
}

# ── the end checks ──────────────────────────────────────────────
cmd_check(){
  local P="$SOAK_STATE/pull" h
  mkdir -p "$P"
  for h in $(tn_hosts); do
    mkdir -p "$P/$h"
    scp -q -r "${SSHO[@]}" "$(tn_host_ssh "$h"):anet-testnet/$TESTNET_RUN_ID/soak/logs" "$P/$h/" || true
  done
  on ink89 "$SH dump --nodes d1a,d1b,d1c --hubs hub1" > "$P/dump-ink89.json"
  on ink90 "$SH dump --nodes d2a,d2b,d2c,off2 --hubs hub2" > "$P/dump-ink90.json"
  curl -s -m 20 --noproxy '*' "$(tn_hub_url hub1)/x402/supply" > "$P/supply-hub1.json"
  curl -s -m 20 --noproxy '*' "$(tn_hub_url hub2)/x402/supply" > "$P/supply-hub2.json"
  for h in hub1 hub2; do curl -s -m 20 --noproxy '*' "$(tn_hub_url $h)/hub/identity" > "$P/identity-$h.json"; done
  # canary scan: every canary either side wrote, against both hubs' data and logs; the positive control
  # is the same scan over a provider's own store
  cat "$P"/*/logs/canaries-*.txt 2>/dev/null | sort -u > "$P/canaries.txt"
  log "canaries: $(wc -l < "$P/canaries.txt")"
  for h in $(tn_hosts); do scp -q "${SSHO[@]}" "$P/canaries.txt" "$(tn_host_ssh "$h"):anet-testnet/$TESTNET_RUN_ID/soak/canaries.txt"; done
  on ink89 "$SH scan --list $RB/soak/canaries.txt --paths $RB/nodes/hub1/data $RB/nodes/hub1/hub1.log $RB/nodes/d1a/d1a-peer.log $RB/nodes/d1c/d1c-peer.log" > "$P/scan-hub1.json"
  on ink90 "$SH scan --list $RB/soak/canaries.txt --paths $RB/nodes/hub2/data $RB/nodes/hub2/hub2.log $RB/nodes/d2a/d2a-peer.log" > "$P/scan-hub2.json"
  on ink90 "$SH scan --list $RB/soak/canaries.txt --paths $RB/nodes/d2b/home/.anet/interactions" > "$P/scan-control-d2b.json"
  on ink89 "$SH scan --list $RB/soak/canaries.txt --paths $RB/nodes/d1c/home/.anet/interactions" > "$P/scan-control-d1c.json"
  if [ "${GDUMP_END:-0}" = 1 ]; then
    # every Go process of the run, after the checks above that need them running; they exit
    local n
    for n in $(tn_nodes hub) $(daemons); do
      gdump "$n"
      [ "$(tn_node_peer "$n")" = - ] || gdump "$n-peer"
    done
    gdump off2-backend
    for h in $(tn_hosts); do
      scp -q -r "${SSHO[@]}" "$(tn_host_ssh "$h"):anet-testnet/$TESTNET_RUN_ID/soak/logs/gdump" "$(tn_host_ssh "$h"):anet-testnet/$TESTNET_RUN_ID/soak/logs/gdump.jsonl" "$P/$h/logs/" || true
    done
  fi
  cp "$SOAK_STATE/samples.jsonl" "$P/" 2>/dev/null || true
  cp "$SOAK_STATE/chaos.log" "$P/" 2>/dev/null || true
  python3 "$HERE/soak/soakreport.py" "$SOAK_STATE" | tee "$SOAK_STATE/report.md"
}

cmd_down(){
  local h n
  for h in $(tn_hosts); do on "$h" "touch $RB/soak/run/stop" || true; done
  for n in $REQUESTERS; do stop_helper "$(host_of "$n")" "load-$n" || true; done
  stop_helper ink89 llm89; stop_helper ink90 llm90
  stop_helper ink89 backend-d1c; stop_helper ink90 backend-d2b
  for n in $(tn_nodes daemon) $(tn_nodes official); do
    dep stop "$n" || true
    [ "$(tn_node_peer "$n")" = - ] || dep stop "$n-peer" || true
  done
  dep stop off2-backend || true
  dep stop hub1 || true; dep stop hub2 || true
  for h in $(tn_hosts); do
    on "$h" "for p in /proc/[0-9]*; do e=\$(readlink -f \$p/exe 2>/dev/null) || continue; case \$e in \$HOME/anet-testnet/*) echo \"left: \${p#/proc/} \$e\";; esac; \
      case \$e in */python3*) ;; *) continue ;; esac; \
      tr '\0' ' ' < \$p/cmdline 2>/dev/null | grep -q \"anet-testnet/$TESTNET_RUN_ID/soak/soakhost.py\" && echo \"left: \${p#/proc/} soakhost\"; done; \
      ss -tlnH | awk '{print \$4}' | grep -E ':47[1-4][0-9][0-9]\$' | sed 's/^/listening: /' || true"
  done
}

cmd_status(){
  dep status ink89 || true; dep status ink90 || true
  local h; for h in $(tn_hosts); do on "$h" "ls $RB/soak/run/; tail -n 3 $RB/soak/logs/events-*.jsonl 2>/dev/null | cut -c1-200"; done
}

case "${1:-}" in
  setup)  cmd_setup ;;
  smoke)  cmd_smoke ;;
  run)    shift; cmd_run "${1:-4}" ;;
  sample) shift; cmd_sample "${1:-manual}" ;;
  stop)   cmd_stop ;;
  check)  cmd_check ;;
  down)   cmd_down ;;
  status) cmd_status ;;
  gdump)  shift; gdump "$@" ;;
  helpers) helpers_up ;;
  plans)  write_plans ;;
  *) sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
