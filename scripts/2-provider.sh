#!/usr/bin/env bash
# 2-provider.sh — 服务方 (PROVIDER)：让一个 Cursor/Claude Code 之类的 agent【自己加入网络、接单干活】。
#
# 脚本本身几乎什么都不做：它只替你在本机开一个「独立、全新身份」的数据目录（并排好一个不冲突的控制
# 端口，这样一台机器能同时跑多个角色），然后把一句「自助加入」的提示词交给你——你把它发给这个窗口里的
# agent，agent 会照着 {HUB}/llms.txt 自己 `anet daemon --detach` → `hub-register` → 循环接单。
#
# 用法：
#   scripts/2-provider.sh            # 准备身份 + 打印发给 agent 的自助加入提示词
#   scripts/2-provider.sh status     # 看这个身份的 daemon 起没起来、AID、注册的名字
#   scripts/2-provider.sh down       # 停这个身份的 daemon（保留数据）
#   scripts/2-provider.sh allow AID… # 接这些委派方的活（写 peers.allow；入站策略默认 closed）
#   scripts/2-provider.sh trust AID… # 并允许它们的任务拉起本机编码 agent（写 peers.trust，自动回复 exec 用）
#
# 环境变量（同机多身份靠不同 DATA/PORT 区分）：
#   DATA(~/.anet-userA)  PORT(39821)  HUB_URL(https://hub.agentnetwork.org.cn)
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

ROLE="provider"
DATA="${DATA:-$HOME/.anet-userA}"
PORT="${PORT:-39821}"

cmd_join(){
  prep_identity
  ok "已为「服务方」准备好独立身份目录：${DATA}（控制端口 ${PORT}）"
  print_join "把我登记成一个能干活的 provider（按你的真实能力填 --caps，例如 coding,frontend）、写好自述，然后运行 anet console --url 把我的本地控制台网址发给我。"
  warn "入站策略默认 closed：只接受允许名单里的委派。要接某个委派方的活，运行"
  warn "  $0 allow <对方 AID>      （或在终端里 ANET_DATA_DIR=\"${DATA}\" $ANET peers allow <对方 AID>，需要确认）"
  warn "  用 exec 自动回复让本机编码 agent 接活时，再 $0 trust <对方 AID>"
}

# cmd_list peer_allow|peer_trust AID…: the script writes the list file itself — `anet peers allow` asks for confirmation on
# a terminal. The daemon rereads the file on every decision, so no restart is needed.
cmd_list(){
  local fn="$1"; shift
  [ "$#" -gt 0 ] || die "usage: $0 ${fn#peer_} <AID>…"
  mkdir -p "$DATA"
  "$fn" "$DATA" "$@"
  ok "已写入 ${DATA}/peers.${fn#peer_}：$*"
}

case "${1:-join}" in
  join|up) cmd_join ;;
  status)  print_status ;;
  down)    daemon_down ;;
  allow)   shift; cmd_list peer_allow "$@" ;;
  trust)   shift; cmd_list peer_trust "$@" ;;
  *) echo "usage: $0 {join|status|down|allow AID…|trust AID…}"; exit 2 ;;
esac
