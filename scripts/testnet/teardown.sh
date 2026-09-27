#!/usr/bin/env bash
# teardown.sh — 拆掉一台主机上的测试网。
#
#   bash scripts/testnet/teardown.sh <host>              停掉该主机上全部 anet-testnet-* 单元与测试网进程,
#                                                        删除整个 /opt/anet-testnet(非 root:~/anet-testnet)
#   bash scripts/testnet/teardown.sh <host> --run tn1    只拆这一次运行:anet-testnet-tn1-* 与 <base>/tn1
#   bash scripts/testnet/teardown.sh <host> --dry-run    只列出将要停的单元、杀的进程、删的目录
#   bash scripts/testnet/teardown.sh all [...]           对 topology.env 里的每台主机做同样的事
#
# 它停的只有两类东西:名字以 anet-testnet- 开头的 systemd 单元,以及可执行文件位于测试网目录之下的
# 进程(pid 文件里的,和兜底按可执行文件路径找到的)。它删的只有 /opt/anet-testnet 或 ~/anet-testnet
# (或其下的一次运行目录),删之前把路径解析成真实路径再核对一遍,不是这两个就拒绝。
# 现有的 anet-dmax、anet-hub、anet4、anet-face-daemon……以及 /opt/anet-test(旧 QA 目录,注意少一个 net)
# 都不在它的匹配范围内。
set -euo pipefail

main(){
  local here; here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  # shellcheck source=common.sh
  . "$here/common.sh"

  [ $# -ge 1 ] || { sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
  local target=$1; shift
  local scope=all dry=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --run)     scope=$2; shift 2 ;;
      --dry-run) dry=1; shift ;;
      *)         tn_die "unknown argument: $1" ;;
    esac
  done
  if [ "$scope" != all ]; then
    [[ "$scope" =~ ^[a-z0-9][a-z0-9-]{0,23}$ ]] || tn_die "bad run id: $scope"
  fi

  local hosts=() h
  if [ "$target" = all ]; then mapfile -t hosts < <(tn_hosts)
  else tn_host_ssh "$target" >/dev/null || tn_die "unknown host: $target"; hosts=("$target"); fi

  for h in "${hosts[@]}"; do
    tn_info "── teardown $h ($(tn_host_ssh "$h")) scope=$scope$([ "$dry" = 1 ] && echo ' (dry run)')"
    tn_remote "$h" rt_teardown "$scope" "$dry" || tn_warn "$h: teardown reported an error"
  done
  tn_ok "done"
}

main "$@"
