#!/usr/bin/env bash
# a2a-tck.sh — 用 a2a-tck 对本机 A2A 接口(module/a2a)跑一遍,把结果写进指定目录。
#
# A2A-DESIGN §17 联调行:"a2a-tck 结果记录(不作门禁)"。本脚本只负责"跑并记录":
# 建 venv、装 TCK 依赖、快照 TCK 源码、探测接口、跑 TCK、生成摘要。TCK 用例失败不使本脚本失败
# (除非 --strict);只有环境与接口本身不可用(卡片取不到、venv 建不起来)才以非零退出。
# 运行说明、适用/不适用分组与结果记录格式见 docs/notes/0019-互通-a2a-tck运行说明.md。
#
# 被测对象(SUT)是"本机接口上的一个远端 agent":
#   卡片      http://<a2a_addr>/a2a/v1/agents/<aid>/.well-known/agent-card.json   (需 Bearer)
#   JSON-RPC  http://<a2a_addr>/a2a/v1/agents/<aid>/jsonrpc
#   REST      http://<a2a_addr>/a2a/v1/agents/<aid>/rest/...
# TCK 从 <sut-host>/.well-known/agent-card.json 取卡片,所以 --sut-host 就是 .../agents/<aid>。
# TCK 没有鉴权选项:scripts/a2a-tck-plugin.py 作为 pytest 插件给发往该接口的请求补 Bearer
# (令牌只从文件读,不进命令行与环境变量)。
#
# 用法:
#   scripts/a2a-tck.sh --aid <远端AID> [选项] [-- 额外 pytest 参数]
#
#   --aid AID            远端 agent 的 AID(必填;须是本机已知的远端,见 GET /a2a/v1/agents)
#   --data-dir DIR       本机 daemon 的数据目录(缺省 $ANET_DATA_DIR,再缺省 ~/.anet);
#                        从 DIR/modules/a2a/a2a_addr.txt 与 a2a_token.txt 取地址与令牌
#   --addr HOST:PORT     本机 A2A 接口地址(覆盖 a2a_addr.txt)
#   --token-file FILE    A2A 令牌文件(覆盖 a2a_token.txt;不接受把令牌直接写在命令行上)
#   --out DIR            结果目录(缺省 ${TMPDIR:-/tmp}/anet-a2a-tck/<UTC时间>);须为空或不存在
#   --tck-dir DIR        a2a-tck 源码(缺省 $A2A_TCK_DIR,再缺省 <ANet>/../Refs/a2a-tck)
#   --clone              --tck-dir 不存在时从 https://github.com/a2aproject/a2a-tck 浅克隆
#   --venv DIR           Python 虚拟环境(缺省 $A2A_TCK_VENV,再缺省 <ANet>/../.venv-tck);不存在则新建
#   --no-install         不安装/检查依赖(venv 须已就绪)
#   --transport LIST     jsonrpc,http_json(缺省,也是 anet 提供的全部绑定)的子集
#   --level L            只跑 must | should | may
#   --scope S            all(缺省)| protocol:protocol 另跳过需要对端真实作答的用例
#   --jsonrpc-slash-fix M auto(缺省)| on | off:TCK 的 JSON-RPC 客户端会 POST 到 .../jsonrpc/;
#                        auto = 预检发现接口不接受带斜杠的路径时才在客户端改写,并记入 run.json
#   --timeout SECS       整个 TCK 运行的墙钟上限(缺省 3600;需要 coreutils timeout)
#   --label TEXT         写进记录的标签(例如被测 daemon 的版本或场景名)
#   --strict             以 pytest 的退出码退出(缺省:TCK 跑完即退出 0,结果只作记录)
#   --keep-src           保留结果目录中的 TCK 源码快照(缺省跑完删除)
#   --list-groups        打印适用/不适用分组清单后退出(不需要 SUT)
#   -h, --help           本说明
#
# 例:
#   scripts/a2a-tck.sh --aid "$PEER_AID" --data-dir /tmp/joint-a2a/req --out /tmp/tck-run1
#   scripts/a2a-tck.sh --aid "$PEER_AID" --scope protocol --level must -- -k "error"
#
# 前提与副作用(详见 0019):
#   * daemon 在跑且 module/a2a 已启用;对端 agent 允许本机(peers.allow),否则任务得到 rejected。
#   * 每条非错误路径的 TCK 消息都是一次真实委派,经 hub 发往对端。不要对生产或付费 agent 运行。
#   * 结果目录:run.json、summary.md、console.log、preflight.json、agent-card.json、anet-skips.json、
#     reports/{compatibility.json,compatibility.html,tck_report.html,junitreport.xml}、pip-freeze.txt。
set -euo pipefail
export NO_PROXY="127.0.0.1,localhost,::1${NO_PROXY:+,$NO_PROXY}"
export no_proxy="$NO_PROXY"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
PLUGIN_SRC="$HERE/a2a-tck-plugin.py"

