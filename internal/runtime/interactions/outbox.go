package interactions

// outbox.go is the outbound queue (engine §2.1, core §2.16): one row per
// message this node must deliver reliably — a delegation, a conversation
// message, a status, a result, a notice — and one version per sealing of
// it, each with the ack_key that checks the recipient's delivery token.
//
// The queue is the engine's (engine ENG-G1): only internal/daemon's
// outbox_*.go, reseal.go and dack_status.go call the methods here that
// write it (TestOnlyTheEngineWritesTheOutbox). Every write is one
// transaction of the caller's.
//
//   - A row is written by EnqueueOutbox, unsealed (ENG-Q3): its body, under
//     the task's data key (column outbox.body, FS-L2), and the identity of
//     the message — mid, ts, exp — which every version of it carries
//     unchanged (ENG-Q2, SI-82).
//   - An attempt that seals writes the version and its ack_key in the
//     transaction that marks the row attempting (SealOutboxVersion, A8),
//     before any byte of it leaves the process (SI-83); one that does not
//     seal marks it with BeginOutboxAttempt. EndOutboxAttempt records the
//     outcome (A12). A version replaced before it was handed over goes in
//     the transaction that replaces it (ENG-S5).
//   - A row goes only when it is confirmed or given up (ENG-E1), with all
//     its versions, in one transaction (DeleteOutboxRow, ENG-S6). The
//     database is opened with secure_delete, so the ack_keys' bytes do not
//     stay in the file.

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

// OutboxItem is a message to queue (EnqueueOutbox).
type OutboxItem struct {
	// IX, ToAID and Type are the interaction, the recipient and the inner
	// type; Kind is the engine's row kind: the ChatMsg kind of a message,
	// the state or notice of a status, "" otherwise.
	IX, ToAID, Type, Kind string
	// MID is the inner message id (16 bytes): the message's id on the
	// wire, under which this node records it too (an internal note item).
	MID []byte
	// Body is the inner body. It is stored under the task's data key.
	Body []byte
	// TS and Exp are the inner ts and exp (unix ms), fixed now (ENG-Q2).
	TS, Exp uint64
	// LastError is what the row starts with: home_unreachable when the
	// recipient could not be looked up (engine ENG-Q5).
	LastError string
}

// OutboxRow is one outbox row as read, without its body and envelope.
type OutboxRow struct {
	ID int64
	// IX, ToAID, Type and Kind say what the row is (OutboxItem).
	IX, ToAID, Type, Kind string
	// MID is the inner message id every version carries.
	MID []byte
	// TS and Exp are the inner ts and exp (unix ms).
	TS, Exp uint64
	// Attempts counts failed attempts; NextAt is the backoff (unix ms).
	Attempts int
	NextAt   int64
	// LastError is a code, then ": " and the detail (engine ENG-S4).
	LastError string
	CreatedAt int64
	// HandedAt is when a byte of the row first left the process; 0 never.
	HandedAt int64
	// Attempting: a version was written and may be on its way (A8).
	Attempting bool
	// Frozen: the delegation's task was canceled; not transmitted again.
	Frozen bool
	// CheckAt is the next status query (unix ms), 0 for none; CheckN the
	// queued answers since the last hand-over (ENG-D1).
	CheckAt int64
	CheckN  int
	// CurVer is the current version, 0 before the first sealing.
	CurVer uint64
	// SealedKID and SealedKidNA are the recipient's epoch key the current
	// version is sealed to and its not_after; SealedKSS and SealedNoticeAt
	// the key state it was signed under and its notice's observed_at
	// (engine ENG-S3).
	SealedKID      []byte
	SealedKidNA    uint64
	SealedKSS      uint64
	SealedNoticeAt uint64
}

// OutboxVer is one version of a row (outbox_ver).
type OutboxVer struct {
	RowID int64
	Ver   uint64
	// AckKey is the version's first-layer export (seal.AckKey bytes).
	AckKey []byte
	// HandedAt is when it was first handed over; 0 never.
	HandedAt int64
	// RID is the hub's id for it; nil when no hub took it with one.
	RID []byte
	// Hub is the hub (URL) it was sent to; ViaHub the hub it was queued at
	// when another (DLV-W-12a).
	Hub, ViaHub string
	// HubState is the last status answer: "", OutboxQueued or
	// OutboxUnknown.
	HubState string
}

// The hub states a version can have (the hub_state column): what the last
// status query said of it, "" before the first.
const (
	OutboxQueued  = "queued"
	OutboxUnknown = "unknown"
)

// MaxHandedVersions is how many handed versions a row keeps (engine
// ENG-R9): past it the earliest goes, never the current one.
const MaxHandedVersions = 64

// ColOutboxDigest names the dedupe digest of a row: an HMAC of the body
// under the task's mac_key, so the column says nothing of the content to
// whoever does not hold the key.
const ColOutboxDigest = "outbox.digest"

