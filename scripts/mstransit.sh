#!/usr/bin/env bash
# mstransit.sh — 经 ModelScope 私有仓库中转文件,内容加密。
#
# 为什么存在:ink93 直连 emax 只有几十 KB/s,经 cmax 中转也会波动到几 KB/s,
# 而三台机器到 modelscope.cn 都是百毫秒级。所以大文件走 ModelScope,不走机器之间。
#
# 加密的边界写清楚:**明文不上传**。文件先用一次性随机口令做 AES-256 加密,
# 只有密文进 ModelScope;口令经 ssh 传给对端(那条通道本来就是加密的,而且口令
# 只有几十字节,慢链路对它无所谓)。所以 ModelScope 拿到的是它解不开的字节,
# 而密钥从不经过它。仓库本身也是私有的 —— 两层里任一层单独都不够。
#
#   push <本地文件> [远端对象名]      加密并上传,打印 “对象名 口令 sha256”
#   pull <对象名> <口令> <输出文件>   下载、解密、核对 sha256
#   fetch <对象名> <口令> <目标机> <远端路径>
#                                     让目标机自己去 ModelScope 取(推荐:
#                                     数据不经过本机,口令走 ssh)
#
# 令牌取自 $MODELSCOPE_TOKEN,或 Tokens/ANET_GH_TOKEN 里的 MODELSCOPE_TOKEN=。
set -euo pipefail

REPO=${MS_REPO:-inksong/anet-transit}
VENV=${MS_VENV:-/tmp/ms-venv}
TOKENS=${MS_TOKENS:-/data/projs/anet-oss/Tokens/ANET_GH_TOKEN}

token() {
  if [ -n "${MODELSCOPE_TOKEN:-}" ]; then printf '%s' "$MODELSCOPE_TOKEN"; return; fi
  grep '^MODELSCOPE_TOKEN=' "$TOKENS" | cut -d= -f2- | tr -d '\n'
}

py() { MODELSCOPE_API_TOKEN="$(token)" MS_REPO="$REPO" "$VENV/bin/python" "$@"; }

cmd_push() {
  local src=$1 name=${2:-$(basename "$1").$(date +%s).enc}
  [ -f "$src" ] || { echo "没有这个文件: $src" >&2; exit 1; }
  # 口令来自 /dev/urandom,一次性、不落盘、不进任何日志。
  local pass; pass=$(head -c 32 /dev/urandom | base64 -w0 | tr -d '=+/' | head -c 40)
  local sum; sum=$(sha256sum "$src" | cut -d' ' -f1)
  local tmp; tmp=$(mktemp /tmp/mstransit.XXXXXX)
  trap 'rm -f "$tmp"' RETURN
  # -pbkdf2 而非默认的单轮 MD5:后者对口令的保护弱到不值一提。
  openssl enc -aes-256-cbc -pbkdf2 -iter 200000 -salt -pass "pass:$pass" -in "$src" -out "$tmp"
  MSTRANSIT_FILE="$tmp" MSTRANSIT_NAME="$name" py - <<'PY'
import os
from modelscope.hub.api import HubApi
api = HubApi(); api.login(os.environ['MODELSCOPE_API_TOKEN'])
api.upload_file(
    path_or_fileobj=os.environ['MSTRANSIT_FILE'],
    path_in_repo=os.environ['MSTRANSIT_NAME'],
    repo_id=os.environ['MS_REPO'],
    commit_message='transit',
)
PY
  echo
  echo "对象名  $name"
  echo "口令    $pass"
  echo "sha256  $sum"
  echo
  echo "对端取回:"
  echo "  bash scripts/mstransit.sh pull '$name' '$pass' <输出文件>"
}

cmd_pull() {
  local name=$1 pass=$2 out=$3
  local tmp; tmp=$(mktemp /tmp/mstransit.XXXXXX)
  trap 'rm -f "$tmp"' RETURN
  MSTRANSIT_NAME="$name" MSTRANSIT_OUT="$tmp" py - <<'PY'
import os, shutil
from modelscope.hub.api import HubApi
from modelscope.hub.file_download import model_file_download
api = HubApi(); api.login(os.environ['MODELSCOPE_API_TOKEN'])
p = model_file_download(model_id=os.environ['MS_REPO'], file_path=os.environ['MSTRANSIT_NAME'])
shutil.copyfile(p, os.environ['MSTRANSIT_OUT'])
PY
  openssl enc -d -aes-256-cbc -pbkdf2 -iter 200000 -pass "pass:$pass" -in "$tmp" -out "$out"
  echo "已解密到 $out"
  echo "sha256  $(sha256sum "$out" | cut -d' ' -f1)"
}

# fetch:目标机自己去取。数据完全不经过本机,这是这个脚本存在的主要理由。
cmd_fetch() {
  local name=$1 pass=$2 host=$3 dest=$4
  ssh -o ConnectTimeout=20 "$host" "MODELSCOPE_TOKEN='$(token)' bash -s" <<REMOTE
set -euo pipefail
command -v python3 >/dev/null || { echo "对端没有 python3" >&2; exit 1; }
[ -d /opt/ms-venv ] || { python3 -m venv /opt/ms-venv && /opt/ms-venv/bin/pip -q install -U pip modelscope; }
tmp=\$(mktemp)
MODELSCOPE_API_TOKEN="\$MODELSCOPE_TOKEN" MS_REPO='$REPO' MSTRANSIT_NAME='$name' MSTRANSIT_OUT="\$tmp" /opt/ms-venv/bin/python - <<'PY'
import os, shutil
from modelscope.hub.api import HubApi
from modelscope.hub.file_download import model_file_download
api = HubApi(); api.login(os.environ['MODELSCOPE_API_TOKEN'])
p = model_file_download(model_id=os.environ['MS_REPO'], file_path=os.environ['MSTRANSIT_NAME'])
shutil.copyfile(p, os.environ['MSTRANSIT_OUT'])
PY
openssl enc -d -aes-256-cbc -pbkdf2 -iter 200000 -pass 'pass:$pass' -in "\$tmp" -out '$dest'
rm -f "\$tmp"
echo "落地 \$(stat -c%s '$dest') 字节  sha256 \$(sha256sum '$dest' | cut -d' ' -f1)"
REMOTE
}

case "${1:-}" in
  push)  shift; cmd_push "$@" ;;
  pull)  shift; cmd_pull "$@" ;;
  fetch) shift; cmd_fetch "$@" ;;
  *) sed -n '2,26p' "$0"; exit 2 ;;
esac
