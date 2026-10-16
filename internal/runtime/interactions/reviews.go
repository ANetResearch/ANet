package interactions

// reviews.go is the review store of an outbound task (HPP §3.5, PAY-V9): the
// review_outbox table that holds a signed review until it is uploaded, and the
// spending of the one-time inputs the review is signed from (review_seed and
// review_rsalt on the task's row).

import (
	"database/sql"
	"errors"
	"fmt"
)

// reviewOutboxSchema is the table of signed reviews waiting for their upload.
// receipt and review are the two objects as the hub receives them; neither
// holds task content.
var reviewOutboxSchema = []string{
	`CREATE TABLE review_outbox (
	   ix       TEXT PRIMARY KEY,
	   due_at   INTEGER NOT NULL,
	   receipt  BLOB NOT NULL,
	   review   BLOB NOT NULL,
	   attempts INTEGER NOT NULL DEFAULT 0,
	   first_at INTEGER NOT NULL
	 )`,
	`CREATE INDEX idx_review_outbox_due ON review_outbox(due_at)`,
}

// ReviewOutboxRow is one signed review waiting for its upload.
type ReviewOutboxRow struct {
	IX       string
	DueAt    int64 // unix ms: not before
	Receipt  []byte
	Review   []byte
	Attempts int
	FirstAt  int64 // unix ms: when it was queued
}

// ErrReviewQueued: the task has already been reviewed.
var ErrReviewQueued = errors.New("interactions: the task has already been reviewed")

// QueueReview stores the signed review on the task and the row of the outbox
// that uploads it, in one transaction. A task is reviewed once: a second call
// is ErrReviewQueued.
func (s *Store) QueueReview(id string, receipt, review []byte, dueAt int64) error {
	if id == "" || len(receipt) == 0 || len(review) == 0 {
		return fmt.Errorf("%w: a queued review needs the task, the receipt and the review", ErrBadInput)
	}
	return s.Update(func(t *Tx) error {
		var have []byte
		if err := t.tx.QueryRow(`SELECT review FROM interaction WHERE id=?`, id).Scan(&have); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if len(have) > 0 {
			return ErrReviewQueued
		}
		b, err := rowBox(t.tx, id)
		if err != nil {
			return err
		}
		stored, err := b.put(ColReview, review)
		if err != nil {
			return err
		}
		if _, err := t.tx.Exec(`UPDATE interaction SET review=? WHERE id=?`, stored, id); err != nil {
			return err
		}
		_, err = t.tx.Exec(`INSERT INTO review_outbox(ix,due_at,receipt,review,attempts,first_at) VALUES(?,?,?,?,0,?)`,
			id, dueAt, receipt, review, t.now)
		return err
	})
}

// DueReviews returns the queued reviews whose time has come at nowMS, oldest
// due first, at most limit.
func (s *Store) DueReviews(nowMS int64, limit int) ([]ReviewOutboxRow, error) {
	rows, err := s.db.Query(`SELECT ix,due_at,receipt,review,attempts,first_at FROM review_outbox
		WHERE due_at <= ? ORDER BY due_at, ix LIMIT ?`, nowMS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReviewRows(rows)
}

// PendingReviews returns every queued review, oldest first.
func (s *Store) PendingReviews() ([]ReviewOutboxRow, error) {
	rows, err := s.db.Query(`SELECT ix,due_at,receipt,review,attempts,first_at FROM review_outbox ORDER BY first_at, ix`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReviewRows(rows)
}

func scanReviewRows(rows *sql.Rows) ([]ReviewOutboxRow, error) {
	var out []ReviewOutboxRow
	for rows.Next() {
		var r ReviewOutboxRow
		if err := rows.Scan(&r.IX, &r.DueAt, &r.Receipt, &r.Review, &r.Attempts, &r.FirstAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RetryReview records a failed upload attempt and the time of the next one.
func (s *Store) RetryReview(id string, nextAt int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE review_outbox SET attempts = attempts + 1, due_at = ? WHERE ix = ?`, nextAt, id)
	return err
}

// FinishReview removes a review from the outbox and spends the task's
// one-time inputs: the hub stored it, or already held one (HPP PAY-V9).
func (s *Store) FinishReview(id string) error {
	return s.Update(func(t *Tx) error {
		if _, err := t.tx.Exec(`DELETE FROM review_outbox WHERE ix = ?`, id); err != nil {
			return err
		}
		_, err := t.tx.Exec(`UPDATE interaction SET review_seed = NULL, review_rsalt = NULL WHERE id = ?`, id)
		return err
	})
}

// DropReview removes a review from the outbox without a verdict: the upload
// was given up. The one-time inputs stay until their own term.
func (s *Store) DropReview(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM review_outbox WHERE ix = ?`, id)
	return err
}

// PurgeReviewSecrets spends the one-time inputs of the tasks that ended before
// cutoffMS (unix ms) and returns how many. A review can be started for 30 days
// after the task ends; after that its seed and salt are of no use.
func (s *Store) PurgeReviewSecrets(cutoffMS int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE interaction SET review_seed = NULL, review_rsalt = NULL
		WHERE review_seed IS NOT NULL AND state_at < ?
		  AND state IN ('completed','failed','canceled','rejected')`, cutoffMS)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