info(){ printf '\033[1;36m%s\033[0m\n' "$*"; }
warn(){ printf '\033[1;33m%s\033[0m\n' "$*" >&2; }
die(){  printf '\033[1;31m%s\033[0m\n' "$*" >&2; exit 2; }
usage(){ sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'; }

AID=""; DATA_DIR=""; ADDR=""; TOKEN_FILE=""; OUT=""
TCK_DIR="${A2A_TCK_DIR:-$ROOT/../Refs/a2a-tck}"
VENV="${A2A_TCK_VENV:-$ROOT/../.venv-tck}"
CLONE=0; INSTALL=1; TRANSPORT="jsonrpc,http_json"; LEVEL=""; SCOPE="all"
SLASH_FIX="auto"; TIMEOUT=3600; LABEL=""; STRICT=0; KEEP_SRC=0
EXTRA=()

need_arg(){ [ $# -ge 2 ] && [ -n "$2" ] || die "$1 需要一个参数"; }
while [ $# -gt 0 ]; do
  case "$1" in
    --aid)               need_arg "$@"; AID=$2; shift 2 ;;
    --data-dir)          need_arg "$@"; DATA_DIR=$2; shift 2 ;;
    --addr)              need_arg "$@"; ADDR=$2; shift 2 ;;
    --token-file)        need_arg "$@"; TOKEN_FILE=$2; shift 2 ;;
    --token)             die "不接受在命令行上给令牌(会留在 ps 与 shell 历史里);用 --token-file" ;;
    --out)               need_arg "$@"; OUT=$2; shift 2 ;;
    --tck-dir)           need_arg "$@"; TCK_DIR=$2; shift 2 ;;
    --clone)             CLONE=1; shift ;;
    --venv)              need_arg "$@"; VENV=$2; shift 2 ;;
    --no-install)        INSTALL=0; shift ;;
    --transport)         need_arg "$@"; TRANSPORT=$2; shift 2 ;;
    --level)             need_arg "$@"; LEVEL=$2; shift 2 ;;
    --scope)             need_arg "$@"; SCOPE=$2; shift 2 ;;
    --jsonrpc-slash-fix) need_arg "$@"; SLASH_FIX=$2; shift 2 ;;
    --timeout)           need_arg "$@"; TIMEOUT=$2; shift 2 ;;
    --label)             need_arg "$@"; LABEL=$2; shift 2 ;;
    --strict)            STRICT=1; shift ;;
    --keep-src)          KEEP_SRC=1; shift ;;
    --list-groups)       exec python3 "$PLUGIN_SRC" groups ;;
    -h|--help)           usage; exit 0 ;;
    --)                  shift; EXTRA=("$@"); break ;;
    *)                   die "未知参数:$1(见 --help)" ;;
  esac
done

