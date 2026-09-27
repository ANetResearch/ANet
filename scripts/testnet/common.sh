# shellcheck shell=bash
# common.sh — 本机侧的公共函数(source 它,不要直接运行)。
#
# 读 topology.env,提供表查询、校验、以及 tn_remote:把 remote.sh 的函数库连同一行调用经 ssh 的
# stdin 送到远端 `bash -s` 执行。远端不落任何脚本文件,除了 deploy 自己写进隔离目录的启动器。

TN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TN_ANET_ROOT="$(cd "$TN_DIR/../.." && pwd)"
TN_WS="$(dirname "$TN_ANET_ROOT")"
TN_TOPOLOGY="${TESTNET_TOPOLOGY:-$TN_DIR/topology.env}"
[ -f "$TN_TOPOLOGY" ] || { echo "topology not found: $TN_TOPOLOGY" >&2; exit 1; }
# shellcheck source=topology.env
. "$TN_TOPOLOGY"
TESTNET_BUILD_DIR=${TESTNET_BUILD_DIR:-$TN_WS/.testnet-build}

TN_SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=4)

tn_die(){  printf '\033[1;31m%s\033[0m\n' "$*" >&2; exit 1; }
tn_info(){ printf '\033[1;36m%s\033[0m\n' "$*" >&2; }
tn_ok(){   printf '\033[1;32m%s\033[0m\n' "$*" >&2; }
tn_warn(){ printf '\033[1;33m%s\033[0m\n' "$*" >&2; }

# tn_rows TABLE: the table without comments and blank lines.
tn_rows(){ printf '%s\n' "$1" | sed -e 's/#.*//' | awk 'NF'; }

# ── hosts ────────────────────────────────────────────────────────
tn_hosts(){ tn_rows "$TESTNET_HOSTS" | awk '{print $1}'; }
tn_host_field(){ tn_rows "$TESTNET_HOSTS" | awk -v h="$1" -v f="$2" '$1==h {print $f; ok=1} END {exit !ok}'; }
tn_host_ssh(){    tn_host_field "$1" 2; }
tn_host_arch(){   tn_host_field "$1" 3; }
tn_host_dial(){   tn_host_field "$1" 4; }
tn_host_bind(){   tn_host_field "$1" 5; }
tn_host_island(){ tn_host_field "$1" 6; }
tn_arches(){ tn_rows "$TESTNET_HOSTS" | awk '{print $3}' | sort -u; }

# ── nodes ────────────────────────────────────────────────────────
# tn_nodes [ROLE] [HOST]: node names, in table order.
tn_nodes(){ tn_rows "$TESTNET_NODES" | awk -v r="${1:-}" -v h="${2:-}" '(r=="" || $2==r) && (h=="" || $3==h) {print $1}'; }
tn_node_field(){ tn_rows "$TESTNET_NODES" | awk -v n="$1" -v f="$2" '$1==n {print $f; ok=1} END {exit !ok}'; }
tn_node_role(){  tn_node_field "$1" 2; }
tn_node_host(){  tn_node_field "$1" 3; }
tn_node_port(){  tn_node_field "$1" 4; }
tn_node_hub(){   tn_node_field "$1" 5; }
tn_node_peer(){  tn_node_field "$1" 6; }
tn_node_bin(){   tn_node_field "$1" 7; }

# tn_hub_url HUB [VIEWER_HOST]: the URL VIEWER_HOST uses to reach HUB. Same island → the hub host's
# dial address; another island → the bridge address of the viewer's island (bridge.sh fed-up).
tn_hub_url(){
  local hub=$1 viewer=${2:-} hh port
  hh=$(tn_node_host "$hub") || tn_die "unknown hub $hub"
  port=$(tn_node_port "$hub")
  if [ -n "$viewer" ] && [ "$(tn_host_island "$viewer")" != "$(tn_host_island "$hh")" ]; then
    local var="TESTNET_BRIDGE_ADDR_$(tn_host_island "$viewer")"
    [ -n "${!var:-}" ] || tn_die "no bridge address for island $(tn_host_island "$viewer") ($var)"
    printf 'http://%s:%s' "${!var}" "$port"
  else
    printf 'http://%s:%s' "$(tn_host_dial "$hh")" "$port"
  fi
}

tn_unit(){ printf '%s-%s-%s' "$TESTNET_UNIT_PREFIX" "$TESTNET_RUN_ID" "$1"; }

