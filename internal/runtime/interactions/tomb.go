package interactions

// tomb.go is the cancel tombstone table (DLV §3.1, DLV-ST-6, DLV-ST-7; engine §4.5).
//
// A cancel can reach this node before the delegation it cancels: the two
// take different paths (p2p and the hub), or a hub holds the delegation
// back. A cancel that names a task this node does not hold — not in the
// interaction table, not held for approval — is acknowledged, and leaves a
// tombstone for its sender and interaction id in the same transaction as
// its replay row. A delegation that arrives later for that (sender, ix)
// meets the tombstone and is refused with the reason canceled instead of
// being run (T11). Only the sender itself, authenticated in step 7, writes
// a tombstone for its own (sender, ix) (DLV-R-12): nobody can cancel
// another requester's task in advance.
//
// A tombstone lasts until the cancel's expiry plus CancelTombMarginMS. The
// table is bounded: past CancelTombPerSender tombstones of one sender, or
// CancelTombTotal in all, the ones that expire first are deleted, and the
// caller counts them as cancel-tombstone-evicted.

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
)

// Bounds of the cancel tombstones (DLV-ST-6, ST-7). They are constants, not
// settings (core Q-DLV-6).
const (
	// CancelTombMarginMS is how long a tombstone outlives the cancel that
	// left it: the cancel's inner exp plus five minutes.
	CancelTombMarginMS = 5 * 60 * 1000
	// CancelTombPerSender bounds one sender's tombstones.
	CancelTombPerSender = 1024
	// CancelTombTotal bounds the table.
	CancelTombTotal = 100_000
)

// cancelTombCaps are the bounds PutCancelTomb applies: the constants, which
// a test lowers to exercise the eviction without writing a hundred thousand
// rows (export_test.go).
var cancelTombCaps = struct{ perSender, total int }{CancelTombPerSender, CancelTombTotal}

// cancelTombSchema is the tombstone table (DLV §3.1).
var cancelTombSchema = []string{
	`CREATE TABLE cancel_tomb (
	   from_aid TEXT NOT NULL,
	   ix       TEXT NOT NULL,
	   exp      INTEGER NOT NULL,
	   PRIMARY KEY (from_aid, ix)
	 )`,
	`CREATE INDEX idx_cancel_tomb_exp ON cancel_tomb(exp)`,
}

// PutCancelTomb records the tombstone of a cancel from `from` for task ix,
// which this node does not hold, in the transaction that records the
// cancel's replay row (engine §4.7). cancelExp is the cancel's inner exp
// (unix ms); the tombstone lasts until cancelExp + CancelTombMarginMS, and a
// second cancel for the same (from, ix) keeps whichever lasts longer.
//
// Past CancelTombPerSender tombstones of the sender, or CancelTombTotal in
// all, the ones that expire first are deleted in the same transaction — the
// new one too, when it is among them. evicted is how many went; the caller
// counts them as cancel-tombstone-evicted.
func (t *Tx) PutCancelTomb(from, ix string, cancelExp uint64) (evicted int, err error) {
	if from == "" || ix == "" {
		return 0, fmt.Errorf("%w: a cancel tombstone needs a sender and an interaction id", ErrBadInput)
	}
	exp := clampMS(cancelExp)
	if exp > math.MaxInt64-CancelTombMarginMS {
		exp = math.MaxInt64
	} else {
		exp += CancelTombMarginMS
	}
	if _, err := t.tx.Exec(`INSERT INTO cancel_tomb(from_aid, ix, exp) VALUES(?,?,?)
		ON CONFLICT(from_aid, ix) DO UPDATE SET exp = MAX(exp, excluded.exp)`, from, ix, exp); err != nil {
		return 0, err
	}
	own, err := evictCancelTombs(t.tx, from, cancelTombCaps.perSender)
	if err != nil {
		return 0, err
	}
	all, err := evictCancelTombs(t.tx, "", cancelTombCaps.total)
	if err != nil {
		return 0, err
	}
	return own + all, nil
}

// evictCancelTombs deletes the tombstones past limit — of sender from, or of
// the whole table when from is empty — those that expire first, and returns
// how many it deleted.
func evictCancelTombs(tx *sql.Tx, from string, limit int) (int, error) {
	count, victims := `SELECT COUNT(*) FROM cancel_tomb`, `SELECT rowid FROM cancel_tomb ORDER BY exp ASC, rowid ASC LIMIT ?`
	var args []any
	if from != "" {
		count = `SELECT COUNT(*) FROM cancel_tomb WHERE from_aid=?`
		victims = `SELECT rowid FROM cancel_tomb WHERE from_aid=? ORDER BY exp ASC, rowid ASC LIMIT ?`
		args = []any{from}
	}
	var n int
	if err := tx.QueryRow(count, args...).Scan(&n); err != nil {
		return 0, err
	}
	if n <= limit {
		return 0, nil
	}
	res, err := tx.Exec(`DELETE FROM cancel_tomb WHERE rowid IN (`+victims+`)`, append(args, n-limit)...)
	if err != nil {
		return 0, err
	}
	gone, err := res.RowsAffected()
	return int(gone), err
}

// HasCancelTomb reports whether `from` left a tombstone for ix that is
// still in force at nowMS (unix ms): one whose exp is after nowMS. An
// expired row the sweep has not deleted yet does not count.
func (s *Store) HasCancelTomb(from, ix string, nowMS uint64) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM cancel_tomb WHERE from_aid=? AND ix=? AND exp > ?`,
		from, ix, clampMS(nowMS)).Scan(&one)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	}
	return false, err
}

// PurgeCancelTombs deletes the tombstones whose exp is at or before nowMS
// (unix ms) and returns how many. The daemon runs it in the same sweep as
// PurgeRefused (DLV-ST-7).
func (s *Store) PurgeCancelTombs(nowMS uint64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM cancel_tomb WHERE exp <= ?`, clampMS(nowMS))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// clampMS is a unix-ms time as SQLite stores it: an int64, the largest one
// for a time past it.
func clampMS(ms uint64) int64 {
	if ms > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(ms)
}
