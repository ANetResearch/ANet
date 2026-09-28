package interactions

// refused.go is the refused-delegation table (A2A-DESIGN §3.6 step 9
// [redteam:F5]): one row per delegation the receive path refused in step 9,
// keyed like the replay table by (sender AID, message id) and kept until the
// message's expiry, in the same database.
//
// A refusal is final. Step 9 judges a delegation under the policy and the
// quotas of the moment, and the requester is told `rejected`; the same
// signed envelope presented again after a restart, after the in-memory list
// forgot it, or after the operator changed the policy must not be judged
// again and accepted. The replay table cannot hold it: its rows commit with
// the business write of an accepted message, and a refusal writes none.
//
// The table is bounded. Past perSender rows for one sender, or total rows in
// all, the oldest rows (by message time) go, and the floor of each sender
// whose rows went rises to the newest message time among them. A delegation
// at or below its sender's floor that the replay table does not hold is
// treated as refused: a replay of an evicted row stays refused, at the price
// of refusing a first delivery from the same sender that arrives later than
// that sender's own refusals which pushed the floor past it (§21).
//
// A floor is only ever the sender's own. The message time is the sender's
// to choose (up to the clock skew into the future), so a floor shared by
// everyone would let refusals of throwaway identities, dated ahead, push it
// past the send time of every other sender's next delegation and drop them
// all without a reply [redteam:F5]. The floors are bounded too: past Floors
// of them, the ones that expire first are forgotten, and a replay of a
// refusal whose row and floor both went is judged again (§21).

import (
	"database/sql"
	"errors"
	"fmt"
)

// Default bounds of the refused table. A row is a few hundred bytes with
// its indexes; a floor, one per sender, somewhat less.
const (
	DefaultRefusedPerSender = 1024
	DefaultRefusedTotal     = 100_000
	DefaultRefusedFloors    = 100_000
)

// RefusedCaps are the bounds of the refused table; zero means the default.
type RefusedCaps struct {
	PerSender int
	Total     int
	Floors    int
}

func (c RefusedCaps) orDefault() RefusedCaps {
	if c.PerSender <= 0 {
		c.PerSender = DefaultRefusedPerSender
	}
	if c.Total <= 0 {
		c.Total = DefaultRefusedTotal
	}
	if c.Floors <= 0 {
		c.Floors = DefaultRefusedFloors
	}
	return c
}

// SetRefusedCaps changes the bounds of the refused table; zero restores a
// default. Intended for tests that exercise eviction without writing a
// hundred thousand rows.
func (s *Store) SetRefusedCaps(c RefusedCaps) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusedCaps = c
}

