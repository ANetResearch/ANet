#!/usr/bin/env bash
# joint-fleet.sh — agent 群控:一台机器上的 agent 工具驱动多台机器上的 anet 节点。
#
# 这是本次发布真正要保的路径。它跨三个进程边界:
#   控制端 anet daemon → ANetHub(中继)→ 三个 worker daemon → 各自拉起本机编码 agent
#
# worker 的编码 agent 用桩替代:真的 claude/codex/cursor/opencode 需要各自的密钥和
# 网络,在 CI 里跑不了,也不是这里要验的东西。要验的是 anet 这一侧:委派到不到、
# 拉起的 argv 对不对、答复回不回得来、一个 worker 卡住会不会拖垮其它 worker、
# 撤销授权是否立即生效。桩实现了被替代方的完整契约:成功、失败、静默、卡死四种。
#
# 前置(都在 $J,默认 /tmp/joint-fleet):
#   anet        go build ./cmd/anet
#   anet-hub    go build ./cmd/anet-hub   (ANetHub)
# 两者都由本脚本自己构建。
set -uo pipefail
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost

J=${J:-/tmp/joint-fleet}
HUB_ADDR=127.0.0.1:29188
HUB_URL=http://$HUB_ADDR
CTRL_PORT=29190          # 控制端
W_PORTS=(29191 29192 29193)
W_NAMES=(alpha beta gamma)

pass=0; fail=0
ok(){   printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; pass=$((pass+1)); }
no(){   printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; fail=$((fail+1)); }
hd(){   printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }

cleanup(){
  for p in "${W_PORTS[@]}" "$CTRL_PORT"; do
    curl -s -m 2 -X POST "http://127.0.0.1:$p/stop" >/dev/null 2>&1 || true
  done
  pkill -x anet 2>/dev/null; pkill -x anet-hub 2>/dev/null
  sleep 1
}
trap cleanup EXIT

hd "0/7  建栈:一个 hub、一个控制端、三个 worker"
rm -rf "$J"; mkdir -p "$J"
ROOT=$(cd "$(dirname "$0")/.." && pwd)
CGO_ENABLED=0 go build -o "$J/anet" "$ROOT/cmd/anet" || { echo "build anet failed"; exit 1; }
# HUB_SRC lets CI point at wherever it checked ANetHub out; the sibling
# directory is the local layout.
HUB_SRC=${HUB_SRC:-$ROOT/../ANetHub}
CGO_ENABLED=0 go build -C "$HUB_SRC" -o "$J/anet-hub" ./cmd/anet-hub || { echo "build hub failed ($HUB_SRC)"; exit 1; }
cd "$J"

# 四种桩,实现被替代方的完整契约,不只是快乐路径。
cat > "$J/agent-ok.sh" <<'STUB'
#!/bin/sh
# 成功:把收到的提示回声出去,好让断言能确认提示真的到了 agent 手里。
printf 'FLEET-OK %s\n' "$*"
STUB
cat > "$J/agent-fail.sh" <<'STUB'
#!/bin/sh
echo "the model refused" >&2
exit 4
STUB
cat > "$J/agent-silent.sh" <<'STUB'
#!/bin/sh
exit 0
STUB
cat > "$J/agent-hang.sh" <<'STUB'
#!/bin/sh
sleep 300
STUB
chmod +x "$J"/agent-*.sh

setsid ./anet-hub --addr "$HUB_ADDR" --data "$J/hub" >"$J/hub.log" 2>&1 </dev/null &
sleep 3
curl -sf -m 5 "$HUB_URL/agents" >/dev/null && ok "hub 起来了" || no "hub 没起来: $(tail -2 "$J/hub.log")"

