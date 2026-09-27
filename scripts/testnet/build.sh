#!/usr/bin/env bash
# build.sh — 在本机为测试网交叉编译全部二进制(linux/<拓扑里各主机的架构>)。
#
#   bash scripts/testnet/build.sh                        按 topology.env 的架构构建
#   bash scripts/testnet/build.sh -t shell               daemon(anet)带 -tags shell
#   bash scripts/testnet/build.sh --hub-tags taskboard   hub 带 -tags taskboard
#   bash scripts/testnet/build.sh --variant min=no_mcp,no_a2a --variant shell=shell
#                                                        另出 anet-min、anet-shell 两个变体
#   bash scripts/testnet/build.sh --arch arm64 -o DIR    指定架构 / 输出目录
#
# 产物:<输出目录>/linux-<arch>/{anet, anet-<变体>…, anet-hub, anet-hub-admin, anetpeer, anetfixture,
# a2aprobe 与 a2ashape-hermes.test(scripts/joint-a2a.sh 用;ANet/tools/a2aprobe 存在时),
# anet-official(ANet/cmd/anet-official 存在时), SHA256SUMS, MANIFEST}。默认输出目录见 topology.env
# 的 TESTNET_BUILD_DIR(空 = 工作区根下的 .testnet-build)。
#
# 工作区:优先用本仓旁边的 env.sh(wt/<包>/env.sh,它的 GOWORK 指向这个工作树的三仓),否则用
# /data/projs/anet-dev/.anet-env.sh。构建前核对 GOWORK 里 use 的 ANet 目录就是本脚本所在的仓 ——
# 在工作树里用主检出的 go.work 构建,会静悄悄地编进另一份源码。
#
# hub 的网页是仓里已提交的嵌入副本(internal/aghub/web/index.html),这里不重建 webui;MANIFEST 记下这一点。
set -euo pipefail