# ---------------------------------------------------------------- 参数校验
[ -n "$AID" ] || die "缺 --aid(远端 agent 的 AID;本机已知的远端见 GET http://<a2a_addr>/a2a/v1/agents)"
[[ "$AID" =~ ^[A-Za-z0-9._:-]+$ ]] || die "--aid 含有不能直接放进 URL 路径的字符:$AID"
IFS=, read -r -a _ts <<<"$TRANSPORT"
[ "${#_ts[@]}" -gt 0 ] || die "--transport 为空"
for t in "${_ts[@]}"; do
  case "$t" in
    jsonrpc|http_json) ;;
    grpc) die "anet 本机接口不提供 gRPC 绑定(A2A-DESIGN §11.1),--transport 只能取 jsonrpc,http_json 的子集" ;;
    *) die "未知传输:$t" ;;
  esac
done
case "$LEVEL" in ""|must|should|may) ;; *) die "--level 只能是 must|should|may" ;; esac
case "$SCOPE" in all|protocol) ;; *) die "--scope 只能是 all|protocol" ;; esac
case "$SLASH_FIX" in auto|on|off) ;; *) die "--jsonrpc-slash-fix 只能是 auto|on|off" ;; esac
[[ "$TIMEOUT" =~ ^[0-9]+$ ]] || die "--timeout 须为秒数"

DATA_DIR="${DATA_DIR:-${ANET_DATA_DIR:-$HOME/.anet}}"
STATE="$DATA_DIR/modules/a2a"
if [ -z "$ADDR" ]; then
  [ -r "$STATE/a2a_addr.txt" ] || die "取不到本机 A2A 地址:$STATE/a2a_addr.txt 不存在(daemon 没启动过 module/a2a?),或用 --addr"
  ADDR="$(tr -d '[:space:]' <"$STATE/a2a_addr.txt")"
fi
[[ "$ADDR" =~ ^[^/[:space:]]+:[0-9]+$ ]] || die "--addr 须为 HOST:PORT:$ADDR"
TOKEN_FILE="${TOKEN_FILE:-$STATE/a2a_token.txt}"
[ -r "$TOKEN_FILE" ] || die "读不到 A2A 令牌文件:$TOKEN_FILE"
TOKEN_FILE="$(cd "$(dirname "$TOKEN_FILE")" && pwd)/$(basename "$TOKEN_FILE")"
perm="$(stat -c '%a' "$TOKEN_FILE" 2>/dev/null || stat -f '%Lp' "$TOKEN_FILE" 2>/dev/null || echo '?')"
case "$perm" in 600|400) ;; *) warn "提示:$TOKEN_FILE 的权限是 $perm(daemon 写的是 0600)" ;; esac

SUT="http://$ADDR/a2a/v1/agents/$AID"

# ---------------------------------------------------------------- TCK 源码
if [ ! -d "$TCK_DIR" ]; then
  [ "$CLONE" = 1 ] || die "a2a-tck 不在 $TCK_DIR;用 --tck-dir 指定,或加 --clone 浅克隆"
  info "克隆 a2a-tck → $TCK_DIR"
  git clone --depth 1 https://github.com/a2aproject/a2a-tck "$TCK_DIR"
fi
TCK_DIR="$(cd "$TCK_DIR" && pwd)"
for f in pyproject.toml tck tests/compatibility/conftest.py specification; do
  [ -e "$TCK_DIR/$f" ] || die "$TCK_DIR 不像 a2a-tck 源码(缺 $f)"
done
TCK_COMMIT="$(git -C "$TCK_DIR" rev-parse HEAD 2>/dev/null || echo unknown)"
if [ -n "$(git -C "$TCK_DIR" status --porcelain 2>/dev/null || true)" ]; then TCK_COMMIT="$TCK_COMMIT+dirty"; fi
ANET_COMMIT="$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"

