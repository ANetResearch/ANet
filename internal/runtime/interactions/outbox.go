package interactions

// outbox.go is the outbound retry queue (A2A-DESIGN §3.5 step 4, §4.2): a
// result, status or cancel message that must reach its peer is recorded here
// in the same transaction as the state write that produced it, then sent.
// A failed attempt is retried with exponential backoff, and a daemon restart
// resumes the queue from this table.
//
// The envelope bytes are stored once sealed, so every retry sends the same
// bytes (§3.3: a retry is never a second message). A row whose envelope
// could not be sealed at enqueue time (the recipient's keys were not
// available) keeps the body and the message id it will be sealed under, and
// is sealed by the retry loop. Every row has a deadline (exp) from the
// moment it is queued, sealed or not.
//
// A row carries a digest of its body: the same body queued again for the
// same interaction and type is the row already there, not a second one.
//
// Rows for one interaction and one peer are delivered in the order they
// were queued ([redteam:F23]): a row is due only when no earlier row for
// the same (ix, to_aid) is still queued. Each row backs off on its own, so
// without the order a cancel or a follow-up queued after a delegation that
// is backing off went out first, reached a provider that did not know the
// task, and was answered TaskNotFound; the delegation came after it and
// ran.

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// OutboxItem is one queued outbound message.
type OutboxItem struct {
	ID       int64
	IX       string
	ToAID    string
	Type     string // inner type, e.g. anet.result/1
	Body     []byte // the message body, kept until the envelope is sealed
	Envelope []byte // sealed envelope bytes; empty until sealed
	// Exp is the row's deadline (unix ms): the envelope's exp once sealed,
	// and the exp it will be sealed with before that. A row from before
	// deadlines were kept for unsealed rows has 0 (see Deadline).
	Exp uint64
	// MID is the inner message id the row is sealed under (16 bytes), kept
	// so a row sealed late carries the id this node recorded for the
	// message when it was queued. Empty on older rows.
	MID []byte
	// Digest is SHA-256 of the body: the dedupe key within (IX, Type).
	// EnqueueOutbox computes it from Body when it is not given.
	Digest    []byte
	Attempts  int
	NextAt    int64 // unix ms
	LastError string
	CreatedAt int64 // unix ms
	// MaybeDelivered records that an attempt may have delivered the
	// envelope although it did not succeed: it failed in a way that may
	// still have delivered it (a direct transport that timed out waiting for
	// the far side, a hub request cut off mid-way), or it was begun and its
	// outcome never recorded (the process stopped during it, the write
	// failed). Abandoning such a row does not say the message never arrived
	// ([redteam:F12]), and a delegation in that state is not withdrawn
	// ([redteam:F23]).
	MaybeDelivered bool
}

// Bits of the maybe_delivered column.
const (
	// outboxMaybe: a finished attempt may have delivered the row.
	outboxMaybe = 1
	// outboxAttempting: an attempt was begun (BeginOutboxAttempt) and its
	// outcome not yet recorded (EndOutboxAttempt, or the row's deletion).
	// Found set when an attempt begins, it is a previous attempt whose
	// outcome was never recorded, and counts as outboxMaybe.
	outboxAttempting = 2
)

