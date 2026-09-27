package interactions

// peers.go holds the two tables the wire-2 receive path needs next to the
// interaction log (A2A-DESIGN §3.6, §3.8):
//
//   - replay: one row per accepted inner message, keyed by (sender AID,
//     message id), kept until the message's expiry. It is written in the
//     same SQLite transaction as the business write the message caused, so
//     a crash or a store error never leaves a message recorded as handled
//     when its effect was not stored, or stored twice when it was.
//   - peer_identity: the persisted key history (KEL) and encryption key set
//     of each peer this node has an authorized relationship with. It holds
//     the high-water marks that make a truncated KEL or an older key set
//     detectable across restarts.
//
// Both live in interactions.db because §3.6 step 10 requires the replay row
// to commit together with the interaction and message rows.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// execer is what a write helper needs from either the database handle or an
// open transaction. *sql.DB and *sql.Tx both satisfy it.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Tx is one write transaction over the store, opened by Update.
//
// The methods mirror the Store writers the receive path uses. They run on
// the transaction, so every write made through one Tx commits or rolls
// back together. A Tx must not be used after the function passed to Update
// returns.
type Tx struct {
	tx  *sql.Tx
	now int64 // unix ms at the start of the transaction, for state_at
}

// Update runs fn inside one SQLite transaction and commits it when fn
// returns nil; any error from fn rolls every write back and is returned
// unchanged.
//
// Writes are serialized on the store mutex for the whole of fn, so fn must
// only touch the database: no network calls, no signing, no waiting.
// Readers are not blocked (WAL).
func (s *Store) Update(fn func(*Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("interactions: begin: %w", err)
	}
	if err := fn(&Tx{tx: tx, now: s.nowMS()}); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("interactions: commit: %w", err)
	}
	return nil
}

// Get reads one interaction inside the transaction.
func (t *Tx) Get(id string) (*Interaction, error) {
	return scanOne(t.tx.QueryRow(`SELECT `+ixColumns+` FROM interaction WHERE id=?`, id))
}

// Put is Store.Put inside the transaction.
func (t *Tx) Put(id string, role Role, peerAID, goal, requestCID string, requestDoc []byte) error {
	return create(t.tx, New{ID: id, Role: role, PeerAID: peerAID, Goal: goal, RequestCID: requestCID, RequestDoc: requestDoc}, t.now)
}

// Create is Store.Create inside the transaction.
func (t *Tx) Create(n New) error { return create(t.tx, n, t.now) }

// AddMessage is Store.AddMessage inside the transaction.
func (t *Tx) AddMessage(interactionID, senderAID, kind, body string) (int64, error) {
	seq, _, err := addMessage(t.tx, MessageRecord{InteractionID: interactionID, SenderAID: senderAID, Kind: kind, Body: body})
	return seq, err
}

// AddMessageID is Store.AddMessageID inside the transaction.
func (t *Tx) AddMessageID(interactionID, senderAID, kind, body, msgID string) (int64, bool, error) {
	return addMessage(t.tx, MessageRecord{InteractionID: interactionID, SenderAID: senderAID, Kind: kind, Body: body, MsgID: msgID})
}

// AddMessageRecord is Store.AddMessageRecord inside the transaction.
func (t *Tx) AddMessageRecord(m MessageRecord) (int64, bool, error) { return addMessage(t.tx, m) }

// SetResult is Store.SetResult inside the transaction.
func (t *Tx) SetResult(id string, result []byte, resultCID string, receipt []byte, verified Verification) error {
	return finish(t.tx, id, Finish{State: StateCompleted, Result: result, ResultCID: resultCID, Receipt: receipt, Verified: verified}, t.now)
}

// SetFailed is Store.SetFailed inside the transaction.
func (t *Tx) SetFailed(id string, detail []byte) error {
	return finish(t.tx, id, Finish{State: StateFailed, Result: detail, Verified: VerificationUnknown}, t.now)
}

// Finish is Store.Finish inside the transaction.
func (t *Tx) Finish(id string, f Finish) error { return finish(t.tx, id, f, t.now) }

// SetState is Store.SetState inside the transaction.
func (t *Tx) SetState(id string, st State) (bool, error) { return setState(t.tx, id, st, t.now) }

// SetLateResult is Store.SetLateResult inside the transaction, so a result
// that arrives after the task ended commits with its replay row
// (A2A-DESIGN §3.6 step 10, §4.2).
func (t *Tx) SetLateResult(id string, result []byte, resultCID string, receipt []byte, verified Verification) (bool, error) {
	return setLateResult(t.tx, id, result, resultCID, receipt, verified)
}

// SetPeerKeys is Store.SetPeerKeys inside the transaction.
func (t *Tx) SetPeerKeys(id string, kel, keys []byte) error { return setPeerKeys(t.tx, id, kel, keys) }