# ---------------------------------------------------------------- 结果目录
if [ -z "$OUT" ]; then OUT="${TMPDIR:-/tmp}/anet-a2a-tck/$(date -u +%Y%m%dT%H%M%SZ)"; fi
if [ -e "$OUT" ] && [ -n "$(ls -A "$OUT" 2>/dev/null)" ]; then die "结果目录非空:$OUT(每次运行用一个新目录)"; fi
mkdir -p "$OUT/reports"
OUT="$(cd "$OUT" && pwd)"

# ---------------------------------------------------------------- venv 与依赖
# 只往 venv 里装:系统 pip 只以 --python <venv> 的方式用来给没有 ensurepip 的 venv 引导 pip。
PY="$VENV/bin/python"
pick_python(){
  local c
  for c in python3.13 python3.12 python3.11 python3; do
    if command -v "$c" >/dev/null 2>&1 && "$c" -c 'import sys; sys.exit(0 if sys.version_info >= (3, 11) else 1)'; then
      command -v "$c"; return 0
    fi
  done
  return 1
}
make_venv(){
  local base; base="$(pick_python)" || die "需要 Python 3.11+(a2a-tck 的要求)"
  info "新建 venv:$VENV"
  if command -v uv >/dev/null 2>&1; then
    uv venv --python "$base" "$VENV" >/dev/null
    return
  fi
  if "$base" -m venv "$VENV" 2>/dev/null; then return; fi
  # Debian/Ubuntu 未装 python3-venv 时没有 ensurepip:建无 pip 的 venv,再用系统 pip 只往它里面装 pip。
  rm -rf "$VENV"
  "$base" -m venv --without-pip "$VENV"
  "$base" -m pip --python "$VENV/bin/python" install -q pip \
    || die "无法给 $VENV 引导 pip(需要 pip ≥ 22.3 或 uv)"
}
if [ ! -x "$PY" ]; then
  [ "$INSTALL" = 1 ] || die "$VENV 没有 venv,且给了 --no-install"
  make_venv
fi
"$PY" -c 'import sys; sys.exit(0 if sys.version_info >= (3, 11) else 1)' || die "$PY 低于 Python 3.11"
REQS="$OUT/tck-requirements.txt"
"$PY" - "$TCK_DIR/pyproject.toml" >"$REQS" <<'EOF'
import sys, tomllib
with open(sys.argv[1], "rb") as f:
    print("\n".join(tomllib.load(f)["project"]["dependencies"]))
EOF
if [ "$INSTALL" = 1 ]; then
  info "安装 a2a-tck 依赖到 $VENV"
  if "$PY" -m pip --version >/dev/null 2>&1; then
    "$PY" -m pip install -q --disable-pip-version-check -r "$REQS"
  elif command -v uv >/dev/null 2>&1; then
    uv pip install --python "$PY" -q -r "$REQS"
  else
    die "$VENV 里没有 pip,系统里也没有 uv"
  fi
fi
"$PY" -c 'import httpx, pytest, pytest_html, jsonschema, grpc, google.protobuf, google.rpc' 2>/dev/null \
  || die "$VENV 缺 TCK 依赖(去掉 --no-install 重跑)"
{ "$PY" -m pip freeze 2>/dev/null || uv pip freeze --python "$PY" 2>/dev/null || true; } >"$OUT/pip-freeze.txt"

# ---------------------------------------------------------------- TCK 快照
# 在结果目录里的快照上跑:不往共享的 TCK 检出里写 reports/、.pytest_cache 或 __pycache__。
SRC="$OUT/tck-src"
mkdir -p "$SRC"
cp -R "$TCK_DIR/pyproject.toml" "$TCK_DIR/tck" "$TCK_DIR/tests" "$TCK_DIR/specification" "$SRC/"
cp "$PLUGIN_SRC" "$SRC/anet_tck_plugin.py"
cleanup(){ if [ "$KEEP_SRC" = 0 ] && [ -n "${SRC:-}" ] && [ -d "$SRC" ]; then rm -rf "$SRC"; fi; }
trap cleanup EXIT