func (s *Store) migrateOutbox() error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS outbox (
		   id INTEGER PRIMARY KEY AUTOINCREMENT,
		   ix TEXT NOT NULL,
		   to_aid TEXT NOT NULL,
		   typ TEXT NOT NULL,
		   body BLOB,
		   envelope BLOB,
		   exp INTEGER NOT NULL DEFAULT 0,
		   attempts INTEGER NOT NULL DEFAULT 0,
		   next_at INTEGER NOT NULL DEFAULT 0,
		   last_error TEXT NOT NULL DEFAULT '',
		   created_at INTEGER NOT NULL
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_next ON outbox(next_at)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_to ON outbox(to_aid)`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("interactions: migrate outbox: %w", err)
		}
	}
	for _, col := range []struct{ name, decl string }{
		{"mid", "BLOB"},
		{"digest", "BLOB"},
		{"maybe_delivered", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := addColumn(s.db, "outbox", col.name, col.decl); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`CREATE INDEX IF NOT EXISTS idx_outbox_ix ON outbox(ix, typ)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_chain ON outbox(ix, to_aid, id)`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("interactions: migrate outbox: %w", err)
		}
	}
	return nil
}

// outboxHead is the condition that row o has no earlier row queued for the
// same interaction and peer: it is at the head of its queue.
const outboxHead = `NOT EXISTS (SELECT 1 FROM outbox p WHERE p.ix = o.ix AND p.to_aid = o.to_aid AND p.id < o.id)`

const outboxColumns = `id,ix,to_aid,typ,body,envelope,exp,attempts,next_at,last_error,created_at,mid,digest,maybe_delivered`

// OutboxLifetimeMS is how long a row is kept when it carries no deadline of
// its own (a row queued unsealed before deadlines were recorded): 14 days
// from when it was queued, the lifetime of every message (A2A-DESIGN §3.5).
const OutboxLifetimeMS = 14 * 24 * 3600 * 1000

// Deadline is when the row is abandoned (unix ms): its exp, or, for an
// older unsealed row without one, 14 days after it was queued.
func (it *OutboxItem) Deadline() uint64 {
	if it.Exp != 0 {
		return it.Exp
	}
	return uint64(it.CreatedAt) + OutboxLifetimeMS
}

// OutboxDigest is the dedupe digest of a row's body.
func OutboxDigest(body []byte) []byte {
	h := sha256.Sum256(body)
	return h[:]
}

// EnqueueOutbox records a message to deliver, inside the transaction that
// produced it, due immediately. It returns the row id. A row for the same
// interaction and type with the same body digest is already the message:
// its id is returned and nothing is inserted, so a message queued twice is
// sent once.
func (t *Tx) EnqueueOutbox(it OutboxItem) (int64, error) {
	return enqueueOutbox(t.tx, it)
}

// EnqueueOutbox is Tx.EnqueueOutbox outside a transaction.
func (s *Store) EnqueueOutbox(it OutboxItem) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return enqueueOutbox(s.db, it)
}

func enqueueOutbox(e execer, it OutboxItem) (int64, error) {
	if it.IX == "" || it.ToAID == "" || it.Type == "" || (len(it.Envelope) == 0 && len(it.Body) == 0) {
		return 0, fmt.Errorf("%w: outbox item needs an interaction, a recipient, a type and a body or envelope", ErrBadInput)
	}
	if len(it.Digest) == 0 && len(it.Body) > 0 {
		it.Digest = OutboxDigest(it.Body)
	}
	if len(it.Digest) > 0 {
		var prior int64
		err := e.QueryRow(`SELECT id FROM outbox WHERE ix=? AND typ=? AND digest=? ORDER BY id LIMIT 1`,
			it.IX, it.Type, it.Digest).Scan(&prior)
		switch {
		case err == nil:
			return prior, nil
		case !errors.Is(err, sql.ErrNoRows):
			return 0, err
		}
	}
	now := time.Now().UnixMilli()
	res, err := e.Exec(`INSERT INTO outbox(ix,to_aid,typ,body,envelope,exp,attempts,next_at,last_error,created_at,mid,digest)
	   VALUES(?,?,?,?,?,?,0,?,'',?,?,?)`,
		it.IX, it.ToAID, it.Type, nilIfEmpty(it.Body), nilIfEmpty(it.Envelope), int64(it.Exp), it.NextAt, now,
		nilIfEmpty(it.MID), nilIfEmpty(it.Digest))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DueOutbox returns up to limit rows whose next attempt is at or before now
// (unix ms), oldest first: each at the head of its (ix, to_aid) queue.
func (s *Store) DueOutbox(now int64, limit int) ([]OutboxItem, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.queryOutbox(`SELECT `+outboxColumns+` FROM outbox o WHERE next_at <= ? AND `+outboxHead+
		` ORDER BY next_at, id LIMIT ?`, now, limit)
}

// DueOutboxIDs is DueOutbox without the rows: only their ids. The retry
// loop reads each row again under its lock, and a batch of rows carrying
// their envelopes (a delegation with files is up to the hub's 96 MiB cap)
// is not held in memory at once for that.
func (s *Store) DueOutboxIDs(now int64, limit int) ([]int64, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id FROM outbox o WHERE next_at <= ? AND `+outboxHead+
		` ORDER BY next_at, id LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// OutboxAhead returns the id of the earliest row queued before row id for
// the same interaction and peer, if there is one: the row that has to be
// delivered (or abandoned) before id goes out.
func (s *Store) OutboxAhead(id int64) (int64, bool, error) {
	var ahead int64
	err := s.db.QueryRow(`SELECT p.id FROM outbox o JOIN outbox p ON p.ix = o.ix AND p.to_aid = o.to_aid AND p.id < o.id
	   WHERE o.id = ? ORDER BY p.id LIMIT 1`, id).Scan(&ahead)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return ahead, true, nil
}

// OutboxHas reports whether row id is still queued (without reading its
// envelope).
func (s *Store) OutboxHas(id int64) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM outbox WHERE id=?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// OutboxQueued reports whether row id is still queued and, if so, whether
// an attempt may have delivered it (OutboxItem.MaybeDelivered), read inside
// the transaction.
func (t *Tx) OutboxQueued(id int64) (queued, maybeDelivered bool, err error) {
	var maybe int64
	err = t.tx.QueryRow(`SELECT maybe_delivered FROM outbox WHERE id=?`, id).Scan(&maybe)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, maybe != 0, nil
}

// DeleteOutboxQueue removes every row queued for one interaction and peer,
// inside the transaction, and returns how many there were.
func (t *Tx) DeleteOutboxQueue(ix, toAID string) (int64, error) {
	res, err := t.tx.Exec(`DELETE FROM outbox WHERE ix=? AND to_aid=?`, ix, toAID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Outbox returns the rows queued for an interaction, oldest first.
func (s *Store) Outbox(ix string) ([]OutboxItem, error) {
	return s.queryOutbox(`SELECT `+outboxColumns+` FROM outbox WHERE ix=? ORDER BY id`, ix)
}

// GetOutbox returns one row, or ErrNotFound.
func (s *Store) GetOutbox(id int64) (*OutboxItem, error) {
	items, err := s.queryOutbox(`SELECT `+outboxColumns+` FROM outbox WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrNotFound
	}
	return &items[0], nil
}

