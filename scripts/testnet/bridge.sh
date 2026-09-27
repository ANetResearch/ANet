#!/usr/bin/env bash
# bridge.sh — 在编排机(ink88)上架起测试网需要的本地转发。只在本机起 ssh 进程,远端什么也不改。
#
#   bash scripts/testnet/bridge.sh ctl-up     把每个 daemon/official 的远端控制口(127.0.0.1:<port>)原号转到
#                                             本机 127.0.0.1:<port>。端口在整个测试网唯一,所以按本机多进程写的
#                                             脚本里 "127.0.0.1:$PORT" 的写法对远端节点照样成立
#   bash scripts/testnet/bridge.sh fed-up     跨岛联邦转发:对 TESTNET_FEDERATION_BRIDGED 的每一对,
#                                             在 ink88 本岛地址上开 <对端hub端口>,经 ssh 转到对岸 hub
#   bash scripts/testnet/bridge.sh mirror     把每个节点的 config.json、control_token.txt 拷到
#                                             $TESTNET_STATE/nodes/<名>/.anet/(目录 0700、文件 0600),并写
#                                             $TESTNET_STATE/nodes.env(每个节点的控制口、hub、AID、所在主机)
#   bash scripts/testnet/bridge.sh status | down
#
# 为什么需要:两岛之间不通(见 topology.env 顶部),daemon 控制面只绑 loopback。转发进程的 pid 记在
# $TESTNET_STATE/bridge/,down 只杀这些 pid(且核对它们确实是 ssh)。
set -euo pipefail

main(){
  local here; here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  # shellcheck source=common.sh
  . "$here/common.sh"
  tn_validate
  BR="$TESTNET_STATE/bridge"; mkdir -p "$BR"
  case "${1:-}" in
    ctl-up)  ctl_up ;;
    fed-up)  fed_up ;;
    mirror)  mirror ;;
    status)  status ;;
    down)    down ;;
    *)       sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
  esac
}

local_port_free(){ [ -z "$(ss -tlnH "( sport = :$1 )" 2>/dev/null)" ]; }

# forward NAME TARGET SPEC…: one background `ssh -N` carrying the -L specs, pid in $BR/NAME.pid.
forward(){
  local name=$1 target=$2; shift 2
  if [ -f "$BR/$name.pid" ] && kill -0 "$(cat "$BR/$name.pid")" 2>/dev/null; then
    tn_info "$name already up (pid $(cat "$BR/$name.pid"))"; return 0
  fi
  local args=() s
  for s in "$@"; do args+=(-L "$s"); done
  setsid ssh -N "${TN_SSH_OPTS[@]}" -o ExitOnForwardFailure=yes "${args[@]}" "$target" \
    >"$BR/$name.log" 2>&1 </dev/null &
  echo $! > "$BR/$name.pid"
  sleep 2
  kill -0 "$(cat "$BR/$name.pid")" 2>/dev/null || { cat "$BR/$name.log" >&2; tn_die "$name: ssh forward failed"; }
  tn_ok "$name up: $*"
}

ctl_up(){
  local h n p specs
  for h in $(tn_hosts); do
    specs=()
    for n in $(tn_nodes daemon "$h") $(tn_nodes official "$h"); do
      p=$(tn_node_port "$n")
      local_port_free "$p" || tn_die "local port $p is busy; cannot mirror $n's control port"
      specs+=("127.0.0.1:$p:127.0.0.1:$p")
    done
    [ ${#specs[@]} -gt 0 ] || continue
    forward "ctl-$h" "$(tn_host_ssh "$h")" "${specs[@]}"
  done
}

fed_up(){
  local pair a b
  while read -r a b; do
    [ -n "$b" ] || continue
    fed_one "$a" "$b"; fed_one "$b" "$a"
  done < <(tn_rows "$TESTNET_FEDERATION_BRIDGED")
}

# fed_one VIEWER PEER: make PEER reachable from VIEWER's island at the bridge address of that island.
fed_one(){
  local viewer=$1 peer=$2 island var addr port ph
  island=$(tn_host_island "$(tn_node_host "$viewer")"); var="TESTNET_BRIDGE_ADDR_$island"; addr=${!var:-}
  [ -n "$addr" ] || tn_die "no $var"
  ip -4 -o addr show | grep -q "inet $addr/" || tn_die "$addr is not an address of this machine — run bridge.sh on the bridge host"
  port=$(tn_node_port "$peer"); ph=$(tn_node_host "$peer")
  local_port_free "$port" || [ -f "$BR/fed-$peer-for-$island.pid" ] || tn_die "local port $port is busy"
  forward "fed-$peer-for-$island" "$(tn_host_ssh "$ph")" "$addr:$port:$(tn_host_dial "$ph"):$port"
}

mirror(){
  local n h dir env="$TESTNET_STATE/nodes.env" aid
  ( umask 077; mkdir -p "$TESTNET_STATE/nodes"; : > "$env.tmp" )
  for n in $(tn_nodes daemon) $(tn_nodes official); do
    h=$(tn_node_host "$n"); dir="$TESTNET_STATE/nodes/$n/.anet"
    ( umask 077; mkdir -p "$dir"
      tn_remote "$h" rt_file "$n" home/.anet/config.json > "$dir/config.json"
      tn_remote "$h" rt_file "$n" home/.anet/control_token.txt > "$dir/control_token.txt" )
    aid=$(tn_remote "$h" rt_aid "$n" "$(tn_node_port "$n")" || true)
    {
      printf 'TN_%s_HOST=%q\n' "$n" "$h"
      printf 'TN_%s_CTL=%q\n'  "$n" "127.0.0.1:$(tn_node_port "$n")"
      printf 'TN_%s_HUB=%q\n'  "$n" "$(tn_hub_url "$(tn_node_hub "$n")")"
      printf 'TN_%s_AID=%q\n'  "$n" "$aid"
      printf 'TN_%s_HOME=%q\n' "$n" "$TESTNET_STATE/nodes/$n"
    } >> "$env.tmp"
    tn_ok "$n mirrored (aid ${aid:-?})"
  done
  for n in $(tn_nodes hub); do printf 'TN_%s_URL=%q\n' "$n" "$(tn_hub_url "$n")" >> "$env.tmp"; done
  mv -f "$env.tmp" "$env"
  tn_ok "wrote $env"
}

status(){
  local f pid
  for f in "$BR"/*.pid; do
    [ -f "$f" ] || { echo "(no forwards)"; return 0; }
    pid=$(cat "$f")
    if kill -0 "$pid" 2>/dev/null; then echo "up    $(basename "$f" .pid)  pid $pid"
    else echo "dead  $(basename "$f" .pid)"; fi
  done
}

down(){
  local f pid
  for f in "$BR"/*.pid; do
    [ -f "$f" ] || continue
    pid=$(cat "$f")
    if kill -0 "$pid" 2>/dev/null && [ "$(cat "/proc/$pid/comm" 2>/dev/null)" = ssh ]; then
      kill -TERM "$pid"; tn_ok "stopped $(basename "$f" .pid)"
    fi
    rm -f "$f"
  done
}

main "$@"
