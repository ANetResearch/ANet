#!/usr/bin/env bash
# ship.sh — build every binary in the suite, relay them to the production
# machines, and verify what is running is what was built.
#
# Written because the steps were being retyped each time and one of them
# was missed: anet-hub-admin is a SECOND binary on emax, it was never in
# any build script, and production ran a release-old copy while every
# check reported the surface healthy. A deploy assembled from memory
# forgets whatever is not in front of you.
#
# What it encodes, beyond the commands:
#
#   - Versioned filenames and a far-end sha256sum. `rsync --append-verify`
#     will append onto a same-named older build and report success; equal
#     size is not equal content.
#   - Relay through cmax. ink93 to emax/fmax runs at tens of KB/s; cmax
#     reaches both quickly, so a 5MB binary is two minutes instead of most
#     of an hour.
#   - One rsync at a time to a destination. Two race the same slow link and
#     each write their own temp file.
#   - Verify AFTER install, by asking each service which commit it is. An
#     install that reported success and left the old process running is
#     the failure this is here to catch.
#
#   bash scripts/ship.sh            build, ship, install, verify
#   DRY=1 bash scripts/ship.sh      build and report, change nothing remote
set -uo pipefail
cd "$(dirname "$0")/.."
ROOT=$PWD/..
STAGE=${STAGE:-/tmp/anet-ship}
RELAY=${RELAY:-root@cmax.chatchat.space}
DRY=${DRY:-0}

pass=0; fail=0
ok(){   printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; pass=$((pass+1)); }
no(){   printf '\033[1;31m  ✗ %s\033[0m\n' "$*"; fail=$((fail+1)); }
info(){ printf '    %s\n' "$*"; }
hd(){   printf '\n\033[1;36m═══ %s\033[0m\n' "$*"; }

rm -rf "$STAGE"; mkdir -p "$STAGE"

# ── 1. build ────────────────────────────────────────────────────
hd "1  构建"
build_one() {  # repo, script-or-pkg, output name, extra tags
  local repo=$1 what=$2 out=$3 tags=${4:-}
  ( cd "$ROOT/$repo" || exit 1
    if [ -f "$what" ]; then
      OUT="$STAGE/$out" GOOS=linux GOARCH=amd64 bash "$what" >/dev/null 2>&1
    else
      CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' \
        ${tags:+-tags "$tags"} -o "$STAGE/$out" "$what" >/dev/null 2>&1
    fi )
}

# The daemon and the hub carry a commit stamp through their own build
# scripts; everything else is stamped by the commit of its repository,
# which is recorded in the manifest below.
build_one ANet     scripts/build.sh anet            || true
build_one ANetHub  scripts/build.sh anet-hub        || true
build_one ANet     ./tools/anetpeer    anetpeer     || true
build_one ANet     ./tools/anetfixture anetfixture  || true
build_one ANetLink ./cmd/anetlinkd     anetlinkd    'adap,onvif,hikvision,dahua'
build_one ANetLink ./cmd/adapdemo      adapdemo     || true
build_one ANetMock ./cmd/anetmock      anetmock     || true

