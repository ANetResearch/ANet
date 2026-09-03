#!/usr/bin/env bash
# container-shell-test.sh — 白板 Debian 容器里的 shell 模块,经真 hub 被远程调用。
#
# 与 joint-shell.sh 的区别是"起点"。那个脚本在本机起 hub 和两个 daemon,验的是
# 接缝;这个脚本从一个什么都没装的 debian:12 容器开始,用线上的 install.sh 装,
# 经 hub.agentnetwork.org.cn 从另一台机器调用它,验的是**真实新用户会走的那条路**。
#
# 只有这一层能查出的东西:白板系统缺什么(debian:12 既无 curl 也无 ca-certificates,
# 没有后者 HTTPS 直接失败;uptime 属于 procps 也不在),以及"改了模块配置、重启了,
# 但没重新注册,于是能力不在目录里"——它不报错,只是让节点查不到。
#
# 前置(容器侧,见 §0 的注释):
#   docker run -d --name anet-debian-test --dns 114.114.114.114 debian:12 sleep infinity
#   apt-get update && apt-get install -y curl ca-certificates
#   curl -fsSL https://agentnetwork.org.cn/install.sh | sh -s -- --shell --system \
#     --hub https://hub.agentnetwork.org.cn --name dmax-debian-box
#   然后写 /root/.anet/config.json 的 modules.shell 与 /etc/anet/shell-allow
#
# 前置(调用方侧):$SP/caller.env 里写 CALLER=<本机 AID> 与 BOX=<容器 AID>
set -uo pipefail
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
SP=/tmp/claude-1001/-data-projs-anet-oss/ccf0656d-5802-4caa-a7e0-1dc3504bf1ba/scratchpad
source $SP/caller.env
export ANET_DATA_DIR=$SP/caller
A=/data/projs/anet-oss/ANet/anet
DM="ssh -o ConnectTimeout=20 root@dmax.chatchat.space"

pass=0; fail=0
ok(){ printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; pass=$((pass+1)); }
no(){ printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; fail=$((fail+1)); }
hd(){ printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }

box(){ $DM "docker exec anet-debian-test bash -lc '$1'" 2>/dev/null; }

cap(){  # cap <capability> [args-json]
  local ix
  ix=$($A delegate "$BOX" --capability "$1" ${2:+--args "$2"} 2>/dev/null \
        | python3 -c 'import sys,json;print(json.load(sys.stdin).get("interaction_id",""))' 2>/dev/null)
  [ -n "$ix" ] || { echo '{"error":"delegate refused"}'; return 1; }
  for _ in $(seq 1 60); do
    local r
    r=$($A results 2>/dev/null | python3 -c "
import sys,json
try: d=json.load(sys.stdin)
except Exception: sys.exit()
for x in d.get('results') or []:
    if x['interaction_id']=='$ix': print(x['result']); break
")
    [ -n "$r" ] && { echo "$r"; return 0; }
    sleep 1
  done
  echo '{"error":"timed out"}'; return 1
}
field(){ python3 -c "import sys,json;print(json.load(sys.stdin).get('$1',''))" 2>/dev/null; }
state(){ python3 -c "import sys,json;print((json.load(sys.stdin).get('evidence') or {}).get('observed_state',''))" 2>/dev/null; }

# 从空名单开始,并确保能力已经通告到目录 —— 能力清单是 hub-register 那一刻
# 折进去的,改了配置光重启不会更新它。
box ': > /etc/anet/shell-allow'
box 'anet hub-register https://hub.agentnetwork.org.cn --name dmax-debian-box >/dev/null 2>&1'
sleep 2

hd "1/8  名单为空时,远程调用被拒"
E=$(cap 'shell.run@uptime')
[ "$(echo "$E"|field status)" = UNAVAILABLE ] && ok "拒绝: $(echo "$E"|field message)" || no "空名单没有拒绝: $E"

hd "2/8  把调用方 AID 写进名单(不重启)"
box "echo '$CALLER' >> /etc/anet/shell-allow"
# 白板 Debian 12 里没有 uptime(procps 未装),所以断言用容器里确实有的命令。
# uptime 仍留在配置里 —— 它证明"命令不存在"会诚实地回 FAILED/exit 127。
E=$(cap 'shell.run@disk')
[ "$(echo "$E"|field status)" = OK ] && ok "加一行即生效,未重启" || no "加进名单后仍被拒: $E"
echo "$E"|state|grep -q 'Filesystem' && ok "拿到真实回显: $(echo "$E"|state|sed -n 2p|cut -c1-60)" || no "无回显"
E=$(cap 'shell.run@uptime')
[ "$(echo "$E"|field status)" = FAILED ] && ok "白板 Debian 没有 uptime,诚实回 FAILED exit $(echo "$E"|python3 -c "import sys,json;print(int((json.load(sys.stdin).get('metrics') or {}).get('exit_code',-1)))")" \
  || no "命令不存在时未如实报告"

hd "3/8  以谁的身份执行"
E=$(cap 'shell.run@whoami')
W=$(echo "$E"|state|tr -d '\n')
[ "$W" = root ] && ok "以 root 执行 —— 容器里 daemon 就是 root 起的,模块不提权也不降权" \
  || no "预期 root,得到 '$W'"
E=$(cap 'shell.run@osinfo')
echo "$E"|state|grep -q 'Debian GNU/Linux 12' && ok "确实是白板 Debian 12 容器" || no "osinfo: $(echo "$E"|state|head -1)"

hd "4/8  网络来的参数不能变成命令"
E=$(cap 'shell.run@echo' '{"argv":["hi; touch /tmp/PWNED","&& id","$(id)","`id`"]}')
O=$(echo "$E"|state)
case "$O" in
  *uid=*) no "参数被解释执行了: $O" ;;
  *'hi; touch'*) ok "元字符原样返回: $(echo "$O"|cut -c1-70)" ;;
  *) no "非预期输出: $O" ;;