# 每个节点一个 HOME,控制端口写死,免得自动分配之后脚本找不到它们。
mkhome(){  # mkhome <dir> <port>
  mkdir -p "$1/.anet"
  python3 -c "
import json,os,sys
p=sys.argv[1]+'/.anet/config.json'
c=json.load(open(p)) if os.path.exists(p) else {'accept_delegations':True}
c['control_addr']='127.0.0.1:'+sys.argv[2]
json.dump(c,open(p,'w'),indent=1)" "$1" "$2"
}
CTRL_HOME=$J/ctrl
mkhome "$CTRL_HOME" "$CTRL_PORT"
setsid env HOME="$CTRL_HOME" ./anet daemon >"$J/ctrl.log" 2>&1 </dev/null &
for i in 0 1 2; do
  mkhome "$J/w-${W_NAMES[$i]}" "${W_PORTS[$i]}"
  setsid env HOME="$J/w-${W_NAMES[$i]}" ./anet daemon >"$J/w-${W_NAMES[$i]}.log" 2>&1 </dev/null &
done
sleep 4

tok(){ cat "$1/.anet/control_token.txt"; }
api(){ # api <home> <port> <path> <json>
  curl -s -m 60 -H "Authorization: Bearer $(tok "$1")" -H 'Content-Type: application/json' \
       -d "$4" "http://127.0.0.1:$2$3"
}
up=0
for i in 0 1 2; do
  curl -sf -m 5 "http://127.0.0.1:${W_PORTS[$i]}/ping" >/dev/null && up=$((up+1))
done
[ "$up" = 3 ] && ok "三个 worker 都起来了" || no "只有 $up/3 个 worker 起来了"
curl -sf -m 5 "http://127.0.0.1:$CTRL_PORT/ping" >/dev/null && ok "控制端起来了" || no "控制端没起来: $(tail -2 "$J/ctrl.log")"

hd "1/7  三个 worker 各自接入 hub,并把本机编码 agent 挂上"
declare -a W_AID
for i in 0 1 2; do
  n=${W_NAMES[$i]}; h=$J/w-$n; p=${W_PORTS[$i]}
  api "$h" "$p" /hub-register "{\"hub\":\"$HUB_URL\",\"name\":\"worker-$n\",\"caps\":[\"code.write\"]}" >/dev/null
  # exec 后端 + 桩:agent 用 claude 这一档,binary 用 command 覆盖掉。
  api "$h" "$p" /autoreply "{\"backend\":\"exec\",\"agent\":\"claude\",\"command\":\"$J/agent-ok.sh\",\"poll_interval_seconds\":1}" >/dev/null
  W_AID[$i]=$(api "$h" "$p" /status '{}' | python3 -c 'import sys,json;print(json.load(sys.stdin).get("aid",""))')
done
api "$CTRL_HOME" "$CTRL_PORT" /hub-register "{\"hub\":\"$HUB_URL\",\"name\":\"fleet-control\"}" >/dev/null
sleep 2
# 控制端只派活、不提供能力,按设计不进公开目录(没有 caps 也没有 profile 的
# 纯请求方是不上架的),所以这里数的是 worker。
REG=$(curl -s -m 5 "$HUB_URL/agents" | python3 -c 'import sys,json;print(len(json.load(sys.stdin).get("agents") or []))')
[ "$REG" = 3 ] && ok "三个 worker 都在 hub 目录里(控制端无能力,按设计不上架)" || no "hub 目录里有 $REG 个,应为 3"
CTRL_REG=$(api "$CTRL_HOME" "$CTRL_PORT" /status '{}' | python3 -c 'import sys,json;print(json.load(sys.stdin).get("hub_url") or "")')
[ -n "$CTRL_REG" ] && ok "控制端自己已接入 hub($CTRL_REG)" || no "控制端没接上 hub"
FOUND=$(api "$CTRL_HOME" "$CTRL_PORT" /find '{"capability":"code.write"}' \
        | python3 -c 'import sys,json;print(len(json.load(sys.stdin).get("agents") or []))')
[ "$FOUND" = 3 ] && ok "控制端按能力找到了全部 3 个 worker" || no "按能力只找到 $FOUND 个 worker"

