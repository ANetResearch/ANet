#!/usr/bin/env bash
# hub-db-roll-docker.sh — maintenance for the CONTAINERISED anet-hub's hub.db
# (Research-Galaxy compose service anet-hub, volume research-galaxy_anethubdata).
#
# Since 2026-09-29 that hub runs ANetHub v0.2.1 (hub wire 2). The wire-1
# version of this script deleted delivered relay rows by delivered_at; wire 2
# has no such column: the hub deletes a relay row when it is acked and drops
# undelivered ones after -relay-ttl (14 days) by itself. This is now the
# ANetHub v0.2.1 deploy/hub-db-roll.sh with two defaults changed (the data
# directory and the log file); see that file for the policy:
#   - PRAGMA wal_checkpoint(TRUNCATE) on every run;
#   - on Sundays a rotating backup (newest 2) WITHOUT relay_message rows, in the
#     data directory, and VACUUM when the freelist exceeds 20% of pages.
# Run by hub-db-roll-docker.service (as root) from hub-db-roll-docker.timer.
# ANet docs/notes/0039.
#
# Environment:
#   HUB_DATA_DIR   the hub's data directory (default: the docker volume below)
#   HUB_ROLL_LOG   log file (default /var/log/hub-db-roll-docker.log)
#   FORCE_WEEKLY=1 run the Sunday part today
set -euo pipefail

DATA_DIR=${HUB_DATA_DIR:-/var/lib/docker/volumes/research-galaxy_anethubdata/_data}
DB="$DATA_DIR/hub.db"
LOG=${HUB_ROLL_LOG:-/var/log/hub-db-roll-docker.log}

command -v sqlite3 >/dev/null || { echo "hub-db-roll: sqlite3 is required" >&2; exit 1; }
[ -f "$DB" ] || { echo "hub-db-roll: $DB not found (set HUB_DATA_DIR)" >&2; exit 1; }

SQL() { sqlite3 "$DB" ".timeout 5000" "$@"; }
log() { echo "[$(date -Is)] $*" >>"$LOG"; }
# sqlstr quotes a path as an SQL string literal (HUB_DATA_DIR comes from the caller).
sqlstr() { printf "'%s'" "${1//\'/\'\'}"; }

size_before=$(stat -c %s "$DB")
backlog=$(SQL "SELECT COUNT(*) FROM relay_message;")
log "START db=$((size_before/1024/1024))MB relay_backlog=$backlog"

SQL "PRAGMA wal_checkpoint(TRUNCATE);" >/dev/null
log "wal_checkpoint(TRUNCATE) done"

# A temporary backup copy from an earlier run that was killed before its
# EXIT trap (SIGKILL, power loss) still holds every relay row; it is
# nobody's backup and would otherwise stay on disk.
for stale in "$DATA_DIR"/.hub-backup-*.tmp.db; do
    [ -e "$stale" ] || continue
    rm -f -- "$stale" "$stale-journal" "$stale-wal" "$stale-shm"
    log "removed a temporary copy left by an interrupted run: $stale"
done

# Sunday: rotating backup without relay rows (keep 2) + conditional VACUUM
if [ "$(date +%u)" = "7" ] || [ "${FORCE_WEEKLY:-0}" = 1 ]; then
    backup="$DATA_DIR/hub-backup-$(date +%Y%m%d).db"
    if [ ! -e "$backup" ]; then
        tmp="$DATA_DIR/.hub-backup-$(date +%Y%m%d).tmp.db"
        rm -f -- "$tmp" "$tmp-journal" "$tmp-wal" "$tmp-shm"
        # The temporary copy still holds relay rows until the DELETE below
        # has run; it is removed on every exit path, including a failed
        # statement under set -e, so that it does not outlive this run.
        trap 'rm -f -- "$tmp" "$tmp-journal" "$tmp-wal" "$tmp-shm"' EXIT
        SQL "VACUUM INTO $(sqlstr "$tmp");"
        sqlite3 "$tmp" ".timeout 5000" \
            "PRAGMA secure_delete=ON;" \
            "DELETE FROM relay_message;" >/dev/null
        sqlite3 "$tmp" ".timeout 5000" "VACUUM INTO $(sqlstr "$backup");"
        rm -f -- "$tmp" "$tmp-journal" "$tmp-wal" "$tmp-shm"
        left=$(sqlite3 "$backup" "SELECT COUNT(*) FROM relay_message;")
        if [ "$left" != "0" ]; then
            log "ERROR: backup $backup holds $left relay rows; removing it"
            rm -f -- "$backup"
            exit 1
        fi
        log "weekly backup: $backup ($(stat -c %s "$backup" | awk '{printf "%dMB", $1/1024/1024}'), relay rows excluded)"
    fi
    # Keep the newest 2 backups. Not `ls | tail | while`: with pipefail an
    # ls that matches nothing fails the pipeline, and set -e then ended the
    # run before the VACUUM and the END line.
    mapfile -t backups < <(ls -1t -- "$DATA_DIR"/hub-backup-*.db 2>/dev/null || true)
    for old in "${backups[@]:2}"; do
        rm -f -- "$old" "$old-wal" "$old-shm" "$old-journal"
        log "pruned old backup: $old"
    done
    # VACUUM main db if freelist > 20% of pages
    freelist=$(SQL "PRAGMA freelist_count;")
    pages=$(SQL "PRAGMA page_count;")
    if [ "$pages" -gt 0 ] && [ $((freelist * 100 / pages)) -gt 20 ]; then
        log "VACUUM: freelist=$freelist/$pages pages"
        SQL "VACUUM;"
        SQL "PRAGMA wal_checkpoint(TRUNCATE);" >/dev/null
        log "VACUUM done"
    fi
fi

size_after=$(stat -c %s "$DB")
log "END db=$((size_after/1024/1024))MB (was $((size_before/1024/1024))MB)"
