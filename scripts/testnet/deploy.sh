#!/usr/bin/env bash
# deploy.sh — 把测试网的一个角色部署到一台主机。拓扑(谁在哪台、用哪个端口)全在 topology.env。
#
#   bash scripts/testnet/deploy.sh <host> <role> [节点名…]
#       host   topology.env 主机表里的别名(ink89 ink90 dmax cmax)
#       role   hub1 | hub2 | hub3 | <任一 hub 节点名>   部署这个 hub(必须在该主机上)
#              hub                                     该主机上的全部 hub
#              daemon [名…]                            该主机上的全部(或指定)daemon 身份
#              official [名…]                          official 节点:anet-official 后端 + 一个 daemon 身份
#              all                                     该主机上的全部节点,顺序 hub → daemon → official
#   bash scripts/testnet/deploy.sh federate            按 TESTNET_FEDERATION 写各 hub 的 federation.json 并重启
#   bash scripts/testnet/deploy.sh everything          全拓扑:各 hub → federate → 各 daemon/official
#   bash scripts/testnet/deploy.sh status [host]       各主机上测试网在跑什么
#   bash scripts/testnet/deploy.sh restart|stop <节点>[-peer|-admin|-backend]
#                                                      重启/停掉一个进程(不重传二进制、不改配置;测"对端离线"用)
#   bash scripts/testnet/deploy.sh plan                只打印拓扑与将要做的事,不连任何主机
#
# 环境变量:
#   TESTNET_RUN_ID=tn1     运行标识(目录与单元名都带它)
#   REGISTER=1             daemon 起来后向它的 hub /hub-register(名字 tn-<节点>),有 anetpeer 的再
#                          /p2p-advertise 它的拨号地址(否则没有对端会直连它);0 = 都不做
#   HUB_ADMIN=0            1 = 每个 hub 旁起 anet-hub-admin(127.0.0.1:<hub端口+50>,令牌只存远端 0600 文件)
#   REWRITE_CONFIG=0       1 = 覆盖已有的 daemon config.json(默认保留:身份与测试写入的配置跨重部署保留)
#   FEDERATE_BRIDGED=0     1 = federate 时也写跨岛对(先 bridge.sh fed-up)
#   TESTNET_MODE=auto      auto | system | user(见 README)
#
# 先 build.sh。部署顺序有依赖:daemon 注册需要它的 hub 已经在跑(cmax 的 daemon 用 dmax 上的 hub3)。
#
# 安全边界(README 详述):只写 /opt/anet-testnet/<run>/(root)或 ~/anet-testnet/<run>/(非 root);
# 只起停 anet-testnet-<run>-* 单元或自己 pid 文件里的进程;端口被占即拒绝;不碰任何现有单元、目录、端口。
set -euo pipefail