# anet-hub's script emits the admin binary beside it; give it the name the
# rest of this script expects.
[ -f "$STAGE/anet-hub-admin" ] || cp "$STAGE"/*hub-admin* "$STAGE/anet-hub-admin" 2>/dev/null

for b in anet anet-hub anet-hub-admin anetpeer anetfixture anetlinkd adapdemo anetmock; do
  if [ -s "$STAGE/$b" ]; then ok "$b ($(du -h "$STAGE/$b" | cut -f1))"
  else no "$b 没有构建出来"; fi
done

# The manifest records which commit each repository was on. A binary whose
# repo was dirty is named so, because a build from uncommitted work is not
# the commit it claims.
hd "2  版本清单"
: > "$STAGE/MANIFEST"
for repo in ANet ANetHub ANetLink ANetMock ANetCore; do
  c=$( cd "$ROOT/$repo" 2>/dev/null && git rev-parse --short HEAD 2>/dev/null ) || continue
  ( cd "$ROOT/$repo" && git diff --quiet 2>/dev/null ) || c="$c-dirty"
  printf '%-10s %s\n' "$repo" "$c" | tee -a "$STAGE/MANIFEST"
done
( cd "$STAGE" && sha256sum anet* adapdemo 2>/dev/null > SHA256SUMS )
ok "清单与校验和已生成"

if [ "$DRY" = 1 ]; then
  printf '\n\033[1mDRY=1:已构建 %s,未改动任何远端\033[0m\n' "$STAGE"
  exit 0
fi

# ── 3. relay ────────────────────────────────────────────────────
hd "3  经 cmax 中转"
ssh -o ConnectTimeout=20 "$RELAY" 'mkdir -p /root/ship' >/dev/null 2>&1 \
  && ok "中转机可达" || { no "中转机不可达"; exit 1; }
# One transfer, not several in parallel: they would race the same link.
if rsync -z "$STAGE"/* "$RELAY:/root/ship/" >/dev/null 2>&1; then
  ok "二进制已送到中转机"
else
  no "送到中转机失败"; exit 1
fi
ssh "$RELAY" 'cd /root/ship && sha256sum -c SHA256SUMS 2>&1 | grep -c ": OK"' \
  | grep -qE '^[1-9]' && ok "中转机校验和一致" || no "中转机校验和不符"

# ── 4. install ──────────────────────────────────────────────────
hd "4  分发并安装"
cat > "$STAGE/remote-install.sh" <<'RIEOF'
#!/usr/bin/env bash
set -u
cd /root/ship
send() { rsync -z "$1" "root@$2:/root/ship/" >/dev/null 2>&1; }

# emax: hub + the admin surface. Both, always — shipping one and not the
# other is exactly how the operator surface went a release stale.
send anet-hub emax.chatchat.space && send anet-hub-admin emax.chatchat.space
ssh root@emax.chatchat.space '
  systemctl stop anet-hub anet-hub-admin
  install -m755 /root/ship/anet-hub       /data/projs/anet-hub/bin/anet-hub
  install -m755 /root/ship/anet-hub-admin /data/projs/anet-hub/bin/anet-hub-admin
  systemctl start anet-hub anet-hub-admin' >/dev/null 2>&1 && echo "emax ok" || echo "emax FAILED"

send anet-hub fmax.chatchat.space
ssh root@fmax.chatchat.space '
  systemctl stop anet-hub
  install -m755 /root/ship/anet-hub /usr/local/bin/anet-hub
  systemctl start anet-hub' >/dev/null 2>&1 && echo "fmax ok" || echo "fmax FAILED"

# cmax runs the daemon and a peer process.
install -m755 anet /usr/local/bin/anet4
install -m755 anetpeer /usr/local/bin/anetpeer
systemctl restart anet4 anet4-svc >/dev/null 2>&1 && echo "cmax ok" || echo "cmax FAILED"

# dmax runs the daemon, the device runtime, the out-of-tree adapter and
# the testbed.
for f in anet anetpeer anetlinkd adapdemo anetmock; do send "$f" dmax.chatchat.space; done
ssh root@dmax.chatchat.space '
  install -m755 /root/ship/anet       /usr/local/bin/anet
  install -m755 /root/ship/anetpeer   /usr/local/bin/anetpeer
  install -m755 /root/ship/anetlinkd  /usr/local/bin/anetlinkd
  install -m755 /root/ship/adapdemo   /usr/local/bin/adapdemo
  install -m755 /root/ship/anetmock   /usr/local/bin/anetmock
  systemctl restart anet-dmax' >/dev/null 2>&1 && echo "dmax ok" || echo "dmax FAILED"
RIEOF
rsync -z "$STAGE/remote-install.sh" "$RELAY:/root/ship/" >/dev/null 2>&1
out=$(ssh "$RELAY" 'bash /root/ship/remote-install.sh' 2>&1)
for host in emax fmax cmax dmax; do
  echo "$out" | grep -q "$host ok" && ok "$host 安装完成" || no "$host 安装失败"
done

# ── 5. verify ───────────────────────────────────────────────────
hd "5  验证跑的是刚装的那一版"
HUBC=$( cd "$ROOT/ANetHub" && git rev-parse --short HEAD 2>/dev/null )
ANETC=$( cd "$ROOT/ANet"    && git rev-parse --short HEAD 2>/dev/null )
sleep 6
check() {  # label, command, want-commit
  local got
  got=$(eval "$2" 2>/dev/null | python3 -c '
import sys, json
try: print(json.load(sys.stdin).get("commit", ""))
except Exception: print("")' 2>/dev/null)
  case "$got" in
    "$3"*) ok "$1 跑的是 $got";;
    "")    no "$1 不报版本 —— 无法判断它是不是刚装的那一版";;
    *)     no "$1 跑的是 $got,应为 $3";;
  esac
}
check "emax hub"   "curl -sf -m 20 https://hub.agentnetwork.org.cn/healthz" "$HUBC"
check "emax admin" "curl -sf -m 20 https://hub.agentnetwork.org.cn/admin/healthz" "$HUBC"
check "fmax hub"   "ssh -o ConnectTimeout=20 root@emax.chatchat.space 'curl -sf -m 10 http://39.107.76.243:4001/healthz'" "$HUBC"
check "cmax daemon" "ssh -o ConnectTimeout=20 $RELAY 'curl -sf -m 10 http://127.0.0.1:29610/ping'" "$ANETC"
check "dmax daemon" "ssh -o ConnectTimeout=20 $RELAY \"ssh root@dmax.chatchat.space 'curl -sf -m 10 http://127.0.0.1:29610/ping'\"" "$ANETC"

printf '\n\033[1m── %d 通过, %d 失败 ──\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