// outboxSchema is the v0.3.0 outbound queue. The columns are engine
// §2.1's and no others (ENG-S1): no access column, the credential is made
// again for every attempt (ENG-I2).
var outboxSchema = []string{
	`CREATE TABLE outbox (
	   id INTEGER PRIMARY KEY AUTOINCREMENT,
	   ix TEXT NOT NULL,
	   to_aid TEXT NOT NULL,
	   typ TEXT NOT NULL,
	   kind TEXT NOT NULL DEFAULT '',
	   mid BLOB NOT NULL,
	   digest BLOB NOT NULL,
	   body BLOB,
	   envelope BLOB,
	   cur_ver INTEGER NOT NULL DEFAULT 0,
	   ts INTEGER NOT NULL,
	   exp INTEGER NOT NULL,
	   attempts INTEGER NOT NULL DEFAULT 0,
	   next_at INTEGER NOT NULL DEFAULT 0,
	   last_error TEXT NOT NULL DEFAULT '',
	   created_at INTEGER NOT NULL,
	   handed_at INTEGER,
	   attempting INTEGER NOT NULL DEFAULT 0,
	   frozen INTEGER NOT NULL DEFAULT 0,
	   check_at INTEGER,
	   check_n INTEGER NOT NULL DEFAULT 0,
	   sealed_kid BLOB,
	   sealed_kid_na INTEGER NOT NULL DEFAULT 0,
	   sealed_kss INTEGER NOT NULL DEFAULT 0,
	   sealed_notice_at INTEGER NOT NULL DEFAULT 0
	 )`,
	`CREATE INDEX idx_outbox_next ON outbox(next_at)`,
	`CREATE INDEX idx_outbox_check ON outbox(check_at) WHERE check_at IS NOT NULL`,
	`CREATE INDEX idx_outbox_exp ON outbox(exp)`,
	`CREATE INDEX idx_outbox_chain ON outbox(ix, to_aid, id)`,
	`CREATE INDEX idx_outbox_dedupe ON outbox(ix, to_aid, typ, digest)`,
	`CREATE TABLE outbox_ver (
	   row_id INTEGER NOT NULL,
	   ver INTEGER NOT NULL,
	   ack_key BLOB NOT NULL,
	   handed_at INTEGER,
	   rid BLOB,
	   via_hub TEXT NOT NULL DEFAULT '',
	   hub TEXT NOT NULL DEFAULT '',
	   hub_state TEXT NOT NULL DEFAULT '',
	   PRIMARY KEY(row_id, ver)
	 )`,
}

// outboxCols are the columns OutboxRow reads, in its order.
const outboxCols = `id,ix,to_aid,typ,kind,mid,ts,exp,attempts,next_at,last_error,created_at,
	COALESCE(handed_at,0),attempting,frozen,COALESCE(check_at,0),check_n,cur_ver,sealed_kid,sealed_kid_na,sealed_kss,sealed_notice_at`

// verCols are the columns OutboxVer reads, in its order.
const verCols = `row_id,ver,ack_key,COALESCE(handed_at,0),rid,hub,via_hub,hub_state`

// rowScanner is *sql.Row or *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

func scanOutboxRow(r rowScanner) (OutboxRow, error) {
	var o OutboxRow
	var ts, exp, kidNA, kss, noticeAt, curVer int64
	var attempting, frozen int
	err := r.Scan(&o.ID, &o.IX, &o.ToAID, &o.Type, &o.Kind, &o.MID, &ts, &exp, &o.Attempts, &o.NextAt,
		&o.LastError, &o.CreatedAt, &o.HandedAt, &attempting, &frozen, &o.CheckAt, &o.CheckN, &curVer,
		&o.SealedKID, &kidNA, &kss, &noticeAt)
	o.TS, o.Exp, o.SealedKidNA, o.SealedKSS, o.SealedNoticeAt = uint64(ts), uint64(exp), uint64(kidNA),
		uint64(kss), uint64(noticeAt)
	o.CurVer, o.Attempting, o.Frozen = uint64(curVer), attempting != 0, frozen != 0
	return o, err
}

func scanVer(r rowScanner) (OutboxVer, error) {
	var v OutboxVer
	var ver int64
	err := r.Scan(&v.RowID, &ver, &v.AckKey, &v.HandedAt, &v.RID, &v.Hub, &v.ViaHub, &v.HubState)
	v.Ver = uint64(ver)
	return v, err
}

// outboxDigest is the dedupe key of a body queued for task ix: an HMAC
// under the task's mac_key, which the queue keeps while it has rows
// (engine ENG-S7).
func outboxDigest(e execer, ix string, body []byte) ([]byte, error) {
	k, err := macKey(e, ix)
	if err != nil {
		return nil, err
	}
	defer clear(k)
	m := hmac.New(sha256.New, k)
	m.Write([]byte(ColOutboxDigest))
	m.Write([]byte{0})
	m.Write(body)
	return m.Sum(nil), nil
}