main(){
  local here; here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  # shellcheck source=common.sh
  . "$here/common.sh"
  tn_validate

  REGISTER=${REGISTER:-1}; HUB_ADMIN=${HUB_ADMIN:-0}; REWRITE_CONFIG=${REWRITE_CONFIG:-0}
  FEDERATE_BRIDGED=${FEDERATE_BRIDGED:-0}

  case "${1:-}" in
    ""|-h|--help) sed -n '2,33p' "$0" | sed 's/^# \{0,1\}//'; return 0 ;;
    plan)       plan; return 0 ;;
    status)     local h; for h in ${2:-$(tn_hosts)}; do tn_remote "$h" rt_status || tn_warn "$h: status failed"; done; return 0 ;;
    federate)   federate; return 0 ;;
    everything) everything; return 0 ;;
    restart|stop)
      local u=${2:-} base; [ -n "$u" ] || tn_die "usage: deploy.sh $1 <node>[-peer|-admin|-backend]"
      base=${u%-peer}; base=${base%-admin}; base=${base%-backend}
      tn_node_host "$base" >/dev/null || tn_die "unknown node: $base"
      if [ "$1" = restart ]; then tn_remote "$(tn_node_host "$base")" rt_restart "$u"
      else tn_remote "$(tn_node_host "$base")" rt_halt "$u"; fi
      return 0 ;;
  esac

  local host=$1 role=${2:-}; shift 2 || tn_die "usage: deploy.sh <host> <role> [names…]"
  tn_host_ssh "$host" >/dev/null || tn_die "unknown host: $host (known: $(tn_hosts | tr '\n' ' '))"
  local nodes=()
  case "$role" in
    hub|daemon|official) if [ $# -gt 0 ]; then nodes=("$@"); else mapfile -t nodes < <(tn_nodes "$role" "$host"); fi ;;
    all)  mapfile -t nodes < <(tn_nodes hub "$host"; tn_nodes daemon "$host"; tn_nodes official "$host") ;;
    *)    [ "$(tn_node_role "$role" 2>/dev/null)" = hub ] || tn_die "unknown role: $role"
          nodes=("$role") ;;
  esac
  [ ${#nodes[@]} -gt 0 ] || tn_die "topology.env assigns no '$role' node to host $host"
  local n
  for n in "${nodes[@]}"; do
    [ "$(tn_node_host "$n" 2>/dev/null)" = "$host" ] || tn_die "node $n is not on host $host in topology.env"
    [ "$role" = all ] || [ "$role" = "$n" ] || [ "$(tn_node_role "$n")" = "$role" ] \
      || tn_die "node $n is a $(tn_node_role "$n"), not a $role"
  done

  ship_bins "$host" "${nodes[@]}"
  for n in "${nodes[@]}"; do deploy_node "$n"; done
  tn_ok "deployed on $host: ${nodes[*]}  (run $TESTNET_RUN_ID)"
}

plan(){
  printf 'run id   %s\nbuild    %s\nstate    %s\n\n' "$TESTNET_RUN_ID" "$TESTNET_BUILD_DIR" "$TESTNET_STATE"
  local h n
  for h in $(tn_hosts); do
    printf '%-6s %-20s %s  island=%s  bind=%s\n' "$h" "$(tn_host_ssh "$h")" "linux/$(tn_host_arch "$h")" \
      "$(tn_host_island "$h")" "$(tn_host_bind "$h")"
    for n in $(tn_nodes "" "$h"); do
      case "$(tn_node_role "$n")" in
        hub) printf '    %-6s hub       %s   unit %s\n' "$n" "$(tn_hub_url "$n")" "$(tn_unit "$n")" ;;
        *)   printf '    %-6s %-9s ctl 127.0.0.1:%s → %s  p2p %s  bin %s\n' "$n" "$(tn_node_role "$n")" \
               "$(tn_node_port "$n")" "$(tn_hub_url "$(tn_node_hub "$n")" "$h")" "$(tn_node_peer "$n")" "$(tn_node_bin "$n")" ;;
      esac
    done
  done
  echo; echo "federation (direct):";  tn_rows "$TESTNET_FEDERATION" | sed 's/^/    /'
  echo "federation (bridged, FEDERATE_BRIDGED=1):"; tn_rows "$TESTNET_FEDERATION_BRIDGED" | sed 's/^/    /'
}

