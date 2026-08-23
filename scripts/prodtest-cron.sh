#!/usr/bin/env bash
# prodtest-cron.sh — run the live check on a schedule and keep the history.
#
# A test procedure that only runs when somebody remembers is one that
# rots, and this one checks the things most likely to break quietly: two
# hubs federating, two clocks agreeing, ledgers adding up, an agent still
# collecting its mail. None of those announce themselves when they stop.
#
# Installed on cmax, which has fast paths to both hubs. Results go to a
# dated file and the exit status goes to journald, so `systemctl status`
# and `journalctl -u prodtest` both tell the truth without anything
# external having to be reachable.
#
#   RESULTS   where the runs are kept (default /var/log/anet-prodtest)
#   KEEP      how many to keep (default 90 — about a month of hourly runs
#             at the default timer, and small: each run is a few KB)
set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
RESULTS=${RESULTS:-/var/log/anet-prodtest}
KEEP=${KEEP:-90}
mkdir -p "$RESULTS"

stamp=$(date -u +%Y%m%dT%H%M%SZ)
out="$RESULTS/$stamp.log"

# --no-write on the scheduled run.
#
# The full run spends credit, writes to both nodes' evidence chains and
# leaves a review behind. Doing that every hour would fill the ledger with
# test traffic and slowly drain whatever balance the nodes hold — the
# monitor would become the biggest thing on the network it monitors. The
# read-only half still covers what actually rots: both hubs up and
# publishing their keys, the ledgers balancing, three daemons registered
# where they belong, and the directory crossing between hubs.
#
# Pass FULL=1 to run the writing half, for a release check.
mode=--no-write
[ "${FULL:-0}" = 1 ] && mode=""

start=$(date -u +%s)
# shellcheck disable=SC2086
bash "$HERE/prodtest.sh" $mode >"$out" 2>&1
status=$?
elapsed=$(( $(date -u +%s) - start ))

self=$(cat "$HERE/VERSION" 2>/dev/null || echo unstamped)
summary=$(grep -oE '── [0-9]+ 通过, [0-9]+ 失败.*' "$out" | tail -1)
[ -z "$summary" ] && summary="(no summary — the run did not reach the end)"

# One line to journald either way. Silence is not success: a run that
# died halfway leaves no summary line, and that has to be as visible as a
# failing check.
if [ "$status" -eq 0 ]; then
    echo "prodtest ok in ${elapsed}s [$self] — $summary ($out)"
else
    echo "prodtest FAILED (exit $status) in ${elapsed}s [$self] — $summary ($out)" >&2
    # The failing lines inline, so an operator reading the journal does
    # not have to go and open the file to learn what broke.
    grep -E '✗' "$out" | sed 's/^/  /' >&2
fi

# Keep the history bounded. A monitor that fills a disk has become the
# outage — and the box next door already grew a 5.4G log once.
ls -1t "$RESULTS"/*.log 2>/dev/null | tail -n +$((KEEP + 1)) | xargs -r rm -f

# A pointer to the newest run, so anything looking for "the current state"
# has one path rather than having to sort filenames.
ln -sfn "$out" "$RESULTS/latest.log"
exit "$status"