// EnqueueOutbox queues a message in the caller's transaction (engine §2.2)
// and returns its row; fresh is false when the same message was queued
// already: a row of the same task, recipient, type and kind with the same
// body (ENG-Q4), which is returned and nothing is written. The task gets a
// data key here when it has none (a notice about a delegation no longer
// held); the row's deletion releases it (DeleteOutboxRow).
func (t *Tx) EnqueueOutbox(it OutboxItem) (id int64, fresh bool, err error) {
	if it.IX == "" || it.ToAID == "" || it.Type == "" || len(it.MID) == 0 || it.Body == nil || it.Exp == 0 {
		return 0, false, fmt.Errorf("%w: an outbox row needs an interaction, a recipient, a type, a message id, a body and an exp",
			ErrBadInput)
	}
	b, err := ensureDEK(t.tx, it.IX, t.now)
	if err != nil {
		return 0, false, err
	}
	digest, err := outboxDigest(t.tx, it.IX, it.Body)
	if err != nil {
		return 0, false, err
	}
	var prior int64
	switch err := t.tx.QueryRow(`SELECT id FROM outbox WHERE ix=? AND to_aid=? AND typ=? AND kind=? AND digest=? ORDER BY id LIMIT 1`,
		it.IX, it.ToAID, it.Type, it.Kind, digest).Scan(&prior); {
	case err == nil:
		return prior, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, false, err
	}
	body, err := b.put(ColOutboxBody, it.Body)
	if err != nil {
		return 0, false, err
	}
	lastErr := it.LastError
	if len(lastErr) > maxLastError {
		lastErr = lastErr[:maxLastError]
	}
	res, err := t.tx.Exec(`INSERT INTO outbox(ix,to_aid,typ,kind,mid,digest,body,ts,exp,last_error,created_at)
	   VALUES(?,?,?,?,?,?,?,?,?,?,?)`, it.IX, it.ToAID, it.Type, it.Kind, it.MID, digest, body,
		int64(it.TS), int64(it.Exp), lastErr, t.now)
	if err != nil {
		return 0, false, err
	}
	id, err = res.LastInsertId()
	return id, true, err
}

// ErrOutboxFrozen is an attempt's A8 on a row frozen since it was read: a
// canceled delegation is not transmitted again (engine ENG-O6).
var ErrOutboxFrozen = errors.New("interactions: the outbox row is frozen")

// maxLastError bounds the stored last_error.
const maxLastError = 512

// OutboxState returns row id and its versions, oldest version first, or
// ErrNotFound.
func (s *Store) OutboxState(id int64) (*OutboxRow, []OutboxVer, error) { return outboxState(s.db, id) }

// OutboxState is Store.OutboxState inside the transaction.
func (t *Tx) OutboxState(id int64) (*OutboxRow, []OutboxVer, error) { return outboxState(t.tx, id) }