# wait_reply <interaction_id> <秒数> — 等对方(worker)发回的第一条非空消息。
#
# 判据是 from=="them"。第一版按 sender_aid 过滤,而 /threads 的消息里根本没有
# 这个字段,于是它永远为 None、永远不等于控制端的 AID,函数第一次轮询就把控制端
# 自己刚发出的委派正文当成了答复返回。后果不是少测了什么,是测出了假绿:
# "一台卡住不拖垮另一台" 这条在 worker 从未回话的情况下照样通过,而且函数提前
# 返回让后面的用例在上一条还没被处理时就改了配置,把两条 error_reply 也测串了。
wait_reply(){
  local ix=$1 secs=$2 i r
  for i in $(seq 1 "$secs"); do
    r=$(api "$CTRL_HOME" "$CTRL_PORT" /threads '{}' | python3 -c "
import sys,json
for t in json.load(sys.stdin).get('threads') or []:
    if t.get('interaction_id')=='$ix':
        for m in t.get('messages') or []:
            if m.get('from')=='them' and (m.get('body') or '').strip():
                print(m['body']); break
        break
" 2>/dev/null)
    [ -n "$r" ] && { printf '%s' "$r"; return 0; }
    sleep 1
  done
  return 1
}

hd "2/7  一条委派:控制端派活,worker 拉起本机 agent 作答"
IX=$(api "$CTRL_HOME" "$CTRL_PORT" /delegate \
      "{\"provider\":\"${W_AID[0]}\",\"goal\":\"SENTINEL-TASK-ONE\"}" \
      | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
[ -n "$IX" ] && ok "委派已投出($IX)" || no "委派没投出去"
REPLY=$(wait_reply "$IX" 40)
if [ -n "$REPLY" ]; then
  ok "worker 的答复回来了"
  case "$REPLY" in
    *FLEET-OK*) ok "答复确实来自被拉起的编码 agent(不是 anet 自己编的)" ;;
    *)          no "答复不是 agent 产出的: $(printf '%s' "$REPLY" | head -c 120)" ;;
  esac
  case "$REPLY" in
    *SENTINEL-TASK-ONE*) ok "委派的正文原样传给了 agent" ;;
    *)                   no "agent 没收到委派正文: $(printf '%s' "$REPLY" | head -c 120)" ;;
  esac
else
  no "40 秒内没等到答复"; no "(答复内容无从检查)"; no "(提示传递无从检查)"
fi

hd "3/7  同时派给三台:群控的最小形态"
declare -a IXS
for i in 0 1 2; do
  IXS[$i]=$(api "$CTRL_HOME" "$CTRL_PORT" /delegate \
            "{\"provider\":\"${W_AID[$i]}\",\"goal\":\"FANOUT-${W_NAMES[$i]}\"}" \
            | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
done
got=0; wrong=0
for i in 0 1 2; do
  r=$(wait_reply "${IXS[$i]}" 40)
  [ -n "$r" ] && got=$((got+1))
  case "$r" in *FANOUT-${W_NAMES[$i]}*) ;; *) wrong=$((wrong+1)) ;; esac
done
[ "$got" = 3 ] && ok "三台并发派活全部回话" || no "只有 $got/3 台回了话"
[ "$wrong" = 0 ] && ok "每台干的是派给它自己的那件事(没有串台)" || no "$wrong 台答复对不上自己的任务"

hd "4/7  一台卡住,不能拖垮另外两台"
api "$J/w-${W_NAMES[1]}" "${W_PORTS[1]}" /autoreply \
    "{\"backend\":\"exec\",\"agent\":\"claude\",\"command\":\"$J/agent-hang.sh\",\"api_timeout_seconds\":5,\"poll_interval_seconds\":1}" >/dev/null
