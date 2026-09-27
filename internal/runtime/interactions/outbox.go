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
}

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
	} {
		if err := addColumn(s.db, "outbox", col.name, col.decl); err != nil {
			return err
		}
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_outbox_ix ON outbox(ix, typ)`); err != nil {
		return fmt.Errorf("interactions: migrate outbox: %w", err)
	}
	return nil
}

const outboxColumns = `id,ix,to_aid,typ,body,envelope,exp,attempts,next_at,last_error,created_at,mid,digest`

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

// DueOutbox returns up to limit rows whose next attempt is at or before now (unix ms), oldest first.
func (s *Store) DueOutbox(now int64, limit int) ([]OutboxItem, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.queryOutbox(`SELECT `+outboxColumns+` FROM outbox WHERE next_at <= ? ORDER BY next_at, id LIMIT ?`, now, limit)
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

// DeleteOutbox removes a delivered or abandoned row.
func (s *Store) DeleteOutbox(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM outbox WHERE id=?`, id)
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
		var exp int64
		if err := rows.Scan(&it.ID, &it.IX, &it.ToAID, &it.Type, &it.Body, &it.Envelope, &exp,
			&it.Attempts, &it.NextAt, &it.LastError, &it.CreatedAt, &it.MID, &it.Digest); err != nil {
			return nil, err
		}
		it.Exp = uint64(exp)
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
