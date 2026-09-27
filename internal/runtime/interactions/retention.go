package interactions

// retention.go deletes finished interactions of one trust class after a
// retention period (A2A-DESIGN §21 item 4, decision Q15: public_cap
// interactions are kept 7 days after they end).

import "fmt"

// Pruned counts the rows one PruneTerminal call deleted.
type Pruned struct {
	Interactions int64
	Messages     int64
	Attachments  int64
}

// PruneTerminal deletes the interactions of the given trust that reached a
// terminal state before cutoff (unix ms, compared with state_at), together
// with their messages and attachments; the peer key material held on the
// row (peer_kel, peer_keys) goes with it. An interaction with a message
// still in the outbound retry queue is kept until the queue lets go of it,
// so a result that has not reached the requester is not deleted under the
// retry loop. The replay rows are not touched: they are keyed by message
// and expire on their own (PurgeReplay), and they are what refuses a
// replayed envelope after the interaction is gone.
//
// Everything is deleted in one transaction.
func (s *Store) PruneTerminal(trust string, cutoff int64) (Pruned, error) {
	var n Pruned
	if trust == "" {
		return n, fmt.Errorf("%w: a trust class is required", ErrBadInput)
	}
	cond := `trust = ? AND state IN ` + terminalSQL + ` AND state_at < ?
	   AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.ix = interaction.id)`
	sel := `SELECT id FROM interaction WHERE ` + cond
	err := s.Update(func(t *Tx) error {
		res, err := t.tx.Exec(`DELETE FROM attachment WHERE interaction_id IN (`+sel+`)`, trust, cutoff)
		if err != nil {
			return err
		}
		if n.Attachments, err = res.RowsAffected(); err != nil {
			return err
		}
		if res, err = t.tx.Exec(`DELETE FROM message WHERE interaction_id IN (`+sel+`)`, trust, cutoff); err != nil {
			return err
		}
		if n.Messages, err = res.RowsAffected(); err != nil {
			return err
		}
		if res, err = t.tx.Exec(`DELETE FROM interaction WHERE `+cond, trust, cutoff); err != nil {
			return err
		}
		n.Interactions, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return Pruned{}, fmt.Errorf("interactions: prune %s: %w", trust, err)
	}
	return n, nil
}
