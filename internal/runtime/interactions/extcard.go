package interactions

// extcard.go is the requester's cache of extended cards (HPP §3.5, PAY-C14):
// for each agent, the last extended card this node fetched end to end and
// verified (anet.card/1), with what it was checked against. It is written
// outside any receive transaction (engine §4.7, last line) and read lazily:
// a row is fresh while it is at most a day old, its public_seq is not below
// the seq of the agent's public card this node last admitted, and its
// key_state_seq is the head of the agent's key history (the daemon judges
// that; this file only keeps the row). A row more than a day old is never
// used again and goes the next time any row is written.
//
// The card is not the public card: it never enters the public card's
// high-water mark (peer_identity card_seq, card_hash). A provider signs
// its extended card under its public card's seq, so the same seq with
// another payload there would be taken for a SEQ_FORK.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ExtCardLife is how long a cached extended card may be used (HPP §3.5).
const ExtCardLife = 24 * 60 * 60 * 1000 // milliseconds

// PeerExtCard is one row of peer_ext_card.
type PeerExtCard struct {
	AID string
	// Card is the extended card's bytes exactly as signed.
	Card []byte
	// PublicSeq is the card's anet-card seq, which its signer gives the
	// public card of the same moment; KeyStateSeq the key state it was
	// signed under.
	PublicSeq   uint64
	KeyStateSeq uint64
	// Variant is "list" when the card lists a skill the public card of the
	// moment did not (allow_only capabilities), "general" otherwise.
	Variant string
	// Prices are the card's price list: skill id to credits.
	Prices map[string]uint64
	// FetchedAt is when the card was verified and kept (unix ms).
	FetchedAt uint64
}

var extCardSchema = []string{
	`CREATE TABLE peer_ext_card (
	   aid           TEXT PRIMARY KEY,
	   card          BLOB NOT NULL,
	   public_seq    INTEGER NOT NULL,
	   key_state_seq INTEGER NOT NULL,
	   variant       TEXT NOT NULL,
	   prices        TEXT NOT NULL,
	   fetched_at    INTEGER NOT NULL
	 )`,
}

// PutPeerExtCard keeps c as aid's extended card, replacing the one kept
// before, and drops every row more than ExtCardLife older than c.
func (s *Store) PutPeerExtCard(c PeerExtCard) error {
	if c.AID == "" || len(c.Card) == 0 {
		return fmt.Errorf("%w: an extended card needs its agent and its bytes", ErrBadInput)
	}
	if c.Variant != "general" && c.Variant != "list" {
		return fmt.Errorf("%w: extended card variant %q", ErrBadInput, c.Variant)
	}
	prices := c.Prices
	if prices == nil {
		prices = map[string]uint64{}
	}
	pj, err := json.Marshal(prices)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO peer_ext_card (aid, card, public_seq, key_state_seq, variant, prices, fetched_at)
	    VALUES (?, ?, ?, ?, ?, ?, ?)
	    ON CONFLICT(aid) DO UPDATE SET card=excluded.card, public_seq=excluded.public_seq,
	      key_state_seq=excluded.key_state_seq, variant=excluded.variant, prices=excluded.prices,
	      fetched_at=excluded.fetched_at`,
		c.AID, c.Card, int64(c.PublicSeq), int64(c.KeyStateSeq), c.Variant, string(pj), int64(c.FetchedAt)); err != nil {
		return err
	}
	if c.FetchedAt > ExtCardLife {
		if _, err := tx.Exec(`DELETE FROM peer_ext_card WHERE fetched_at < ?`, int64(c.FetchedAt-ExtCardLife)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PeerExtCard is aid's kept extended card; found false when there is none.
func (s *Store) PeerExtCard(aid string) (c PeerExtCard, found bool, err error) {
	var pubSeq, ksSeq, at int64
	var pj string
	err = s.db.QueryRow(`SELECT aid, card, public_seq, key_state_seq, variant, prices, fetched_at
	    FROM peer_ext_card WHERE aid = ?`, aid).Scan(&c.AID, &c.Card, &pubSeq, &ksSeq, &c.Variant, &pj, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return PeerExtCard{}, false, nil
	}
	if err != nil {
		return PeerExtCard{}, false, err
	}
	c.PublicSeq, c.KeyStateSeq, c.FetchedAt = uint64(pubSeq), uint64(ksSeq), uint64(at)
	if err := json.Unmarshal([]byte(pj), &c.Prices); err != nil {
		return PeerExtCard{}, false, fmt.Errorf("interactions: the kept extended card of %s: %w", aid, err)
	}
	return c, true, nil
}

// DeletePeerExtCard forgets aid's extended card.
func (s *Store) DeletePeerExtCard(aid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM peer_ext_card WHERE aid = ?`, aid)
	return err
}
