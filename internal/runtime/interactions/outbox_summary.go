package interactions

// outbox_summary.go counts the outbound queue for what the user is shown of
// it (DLV-P-4, DLV-L-7): /status.delivery and doctor's delivery.outbox read
// the same numbers from here.

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Limits of the summary: a row that has not left this node for stuckAfterMS
// with a network failure is stuck; a row handed over unconfirmed for
// unconfirmedAfterMS is reported (DLV-P-4).
const (
	stuckAfterMS       = 3600_000
	unconfirmedAfterMS = 24 * 3600_000
)

// OutboxSummary is the outbound queue in numbers.
type OutboxSummary struct {
	// NotSent counts the rows no byte of which has left this node;
	// Awaiting the rows handed over and not yet confirmed, QueuedAtHub those
	// among them a hub holds queued.
	NotSent, Awaiting, QueuedAtHub int
	// OldestAwaitingMS is the age of the oldest awaiting row (0 for none).
	OldestAwaitingMS int64
	// Stuck counts the rows that did not leave although the outgoing gate is
	// open, for over an hour, with a network failure as their last error —
	// not those held by the order rules, a first contact or the gate
	// (DLV-P-4); StuckOldestMS is the age of the oldest and StuckError the
	// last error code of the newest. Unconfirmed24h counts the rows handed
	// over more than a day ago and still unconfirmed.
	Stuck, Unconfirmed24h int
	StuckOldestMS         int64
	StuckError            string
}

// Summary counts the queue as of now (unix ms). gateOpen says whether the
// outgoing gate is open: a closed gate holds rows back for a reason that is
// not a failure, so none is stuck.
func (s *Store) OutboxSummary(now int64, gateOpen bool) (OutboxSummary, error) {
	return outboxSummary(s.db, now, gateOpen)
}

// ReadOutboxSummary is OutboxSummary of the store in dir, opened read-only
// and immutable: it creates and changes nothing. an error wrapping fs.ErrNotExist when there is
// no store yet.
func ReadOutboxSummary(dir string, now int64, gateOpen bool) (OutboxSummary, error) {
	path := filepath.Join(dir, storeFile)
	if _, err := os.Stat(path); err != nil {
		return OutboxSummary{}, err
	}
	db, err := sql.Open("sqlite", readOnlyURI(path))
	if err != nil {
		return OutboxSummary{}, fmt.Errorf("interactions: open %s: %w", path, err)
	}
	defer db.Close()
	return outboxSummary(db, now, gateOpen)
}

func outboxSummary(q querier, now int64, gateOpen bool) (OutboxSummary, error) {
	var o OutboxSummary
	var oldest, stuckOldest sql.NullInt64
	err := q.QueryRow(`SELECT
	  COALESCE(SUM(CASE WHEN handed_at IS NULL AND attempting=0 THEN 1 ELSE 0 END),0),
	  COALESCE(SUM(CASE WHEN handed_at IS NOT NULL OR attempting=1 THEN 1 ELSE 0 END),0),
	  COALESCE(SUM(CASE WHEN (handed_at IS NOT NULL OR attempting=1) AND EXISTS
	     (SELECT 1 FROM outbox_ver v WHERE v.row_id=outbox.id AND v.hub_state=?) THEN 1 ELSE 0 END),0),
	  MIN(CASE WHEN handed_at IS NOT NULL OR attempting=1 THEN created_at END),
	  COALESCE(SUM(CASE WHEN handed_at IS NOT NULL AND handed_at < ? THEN 1 ELSE 0 END),0)
	  FROM outbox`, OutboxQueued, now-unconfirmedAfterMS).Scan(&o.NotSent, &o.Awaiting, &o.QueuedAtHub, &oldest, &o.Unconfirmed24h)
	if err != nil {
		return OutboxSummary{}, err
	}
	if oldest.Valid {
		o.OldestAwaitingMS = max(now-oldest.Int64, 0)
	}
	if !gateOpen {
		return o, nil
	}
	// Stuck: not handed over, over an hour old, last failed on the network
	// (last_error begins with "-"), and not behind an earlier row of the
	// same task and peer (engine ENG-O1, O2).
	err = q.QueryRow(`SELECT COUNT(*), MIN(r.created_at) FROM outbox r
	  WHERE r.handed_at IS NULL AND r.attempting=0 AND r.created_at < ? AND substr(r.last_error,1,1)='-'
	  AND NOT EXISTS (SELECT 1 FROM outbox a WHERE a.ix=r.ix AND a.to_aid=r.to_aid AND a.id<r.id
	      AND (a.handed_at IS NULL OR (r.typ='anet.message/1' AND r.kind IN ('text','end_request') AND a.typ='anet.delegate/1')))`,
		now-stuckAfterMS).Scan(&o.Stuck, &stuckOldest)
	if err != nil {
		return OutboxSummary{}, err
	}
	if stuckOldest.Valid {
		o.StuckOldestMS = max(now-stuckOldest.Int64, 0)
	}
	if o.Stuck > 0 {
		var le string
		if err := q.QueryRow(`SELECT last_error FROM outbox WHERE handed_at IS NULL AND attempting=0 AND created_at < ? AND substr(last_error,1,1)='-'
		  ORDER BY id DESC LIMIT 1`, now-stuckAfterMS).Scan(&le); err == nil {
			o.StuckError = le
		} else if !errors.Is(err, sql.ErrNoRows) {
			return OutboxSummary{}, err
		}
	}
	return o, nil
}