# ship_bins HOST NODES…: copy the binaries these nodes need (only those whose sha256 differs), verify
# on the far end, install into <run>/bin.
ship_bins(){
  local host=$1; shift
  local arch dir rundir need=(anetfixture) n b send=()
  arch=$(tn_host_arch "$host"); dir=$(tn_bin_dir "$arch")
  [ -s "$dir/SHA256SUMS" ] || tn_die "no build for linux/$arch at $dir — run build.sh first"
  for n in "$@"; do
    case "$(tn_node_role "$n")" in
      hub)      need+=(anet-hub)
                if [ "$HUB_ADMIN" = 1 ]; then need+=(anet-hub-admin); fi ;;
      daemon)   need+=("$(tn_node_bin "$n")") ;;
      official) need+=("$(tn_node_bin "$n")" anet-official) ;;
    esac
    if [ "$(tn_node_role "$n")" != hub ] && [ "$(tn_node_peer "$n")" != - ]; then need+=(anetpeer); fi
  done
  mapfile -t need < <(printf '%s\n' "${need[@]}" | sort -u)
  for b in "${need[@]}"; do [ -f "$dir/$b" ] || tn_die "$b is not in $dir (build it: see build.sh --variant / cmd/anet-official)"; done

  rundir=$(tn_remote "$host" rt_prepare) || tn_die "$host: cannot prepare the run directory"
  [[ "$rundir" == */anet-testnet/"$TESTNET_RUN_ID" ]] || tn_die "$host: unexpected run directory '$rundir'"
  local remote_sums; remote_sums=$(tn_remote "$host" rt_bin_sums)
  for b in "${need[@]}"; do
    local want; want=$(awk -v f="$b" '$2==f {print $1}' "$dir/SHA256SUMS")
    printf '%s\n' "$remote_sums" | awk -v f="$b" -v s="$want" '$2==f && $1==s {found=1} END {exit !found}' \
      || send+=("$dir/$b")
  done
  if [ ${#send[@]} -gt 0 ]; then
    tn_info "$host: sending ${#send[@]} file(s) to $rundir/bin: $(for b in "${send[@]}"; do basename "$b"; done | tr '\n' ' ')"
    tn_scp "$host" "$rundir/bin/.incoming" "${send[@]}" "$dir/SHA256SUMS" "$dir/MANIFEST"
  else
    tn_info "$host: binaries already current"
    tn_scp "$host" "$rundir/bin/.incoming" "$dir/SHA256SUMS" "$dir/MANIFEST"
  fi
  tn_remote "$host" rt_install_bins
}

deploy_node(){
  local n=$1 host role port hub hub_url pport cfg64=""
  host=$(tn_node_host "$n"); role=$(tn_node_role "$n"); port=$(tn_node_port "$n")
  case "$role" in
    hub)
      tn_info "── $host: hub $n"
      tn_remote "$host" rt_hub "$n" "$(tn_host_bind "$host")" "$port" "$HUB_ADMIN"
      ;;
    daemon|official)
      hub=$(tn_node_hub "$n"); hub_url=$(tn_hub_url "$hub" "$host"); pport=$(tn_node_peer "$n")
      if [ "$role" = official ]; then
        tn_info "── $host: official $n (backend 127.0.0.1:$((port + 1)), groups $TESTNET_OFFICIAL_GROUPS)"
        [[ "$TESTNET_OFFICIAL_GROUPS" =~ ^[a-z]+(,[a-z]+)*$ ]] || tn_die "TESTNET_OFFICIAL_GROUPS must be a comma list of group names"
        local args=() i
        read -r -a args <<< "$TESTNET_OFFICIAL_ARGS"
        # {PORT} and {GROUPS} here; {TOKEN_FILE} on the far side, where the node directory is.
        for i in "${!args[@]}"; do
          args[i]=${args[i]//\{PORT\}/$((port + 1))}
          args[i]=${args[i]//\{GROUPS\}/$TESTNET_OFFICIAL_GROUPS}
        done
        tn_remote "$host" rt_official_backend "$n" "$port" "${args[@]}"
        cfg64=$(official_config "$n" "$port" "$hub_url")
        # No template: the daemon's configuration is what the backend's own generator prints.
        [ -n "$cfg64" ] || tn_remote "$host" rt_official_config "$n" "$port" "$hub_url" \
          "$TESTNET_OFFICIAL_GROUPS" "$REWRITE_CONFIG"
      else
        tn_info "── $host: daemon $n → $hub_url"
      fi
      tn_remote "$host" rt_daemon "$n" "$port" "$hub_url" "$(tn_node_bin "$n")" \
        "$(tn_host_bind "$host")" "$pport" "$(tn_host_dial "$host")" "$cfg64" "$REWRITE_CONFIG"
      if [ "$REGISTER" = 1 ]; then
        local adv=""
        [ "$pport" = - ] || adv="$(tn_host_dial "$host"):$pport"
        tn_remote "$host" rt_register "$n" "$port" "$hub_url" "tn-$n" "$adv"
      fi
      ;;
  esac
}

# official_config NAME PORT HUB_URL: the template from TESTNET_OFFICIAL_CONFIG with placeholders
# filled ({TOKEN_FILE} is left for rt_daemon, on the far side), base64 on one line; empty when there is
# no template (rt_official_config then generates the configuration on the host).
official_config(){
  local n=$1 port=$2 hub=$3 t
  [ -n "$TESTNET_OFFICIAL_CONFIG" ] || return 0
  [ -f "$TESTNET_OFFICIAL_CONFIG" ] || tn_die "TESTNET_OFFICIAL_CONFIG not found: $TESTNET_OFFICIAL_CONFIG"
  t=$(cat "$TESTNET_OFFICIAL_CONFIG")
  t=${t//\{CONTROL_ADDR\}/127.0.0.1:$port}
  t=${t//\{HUB_URL\}/$hub}
  t=${t//\{BACKEND_URL\}/http://127.0.0.1:$((port + 1))}
  t=${t//\{NAME\}/tn-$n}
  printf '%s\n' "$t" | base64 -w0
}

# federate: for every hub in a pair, fetch its identity, write federation.json naming its peers at the
# address that hub can reach them by, restart it.
federate(){
  local pairs a hubs=() h peers_of json url first
  pairs=$(tn_rows "$TESTNET_FEDERATION")
  if [ "$FEDERATE_BRIDGED" = 1 ]; then
    pairs=$(printf '%s\n%s' "$pairs" "$(tn_rows "$TESTNET_FEDERATION_BRIDGED")")
  fi
  [ -n "$(printf '%s' "$pairs" | awk 'NF')" ] || { tn_warn "no federation pairs"; return 0; }
  mapfile -t hubs < <(printf '%s\n' "$pairs" | awk 'NF {print $1; print $2}' | sort -u)
  declare -A AID
  for h in "${hubs[@]}"; do
    [ "$(tn_node_role "$h" 2>/dev/null)" = hub ] || tn_die "federation names $h, which is not a hub"
    AID[$h]=$(tn_remote "$(tn_node_host "$h")" rt_hub_identity "$(tn_hub_url "$h")")
    [ -n "${AID[$h]}" ] || tn_die "cannot read the identity of $h ($(tn_hub_url "$h")) — is it deployed?"
    tn_info "$h aid ${AID[$h]}"
  done
  for h in "${hubs[@]}"; do
    peers_of=$(printf '%s\n' "$pairs" | awk -v h="$h" '$1==h {print $2} $2==h {print $1}' | sort -u)
    json="{\"delivery\":\"allowlist\",\"discovery\":\"allowlist\",\"home\":\"$(tn_hub_url "$h")\",\"peers\":["
    first=1
    for a in $peers_of; do
      url=$(tn_hub_url "$a" "$(tn_node_host "$h")")
      [ "$first" = 1 ] || json="$json,"
      json="$json{\"aid\":\"${AID[$a]}\",\"endpoint\":\"$url\"}"; first=0
    done
    json="$json]}"
    tn_info "── $h federation: $(printf '%s' "$peers_of" | tr '\n' ' ')"
    tn_remote "$(tn_node_host "$h")" rt_federation "$h" "$(printf '%s\n' "$json" | base64 -w0)" "$(tn_hub_url "$h")"
  done
}

everything(){
  local h n on=()
  for h in $(tn_hosts); do
    mapfile -t on < <(tn_nodes hub "$h")
    [ ${#on[@]} -gt 0 ] || continue
    ship_bins "$h" "${on[@]}"
    for n in "${on[@]}"; do deploy_node "$n"; done
  done
  federate
  for h in $(tn_hosts); do
    mapfile -t on < <(tn_nodes daemon "$h"; tn_nodes official "$h")
    [ ${#on[@]} -gt 0 ] || continue
    ship_bins "$h" "${on[@]}"
    for n in "${on[@]}"; do deploy_node "$n"; done
  done
  tn_ok "testnet $TESTNET_RUN_ID deployed"
}

main "$@"
