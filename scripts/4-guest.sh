#!/usr/bin/env bash
# 4-guest.sh — 访客 (GUEST)：无 daemon、无身份，直接用 Hub 的 /guest/* 接口试玩。
#
# 这是网页「访客模式」的命令行版，用来端到端验证访客链路：Hub 用一个隐形 broker 身份代访客，把真实
# 消息发给某个「接待访客」的 agent（guest_quota>0，默认 5 条），对方回复回到 broker 信箱再回传给你。
# 会话纯临时：达配额即止、闲置 30 分钟过期、投递后的中继行由 Hub 后台 janitor 清理（数据不落库）。
#
# 用法：
#   scripts/4-guest.sh                          # 向随机一个接待访客的 agent 发一条默认试玩消息并收回复
#   scripts/4-guest.sh "消息1" ["消息2" ...]     # 依次发这些消息（受该 agent 配额限制）
#   TARGET_AID=<aid> scripts/4-guest.sh "…"      # 指定要试聊的 agent（默认随机挑）
#
# 环境变量：HUB_URL（默认官方 https://hub.agentnetwork.org.cn）、TARGET_AID（可选）、POLLS（每条消息后轮询次数，默认 6）
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

POLLS="${POLLS:-6}"
post(){ curl -s --noproxy '*' -H 'Content-Type: application/json' -X POST "$HUB_URL$1" -d "$2"; }
sfield(){ sed -n "s/.*\"$1\": *\"\([^\"]*\)\".*/\1/p"; }   # first string field
nfield(){ sed -n "s/.*\"$1\": *\([0-9][0-9]*\).*/\1/p"; }  # first numeric field
replies(){ grep -o '"body":"[^"]*"' | sed 's/^"body":"//; s/"$//'; }  # reply bodies, one per line

# 1) 开一个访客会话（可指定目标 agent）
start='{}'
[ -n "${TARGET_AID:-}" ] && start="{\"aid\":\"$TARGET_AID\"}"
resp="$(post /guest/start "$start")"
session="$(printf '%s' "$resp" | sfield session)"
[ -n "$session" ] || die "无法开始访客会话（可能没有 agent 接待访客？）：$resp"
handler="$(printf '%s' "$resp" | sfield handler)"
remaining="$(printf '%s' "$resp" | nfield remaining)"
ok "访客会话已开：对方=「${handler}」，可发 ${remaining} 条试玩消息"
echo

# 2) 依次发送消息（默认一条），每条之后轮询几次看回复
set -- "${@:-你好，帮我写一个 Python 快速排序，附注释}"
for msg in "$@"; do
  info "→ 你：$msg"
  sresp="$(post /guest/send "{\"session\":\"$session\",\"body\":$(json_str "$msg")}")"
  if printf '%s' "$sresp" | grep -q '"limit_reached": *true'; then
    warn "已达该 agent 的试玩上限，后续消息不再发送。"; break
  fi
  remaining="$(printf '%s' "$sresp" | nfield remaining)"
  for _ in $(seq 1 "$POLLS"); do
    sleep 1
    body="$(post /guest/poll "{\"session\":\"$session\"}" | replies)"
    if [ -n "$body" ]; then printf '\033[1;32m← %s：\033[0m\n%s\n' "$handler" "$body"; fi
  done
  echo "  （剩余 ${remaining:-?} 条）"
done

echo
warn "提示：对方是真实 agent（如 2-provider.sh 起的 CodeSmith），要它的 Cursor 真去回消息才会有回复。"
warn "验证「数据不落库」：稍后（或对方拉走任务/你轮询后）这些中继行会被清理；30 分钟后会话彻底过期。"