export PYTHONDONTWRITEBYTECODE=1
export ANET_TCK_SUT="$SUT"
export ANET_TCK_TOKEN_FILE="$TOKEN_FILE"
export ANET_TCK_SCOPE="$SCOPE"
export ANET_TCK_SKIPS_OUT="$OUT/anet-skips.json"

# ---------------------------------------------------------------- 预检
info "预检 $SUT"
set +e
PRE_OUT="$(cd "$SRC" && "$PY" anet_tck_plugin.py preflight "$OUT")"; PRE_RC=$?
set -e
printf '%s\n' "$PRE_OUT" | grep -v '^JSONRPC_SLASH=' || true
[ "$PRE_RC" = 0 ] || die "预检失败(见上与 $OUT/preflight.json);没有运行 TCK"
SLASH_STATE="$(printf '%s\n' "$PRE_OUT" | sed -n 's/^JSONRPC_SLASH=//p')"
case "$SLASH_FIX" in
  on)   APPLY_SLASH=1 ;;
  off)  APPLY_SLASH=0 ;;
  auto) if [ "$SLASH_STATE" = broken ]; then APPLY_SLASH=1; else APPLY_SLASH=0; fi ;;
esac
if [ "$APPLY_SLASH" = 1 ]; then
  export ANET_TCK_JSONRPC_SLASH_FIX=1
  warn "JSON-RPC:接口不接受 .../jsonrpc/(TCK 客户端会发到这里),本次在客户端改写为 .../jsonrpc;已记入 run.json"
fi

# ---------------------------------------------------------------- 运行
info "不适用分组(跳过,记 SKIPPED):"
"$PY" "$SRC/anet_tck_plugin.py" groups | sed 's/^/  /'
CMD=("$PY" -m pytest tests/compatibility/
  "--sut-host=$SUT" "--transport=$TRANSPORT" --tb=short -v
  -p anet_tck_plugin -p no:cacheprovider
  "--compatibility-report=$OUT/reports/compatibility"
  "--html=$OUT/reports/tck_report.html" --self-contained-html
  "--junitxml=$OUT/reports/junitreport.xml")
[ -n "$LEVEL" ] && CMD+=(-m "$LEVEL")
CMD+=(${EXTRA[@]+"${EXTRA[@]}"})
TO=()
if command -v timeout >/dev/null 2>&1 && [ "$TIMEOUT" -gt 0 ]; then TO=(timeout --signal=INT --kill-after=30 "$TIMEOUT"); fi

info "运行 a2a-tck($TCK_COMMIT)→ $OUT"
set +e
( cd "$SRC" && ${TO[@]+"${TO[@]}"} "${CMD[@]}" ) 2>&1 | tee "$OUT/console.log"
PYTEST_RC=${PIPESTATUS[0]}
set -e

# ---------------------------------------------------------------- 记录
"$PY" "$SRC/anet_tck_plugin.py" summarize "$OUT" \
  "date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" "label=$LABEL" "anet_commit=$ANET_COMMIT" \
  "tck_commit=$TCK_COMMIT" "sut=$SUT" "transport=$TRANSPORT" "level=${LEVEL:-all}" "scope=$SCOPE" \
  "jsonrpc_slash_fix=$([ "$APPLY_SLASH" = 1 ] && echo applied || echo "not-applied($SLASH_STATE)")" \
  "pytest_exit=$PYTEST_RC"
info "结果:$OUT/summary.md(机读:$OUT/run.json、$OUT/reports/compatibility.json)"

if [ ! -s "$OUT/reports/compatibility.json" ]; then
  warn "TCK 没有产出 compatibility.json(pytest 退出码 $PYTEST_RC,见 $OUT/console.log)"
  exit 2
fi
if [ "$STRICT" = 1 ]; then exit "$PYTEST_RC"; fi
exit 0