HANG_IX=$(api "$CTRL_HOME" "$CTRL_PORT" /delegate \
          "{\"provider\":\"${W_AID[1]}\",\"goal\":\"THIS-ONE-HANGS\"}" \
          | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
sleep 2
LIVE_IX=$(api "$CTRL_HOME" "$CTRL_PORT" /delegate \
          "{\"provider\":\"${W_AID[2]}\",\"goal\":\"STILL-ALIVE\"}" \
          | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
LIVE=$(wait_reply "$LIVE_IX" 40)
case "$LIVE" in
  *STILL-ALIVE*) ok "另一台在卡住的那台还挂着时照常回话" ;;
  *)             no "一台卡住把整队拖住了" ;;
esac

hd "5/7  agent 失败与静默:两种都不能冒充成功"
api "$J/w-${W_NAMES[0]}" "${W_PORTS[0]}" /autoreply \
    "{\"backend\":\"exec\",\"agent\":\"claude\",\"command\":\"$J/agent-fail.sh\",\"error_reply\":\"AGENT-FAILED-HERE\",\"poll_interval_seconds\":1}" >/dev/null
FIX=$(api "$CTRL_HOME" "$CTRL_PORT" /delegate \
      "{\"provider\":\"${W_AID[0]}\",\"goal\":\"WILL-FAIL\"}" \
      | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
FR=$(wait_reply "$FIX" 40)
case "$FR" in
  *AGENT-FAILED-HERE*) ok "agent 失败时回的是配置好的失败说明,不是空答复" ;;
  *FLEET-OK*)          no "失败被当成了成功" ;;
  "")                  no "agent 失败时什么都没回(派活方无从区分卡住与失败)" ;;
  *)                   ok "agent 失败时回了一条可辨认的说明"; ;;
esac

api "$J/w-${W_NAMES[0]}" "${W_PORTS[0]}" /autoreply \
    "{\"backend\":\"exec\",\"agent\":\"claude\",\"command\":\"$J/agent-silent.sh\",\"error_reply\":\"AGENT-SAID-NOTHING\",\"poll_interval_seconds\":1}" >/dev/null
SIX=$(api "$CTRL_HOME" "$CTRL_PORT" /delegate \
      "{\"provider\":\"${W_AID[0]}\",\"goal\":\"WILL-BE-SILENT\"}" \
      | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
SR=$(wait_reply "$SIX" 40)
case "$SR" in
  *AGENT-SAID-NOTHING*) ok "agent 一声不吭也走失败路径,而不是回一条空答复" ;;
  "")                   no "agent 静默时派活方什么也收不到" ;;
  *)                    ok "agent 静默时回了一条可辨认的说明" ;;
esac

hd "6/7  证据面:干完的活留在两边的链上"
# 一次自动答复本身不上链 —— 它是一轮对话,不是一次完成。上链的是收据,而收据
# 在交互真正结束时才签。所以这里先把交互走完,再看链。若不走完就断言链上有东西,
# 测的就不是证据面,而是"消息发出去了没有"。
ALPHA_IX=$(api "$CTRL_HOME" "$CTRL_PORT" /delegate \
           "{\"provider\":\"${W_AID[0]}\",\"goal\":\"CLOSE-ME\"}" \
           | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))')
api "$J/w-${W_NAMES[0]}" "${W_PORTS[0]}" /autoreply \
    "{\"backend\":\"exec\",\"agent\":\"claude\",\"command\":\"$J/agent-ok.sh\",\"poll_interval_seconds\":1}" >/dev/null
wait_reply "$ALPHA_IX" 40 >/dev/null
api "$CTRL_HOME" "$CTRL_PORT" /end "{\"interaction_id\":\"$ALPHA_IX\"}" >/dev/null
# worker 那边的自动答复循环会接受结束并回签收据;给它几轮 poll 的时间。
CHAIN=0
for _ in $(seq 1 40); do
  CHAIN=$(api "$J/w-${W_NAMES[0]}" "${W_PORTS[0]}" /evidence '{"limit":200}' \
          | python3 -c 'import sys,json;print(json.load(sys.stdin)["head"]["length"])' 2>/dev/null || echo 0)
  [ "${CHAIN:-0}" -gt 0 ] && break
  sleep 1
