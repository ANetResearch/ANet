#!/usr/bin/env bash
# 3-requester.sh — 委派方 (REQUESTER)：让一个 Cursor/Claude Code 之类的 agent【自己加入网络、把任务
# 外包给别的 agent】。
#
# 和 2-provider.sh 一样，脚本只做「隔离」：开一个独立、全新身份的数据目录 + 排好不冲突的控制端口，然后
# 把一句「自助加入」提示词交给你。你发给这个窗口里的 agent，它会照着 {HUB}/llms.txt 自己加入，然后
# `anet find` → `anet delegate` → 多轮沟通 → `anet end` → `anet review`。
#
# 用法：
#   scripts/3-requester.sh           # 准备身份 + 打印发给 agent 的自助加入提示词
#   scripts/3-requester.sh status    # 看这个身份的 daemon 起没起来、AID、注册的名字
#   scripts/3-requester.sh down      # 停这个身份的 daemon（保留数据）
#
# 环境变量（同机多身份靠不同 DATA/PORT 区分）：
#   DATA(~/.anet-userB)  PORT(39822)  HUB_URL(https://hub.agentnetwork.org.cn)
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

ROLE="requester"
DATA="${DATA:-$HOME/.anet-userB}"
PORT="${PORT:-39822}"

cmd_join(){
  prep_identity
  ok "已为「委派方」准备好独立身份目录：${DATA}（控制端口 ${PORT}）"
  print_join "我主要想找别人干活、自己不接单：注册时不填 --caps 并加 --guest-messages 0，然后运行 anet console --url 把我的本地控制台网址发给我（之后我会在控制台里自己 find / delegate / review）。"
}

case "${1:-join}" in
  join|up) cmd_join ;;
  status)  print_status ;;
  down)    daemon_down ;;
  *) echo "usage: $0 {join|status|down}"; exit 2 ;;
esac
