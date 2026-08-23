#!/usr/bin/env bash
# ship-prodtest.sh — put the live check on the machine that runs it,
# stamped with the commit it came from.
#
# The copy on cmax was hand-copied with nothing tying it to this
# repository. When the deployment moved an endpoint and the script here
# was updated, the deployed copy was not — and it reported failures for
# three hours against hubs that were fine. Shipping through one command
# that records the commit makes that visible in the first line of every
# run rather than diagnosed from symptoms.
set -euo pipefail
cd "$(dirname "$0")"

HOST=${HOST:-root@cmax.chatchat.space}
DEST=${DEST:-/opt/anet-prodtest}

COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
git diff --quiet -- . 2>/dev/null || COMMIT="$COMMIT-dirty"
echo "$COMMIT" > VERSION

tar czf /tmp/prodtest-ship.tgz prodtest.sh prodtest-cron.sh VERSION
rsync -z -e 'ssh -o ConnectTimeout=20' /tmp/prodtest-ship.tgz "$HOST:/tmp/"
ssh -o ConnectTimeout=20 "$HOST" "
  mkdir -p $DEST
  tar xzf /tmp/prodtest-ship.tgz -C $DEST
  chmod +x $DEST/*.sh
  echo \"deployed \$(cat $DEST/VERSION) to $DEST\"
"
rm -f VERSION