esac
box 'test -e /tmp/PWNED && echo yes || echo no' | grep -q '^no' && ok "/tmp/PWNED 未被创建" || no "注入真的建了文件"

hd "5/8  失败的命令报 FAILED,不报 OK"
E=$(cap 'shell.run@fail')
[ "$(echo "$E"|field status)" = FAILED ] && ok "status=FAILED" || no "预期 FAILED,得 $(echo "$E"|field status)"
echo "$E"|python3 -c "import sys,json;m=json.load(sys.stdin).get('metrics') or {};print(int(m.get('exit_code',-1)))" | grep -q '^7$' && ok "退出码 7 原样带回" || no "退出码丢失"
echo "$E"|state|grep -q '出错了' && ok "stderr 完整穿过公网回来" || no "stderr 丢了"

hd "6/8  超时杀掉整个进程组"
box 'rm -f /tmp/SURVIVOR'
S=$(date +%s)
E=$(cap 'shell.run@slow')
D=$(( $(date +%s) - S ))
[ "$(echo "$E"|field status)" = FAILED ] && ok "被杀报 FAILED(耗时 ${D}s)" || no "预期 FAILED,得 $(echo "$E"|field status)"
echo "$E"|field message|grep -qi killed && ok "message 说明是被杀: $(echo "$E"|field message)" || no "message 未说明被杀"
sleep 5
box 'test -e /tmp/SURVIVOR && echo yes || echo no' | grep -q '^no' \
  && ok "后台子进程未存活 —— 杀的是整个进程组,不只是 shell" || no "有进程逃过超时"

hd "7/8  未开启的任意命令执行"
$A find --cap 'shell.exec' 2>/dev/null | grep -q "$BOX" && no "shell.exec 被通告到了目录" || ok "shell.exec 不在目录里"
$A find --cap 'shell.run@disk' 2>/dev/null | grep -q "$BOX" && ok "已定义的命令在目录里可被发现" || no "目录里查不到该节点的能力"
E=$(cap 'shell.exec' '{"command":"id"}')
echo "$E"|state|grep -q 'uid=' && no "任意命令在未开开关时执行了" || ok "什么都没执行"

hd "8/8  收权与证据"
box "sed -i '/$CALLER/d' /etc/anet/shell-allow"
E=$(cap 'shell.run@uptime')
[ "$(echo "$E"|field status)" = UNAVAILABLE ] && ok "删掉一行,下一次调用即被拒(未重启)" || no "收权未生效: $E"
# 链是 base64 CoreDet-CBOR,不是明文 —— grep 事件名什么也找不到,得用 anet evidence 解码。
$DM "docker exec anet-debian-test anet evidence" 2>/dev/null > /tmp/box-ev.json
python3 - "$CALLER" <<'PY' > /tmp/box-ev.txt
import json, sys, collections
recs = json.load(open('/tmp/box-ev.json'))['records']
caller = sys.argv[1]
c = collections.Counter(r['event_type'] for r in recs)
runs, refused = c.get('anet.shell.command', 0), c.get('anet.shell.refused', 0)
named = sum(1 for r in recs if r['event_type'].startswith('anet.shell.')
            and (r['payload'] or {}).get('caller') == caller)
print(runs, refused, named)
PY
read RUNS REFUSED NAMED < /tmp/box-ev.txt
[ "${RUNS:-0}" -ge 5 ] && ok "证据链记下 $RUNS 次执行" || no "执行记录只有 ${RUNS:-0} 条"
[ "${REFUSED:-0}" -ge 2 ] && ok "证据链记下 $REFUSED 次拒绝" || no "拒绝记录只有 ${REFUSED:-0} 条"
[ "${NAMED:-0}" -ge 7 ] && ok "$NAMED 条记录都带验签后的调用方 AID" || no "带调用方 AID 的记录只有 ${NAMED:-0} 条"

printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