// RecordRefused records that the delegation (from, mid), sent at ts and
// valid until exp (unix ms), was refused. Recording it again is a no-op.
// Rows past the bounds are evicted into their senders' floors, and floors
// past theirs are forgotten, in the same transaction.
func (s *Store) RecordRefused(from string, mid []byte, ts, exp uint64) error {
	if from == "" || len(mid) == 0 {
		return fmt.Errorf("%w: a refused row needs a sender and a message id", ErrBadInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	caps := s.refusedCaps.orDefault()
	if !s.refusedCounted {
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM refused`).Scan(&s.refusedRows); err != nil {
			return err
		}
		s.refusedCounted = true
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("interactions: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`INSERT OR IGNORE INTO refused(from_aid, mid, ts, exp) VALUES(?,?,?,?)`,
		from, mid, int64(ts), int64(exp))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	rows := s.refusedRows + 1
	var mine int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM refused WHERE from_aid=?`, from).Scan(&mine); err != nil {
		return err
	}
	own, err := evictRefused(tx, from, mine-caps.PerSender)
	if err != nil {
		return err
	}
	rows -= own
	all, err := evictRefused(tx, "", rows-caps.Total)
	if err != nil {
		return err
	}
	rows -= all
	if own+all > 0 {
		if err := trimRefusedFloors(tx, caps.Floors); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("interactions: commit: %w", err)
	}
	s.refusedRows = rows
	return nil
}

// evictRefused deletes the over oldest rows — of sender from, or of every
// sender when from is empty — and raises the floor of each sender whose
// rows it deleted to cover them: its own floor, never another sender's
// [redteam:F5]. It returns how many it deleted.
func evictRefused(tx *sql.Tx, from string, over int) (int, error) {
	if over <= 0 {
		return 0, nil
	}
	victims := `SELECT rowid FROM refused ORDER BY ts ASC, rowid ASC LIMIT ?`
	vargs := []any{over}
	if from != "" {
		victims = `SELECT rowid FROM refused WHERE from_aid=? ORDER BY ts ASC, rowid ASC LIMIT ?`
		vargs = []any{from, over}
	}
	if _, err := tx.Exec(`INSERT INTO refused_floor(from_aid, ts, exp)
		SELECT from_aid, MAX(ts), MAX(exp) FROM refused WHERE rowid IN (`+victims+`) GROUP BY from_aid
		ON CONFLICT(from_aid) DO UPDATE SET ts=MAX(ts, excluded.ts), exp=MAX(exp, excluded.exp)`,
		vargs...); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM refused WHERE rowid IN (`+victims+`)`, vargs...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// trimRefusedFloors forgets the floors past limit, those that expire first.
func trimRefusedFloors(tx *sql.Tx, limit int) error {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM refused_floor`).Scan(&n); err != nil {
		return err
	}
	if n <= limit {
		return nil
	}
	_, err := tx.Exec(`DELETE FROM refused_floor WHERE from_aid IN
		(SELECT from_aid FROM refused_floor ORDER BY exp ASC, ts ASC LIMIT ?)`, n-limit)
	return err
}

// Refused reports whether the delegation (from, mid) is in the refused
// table, and the sender's floor, 0 when it has none. The caller treats a
// delegation whose message time is at or below the floor, and that the
// replay table does not hold, as refused.
func (s *Store) Refused(from string, mid []byte) (exact bool, floor uint64, err error) {
	var one int
	switch err = s.db.QueryRow(`SELECT 1 FROM refused WHERE from_aid=? AND mid=?`, from, mid).Scan(&one); {
	case err == nil:
		exact = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		return false, 0, err
	}
	var f int64
	switch err = s.db.QueryRow(`SELECT ts FROM refused_floor WHERE from_aid=?`, from).Scan(&f); {
	case err == nil:
		if f > 0 {
			floor = uint64(f)
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return false, 0, err
	}
	return exact, floor, nil
}

// RefusedCount is the number of refused rows, and of floors, stored.
func (s *Store) RefusedCount() (rows, floors int, err error) {
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM refused`).Scan(&rows); err != nil {
		return 0, 0, err
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM refused_floor`).Scan(&floors)
	return rows, floors, err
}

// PurgeRefused deletes refused rows, and floors, whose expiry is before
// cutoff (unix ms): a message past its expiry is refused by the time check
// (§3.6 step 5), so nothing needs to remember it.
func (s *Store) PurgeRefused(cutoff uint64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM refused WHERE exp < ?`, int64(cutoff))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if s.refusedCounted {
		s.refusedRows -= int(n)
	}
	if _, err := s.db.Exec(`DELETE FROM refused_floor WHERE exp < ?`, int64(cutoff)); err != nil {
		return n, err
	}
	return n, nil
}

// migrateRefused creates the refused table and its floors.
func (s *Store) migrateRefused() error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS refused (
		   from_aid TEXT NOT NULL,
		   mid BLOB NOT NULL,
		   ts INTEGER NOT NULL,
		   exp INTEGER NOT NULL,
		   PRIMARY KEY (from_aid, mid)
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_refused_exp ON refused(exp)`,
		`CREATE INDEX IF NOT EXISTS idx_refused_sender_ts ON refused(from_aid, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_refused_ts ON refused(ts)`,
		`CREATE TABLE IF NOT EXISTS refused_floor (
		   from_aid TEXT PRIMARY KEY,
		   ts INTEGER NOT NULL,
		   exp INTEGER NOT NULL
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_refused_floor_exp ON refused_floor(exp)`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("interactions: migrate refused: %w", err)
		}
	}
	return nil
}
