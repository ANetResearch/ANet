package interactions

// waiting.go holds what the requester's no-response deadline reads and
// writes (A2A-DESIGN §4.2, daemon no_response.go): the tasks that have sat
// in one state since before a cutoff, whether a task has heard from its
// peer at all, whether anything of it is still queued to go out, and the
// moment its delegation left this node.

import (
	"fmt"
	"time"
)

// waitingSQL reads idx_ix_waiting alone. The row holds goal, request_doc and
// result, megabytes each for a long message, and state_at is a column added
// after them: a query that tested it on the row would walk every candidate's
// overflow pages each minute (docs/notes/0035).
const waitingSQL = `SELECT id FROM interaction INDEXED BY idx_ix_waiting
	WHERE role=? AND state=? AND state_at<? ORDER BY state_at LIMIT ?`

// Waiting returns the ids of the interactions in role and state whose state
// was last written before before (unix ms), the longest-waiting first, at
// most limit of them.
func (s *Store) Waiting(role Role, state State, before int64, limit int) ([]string, error) {
	if role == "" || !state.Valid() || limit <= 0 {
		return nil, fmt.Errorf("%w: role, a valid state and a limit required", ErrBadInput)
	}
	rows, err := s.db.Query(waitingSQL, string(role), string(state), before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// HeardFrom reports whether interaction id holds a message from sender: a
// status, a text or a payment message, whatever became of it. sender_aid
// comes before the body in the row, so this reads no message body.
func (t *Tx) HeardFrom(id, sender string) (bool, error) {
	var n int
	err := t.tx.QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM message WHERE interaction_id=? AND sender_aid=? LIMIT 1)`,
		id, sender).Scan(&n)
	return n > 0, err
}

// OutboxPending reports whether any message of interaction ix is still in
// the outbound retry queue (not delivered, not abandoned).
func (t *Tx) OutboxPending(ix string) (bool, error) {
	var n int
	err := t.tx.QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM outbox WHERE ix=? LIMIT 1)`, ix).Scan(&n)
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
