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
#   push <本地文件> [远端对象名]      加密并上传,打印 “对象名 口令 明文sha256 密文sha256”
#   pull <对象名> <口令> <密文sha256> <输出文件>
#                                     下载、**先核对密文**、再解密
#   fetch <对象名> <口令> <密文sha256> <目标机> <远端路径>
#                                     让目标机自己去 ModelScope 取(推荐:
#                                     数据不经过本机,口令走 ssh)
#   prep <目标机>                     一次性:在目标机装好 SDK。
#                                     第一次 fetch 会现装,几分钟,常常撞上调用方的
#                                     超时并留下半截状态;先 prep 一次就不会。
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

# verify_cipher 在解密之前把密文与已知摘要比对。
#
# 存在的理由:下载工具的断点续传拼出过一个大小正确、内容错误的文件,而 openssl
# 对它只会报一句 "bad magic number" —— 那句话既不说明是传输坏了,也不说明是口令
# 错了。密文的摘要能把这两种情况分开。
verify_cipher() {
  local file=$1 want=$2
  [ -n "$want" ] || { echo "缺少密文 sha256,拒绝解密" >&2; return 1; }
  local got; got=$(sha256sum "$file" | cut -d' ' -f1)
  [ "$got" = "$want" ] && return 0
  echo "密文校验不符:期望 $want,实得 $got(传输损坏,不是口令问题)" >&2
  return 1
}

cmd_push() {
  local src=$1 name=${2:-$(basename "$1").$(date +%s).enc}
  [ -f "$src" ] || { echo "没有这个文件: $src" >&2; exit 1; }
  # 口令来自 /dev/urandom,一次性、不落盘、不进任何日志。
  local pass; pass=$(head -c 32 /dev/urandom | base64 -w0 | tr -d '=+/' | head -c 40)
  local sum; sum=$(sha256sum "$src" | cut -d' ' -f1)
  local csum
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
  csum=$(sha256sum "$tmp" | cut -d' ' -f1)
  echo
  echo "对象名    $name"
  echo "口令      $pass"
  echo "明文sha   $sum"
  echo "密文sha   $csum"
  echo
  echo "对端取回:"
  echo "  bash scripts/mstransit.sh fetch '$name' '$pass' '$csum' <目标机> <远端路径>"
}

cmd_pull() {
  local name=$1 pass=$2 csum=$3 out=$4
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
  verify_cipher "$tmp" "$csum" || return 1
  openssl enc -d -aes-256-cbc -pbkdf2 -iter 200000 -pass "pass:$pass" -in "$tmp" -out "$out"
  echo "已解密到 $out"
  echo "sha256  $(sha256sum "$out" | cut -d' ' -f1)"
}

# prep:把 SDK 装到目标机。一次性,与传输解耦。
#
# 分开是因为第一次 fetch 会在 ssh 里现装 venv 与 modelscope,耗时几分钟;调用方
# 的超时先到,连接被切断,远端留下半截 venv,下一次还得重来。装和传是两件事。
cmd_prep() {
  local host=$1
  ssh -o ConnectTimeout=20 "$host" 'bash -s' <<'REMOTE'
set -euo pipefail
command -v python3 >/dev/null || { echo "没有 python3" >&2; exit 1; }
if /opt/ms-venv/bin/python -c 'import modelscope' 2>/dev/null; then
  echo "已就绪: $(/opt/ms-venv/bin/python -c 'import modelscope;print(modelscope.__version__)')"
  exit 0
fi
rm -rf /opt/ms-venv
python3 -m venv /opt/ms-venv
/opt/ms-venv/bin/pip -q install -U pip >/dev/null
/opt/ms-venv/bin/pip -q install modelscope >/dev/null
echo "已装 $(/opt/ms-venv/bin/python -c 'import modelscope;print(modelscope.__version__)')"
REMOTE
}

# fetch:目标机自己去取。数据完全不经过本机,这是这个脚本存在的主要理由。
cmd_fetch() {
  local name=$1 pass=$2 csum=$3 host=$4 dest=$5
  ssh -o ConnectTimeout=20 "$host" "MODELSCOPE_TOKEN='$(token)' bash -s" <<REMOTE
set -euo pipefail
/opt/ms-venv/bin/python -c 'import modelscope' 2>/dev/null || {
  echo "对端未就绪,先跑一次:bash scripts/mstransit.sh prep <目标机>" >&2; exit 1; }
tmp=\$(mktemp)
# 最多三轮:密文对不上就清掉 SDK 缓存里那一份再下。
# SDK 的断点续传能拼出一个**大小正确、内容错误**的文件(实测),所以判据只能是
# 密文的 sha256,不能是大小,也不能是下载工具的退出码。
for try in 1 2 3; do
  MODELSCOPE_API_TOKEN="\$MODELSCOPE_TOKEN" MS_REPO='$REPO' MSTRANSIT_NAME='$name' MSTRANSIT_OUT="\$tmp" /opt/ms-venv/bin/python - <<'PY'
import os, shutil
from modelscope.hub.api import HubApi
from modelscope.hub.file_download import model_file_download
api = HubApi(); api.login(os.environ['MODELSCOPE_API_TOKEN'])
p = model_file_download(model_id=os.environ['MS_REPO'], file_path=os.environ['MSTRANSIT_NAME'])
shutil.copyfile(p, os.environ['MSTRANSIT_OUT'])
PY
  got=\$(sha256sum "\$tmp" | cut -d' ' -f1)
  [ "\$got" = '$csum' ] && break
  echo "第 \$try 轮密文校验不符(得 \$got),清缓存重下" >&2
  find /root/.cache/modelscope -name '$name' -delete 2>/dev/null || true
done
if [ "\$got" != '$csum' ]; then
  echo "密文三轮都对不上,拒绝解密。期望 $csum 实得 \$got" >&2
  rm -f "\$tmp"; exit 1
fi
openssl enc -d -aes-256-cbc -pbkdf2 -iter 200000 -pass 'pass:$pass' -in "\$tmp" -out '$dest'
rm -f "\$tmp"
echo "落地 \$(stat -c%s '$dest') 字节  sha256 \$(sha256sum '$dest' | cut -d' ' -f1)"
REMOTE
}

case "${1:-}" in
  push)  shift; cmd_push "$@" ;;
  pull)  shift; cmd_pull "$@" ;;
  fetch) shift; cmd_fetch "$@" ;;
  prep)  shift; cmd_prep "$@" ;;
  *) sed -n '2,30p' "$0"; exit 2 ;;
esac