usage(){ sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

main(){
  local here; here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  # shellcheck source=common.sh
  . "$here/common.sh"

  local tags="" hub_tags="" out="" variants=() arches=() allow_mismatch=0
  while [ $# -gt 0 ]; do
    case "$1" in
      -t|--tags)      tags=$2; shift 2 ;;
      --hub-tags)     hub_tags=$2; shift 2 ;;
      --variant)      [[ "$2" =~ ^[a-z0-9]+=[a-z0-9_,]*$ ]] || tn_die "--variant NAME=TAGS (NAME [a-z0-9]+, TAGS comma list)"
                      variants+=("$2"); shift 2 ;;
      --arch)         arches+=("$2"); shift 2 ;;
      -o|--out)       out=$2; shift 2 ;;
      --allow-gowork-mismatch) allow_mismatch=1; shift ;;
      -h|--help)      usage 0 ;;
      *)              echo "unknown argument: $1" >&2; usage 2 ;;
    esac
  done
  [ ${#arches[@]} -gt 0 ] || mapfile -t arches < <(tn_arches)
  out=${out:-$TESTNET_BUILD_DIR}

  local anet_root="$TN_ANET_ROOT" hub_root="$TN_WS/ANetHub" core_root="$TN_WS/ANetCore"
  [ -d "$hub_root/cmd/anet-hub" ] || tn_die "ANetHub not found next to ANet: $hub_root"

  # ── Go environment ─────────────────────────────────────────────
  if [ -f "$TN_WS/env.sh" ]; then
    # shellcheck disable=SC1091
    . "$TN_WS/env.sh"; tn_info "env: $TN_WS/env.sh"
  elif [ -f /data/projs/anet-dev/.anet-env.sh ]; then
    # shellcheck disable=SC1091
    . /data/projs/anet-dev/.anet-env.sh; tn_info "env: /data/projs/anet-dev/.anet-env.sh"
  fi
  command -v go >/dev/null || tn_die "go not on PATH"
  check_gowork "$anet_root" "$allow_mismatch"

  mkdir -p "$out"
  local arch dir
  for arch in "${arches[@]}"; do
    dir="$out/linux-$arch"
    rm -rf -- "$dir"; mkdir -p "$dir"
    tn_info "── linux/$arch → $dir"
    go_build "$anet_root" ./cmd/anet            "$dir/anet"            "$arch" "$tags"     anet
    local v
    for v in "${variants[@]}"; do
      go_build "$anet_root" ./cmd/anet          "$dir/anet-${v%%=*}"   "$arch" "${v#*=}"   anet
    done
    go_build "$anet_root" ./tools/anetpeer      "$dir/anetpeer"        "$arch" ""          plain
    go_build "$anet_root" ./tools/anetfixture   "$dir/anetfixture"     "$arch" ""          plain
    if [ -d "$anet_root/tools/a2aprobe" ]; then
      # joint-a2a.sh: the a2a-go client, and the Hermes contract as a test binary (hosts have no go).
      go_build "$anet_root" ./tools/a2aprobe    "$dir/a2aprobe"        "$arch" ""          plain
      go_test_bin "$anet_root" ./internal/a2ashape "$dir/a2ashape-hermes.test" "$arch"
    fi
    go_build "$hub_root"  ./cmd/anet-hub        "$dir/anet-hub"        "$arch" "$hub_tags" hub
    go_build "$hub_root"  ./cmd/anet-hub-admin  "$dir/anet-hub-admin"  "$arch" "$hub_tags" hub
    if [ -d "$anet_root/cmd/anet-official" ]; then
      go_build "$anet_root" ./cmd/anet-official "$dir/anet-official"   "$arch" "$tags"     anet
    else
      tn_warn "  anet-official: ANet/cmd/anet-official 不存在,跳过(official 角色部署会拒绝)"
    fi
    write_manifest "$dir" "$arch" "$tags" "$hub_tags" "${variants[*]:-}" "$anet_root" "$hub_root" "$core_root"
    ( cd "$dir" && sha256sum -- * | grep -v -E ' (SHA256SUMS|MANIFEST)$' > SHA256SUMS )
    tn_ok "  $(ls "$dir" | grep -v -E '^(SHA256SUMS|MANIFEST)$' | tr '\n' ' ')"
  done
  tn_ok "build done: $out"
}

# check_gowork ROOT ALLOW: the workspace GOWORK names must include ROOT.
check_gowork(){
  local root=$1 allow=$2 gw wd u found=0
  gw=$(go env GOWORK)
  [ -n "$gw" ] && [ "$gw" != off ] || tn_die "GOWORK is not set; ANet and ANetHub must be built from one workspace (see scripts/joint.sh header)"
  wd=$(dirname "$gw")
  while read -r u; do
    [ "$(cd "$wd" && realpath -m "$u")" = "$(realpath "$root")" ] && found=1
  done < <(awk '/^use *\(/{f=1;next} f&&/\)/{f=0} f{print $1} /^use [^(]/{print $2}' "$gw")
  if [ "$found" = 1 ]; then tn_info "GOWORK $gw includes $root"
  elif [ "$allow" = 1 ]; then tn_warn "GOWORK $gw does not include $root — building another checkout (allowed)"
  else tn_die "GOWORK $gw does not include $root; source the right env.sh or pass --allow-gowork-mismatch"
  fi
}

# go_build REPO PKG OUT ARCH TAGS KIND — KIND anet|hub stamps the version package, plain does not.
go_build(){
  local repo=$1 pkg=$2 outf=$3 arch=$4 tags=$5 kind=$6 commit built ld="-s -w"
  commit=$(repo_commit "$repo"); built=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  case "$kind" in
    anet) local p=github.com/ANetResearch/ANet/internal/version
          ld="$ld -X $p.Commit=$commit -X $p.BuiltAt=$built -X $p.Tags=$tags" ;;
    hub)  local p=github.com/ANetResearch/ANetHub/internal/version
          ld="$ld -X $p.Commit=$commit -X $p.BuiltAt=$built" ;;
  esac
  ( cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath ${tags:+-tags "$tags"} \
      -ldflags "$ld" -o "$outf" "$pkg" ) || tn_die "build failed: $repo $pkg (tags=${tags:-none})"
  printf '  %-16s %-8s tags=%s\n' "$(basename "$outf")" "$commit" "${tags:-<default>}" >&2
}

# go_test_bin REPO PKG OUT ARCH — the test binary of a package (joint-a2a.sh runs one test of it by name).
go_test_bin(){
  local repo=$1 pkg=$2 outf=$3 arch=$4
  ( cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -trimpath -o "$outf" "$pkg" ) \
    || tn_die "test binary build failed: $repo $pkg"
  printf '  %-16s %-8s tags=%s\n' "$(basename "$outf")" "$(repo_commit "$repo")" "<test>" >&2
}

repo_commit(){
  local c
  c=$(git -C "$1" rev-parse --short HEAD 2>/dev/null || echo unknown)
  git -C "$1" diff --quiet 2>/dev/null && git -C "$1" diff --cached --quiet 2>/dev/null || c="$c-dirty"
  printf '%s' "$c"
}

write_manifest(){
  local dir=$1 arch=$2 tags=$3 hub_tags=$4 variants=$5; shift 5
  {
    echo "built_at  $(date -u +%Y-%m-%dT%H:%M:%SZ) on $(hostname)"
    echo "target    linux/$arch  $(go version | awk '{print $3}')"
    echo "tags      anet=${tags:-<default>} hub=${hub_tags:-<default>} variants=${variants:-<none>}"
    local r
    for r in "$@"; do printf 'repo      %-8s %s\n' "$(basename "$r")" "$(repo_commit "$r")"; done
    echo "webui     embedded copy from the ANetHub commit above (not rebuilt)"
    echo "run_id    ${TESTNET_RUN_ID}"
  } > "$dir/MANIFEST"
}

main "$@"
