package interactions

// peercard.go keeps the consumer side of the A2A card high-water rule
// across restarts (A2A-DESIGN §3.8, §10.3; plan 0014 B3-09): for a peer
// this node has a peer_identity row for, the params.seq and payload hash of
// the last network card it admitted. A card is checked against that mark
// when this node reads it from a hub, so a hub serving an older card, or a
// second card under the same seq, is refused after a restart as it is
// before one.
//
// Only an existing row is written. peer_identity rows are made in an
// authorized context (§3.8); reading a directory is not one, so the mark
// of a stranger's card stays in the daemon's bounded memory cache and never
// creates a row here. The write does not touch updated_at either: the LRU
// order is by authorized writes, and a directory read is not one.

import (
	"errors"
	"fmt"
	"strings"
)

// migratePeerCardHash adds card_hash to a peer_identity table created
// before it was kept.
func (s *Store) migratePeerCardHash() error {
	if _, err := s.db.Exec(`ALTER TABLE peer_identity ADD COLUMN card_hash BLOB`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("interactions: migrate peer card hash: %w", err)
	}
	return nil
}

// UpdatePeerCardMark reads the card high-water mark stored for aid and
// passes it to fn, then stores what fn returns, in one transaction. fn gets
// the stored seq and hash (hash empty when none was kept) and returns the
// mark to keep and whether to write it; an error from fn is returned and
// nothing is written. found is false, and fn is not called, when aid has
// no peer_identity row.
func (s *Store) UpdatePeerCardMark(aid string, fn func(seq uint64, hash []byte) (newSeq uint64, newHash []byte, write bool, err error)) (found bool, err error) {
	if aid == "" {
		return false, fmt.Errorf("%w: aid required", ErrBadInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := getPeer(tx, aid)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	seq, hash, write, err := fn(cur.CardSeq, cur.CardHash)
	if err != nil || !write {
		return true, err
	}
	if _, err := tx.Exec(`UPDATE peer_identity SET card_seq=?, card_hash=? WHERE aid=?`,
		int64(seq), hash, aid); err != nil {
		return true, err
	}
	return true, tx.Commit()
}