done
[ "${CHAIN:-0}" -gt 0 ] && ok "交互结束后 worker 链上有 $CHAIN 条记录" || no "交互结束后 worker 链仍是空的"
CEV=$(api "$CTRL_HOME" "$CTRL_PORT" /evidence '{"limit":200}' \
      | python3 -c 'import sys,json;print(json.load(sys.stdin)["head"]["length"])' 2>/dev/null || echo 0)
[ "${CEV:-0}" -gt 0 ] && ok "控制端链上也有 $CEV 条记录(两边各自可自证)" || no "控制端链是空的"

# 撤销:把 worker 从 hub 摘掉,控制端就不该再找得到它
api "$J/w-${W_NAMES[2]}" "${W_PORTS[2]}" /hub-leave '{}' >/dev/null
sleep 2
LEFT=$(api "$CTRL_HOME" "$CTRL_PORT" /find '{"capability":"code.write"}' \
       | python3 -c 'import sys,json;print(len(json.load(sys.stdin).get("agents") or []))')
[ "$LEFT" = 2 ] && ok "worker 退出后即刻从目录消失(3 → 2)" || no "退出后目录仍有 $LEFT 个,应为 2"

hd "7/7  MCP:编码工具把这张网络当自己的工具用"
# mcpserv 的单元测试对着 fake Control 验工具表的形状。这里验的是另一件事:
# `anet mcp` 在 stdio 上说的是不是合法 MCP —— 多一行 stdout 就是 framing 错误,
# 而那种错误只有真客户端连上来才会显形。探针按 Claude Code / Cursor 的顺序走:
# initialize → notifications/initialized → tools/list → tools/call。
MCPOUT=$(python3 "$ROOT/scripts/mcp-probe.py" "$J/anet" "$CTRL_HOME" "$HUB_URL" 2>"$J/mcp.err")
if [ -z "$MCPOUT" ]; then
  no "MCP 探针没能完成握手: $(head -c 200 "$J/mcp.err")"
  no "(工具表无从检查)"; no "(工具调用无从检查)"; no "(错误传递无从检查)"
else
  ok "MCP 握手完成,服务名 $(printf '%s' "$MCPOUT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["server_name"])')"
  TOOLS=$(printf '%s' "$MCPOUT" | python3 -c 'import sys,json;print(",".join(json.load(sys.stdin)["tools"]))')
  MISSING=""
  for t in agents_find task_delegate task_results task_inbox task_message task_end evidence_read credit_balance node_status; do
    case ",$TOOLS," in *,$t,*) ;; *) MISSING="$MISSING $t" ;; esac
  done
  [ -z "$MISSING" ] && ok "九个工具全部报给了客户端" || no "工具表缺:$MISSING"
  NF=$(printf '%s' "$MCPOUT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["found"])')
  [ "$NF" -ge 1 ] && ok "经 MCP 调用 agents_find 找到了 $NF 个 worker(穿到了 hub)" \
                  || no "经 MCP 调用 agents_find 什么也没找到"
  SH=$(printf '%s' "$MCPOUT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["status_hub"])')
  [ -n "$SH" ] && ok "node_status 经 MCP 报出了本节点接入的 hub" || no "node_status 经 MCP 没报出 hub"
  BAD=$(printf '%s' "$MCPOUT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["bad_is_error"])')
  [ "$BAD" = "True" ] && ok "坏参数经 MCP 回的是错误,不是一次成功的空回答" \
                      || no "坏参数经 MCP 被当成成功了"
fi

printf '\n\033[1m── %d 通过, %d 失败 ──\033[0m\n' "$pass" "$fail"
[ "$fail" = 0 ]