func outboxState(q querier, id int64) (*OutboxRow, []OutboxVer, error) {
	o, err := scanOutboxRow(q.QueryRow(`SELECT `+outboxCols+` FROM outbox WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	vers, err := outboxVers(q, id)
	if err != nil {
		return nil, nil, err
	}
	return &o, vers, nil
}

func outboxVers(q querier, id int64) ([]OutboxVer, error) {
	rows, err := q.Query(`SELECT `+verCols+` FROM outbox_ver WHERE row_id=? ORDER BY ver`, id)
	if err != nil {
		return nil, err
	}
	var out []OutboxVer
	for rows.Next() {
		v, err := scanVer(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, v)
	}
	return out, closeRows(rows)
}

// OutboxContent returns row id's body, opened under its task's data key,
// and its envelope: the current version's bytes without outer key 7, nil
// before the first sealing. ErrNotFound when the row is gone,
// ErrContentGone when the task's data key is: the body can no longer be
// read, and the row cannot be sealed.
func (s *Store) OutboxContent(id int64) (body, envelope []byte, err error) {
	var ix string
	var stored []byte
	err = s.db.QueryRow(`SELECT ix,body,envelope FROM outbox WHERE id=?`, id).Scan(&ix, &stored, &envelope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	b, err := boxOf(s.db, ix)
	if err != nil {
		return nil, nil, err
	}
	if body, err = b.get(ColOutboxBody, stored); err != nil {
		return nil, nil, fmt.Errorf("%w: outbox row %d: %v", ErrContentGone, id, err)
	}
	return body, envelope, nil
}

// OutboxQueue returns the rows queued for task ix to `to`, in the order
// they were queued: what the ordering rules read (engine ENG-O1).
func (s *Store) OutboxQueue(ix, to string) ([]OutboxRow, error) {
	return queryOutboxRows(s.db, `SELECT `+outboxCols+` FROM outbox WHERE ix=? AND to_aid=? ORDER BY id`, ix, to)
}

// OutboxRows returns the rows queued for task ix, in the order they were
// queued.
func (s *Store) OutboxRows(ix string) ([]OutboxRow, error) {
	return queryOutboxRows(s.db, `SELECT `+outboxCols+` FROM outbox WHERE ix=? ORDER BY id`, ix)
}

// OutboxRows is Store.OutboxRows inside the transaction.
func (t *Tx) OutboxRows(ix string) ([]OutboxRow, error) {
	return queryOutboxRows(t.tx, `SELECT `+outboxCols+` FROM outbox WHERE ix=? ORDER BY id`, ix)
}

// AllOutboxRows returns every queued row, in the order queued. Closing
// every task at once reads it (closeInteractionsTx), as does a test.
func (t *Tx) AllOutboxRows() ([]OutboxRow, error) {
	return queryOutboxRows(t.tx, `SELECT `+outboxCols+` FROM outbox ORDER BY id`)
}

// OutboxVersions returns the versions of row id inside the transaction,
// oldest first.
func (t *Tx) OutboxVersions(id int64) ([]OutboxVer, error) { return outboxVers(t.tx, id) }

func queryOutboxRows(q querier, query string, args ...any) ([]OutboxRow, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var out []OutboxRow
	for rows.Next() {
		o, err := scanOutboxRow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, o)
	}
	return out, closeRows(rows)
}

// outboxHeld is the condition that row o waits for a row queued before it
// for the same task and peer, as the scan reads rules 1 and 2 of the order
// (engine ENG-O1, O2): an earlier row not handed over yet (an attempt at it
// may be under way), or for a text or end_request message the task's
// delegation, which goes only when it is confirmed. The attempt decides
// with the engine's orderBlocked; this only keeps rows that cannot go from
// crowding the scan.
const outboxHeld = `EXISTS (SELECT 1 FROM outbox p WHERE p.ix = o.ix AND p.to_aid = o.to_aid AND p.id < o.id
	AND (p.handed_at IS NULL OR (o.typ = 'anet.message/1' AND o.kind IN ('text','end_request') AND p.typ = 'anet.delegate/1')))`

// DueOutboxIDs returns up to limit rows that are due at now (unix ms):
// their backoff has passed, their next status query is due, or their
// deadline has; rows an earlier row of their task holds back are left out
// (outboxHeld), the longest due first.
func (s *Store) DueOutboxIDs(now int64, limit int) ([]int64, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id FROM outbox o
	   WHERE (next_at <= ?1 OR (check_at IS NOT NULL AND check_at <= ?1) OR exp < ?1) AND NOT `+outboxHeld+`
	   ORDER BY MIN(next_at, COALESCE(check_at, next_at)), id LIMIT ?2`, now, limit)
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

// OutboxFrozen reports whether row id is frozen now (engine ENG-O6): an
// attempt reads it again before each transport, since a cancel may have
// frozen the row after the attempt began. A row that is gone is not.
func (s *Store) OutboxFrozen(id int64) (bool, error) {
	var frozen bool
	err := s.db.QueryRow(`SELECT frozen FROM outbox WHERE id = ?`, id).Scan(&frozen)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return frozen, err
}

// NextOutboxAt is the earliest time (unix ms) a row becomes due, and false
// when the queue is empty.
func (s *Store) NextOutboxAt() (int64, bool, error) {
	var at sql.NullInt64
	err := s.db.QueryRow(`SELECT MIN(MIN(next_at, COALESCE(check_at, next_at), exp + 1)) FROM outbox`).Scan(&at)
	if err != nil {
		return 0, false, err
	}
	return at.Int64, at.Valid, nil
}

// OutboxLen returns the number of queued rows.
func (s *Store) OutboxLen() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&n)
	return n, err
}

// HasOutboxRows reports whether task ix has a queued row (engine ENG-E2).
func (s *Store) HasOutboxRows(ix string) (bool, error) { return hasOutboxRows(s.db, ix) }

// HasOutboxRows is Store.HasOutboxRows inside the transaction.
func (t *Tx) HasOutboxRows(ix string) (bool, error) { return hasOutboxRows(t.tx, ix) }

// Exists reports whether interaction id is stored, reading nothing of it.
func (t *Tx) Exists(id string) (bool, error) {
	var one int
	err := t.tx.QueryRow(`SELECT 1 FROM interaction WHERE id=?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// hasOutboxRows is HasOutboxRows on q; the retention sweep asks it too
// (candidate.deletable), so a task the queue still holds is never pruned.
func hasOutboxRows(q querier, ix string) (bool, error) {
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM outbox WHERE ix=? LIMIT 1)`, ix).Scan(&n)
	return n > 0, err
}

// HasOutboxDelegation reports whether task ix still has a delegation row:
// one not yet confirmed (engine ENG-T3).
func (s *Store) HasOutboxDelegation(ix string) (bool, error) { return hasOutboxDelegation(s.db, ix) }

// HasOutboxDelegation is Store.HasOutboxDelegation inside the transaction.
func (t *Tx) HasOutboxDelegation(ix string) (bool, error) { return hasOutboxDelegation(t.tx, ix) }

