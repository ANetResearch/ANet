#!/usr/bin/env bash
# lib.sh — shared helpers for the role scripts (source it; don't run it).
#
# The role scripts model a tiny real network against the official Hub (or any Hub given via $HUB_URL):
#   2-provider.sh   服务方：一个后台 daemon（独立身份）登记为「能干活」的 provider，等着接单
#   3-requester.sh  委派方：一个后台 daemon（独立身份），去 find → 委派 → 多轮沟通 → 结束 → 评价
#   4-guest.sh      访客：无 daemon，直接用 Hub 的 /guest/* 接口试玩（验证访客模式 + 其临时清理）
#
# Hub 是官方托管服务；用环境变量 HUB_URL 指定要接入的 Hub（默认官方 https://hub.agentnetwork.org.cn）。
# 这里集中放「所有角色都要的」东西，让每个角色脚本保持薄薄一层、易读。

# loopback 必须绕过系统代理（很多人开着 Clash/VPN），否则 CLI↔本地 daemon 会被劫持成 502。
export NO_PROXY="127.0.0.1,localhost${NO_PROXY:+,$NO_PROXY}"
export no_proxy="127.0.0.1,localhost${no_proxy:+,$no_proxy}"

_LIB_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLI="$_LIB_ROOT"
HUB_URL="${HUB_URL:-https://hub.agentnetwork.org.cn}"

info(){ printf '\033[1;36m%s\033[0m\n' "$*"; }
ok(){   printf '\033[1;32m%s\033[0m\n' "$*"; }
warn(){ printf '\033[1;33m%s\033[0m\n' "$*"; }
die(){  printf '\033[1;31m%s\033[0m\n' "$*" >&2; exit 1; }

# resolve the anet binary: prefer the one on PATH (installed via install.sh), else the repo build.
_resolve_anet(){
  local b; b="$(command -v anet || true)"
  if [ -z "$b" ]; then
    [ -x "$CLI/anet" ] || ( cd "$_LIB_ROOT" && ./build.sh >/dev/null )
    b="$CLI/anet"
  fi
  printf '%s' "$b"
}
ANET="$(_resolve_anet)"

# a: run anet as DATA's identity (each role has its own data dir = its own AID). Requires $DATA set.
a(){ ANET_DATA_DIR="$DATA" "$ANET" "$@"; }

# json_str: encode $1 as a JSON string literal (escapes backslash + double-quote; single-line values).
json_str(){ local s=${1//\\/\\\\}; s=${s//\"/\\\"}; printf '"%s"' "$s"; }

# port_alive PORT → 0 if a daemon answers /ping on that control port.
port_alive(){ curl -s --noproxy '*' -o /dev/null "http://127.0.0.1:$1/ping" 2>/dev/null; }

# aid: print the AID of DATA's daemon (empty if it isn't reachable).
aid(){ a status 2>/dev/null | sed -n 's/.*"aid": *"\([^"]*\)".*/\1/p'; }

# prep_identity: create an isolated, EMPTY data dir for this role and pin ONLY its control port (every
# daemon defaults to 127.0.0.1:39811, so on one machine several roles would clash without this). That is
# the whole job of the test scripts — pure isolation. Identity, `anet daemon --detach`, hub registration,
# capabilities, accept — everything else is left for the AGENT to do by following {{HUB_URL}}/llms.txt.
# Requires $DATA and $PORT. Won't clobber an existing config (an agent may have added hub_url to it).
prep_identity(){
  mkdir -p "$DATA"
  [ -f "$DATA/config.json" ] || printf '{"control_addr":"127.0.0.1:%s"}\n' "$PORT" > "$DATA/config.json"
}

# print_join: hand THIS window's agent a SELF-CONTAINED one-liner. $1 = the role-specific goal clause
# appended after "…用本机的 anet 命令把我加入这个网络，". The one-liner bakes in THIS role's data dir so the
# agent binds the right identity/port on its own — it must NOT depend on the human exporting ANET_DATA_DIR
# in the agent's shell (that was fragile: without it the agent falls back to ~/.anet on the default port
# 39811, and the console URL it returns won't match the port this script pinned in config.json).
print_join(){
  local goal="$1"
  echo
  info "把下面这段原样发给这个窗口里的 Cursor / Claude Code 等 agent —— 它会自己读 llms.txt 并用【这个专属身份】加入："
  echo "────────────────────────────────────────────────────────────"
  echo "我在本机为你准备了一个专属的 anet 身份目录：${DATA}（控制端口已排好为 ${PORT}）。请在每条 anet 命令前都设置环境变量 ANET_DATA_DIR=${DATA}（用这个目录，不要用默认的 ~/.anet），这样我的身份不会和这台机器上的其它 anet 身份冲突。"
  echo
  echo "AgentNetwork 是一个 AI agent 互相委派任务、用可验证回执积累信誉的网络。请阅读 ${HUB_URL}/llms.txt，然后用本机的 anet 命令把我加入这个网络，${goal}"
  echo "────────────────────────────────────────────────────────────"
  echo
  warn "agent 会自己完成：anet daemon --detach → hub-register → anet console --url，然后把控制台网址发回给你。"
  warn "（因为已排好端口 ${PORT}，它给你的网址应是 http://127.0.0.1:${PORT}/console?hub=…）"
  warn "你自己想手动看状态/清理：先 export ANET_DATA_DIR=\"${DATA}\"，再 $0 status ｜ $0 down"
}

# daemon_down: gracefully stop this role's daemon via `anet stop` (targets DATA's own identity); if that
# can't reach it, fall back to killing whatever listens on $PORT.
daemon_down(){
  if a stop >/dev/null 2>&1; then ok "已优雅停止 daemon（anet stop，数据保留在 ${DATA}）"; return 0; fi
  local pids; pids="$(lsof -ti "tcp:$PORT" 2>/dev/null || true)"
  if [ -n "$pids" ]; then kill $pids 2>/dev/null || true; ok "已停止 :${PORT} 上的 daemon（数据保留在 ${DATA}）"; else warn "没有在 :${PORT} 上运行的 daemon"; fi
}

# print_status: a compact status card. NAME is whatever the AGENT registered (not set by the script).
print_status(){
  local id name; id="$(aid)"; name="$(a status 2>/dev/null | sed -n 's/.*"name": *"\([^"]*\)".*/\1/p')"
  printf '%-10s %-14s %-7s %s\n' "ROLE" "NAME" "PORT" "AID"
  printf '%-10s %-14s %-7s %s\n' "$ROLE" "${name:-<未注册>}" "$PORT" "${id:-<daemon未起>}"
  echo "  数据目录   $DATA"
  echo "  控制台     http://127.0.0.1:$PORT/console?hub=$HUB_URL"
}
