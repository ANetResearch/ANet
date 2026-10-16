package interactions

// waiting.go holds what the requester's no-response deadline reads
// (A2A-DESIGN §4.2, daemon no_response.go): the tasks that have sat in one
// state since before a cutoff, and whether a task has heard from its peer
// at all. Whether its delegation is still unconfirmed is the outbound
// queue's (HasOutboxDelegation, outbox.go).

import (
	"fmt"
	"math"
	"time"
)

// waitingSQL reads idx_ix_waiting alone. The row holds goal, request_doc and
// result, megabytes each for a long message, and state_at is a column added
// after them: a query that tested it on the row would walk every candidate's
// overflow pages each minute (an internal note).
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

// UpdatedAtTime is the interaction's updated_at as a time; the zero time when it
// does not read as one.
func (ix *Interaction) UpdatedAtTime() time.Time {
	t, err := time.Parse(time.RFC3339Nano, ix.UpdatedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}