# tn_ports_of NODE: every TCP port the node listens on (derived ones included).
tn_ports_of(){
  local n=$1 p pp
  p=$(tn_node_port "$n"); pp=$(tn_node_peer "$n")
  case "$(tn_node_role "$n")" in
    hub)      echo "$p"; echo $((p + 50)) ;;
    daemon)   echo "$p"; echo $((p + 50)); [ "$pp" = - ] || echo "$pp" ;;
    official) echo "$p"; echo $((p + 1)); echo $((p + 50)); [ "$pp" = - ] || echo "$pp" ;;
  esac
}

# tn_validate: refuse a topology that could collide with anything outside the testnet.
tn_validate(){
  [[ "$TESTNET_RUN_ID" =~ ^[a-z0-9][a-z0-9-]{0,23}$ ]] || tn_die "TESTNET_RUN_ID must match [a-z0-9][a-z0-9-]{0,23}: $TESTNET_RUN_ID"
  [ "$TESTNET_UNIT_PREFIX" = anet-testnet ] || tn_die "TESTNET_UNIT_PREFIX must stay anet-testnet (teardown relies on it)"
  local h n r host hub p seen=""
  for h in $(tn_hosts); do
    [[ "$h" =~ ^[a-z0-9]+$ ]] || tn_die "host alias must be [a-z0-9]+: $h"
    [ -n "$(tn_host_field "$h" 6)" ] || tn_die "host $h: the row needs 6 columns"
  done
  for n in $(tn_nodes); do
    [[ "$n" =~ ^[a-z0-9]+$ ]] || tn_die "node name must be [a-z0-9]+: $n"
    [ -n "$(tn_node_bin "$n")" ] || tn_die "node $n: the row needs 7 columns"
    r=$(tn_node_role "$n"); host=$(tn_node_host "$n"); hub=$(tn_node_hub "$n")
    case "$r" in hub|daemon|official) ;; *) tn_die "node $n: unknown role $r" ;; esac
    tn_host_ssh "$host" >/dev/null || tn_die "node $n: unknown host $host"
    if [ "$r" != hub ]; then
      [ "$(tn_node_role "$hub" 2>/dev/null)" = hub ] || tn_die "node $n: $hub is not a hub"
    fi
    for p in $(tn_ports_of "$n"); do
      [[ "$p" =~ ^[0-9]+$ ]] && [ "$p" -ge "$TESTNET_PORT_MIN" ] && [ "$p" -le "$TESTNET_PORT_MAX" ] \
        || tn_die "node $n: port $p outside $TESTNET_PORT_MIN-$TESTNET_PORT_MAX"
      case " $seen " in *" $p "*) tn_die "port $p used twice in the topology" ;; esac
      seen="$seen $p"
    done
  done
}

# tn_remote HOST FUNC [ARGS...]: run FUNC from remote.sh on HOST. Variables go first as %q
# assignments, then the library, then the one call; bash reads the script from stdin, so nothing on
# the remote side may read stdin (remote.sh redirects every child from /dev/null).
tn_remote(){
  local host=$1; shift
  local target; target=$(tn_host_ssh "$host") || tn_die "unknown host $host"
  {
    printf 'TN_RUN_ID=%q\nTN_PREFIX=%q\nTN_MODE=%q\nTN_PORT_MIN=%q\nTN_PORT_MAX=%q\n' \
      "$TESTNET_RUN_ID" "$TESTNET_UNIT_PREFIX" "${TESTNET_MODE:-auto}" "$TESTNET_PORT_MIN" "$TESTNET_PORT_MAX"
    cat "$TN_DIR/remote.sh"
    printf '%q ' "$@"; printf '\n'
  } | ssh "${TN_SSH_OPTS[@]}" "$target" 'bash -s'
}

# tn_scp HOST DEST FILES...: copy files to an absolute directory on HOST.
tn_scp(){
  local host=$1 dest=$2; shift 2
  local target; target=$(tn_host_ssh "$host") || tn_die "unknown host $host"
  scp -q -C "${TN_SSH_OPTS[@]}" "$@" "$target:$dest/"
}

# tn_bin_dir ARCH: where build.sh put the binaries for ARCH.
tn_bin_dir(){ printf '%s/linux-%s' "$TESTNET_BUILD_DIR" "$1"; }