func hasOutboxDelegation(e execer, ix string) (bool, error) {
	var n int
	err := e.QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM outbox WHERE ix=? AND typ='anet.delegate/1' LIMIT 1)`, ix).Scan(&n)
	return n > 0, err
}

// HandedDelegation is one delegation row the no-response scan looks at.
type HandedDelegation struct {
	ID       int64
	IX       string
	HandedAt int64
}

// HandedDelegationsFirst is the cursor of HandedDelegations's first page.
var HandedDelegationsFirst = HandedDelegation{HandedAt: math.MinInt64}

// HandedDelegations returns the delegation rows that were handed over at
// or before cutoff (unix ms) and are not frozen, earliest first, after the
// cursor (the last of the previous page), at most limit (engine ENG-T1,
// ENG-T4).
func (s *Store) HandedDelegations(cutoff int64, after HandedDelegation, limit int) ([]HandedDelegation, error) {
	rows, err := s.db.Query(`SELECT id, ix, handed_at FROM outbox
	   WHERE typ='anet.delegate/1' AND frozen=0 AND handed_at IS NOT NULL AND handed_at <= ?
	     AND (handed_at, id) > (?, ?) ORDER BY handed_at, id LIMIT ?`, cutoff, after.HandedAt, after.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HandedDelegation
	for rows.Next() {
		var h HandedDelegation
		if err := rows.Scan(&h.ID, &h.IX, &h.HandedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// AttemptingOutboxIDs returns the rows an attempt marked attempting:
// those a process that stopped mid-attempt left (engine §6.2 CR2).
func (s *Store) AttemptingOutboxIDs() ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM outbox WHERE attempting=1 ORDER BY id`)
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

// OutboxSeal is a new version an attempt writes (SealOutboxVersion).
type OutboxSeal struct {
	// AckKey is the version's ack_key, Envelope its bytes without outer
	// key 7.
	AckKey, Envelope []byte
	// KID and KidNA are the recipient's epoch key and its not_after; KSS and
	// NoticeAt the key state signed under and the notice's observed_at.
	KID      []byte
	KidNA    uint64
	KSS      uint64
	NoticeAt uint64
	// Handed are the versions the attempt sealing this one has handed
	// over already (a version answered kid_stale, one a direct path took),
	// recorded as handed at HandedAt before any version is replaced: a
	// version of which a byte left is kept (ENG-S5).
	Handed   []uint64
	HandedAt int64
}

// SealOutboxVersion is step A8 with a new version (engine ENG-R10), in the
// caller's transaction: the version and its ack_key, the row's envelope,
// cur_ver and sealed_* columns, attempting = 1; a version replaced that was
// never handed over is deleted (ENG-S5), and past MaxHandedVersions
// handed versions the earliest is (ENG-R9, evicted reports it). The new
// version's number is returned. Nothing of the row may leave the process
// before this commits (SI-83).
func (t *Tx) SealOutboxVersion(id int64, s OutboxSeal) (ver uint64, evicted int64, err error) {
	if len(s.AckKey) == 0 || len(s.Envelope) == 0 {
		return 0, 0, fmt.Errorf("%w: a version needs its ack_key and envelope", ErrBadInput)
	}
	var cur, frozen int64
	switch err := t.tx.QueryRow(`SELECT cur_ver, frozen FROM outbox WHERE id=?`, id).Scan(&cur, &frozen); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, 0, ErrNotFound
	case err != nil:
		return 0, 0, err
	case frozen != 0:
		return 0, 0, ErrOutboxFrozen
	}
	next := cur + 1
	for _, v := range s.Handed {
		if _, err := t.tx.Exec(`UPDATE outbox_ver SET handed_at=? WHERE row_id=? AND ver=? AND handed_at IS NULL`,
			s.HandedAt, id, v); err != nil {
			return 0, 0, err
		}
	}
	if len(s.Handed) > 0 {
		if _, err := t.tx.Exec(`UPDATE outbox SET handed_at=? WHERE id=? AND handed_at IS NULL`, s.HandedAt, id); err != nil {
			return 0, 0, err
		}
	}
	if _, err := t.tx.Exec(`DELETE FROM outbox_ver WHERE row_id=? AND handed_at IS NULL`, id); err != nil {
		return 0, 0, err
	}
	if _, err := t.tx.Exec(`INSERT INTO outbox_ver(row_id,ver,ack_key) VALUES(?,?,?)`, id, next, s.AckKey); err != nil {
		return 0, 0, err
	}
	if _, err := t.tx.Exec(`UPDATE outbox SET envelope=?, cur_ver=?, sealed_kid=?, sealed_kid_na=?, sealed_kss=?,
	   sealed_notice_at=?, attempting=1 WHERE id=?`, s.Envelope, next, nilIfEmpty(s.KID), int64(s.KidNA),
		int64(s.KSS), int64(s.NoticeAt), id); err != nil {
		return 0, 0, err
	}
	res, err := t.tx.Exec(`DELETE FROM outbox_ver WHERE row_id=?1 AND ver IN (
	   SELECT ver FROM outbox_ver WHERE row_id=?1 AND handed_at IS NOT NULL AND ver <> ?2
	   ORDER BY handed_at, ver LIMIT MAX(0, (SELECT COUNT(*) FROM outbox_ver WHERE row_id=?1 AND handed_at IS NOT NULL AND ver <> ?2) - ?3))`,
		id, next, MaxHandedVersions-1)
	if err != nil {
		return 0, 0, err
	}
	evicted, err = res.RowsAffected()
	return uint64(next), evicted, err
}