// OutboxLen returns the number of queued rows.
func (s *Store) OutboxLen() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&n)
	return n, err
}

// SetOutboxEnvelope stores the sealed envelope of a row and its exp; the
// body is no longer needed and is cleared.
func (s *Store) SetOutboxEnvelope(id int64, envelope []byte, exp uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE outbox SET envelope=?, exp=?, body=NULL WHERE id=?`, envelope, int64(exp), id)
	return err
}

// RescheduleOutbox records a failed attempt and when to try again.
func (s *Store) RescheduleOutbox(id int64, attempts int, nextAt int64, lastErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(lastErr) > 512 {
		lastErr = lastErr[:512]
	}
	_, err := s.db.Exec(`UPDATE outbox SET attempts=?, next_at=?, last_error=? WHERE id=?`, attempts, nextAt, lastErr, id)
	return err
}

// BeginOutboxAttempt records that an attempt at a row is about to send it,
// before anything is sent: until EndOutboxAttempt (or the row's deletion)
// records the outcome, the row counts as possibly delivered
// (OutboxItem.MaybeDelivered). A process that stops during the attempt, or
// a write of its outcome that fails, then leaves a row that says so rather
// than one that says no attempt reached anybody ([redteam:F12],
// [redteam:F23]). The caller holds the row's attempt lock, so a mark found
// already set is such an unrecorded attempt, and is kept as "may have
// delivered".
func (s *Store) BeginOutboxAttempt(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE outbox SET maybe_delivered = maybe_delivered | ? | ((maybe_delivered & ?) >> 1) WHERE id=?`,
		outboxAttempting, outboxAttempting, id)
	return err
}

// EndOutboxAttempt records a failed attempt, when to try again, and
// whether any attempt so far may have delivered the row.
func (s *Store) EndOutboxAttempt(id int64, attempts int, nextAt int64, lastErr string, maybeDelivered bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(lastErr) > 512 {
		lastErr = lastErr[:512]
	}
	maybe := 0
	if maybeDelivered {
		maybe = outboxMaybe
	}
	_, err := s.db.Exec(`UPDATE outbox SET attempts=?, next_at=?, last_error=?, maybe_delivered=? WHERE id=?`,
		attempts, nextAt, lastErr, maybe, id)
	return err
}

// DeleteOutbox removes a delivered or abandoned row.
func (s *Store) DeleteOutbox(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM outbox WHERE id=?`, id)
	return err
}

// DeleteOutbox is Store.DeleteOutbox inside the transaction: an abandoned
// row goes with the state change its abandonment makes.
func (t *Tx) DeleteOutbox(id int64) error {
	_, err := t.tx.Exec(`DELETE FROM outbox WHERE id=?`, id)
	return err
}

func (s *Store) queryOutbox(q string, args ...any) ([]OutboxItem, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var it OutboxItem
		var exp, maybe int64
		if err := rows.Scan(&it.ID, &it.IX, &it.ToAID, &it.Type, &it.Body, &it.Envelope, &exp,
			&it.Attempts, &it.NextAt, &it.LastError, &it.CreatedAt, &it.MID, &it.Digest, &maybe); err != nil {
			return nil, err
		}
		it.Exp, it.MaybeDelivered = uint64(exp), maybe != 0
		out = append(out, it)
	}
	return out, rows.Err()
}

// MessageByMsgID returns the message of an interaction stored under msgID,
// or ErrNotFound. The retry queue uses it to learn what an abandoned row
// was (its MID names the message, A2A-DESIGN 0017 Q9).
func (s *Store) MessageByMsgID(interactionID, msgID string) (*Message, error) {
	if interactionID == "" || msgID == "" {
		return nil, ErrNotFound
	}
	var m Message
	err := s.db.QueryRow(`SELECT seq,interaction_id,sender_aid,kind,body,msg_id,metadata,created_at FROM message
	   WHERE interaction_id=? AND msg_id=?`, interactionID, msgID).
		Scan(&m.Seq, &m.InteractionID, &m.SenderAID, &m.Kind, &m.Body, &m.MsgID, &m.Metadata, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}
