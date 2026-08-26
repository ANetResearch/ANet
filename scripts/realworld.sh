#!/usr/bin/env bash
# realworld.sh — run ANetLink's L1 tier against real services, on a
# machine that is not this one.
#
# The tier brings up real mosquitto, a third-party Modbus server, Home
# Assistant, OPC UA and a Matter device in Docker, and it exercises seven
# adapters against wire formats nobody here wrote. It had never run: two
# of those adapters had therefore never been driven at all.
#
# It runs remotely because the images do not belong on this machine and
# because the host needs Docker Hub. dmax reaches a mirror for official
# images only, so anything namespaced is pulled on the relay and streamed
# over — the relay keeps nothing, it is at 93% disk.
#
#   bash scripts/realworld.sh            bring up, run, report
#   KEEP=1 bash scripts/realworld.sh     leave the services running
set -uo pipefail
cd "$(dirname "$0")/.."
ROOT=$PWD/..
RELAY=${RELAY:-root@cmax.chatchat.space}
HOST=${HOST:-root@dmax.chatchat.space}
REMOTE=${REMOTE:-/data/rw}
KEEP=${KEEP:-0}

pass=0; fail=0
ok(){   printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; pass=$((pass+1)); }
no(){   printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; fail=$((fail+1)); }
info(){ printf '    %s\n' "$*"; }
hd(){   printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }
via(){  ssh -o ConnectTimeout=25 "$RELAY" "ssh -o ConnectTimeout=25 $HOST '$1'" 2>&1; }

hd "1  送源码"
# ANetCore travels with it and is wired in by a replace directive, so the
# tier runs against the same source the rest of the suite does rather than
# a published version that may be behind.
tar czf /tmp/rw-link.tgz -C "$ROOT" --exclude='.git' --exclude='Refs' \
  --exclude='Repos' --exclude='ANetCore' ANetLink 2>/dev/null
tar czf /tmp/rw-core.tgz -C "$ROOT" --exclude='.git' ANetCore 2>/dev/null
rsync -z /tmp/rw-link.tgz /tmp/rw-core.tgz "$RELAY:/root/ship/" >/dev/null 2>&1 \
  && ok "源码已到中转机" || { no "送到中转机失败"; exit 1; }
ssh "$RELAY" "rsync -z /root/ship/rw-link.tgz /root/ship/rw-core.tgz $HOST:/root/ship/" >/dev/null 2>&1 \
  && ok "源码已到运行机" || { no "送到运行机失败"; exit 1; }

# The replace directive is appended by a here-doc on the far side rather
# than by a quoted grep: this string crosses two ssh hops, and a pattern
# with a slash and a caret does not survive them intact — the first
# attempt reported a grep failure that was quoting, not the repository.
cat > /tmp/rw-unpack.sh <<'UNPACK'
#!/usr/bin/env bash
set -eu
mkdir -p REMOTE_DIR && cd REMOTE_DIR
rm -rf ANetLink ANetCore
tar xzf /root/ship/rw-link.tgz
tar xzf /root/ship/rw-core.tgz
cd ANetLink
if ! grep -q "replace github.com/ANetResearch/ANetCore" go.mod; then
  printf 'replace github.com/ANetResearch/ANetCore => ../ANetCore\n' >> go.mod
fi
echo done
UNPACK
sed -i "s|REMOTE_DIR|$REMOTE|g" /tmp/rw-unpack.sh
rsync -z /tmp/rw-unpack.sh "$RELAY:/root/ship/" >/dev/null 2>&1
ssh "$RELAY" "rsync -z /root/ship/rw-unpack.sh $HOST:/root/ship/" >/dev/null 2>&1
out=$(via "bash /root/ship/rw-unpack.sh")
case "$out" in *done*) ok "源码已展开";; *) no "展开失败: ${out:0:120}";; esac

hd "2  起真实服务"
# Restarted every time rather than reused. A Matter device advertises only
# while its pairing window is open and closes it a few minutes in, so a rig
# left from an earlier run has a device that is present and silent — which
# looks like a discovery fault and is not one.
out=$(via "cd $REMOTE/ANetLink && bash test/realworld/up.sh 2>&1 | tail -3")
case "$out" in
  *"L1 rig up"*) ok "L1 服务已起";;
  *) no "起服务失败: $(printf '%s' "$out" | tail -2 | tr '\n' ' ')";;
esac
cams=$(via "grep -c ONVIF_XADDR $REMOTE/ANetLink/test/realworld/env 2>/dev/null || echo 0")
case "$cams" in
  1*) ok "相机架也起来了(端口自动避让)";;
  *)  info "相机架未起 —— ONVIF/RTSP 层将跳过";;
esac

hd "3  跑"
out=$(via "cd $REMOTE/ANetLink && source test/realworld/env && \
  CGO_ENABLED=0 PATH=/usr/local/bin:\$PATH GOPROXY=https://goproxy.cn,direct \
  GOFLAGS=-mod=mod timeout 600 go test -tags realworld -run TestReal -v -timeout 540s \
  ./adapters/... ./internal/... ./matter/... 2>&1")
p=$(printf '%s' "$out" | grep -cE '^--- PASS')
s=$(printf '%s' "$out" | grep -cE '^--- SKIP')
f=$(printf '%s' "$out" | grep -cE '^--- FAIL')
info "PASS $p  SKIP $s  FAIL $f"
[ "$f" -eq 0 ] && ok "没有失败" || no "$f 项失败"
# A run where nothing passed is a run that proved nothing, however green.
[ "$p" -ge 3 ] && ok "$p 项对着真服务通过" \
  || no "只有 $p 项通过 —— 真服务大概没接上"
printf '%s' "$out" | grep -E '^--- (PASS|FAIL)' | sed 's/^/    /' | head -12
if [ "$f" -gt 0 ]; then
  printf '%s' "$out" | grep -B 2 '^--- FAIL' | sed 's/^/    /' | head -12
fi

hd "4  收尾"
if [ "$KEEP" = 1 ]; then
  info "KEEP=1:服务留着(matter 的配对窗口会自行关闭)"
else
  via "cd $REMOTE/ANetLink && bash test/realworld/down.sh >/dev/null 2>&1; echo done" >/dev/null
  ok "容器已停"
fi

printf '\n\033[1m── %d 通过, %d 失败 ──\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