// SetEndRequested is Store.SetEndRequested inside the transaction.
func (t *Tx) SetEndRequested(id, by string) error { return setEndRequested(t.tx, id, by) }

// ClaimReplay records that the message (from, mid) has been handled, valid
// until exp (unix ms). It reports false when a row for (from, mid) already
// exists, in which case the caller must treat the message as a duplicate
// and roll back whatever else it wrote (return an error from the Update
// function, or write nothing else).
func (t *Tx) ClaimReplay(from string, mid []byte, exp uint64) (bool, error) {
	if from == "" || len(mid) == 0 {
		return false, fmt.Errorf("%w: replay row needs a sender and a message id", ErrBadInput)
	}
	res, err := t.tx.Exec(`INSERT OR IGNORE INTO replay(from_aid, mid, exp) VALUES(?,?,?)`, from, mid, int64(exp))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ReplaySeen reports whether a replay row exists for (from, mid).
func (s *Store) ReplaySeen(from string, mid []byte) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM replay WHERE from_aid=? AND mid=?`, from, mid).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// PurgeReplay deletes replay rows whose expiry is before cutoff (unix ms)
// and reports how many it removed. A message past its expiry is refused by
// the receive path's time check (§3.6 step 5), so its row is no longer
// needed to refuse a replay.
func (s *Store) PurgeReplay(cutoff uint64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM replay WHERE exp < ?`, int64(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Pinned reasons for a peer_identity row (§3.8). A row with a non-empty
// reason is never evicted.
const (
	PinAllow    = "allow"    // the peer is on the allow list
	PinTrust    = "trust"    // the peer is on the trust list
	PinHub      = "hub"      // the KEL of a hub this node talks to
	PinIssuer   = "issuer"   // a KEL a module relies on to verify signed objects
	PinOutbound = "outbound" // this node initiated contact with the peer
)

// PeerIdentity is one peer_identity row.
type PeerIdentity struct {
	AID string
	// KEL is the peer's key history (identity.MarshalKEL). It is only
	// replaced by a KEL that extends it; the daemon enforces that rule.
	KEL    []byte
	KELLen int
	// KeySet is the peer's SignedEncKeySet encoding, and KeySetSeq the seq
	// inside it. Replaced only under the consumer high-water rule.
	KeySet    []byte
	KeySetSeq uint64
	// KeysCheckedAt is when the key set was last fetched from a hub or
	// re-verified, unix ms. The sender uses it to pace hub revalidation.
	KeysCheckedAt int64
	// CardSeq and CardHash are the high-water mark of the peer's A2A
	// network card: its params.seq and the payload hash (a2acard.Mark)
	// of the card admitted at that seq. Replaced only under the three-way
	// high-water rule (A2A-DESIGN §3.8, §10.3); CardHash is empty on a row
	// written before it was kept.
	CardSeq      uint64
	CardHash     []byte
	PinnedReason string
	UpdatedAt    int64 // unix ms of the last write
}

// defaultPeerIdentityCap bounds the unpinned rows of peer_identity. The
// bound is large on purpose: eviction only happens on authorized writes,
// never because of inbound traffic, and a row that is evicted loses the
// rollback protection for that peer (§21 item 6).
const defaultPeerIdentityCap = 10000

// SetPeerIdentityCap changes the bound on unpinned peer_identity rows.
// n <= 0 restores the default. Intended for tests that need to exercise
// eviction without writing ten thousand rows.
func (s *Store) SetPeerIdentityCap(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 {
		n = defaultPeerIdentityCap
	}
	s.peerCap = n
}

const peerColumns = `aid,kel,kel_len,keyset,keyset_seq,keys_checked_at,card_seq,card_hash,pinned_reason,updated_at`

// PeerIdentity returns the stored row for aid, or ErrNotFound.
func (s *Store) PeerIdentity(aid string) (*PeerIdentity, error) {
	return getPeer(s.db, aid)
}

func getPeer(e execer, aid string) (*PeerIdentity, error) {
	var p PeerIdentity
	var seq, card int64
	err := e.QueryRow(`SELECT `+peerColumns+` FROM peer_identity WHERE aid=?`, aid).Scan(
		&p.AID, &p.KEL, &p.KELLen, &p.KeySet, &seq, &p.KeysCheckedAt, &card, &p.CardHash, &p.PinnedReason, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.KeySetSeq, p.CardSeq = uint64(seq), uint64(card)
	return &p, nil
}

// UpdatePeerIdentity reads the row for aid (nil when absent), passes a copy
// to fn, and writes back what fn returns, all in one transaction. fn
// returning nil leaves the row unchanged. fn decides what may change (the
// KEL extension rule, the key set high-water rule); this method only makes
// the read-decide-write atomic.
//
// A write never clears a pinned reason: an empty PinnedReason in fn's
// result keeps the stored one. When the write inserted a new unpinned row
// and the unpinned rows now exceed the cap, the least recently written
// unpinned rows that no active interaction refers to are deleted.
func (s *Store) UpdatePeerIdentity(aid string, fn func(cur *PeerIdentity) (*PeerIdentity, error)) error {
	if aid == "" {
		return fmt.Errorf("%w: aid required", ErrBadInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := getPeer(tx, aid)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	var in *PeerIdentity
	if cur != nil {
		c := *cur
		in = &c
	}
	next, err := fn(in)
	if err != nil {
		return err
	}
	if next == nil {
		return nil
	}
	next.AID = aid
	if next.PinnedReason == "" && cur != nil {
		next.PinnedReason = cur.PinnedReason
	}
	next.UpdatedAt = time.Now().UnixMilli()
	if _, err := tx.Exec(`INSERT INTO peer_identity(`+peerColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(aid) DO UPDATE SET kel=excluded.kel, kel_len=excluded.kel_len, keyset=excluded.keyset,
		  keyset_seq=excluded.keyset_seq, keys_checked_at=excluded.keys_checked_at, card_seq=excluded.card_seq,
		  card_hash=excluded.card_hash, pinned_reason=excluded.pinned_reason, updated_at=excluded.updated_at`,
		next.AID, next.KEL, next.KELLen, next.KeySet, int64(next.KeySetSeq), next.KeysCheckedAt,
		int64(next.CardSeq), next.CardHash, next.PinnedReason, next.UpdatedAt); err != nil {
		return err
	}
	if cur == nil && next.PinnedReason == "" {
		if err := evictPeers(tx, s.cap()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// cap returns the configured bound; the caller holds s.mu.
func (s *Store) cap() int {
	if s.peerCap <= 0 {
		return defaultPeerIdentityCap
	}
	return s.peerCap
}

// activeInteractionSQL selects interactions that are not finished. It is
// the single place the eviction rule names the interaction state column;
// the terminal set is terminalSQL, shared with the guarded state writes.
const activeInteractionSQL = `SELECT 1 FROM interaction i WHERE i.peer_aid = p.aid AND i.state NOT IN ` + terminalSQL

// evictPeers deletes unpinned rows beyond limit, oldest write first,
// skipping rows an active interaction, a queued outbound message or a held
// delegation refers to (A2A-DESIGN §3.8).
func evictPeers(e execer, limit int) error {
	var n int
	if err := e.QueryRow(`SELECT COUNT(*) FROM peer_identity WHERE pinned_reason = ''`).Scan(&n); err != nil {
		return err
	}
	if n <= limit {
		return nil
	}
	_, err := e.Exec(`DELETE FROM peer_identity WHERE aid IN (
		SELECT p.aid FROM peer_identity p
		 WHERE p.pinned_reason = '' AND NOT EXISTS (`+activeInteractionSQL+`)
		   AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.to_aid = p.aid)
		   AND NOT EXISTS (SELECT 1 FROM pending q WHERE q.from_aid = p.aid)
		 ORDER BY p.updated_at ASC, p.rowid ASC LIMIT ?)`, n-limit)
	return err
}

// PeerIdentityCount returns the number of rows, pinned and unpinned.
func (s *Store) PeerIdentityCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM peer_identity`).Scan(&n)
	return n, err
}

// migrateWire2 creates the replay and peer_identity tables.
func (s *Store) migrateWire2() error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS replay (
		   from_aid TEXT NOT NULL,
		   mid BLOB NOT NULL,
		   exp INTEGER NOT NULL,
		   PRIMARY KEY (from_aid, mid)
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_replay_exp ON replay(exp)`,
		`CREATE TABLE IF NOT EXISTS peer_identity (
		   aid TEXT PRIMARY KEY,
		   kel BLOB,
		   kel_len INTEGER NOT NULL DEFAULT 0,
		   keyset BLOB,
		   keyset_seq INTEGER NOT NULL DEFAULT 0,
		   keys_checked_at INTEGER NOT NULL DEFAULT 0,
		   card_seq INTEGER NOT NULL DEFAULT 0,
		   pinned_reason TEXT NOT NULL DEFAULT '',
		   updated_at INTEGER NOT NULL DEFAULT 0
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_peer_lru ON peer_identity(pinned_reason, updated_at)`,
		// The eviction rule asks, per candidate row, whether an active
		// interaction names the peer; without this index that is a scan of
		// the interaction table per row.
		`CREATE INDEX IF NOT EXISTS idx_ix_peer ON interaction(peer_aid)`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("interactions: migrate wire 2: %w", err)
		}
	}
	return s.migratePeerCardHash()
}