// BeginOutboxAttempt is step A8 without a new version: the row is marked
// attempting before the current version is sent again. A row frozen since
// it was read is ErrOutboxFrozen, one gone ErrNotFound.
func (t *Tx) BeginOutboxAttempt(id int64) error {
	var frozen int
	switch err := t.tx.QueryRow(`SELECT frozen FROM outbox WHERE id=?`, id).Scan(&frozen); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return err
	case frozen != 0:
		return ErrOutboxFrozen
	}
	_, err := t.tx.Exec(`UPDATE outbox SET attempting=1 WHERE id=?`, id)
	return err
}

// OutboxAccepted is a hub's 200 to one version (engine §2.5).
type OutboxAccepted struct {
	Ver uint64
	// RID is nil when the answer carried none this node can use.
	RID         []byte
	Hub, ViaHub string
}

// OutboxEnd is the outcome of an attempt (EndOutboxAttempt, A12).
type OutboxEnd struct {
	// At is when the attempt ended (unix ms): the handed_at of what was
	// handed over in it.
	At int64
	// Handed are the versions a byte of left the process in this attempt.
	Handed []uint64
	// Accepted, when set, is the hub's 200 to one of them.
	Accepted *OutboxAccepted
	// Attempts, NextAt and LastError replace the row's.
	Attempts  int
	NextAt    int64
	LastError string
	// CheckAt (0 for none) and CheckN replace the row's.
	CheckAt int64
	CheckN  int
}

// EndOutboxAttempt is step A12 in the caller's transaction: the versions
// handed over get their handed_at and the row its own, if they had none;
// the hub's acceptance is recorded on its version (its rid, hub and
// via_hub, and no status answer yet); attempting is cleared; attempts,
// next_at, last_error, check_at and check_n are replaced. A row confirmed
// or given up meanwhile is gone, and nothing is written (ErrNotFound).
func (t *Tx) EndOutboxAttempt(id int64, e OutboxEnd) error {
	lastErr := e.LastError
	if len(lastErr) > maxLastError {
		lastErr = lastErr[:maxLastError]
	}
	res, err := t.tx.Exec(`UPDATE outbox SET attempting=0, attempts=?, next_at=?, last_error=?, check_at=?, check_n=?
	   WHERE id=?`, e.Attempts, e.NextAt, lastErr, nullIfZero(e.CheckAt), e.CheckN, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err == nil {
			err = ErrNotFound
		}
		return err
	}
	for _, v := range e.Handed {
		if _, err := t.tx.Exec(`UPDATE outbox_ver SET handed_at=? WHERE row_id=? AND ver=? AND handed_at IS NULL`,
			e.At, id, v); err != nil {
			return err
		}
	}
	if len(e.Handed) > 0 {
		if _, err := t.tx.Exec(`UPDATE outbox SET handed_at=? WHERE id=? AND handed_at IS NULL`, e.At, id); err != nil {
			return err
		}
	}
	if a := e.Accepted; a != nil {
		if _, err := t.tx.Exec(`UPDATE outbox_ver SET rid=?, hub=?, via_hub=?, hub_state='' WHERE row_id=? AND ver=?`,
			nilIfEmpty(a.RID), a.Hub, a.ViaHub, id, a.Ver); err != nil {
			return err
		}
	}
	return nil
}

// OutboxCheck is what a status query concluded for a row (NoteOutboxCheck).
type OutboxCheck struct {
	// States are the hub's answers by version: OutboxQueued or
	// OutboxUnknown.
	States map[uint64]string
	// CheckAt (0 for none) and CheckN replace the row's.
	CheckAt int64
	CheckN  int
	// Attempts and NextAt replace the row's when SetNext.
	SetNext  bool
	Attempts int
	NextAt   int64
}

// NoteOutboxCheck records a status query's conclusion in the caller's
// transaction (engine ENG-D1–D8). ErrNotFound when the row went meanwhile.
func (t *Tx) NoteOutboxCheck(id int64, c OutboxCheck) error {
	q := `UPDATE outbox SET check_at=?, check_n=? WHERE id=?`
	args := []any{nullIfZero(c.CheckAt), c.CheckN, id}
	if c.SetNext {
		q = `UPDATE outbox SET check_at=?, check_n=?, attempts=?, next_at=? WHERE id=?`
		args = []any{nullIfZero(c.CheckAt), c.CheckN, c.Attempts, c.NextAt, id}
	}
	res, err := t.tx.Exec(q, args...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err == nil {
			err = ErrNotFound
		}
		return err
	}
	for v, st := range c.States {
		if st != OutboxQueued && st != OutboxUnknown {
			return fmt.Errorf("%w: hub state %q", ErrBadInput, st)
		}
		if _, err := t.tx.Exec(`UPDATE outbox_ver SET hub_state=? WHERE row_id=? AND ver=?`, st, id, v); err != nil {
			return err
		}
	}
	return nil
}

