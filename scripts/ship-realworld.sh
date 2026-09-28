#!/usr/bin/env bash
# ship-realworld.sh — install the real-service tier as a daily timer on
# the machine that runs it.
#
# realworld.sh drives it from here, which means it runs when somebody
# remembers. Seven adapters' only coverage against real protocol
# implementations would then lapse quietly on the day nobody did — the
# same way the hourly check on cmax went weeks running a script a third
# its current length while reporting green.
#
# Installed on dmax rather than here: the containers belong on the machine
# with the disk for them, and this one was explicitly excluded.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$PWD/..
RELAY=${RELAY:-root@cmax.chatchat.space}
HOST=${HOST:-root@dmax.chatchat.space}
DEST=${DEST:-/opt/anet-realworld}
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

tar czf "$STAGE/link.tgz" -C "$ROOT" --exclude='.git' --exclude='Refs' \
  --exclude='Repos' --exclude='ANetCore' ANetLink
tar czf "$STAGE/core.tgz" -C "$ROOT" --exclude='.git' ANetCore
echo "$COMMIT" > "$STAGE/VERSION"

# The runner, written here so what runs there is what this repository
# says. It reports one summary line to the journal and keeps its own dated
# output, because journald rotates and a shell redirect does not.
cat > "$STAGE/run.sh" <<'RUNEOF'
#!/usr/bin/env bash
set -uo pipefail
cd /opt/anet-realworld
LOGDIR=/var/log/anet-realworld
mkdir -p "$LOGDIR"
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
LOG="$LOGDIR/$STAMP.log"
export PATH=/usr/local/bin:$PATH GOPROXY=https://goproxy.cn,direct GOFLAGS=-mod=mod
# systemd starts this with no HOME, and Go derives its module cache from
# HOME or GOPATH — without either it refuses to build at all, which
# reads in the log as "no tests ran" rather than as a missing variable.
# Named explicitly so the timer's environment and an interactive run
# resolve the same cache.
export HOME=${HOME:-/root}
export GOMODCACHE=${GOMODCACHE:-/root/go/pkg/mod}
export GOCACHE=${GOCACHE:-/root/.cache/go-build}

{
  echo "commit $(cat VERSION 2>/dev/null)"
  cd ANetLink || exit 1
  bash test/realworld/up.sh 2>&1
  # shellcheck disable=SC1091
  source test/realworld/env
  # The camera rig binds host ports (RTSP 8554/tcp and 8000/udp, ONVIF 8081,
  # proxies 8555/8580). On a machine where something else already holds
  # them — dmax runs another product's mediamtx on 8554 — the rig's RTSP
  # server exits at once while env still names 127.0.0.1:8554, and the
  # camera tests then probe that other server: DESCRIBE /cam answers 404
  # (a failure that is not ANetLink's) and the wrong-path test passes for
  # the wrong reason. Without the rig there is no camera to test, so the
  # camera tests are skipped, and the log says why.
  if [ -n "${ANETLINK_RTSP_URI:-}" ] && \
     [ "$(docker inspect -f '{{.State.Running}}' anetlink-rtsp 2>/dev/null)" != true ]; then
    echo "camera rig not running (anetlink-rtsp: $(docker logs anetlink-rtsp 2>&1 | grep -m1 -iE 'error|ERR' || echo 'not started'));"
    echo "ONVIF/RTSP tests skipped rather than run against whatever holds those ports"
    unset ANETLINK_RTSP_URI ANETLINK_ONVIF_XADDR
  fi
  CGO_ENABLED=0 timeout 600 go test -tags realworld -run TestReal -v -timeout 540s \
    ./adapters/... ./internal/... ./matter/... 2>&1
} > "$LOG" 2>&1
rc=$?
ln -sfn "$LOG" "$LOGDIR/latest.log"

p=$(grep -cE '^--- PASS' "$LOG")
s=$(grep -cE '^--- SKIP' "$LOG")
f=$(grep -cE '^--- FAIL' "$LOG")
echo "realworld: PASS $p SKIP $s FAIL $f  ($LOG)"
# A run where nothing passed proved nothing, however green: the services
# not coming up looks identical to every test skipping.
if [ "$f" -gt 0 ] || [ "$p" -lt 3 ]; then
  grep -E '^--- FAIL' "$LOG" | head -8
  exit 1
fi
exit 0
RUNEOF

cat > "$STAGE/down.sh" <<'DOWNEOF'
#!/usr/bin/env bash
# Always, even after a failed run: a Matter device left advertising past
# its window is a device the next run finds silent and reports as broken.
cd /opt/anet-realworld/ANetLink 2>/dev/null && bash test/realworld/down.sh >/dev/null 2>&1
# ANetLink's down.sh leaves out the camera rig that up.sh starts with
# --network host: left running, it keeps an ONVIF emulator and two TCP
# proxies (8081, 8555, 8580) open on every address the host has, public
# ones included, until the next run.
docker rm -f anetlink-rtsp anetlink-onvif >/dev/null 2>&1
exit 0
DOWNEOF

cp deploy/realworld.service deploy/realworld.timer "$STAGE/"
tar czf /tmp/realworld-ship.tgz -C "$STAGE" .
rsync -z -e 'ssh -o ConnectTimeout=20' /tmp/realworld-ship.tgz "$RELAY:/root/ship/"
ssh -o ConnectTimeout=20 "$RELAY" "rsync -z /root/ship/realworld-ship.tgz $HOST:/root/ship/"

ssh -o ConnectTimeout=20 "$RELAY" "ssh -o ConnectTimeout=20 $HOST '
  set -e
  install -d -o root -g root -m 0755 $DEST && cd $DEST
  tar --no-same-owner --no-same-permissions -xzf /root/ship/realworld-ship.tgz
  rm -rf ANetLink ANetCore
  tar --no-same-owner --no-same-permissions -xzf link.tgz && tar --no-same-owner --no-same-permissions -xzf core.tgz
  cd ANetLink
  grep -q \"replace github.com/ANetResearch/ANetCore\" go.mod || \
    printf \"replace github.com/ANetResearch/ANetCore => ../ANetCore\n\" >> go.mod
  cd $DEST
  # root:root and not writable by anyone else: realworld.service runs run.sh
  # as root, and tar as root would keep the uid of whoever packed it (ship-prodtest.sh).
  chown -R root:root $DEST && chmod -R go-w $DEST
  chmod +x run.sh down.sh
  install -m644 realworld.service realworld.timer /etc/systemd/system/
  systemctl daemon-reload
  systemctl enable --now realworld.timer
  systemctl is-enabled realworld.timer
'"
echo "realworld tier installed on $HOST at $DEST (commit $COMMIT)"
