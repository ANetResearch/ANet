package interactions

// retention.go deletes finished interactions of one trust class after a
// retention period (A2A-DESIGN §21 item 4, decision Q15: public_cap
// interactions are kept 7 days after they end).

import (
	"fmt"
	"strings"
)

// Pruned counts the rows one PruneTerminal call deleted.
type Pruned struct {
	Interactions int64
	Messages     int64
	Attachments  int64
}

// pruneBatch is how many interactions one transaction of PruneTerminal
// deletes. The store lock is held for a transaction, and the receive path
// waits on it; the first sweep after an upgrade (or after a busy week) may
// have a great many rows to delete, so it releases the lock between
// batches. A variable so that a test can use small batches.
var pruneBatch = 500

// PruneTerminal deletes the interactions of the given trust that reached a
// terminal state before cutoff (unix ms, compared with state_at), together
// with their messages and attachments; the peer key material held on the
// row (peer_kel, peer_keys) goes with it.
//
// Two kinds of finished interaction are kept:
//   - one with a message still in the outbound retry queue, until the
//     queue lets go of it, so a result that has not reached the requester
//     is not deleted under the retry loop;
//   - one whose payment is submitted and not settled: the stored payment
//     payload is what a settlement, or an operator reconciling one, needs.
//
// The replay rows are not touched: they are keyed by message and expire on
// their own (PurgeReplay), and they are what refuses a replayed envelope
// after the interaction is gone.
//
// The deletion runs in transactions of pruneBatch interactions each; an
// interaction goes with all its messages and attachments in one of them.
// On an error the counts of the batches already committed are returned
// with it.
func (s *Store) PruneTerminal(trust string, cutoff int64) (Pruned, error) {
	var n Pruned
	if trust == "" {
		return n, fmt.Errorf("%w: a trust class is required", ErrBadInput)
	}
	for {
		b, done, err := s.pruneBatch(trust, cutoff, pruneBatch)
		n.Interactions += b.Interactions
		n.Messages += b.Messages
		n.Attachments += b.Attachments
		if err != nil {
			return n, fmt.Errorf("interactions: prune %s: %w", trust, err)
		}
		if done {
			return n, nil
		}
	}
}

// pruneBatch deletes up to limit interactions PruneTerminal selects, in one
// transaction. done is true when fewer than limit were found.
func (s *Store) pruneBatch(trust string, cutoff int64, limit int) (n Pruned, done bool, err error) {
	err = s.Update(func(t *Tx) error {
		rows, err := t.tx.Query(`SELECT id FROM interaction
		   WHERE trust = ? AND state IN `+terminalSQL+` AND state_at < ? AND pay_state != ?
		     AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.ix = interaction.id)
		   ORDER BY seq LIMIT ?`, trust, cutoff, PaySubmitted, limit)
		if err != nil {
			return err
		}
		var ids []any
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		done = len(ids) < limit
		if len(ids) == 0 {
			return nil
		}
		in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
		for _, d := range []struct {
			q   string
			out *int64
		}{
			{`DELETE FROM attachment WHERE interaction_id IN ` + in, &n.Attachments},
			{`DELETE FROM message WHERE interaction_id IN ` + in, &n.Messages},
			{`DELETE FROM interaction WHERE id IN ` + in, &n.Interactions},
		} {
			res, err := t.tx.Exec(d.q, ids...)
			if err != nil {
				return err
			}
			if *d.out, err = res.RowsAffected(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Pruned{}, false, err
	}
	return n, done, nil
}
