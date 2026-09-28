package interactions

// waiting.go holds what the requester's no-response deadline reads and
// writes (A2A-DESIGN §4.2, daemon no_response.go): the tasks that have sat
// in one state since before a cutoff, whether a task has heard from its
// peer at all, whether anything of it is still queued to go out, and the
// moment its delegation left this node.

import (
	"fmt"
	"math"
	"time"
)

// waitingSQL reads idx_ix_waiting alone. The row holds goal, request_doc and
// result, megabytes each for a long message, and state_at is a column added
// after them: a query that tested it on the row would walk every candidate's
// overflow pages each minute (docs/notes/0035).
const waitingSQL = `SELECT id, peer_aid, state_at FROM interaction INDEXED BY idx_ix_waiting
	WHERE role=? AND state=? AND state_at<? AND (state_at, id) > (?, ?) ORDER BY state_at, id LIMIT ?`

// WaitingTask is one task Waiting found.
type WaitingTask struct {
	ID, PeerAID string
	StateAt     int64 // unix ms
}

// WaitingFirst is the cursor of Waiting's first page.
var WaitingFirst = WaitingTask{StateAt: math.MinInt64}

// Waiting returns the interactions in role and state whose state was last
// written before before (unix ms), the longest-waiting first, after the
// cursor (the last task of the previous page, or WaitingFirst), at most
// limit of them. A caller that skips some of them pages on rather than
// asking for the first page again, so the tasks it skips cannot keep the
// others from being looked at.
func (s *Store) Waiting(role Role, state State, before int64, after WaitingTask, limit int) ([]WaitingTask, error) {
	if role == "" || !state.Valid() || limit <= 0 {
		return nil, fmt.Errorf("%w: role, a valid state and a limit required", ErrBadInput)
	}
	rows, err := s.db.Query(waitingSQL, string(role), string(state), before, after.StateAt, after.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WaitingTask
	for rows.Next() {
		var w WaitingTask
		if err := rows.Scan(&w.ID, &w.PeerAID, &w.StateAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// heardFromSQL reads no message body: sender_aid comes before it in the row.
const heardFromSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM message WHERE interaction_id=? AND sender_aid=? LIMIT 1)`

// outboxPendingSQL reads idx_outbox_ix.
const outboxPendingSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM outbox WHERE ix=? LIMIT 1)`

// HeardFrom reports whether interaction id holds a message from sender: a
// status, a text or a payment message, whatever became of it.
func (s *Store) HeardFrom(id, sender string) (bool, error) {
	var n int
	err := s.db.QueryRow(heardFromSQL, id, sender).Scan(&n)
	return n > 0, err
}

// HeardFrom is Store.HeardFrom inside the transaction.
func (t *Tx) HeardFrom(id, sender string) (bool, error) {
	var n int
	err := t.tx.QueryRow(heardFromSQL, id, sender).Scan(&n)
	return n > 0, err
}

// OutboxPending reports whether any message of interaction ix is still in
// the outbound retry queue (not delivered, not abandoned).
func (s *Store) OutboxPending(ix string) (bool, error) {
	var n int
	err := s.db.QueryRow(outboxPendingSQL, ix).Scan(&n)
	return n > 0, err
}

// OutboxPending is Store.OutboxPending inside the transaction.
func (t *Tx) OutboxPending(ix string) (bool, error) {
	var n int
	err := t.tx.QueryRow(outboxPendingSQL, ix).Scan(&n)
	return n > 0, err
}

// UpdatedAtTime is the interaction's updated_at as a time; the zero time when it
// does not read as one.
func (ix *Interaction) UpdatedAtTime() time.Time {
	t, err := time.Parse(time.RFC3339Nano, ix.UpdatedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// DeleteDeliveredOutbox removes a row the recipient took. For a delegation
// (delegation set) it also records, as the interaction's updated_at, when
// the task left this node: a delegation can wait in the queue for hours
// while the hub is unreachable, and the requester's no-response deadline
// counts from its delivery, not from when the task was made. A task that has
// ended meanwhile is not touched.
func (s *Store) DeleteDeliveredOutbox(id int64, ix string, delegation bool) error {
	return s.Update(func(t *Tx) error {
		if _, err := t.tx.Exec(`DELETE FROM outbox WHERE id=?`, id); err != nil {
			return err
		}
		if !delegation {
			return nil
		}
		_, err := t.tx.Exec(`UPDATE interaction SET updated_at=? WHERE id=? AND state NOT IN `+terminalSQL,
			time.Now().UTC().Format(time.RFC3339Nano), ix)
		return err
	})
}