// SetOutboxNextAt moves row id's backoff to nextAt (unix ms): an attempt
// that found nothing to send waits for what can change that.
func (t *Tx) SetOutboxNextAt(id, nextAt int64) error {
	_, err := t.tx.Exec(`UPDATE outbox SET next_at=? WHERE id=?`, nextAt, id)
	return err
}

// DeleteOutboxRow removes row id and every version of it in the caller's
// transaction (engine ENG-S6): a row confirmed or given up. What the row
// kept for sealing again goes with the last row of its task (ENG-S7): the
// peer's key material of a task that ended, and the task's data key when
// nothing else of it is stored (ReleaseDEK). It reports whether the row was there: a confirmation that
// finds it gone was applied already (ENG-C3).
func (t *Tx) DeleteOutboxRow(id int64) (bool, error) {
	var ix string
	switch err := t.tx.QueryRow(`SELECT ix FROM outbox WHERE id=?`, id).Scan(&ix); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	if _, err := t.tx.Exec(`DELETE FROM outbox_ver WHERE row_id=?`, id); err != nil {
		return false, err
	}
	if _, err := t.tx.Exec(`DELETE FROM outbox WHERE id=?`, id); err != nil {
		return false, err
	}
	if err := releasePeerMaterial(t.tx, ix); err != nil {
		return false, err
	}
	_, err := releaseDEK(t.tx, ix)
	return true, err
}

// deleteOutboxWhere removes the rows cond selects (a condition on outbox,
// with args) and their versions, and releases the data keys they leave
// unneeded. It returns how many rows went.
func (t *Tx) deleteOutboxWhere(cond string, args ...any) (int64, error) {
	rows, err := t.tx.Query(`SELECT id, ix FROM outbox WHERE `+cond, args...)
	if err != nil {
		return 0, err
	}
	var ids []int64
	ixs := map[string]bool{}
	for rows.Next() {
		var id int64
		var ix string
		if err := rows.Scan(&id, &ix); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		ixs[ix] = true
	}
	if err := closeRows(rows); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := t.tx.Exec(`DELETE FROM outbox_ver WHERE row_id=?`, id); err != nil {
			return 0, err
		}
		if _, err := t.tx.Exec(`DELETE FROM outbox WHERE id=?`, id); err != nil {
			return 0, err
		}
	}
	for ix := range ixs {
		if err := releasePeerMaterial(t.tx, ix); err != nil {
			return 0, err
		}
		if _, err := releaseDEK(t.tx, ix); err != nil {
			return 0, err
		}
	}
	return int64(len(ids)), nil
}

// DeleteOutboxQueue removes every row queued for task ix to `to` and their
// versions, and returns how many there were: a delegation given up before
// it left takes what was queued behind it (engine ENG-X3, ENG-O5).
func (t *Tx) DeleteOutboxQueue(ix, to string) (int64, error) {
	return t.deleteOutboxWhere(`ix=? AND to_aid=?`, ix, to)
}

// DeleteOutboxFollowUps removes the text and end_request rows queued for
// task ix to `to` that no byte of has left (engine ENG-X3, ENG-O4); a
// cancel stays.
func (t *Tx) DeleteOutboxFollowUps(ix, to string) (int64, error) {
	return t.deleteOutboxWhere(`ix=? AND to_aid=? AND typ='anet.message/1' AND kind IN ('text','end_request')
	   AND handed_at IS NULL AND attempting=0`, ix, to)
}

// FreezeOutboxDelegation marks the delegation row of task ix to `to`
// frozen (engine ENG-O4): it is not transmitted again. It reports whether
// there was one.
func (t *Tx) FreezeOutboxDelegation(ix, to string) (bool, error) {
	res, err := t.tx.Exec(`UPDATE outbox SET frozen=1 WHERE ix=? AND to_aid=? AND typ='anet.delegate/1'`, ix, to)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// HurryOutbox makes every row queued for task ix to `to` due at now (unix
// ms) — its backoff, and the status query of what the hub took — and
// returns how many there are (engine §4.6).
func (t *Tx) HurryOutbox(ix, to string, now int64) (int64, error) {
	res, err := t.tx.Exec(`UPDATE outbox SET next_at=MIN(next_at, ?1),
	   check_at=CASE WHEN check_at IS NULL THEN NULL ELSE MIN(check_at, ?1) END WHERE ix=?2 AND to_aid=?3`, now, ix, to)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RecoverOutboxAttempts is engine §6.2 CR2, at start: a row marked
// attempting may have been sent by the process that stopped, so its
// current version counts as handed over at now (unix ms), and the mark is
// cleared. It returns how many rows it found.
func (t *Tx) RecoverOutboxAttempts(now int64) (int64, error) { return t.recoverAttempts(now, 0) }

// RecoverOutboxAttempt is RecoverOutboxAttempts for row id alone: an
// attempt that ended without recording its outcome (its last write
// failed) left the mark, and the process knows no attempt is under way.
// It reports whether the row was marked.
func (t *Tx) RecoverOutboxAttempt(id, now int64) (bool, error) {
	n, err := t.recoverAttempts(now, id)
	return n > 0, err
}

// recoverAttempts is CR2 for row id, or for every row when id is 0.
func (t *Tx) recoverAttempts(now, id int64) (int64, error) {
	if _, err := t.tx.Exec(`UPDATE outbox_ver SET handed_at=? WHERE handed_at IS NULL AND EXISTS (
	   SELECT 1 FROM outbox o WHERE o.id = outbox_ver.row_id AND o.attempting=1 AND o.cur_ver = outbox_ver.ver
	   AND (?2 = 0 OR o.id = ?2))`, now, id); err != nil {
		return 0, err
	}
	if _, err := t.tx.Exec(`UPDATE outbox SET handed_at=? WHERE attempting=1 AND handed_at IS NULL AND cur_ver > 0
	   AND (?2 = 0 OR id = ?2)`, now, id); err != nil {
		return 0, err
	}
	res, err := t.tx.Exec(`UPDATE outbox SET attempting=0 WHERE attempting=1 AND (?1 = 0 OR id = ?1)`, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// nullIfZero is v for SQL, NULL for 0.
func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// MessageByMsgID returns the message of an interaction stored under msgID,
// or ErrNotFound. The engine uses it to learn what a row was (its MID
// names the message, A2A-DESIGN an internal note item).
func (s *Store) MessageByMsgID(interactionID, msgID string) (*Message, error) {
	return messageByMsgID(s.db, interactionID, msgID)
}

// MessageByMsgID is Store.MessageByMsgID inside the transaction.
func (t *Tx) MessageByMsgID(interactionID, msgID string) (*Message, error) {
	return messageByMsgID(t.tx, interactionID, msgID)
}

func messageByMsgID(e execer, interactionID, msgID string) (*Message, error) {
	if interactionID == "" || msgID == "" {
		return nil, ErrNotFound
	}
	var m Message
	err := e.QueryRow(`SELECT seq,interaction_id,sender_aid,kind,body,msg_id,metadata,created_at FROM message
	   WHERE interaction_id=? AND msg_id=?`, interactionID, msgID).
		Scan(&m.Seq, &m.InteractionID, &m.SenderAID, &m.Kind, &m.Body, &m.MsgID, &m.Metadata, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b, err := boxOf(e, interactionID)
	if err != nil {
		return nil, err
	}
	if err := openMessage(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// MergeMessageMetaByID is Tx.MergeMessageMeta for the message stored under
// msgID: what the engine writes on a confirmation or an abandonment
// (anet.delivery, engine ENG-C3, ENG-X1). ErrNotFound when there is none.
func (t *Tx) MergeMessageMetaByID(interactionID, msgID string, add map[string]any) error {
	if interactionID == "" || msgID == "" || len(add) == 0 {
		return ErrNotFound
	}
	var seq int64
	err := t.tx.QueryRow(`SELECT seq FROM message WHERE interaction_id=? AND msg_id=?`, interactionID, msgID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return mergeMessageMeta(t.tx, interactionID, seq, add)
}

// TouchUpdatedAt sets interaction ix's updated_at to at while it is not
// terminal: when its delegation was confirmed, from which the no-response
// deadline counts (engine ENG-C3, ENG-T3).
func (t *Tx) TouchUpdatedAt(ix string, at string) error {
	_, err := t.tx.Exec(`UPDATE interaction SET updated_at=? WHERE id=? AND state NOT IN `+terminalSQL, at, ix)
	return err
}

// OpenInteraction is a task that is not terminal, as closing every task
// reads it (OpenInteractions).
type OpenInteraction struct {
	ID, PeerAID  string
	Role         Role
	IsCapability bool
}

// OpenInteractions returns every task that is not terminal, in the order
// they were made: what an emergency rotation, a local state rollback, a
// deactivation or a stop closes (engine ENG-K15, K16).
func (t *Tx) OpenInteractions() ([]OpenInteraction, error) {
	rows, err := t.tx.Query(`SELECT id, peer_aid, role, is_capability FROM interaction WHERE state NOT IN ` + terminalSQL + ` ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	var out []OpenInteraction
	for rows.Next() {
		var o OpenInteraction
		var role string
		var cap int
		if err := rows.Scan(&o.ID, &o.PeerAID, &role, &cap); err != nil {
			rows.Close()
			return nil, err
		}
		o.Role, o.IsCapability = Role(role), cap != 0
		out = append(out, o)
	}
	return out, closeRows(rows)
}
