package interactions

// pending.go is the approval queue of the approve policy (A2A-DESIGN §5.3):
// a delegation from a peer that is neither denied nor allowed is held here,
// not in the interaction table, until the operator approves or rejects it or
// it expires. It is a separate table so that nothing that lists, answers or
// executes interactions can reach a held delegation by accident.
//
// A held item keeps what approval needs and nothing more: the signed
// delegation (with attachment bytes removed), the requester's KEL and key
// set as they arrived, the message time and key state, and up to a bounded
// number of follow-up messages. Attachment bytes are not kept; the metadata
// is.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrPendingFull is returned by PutPending when a limit would be exceeded.
// The caller refuses the delegation.
var ErrPendingFull = errors.New("interactions: approval queue is full")

// ErrPendingFollowupsFull is returned when a held item already carries the
// maximum number of follow-up messages.
var ErrPendingFollowupsFull = errors.New("interactions: held delegation has no room for another message")

// PendingAttachment is the metadata of an attachment of a held delegation.
type PendingAttachment struct {
	Name string `json:"name"`
	Mime string `json:"mime,omitempty"`
	Size int64  `json:"size"`
	CID  string `json:"cid"`
}

// PendingFollowup is a message that arrived for a held delegation.
type PendingFollowup struct {
	Kind     string          `json:"kind"`
	Body     string          `json:"body,omitempty"`
	MsgID    string          `json:"msg_id,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	At       int64           `json:"at"`
}

// PendingItem is one held delegation.
type PendingItem struct {
	IX         string
	FromAID    string
	ArrivedAt  int64  // unix ms, this node's clock
	MsgTS      uint64 // the sealed message's ts, for re-verification at approval
	KeyState   uint64 // the TaskDoc signer's key state seq
	Capability string // capability id, empty for a natural-language task
	RequestCID string
	Bytes      int64  // size of the delegation as received
	Delegate   []byte // DelegateReq encoding without attachment bytes
	KEL        []byte // requester KEL as carried in the message
	Keys       []byte // requester SignedEncKeySet as carried in the message
	// ContextID is the requester's A2A context id.
	ContextID   string
	Attachments []PendingAttachment
	Followups   []PendingFollowup
	// MsgID is the id the goal is recorded under once approved: the
	// delegation's envelope message id in hex, the id the requester
	// recorded it under (an internal note item). Empty on items held before it was kept.
	MsgID string
	// PeerRK is the rk the delegation carried (coredet(RK)), or nil. An
	// approval records it on the task in the same transaction (New.PeerRK,
	// FS-R4).
	PeerRK []byte

	// sealedAtts and sealedFollowups are the stored attachments and
	// followups columns, as read, until openPending decrypts them.
	sealedAtts, sealedFollowups string
}

// pendingSchema is the approval queue. delegate, attachments and followups
// hold data-key ciphertext (dek.go, FS-L2).
var pendingSchema = []string{
	`CREATE TABLE pending (
	   ix TEXT PRIMARY KEY,
	   from_aid TEXT NOT NULL,
	   arrived_at INTEGER NOT NULL,
	   msg_ts INTEGER NOT NULL DEFAULT 0,
	   key_state INTEGER NOT NULL DEFAULT 0,
	   capability TEXT NOT NULL DEFAULT '',
	   request_cid TEXT NOT NULL DEFAULT '',
	   bytes INTEGER NOT NULL DEFAULT 0,
	   delegate BLOB NOT NULL,
	   kel BLOB,
	   keys BLOB,
	   context_id TEXT NOT NULL DEFAULT '',
	   attachments BLOB NOT NULL DEFAULT X'',
	   followups BLOB NOT NULL DEFAULT X'',
	   msg_id TEXT NOT NULL DEFAULT '',
	   peer_rk BLOB
	 )`,
	`CREATE INDEX idx_pending_from ON pending(from_aid)`,
	`CREATE INDEX idx_pending_arrived ON pending(arrived_at)`,
}

const pendingColumns = `ix,from_aid,arrived_at,msg_ts,key_state,capability,request_cid,bytes,delegate,kel,keys,context_id,attachments,followups,msg_id,peer_rk`

func scanPending(sc scanner) (*PendingItem, error) {
	var p PendingItem
	var ts, ks int64
	var atts, fus string
	if err := sc.Scan(&p.IX, &p.FromAID, &p.ArrivedAt, &ts, &ks, &p.Capability, &p.RequestCID, &p.Bytes,
		&p.Delegate, &p.KEL, &p.Keys, &p.ContextID, &atts, &fus, &p.MsgID, &p.PeerRK); err != nil {
		return nil, err
	}
	p.MsgTS, p.KeyState = uint64(ts), uint64(ks)
	p.sealedAtts, p.sealedFollowups = atts, fus
	return &p, nil
}

// openPending replaces the stored delegation, attachments and followups of
// a held item scanPending read with their plaintext, under the item's data
// key (ErrContentGone without one).
func openPending(e execer, p *PendingItem) error {
	b, err := boxOf(e, p.IX)
	if err != nil {
		return err
	}
	if p.Delegate, err = b.get(ColPendingDelegate, p.Delegate); err != nil {
		return err
	}
	atts, err := b.getText(ColPendingAttachments, p.sealedAtts)
	if err != nil {
		return err
	}
	fus, err := b.getText(ColPendingFollowups, p.sealedFollowups)
	if err != nil {
		return err
	}
	p.sealedAtts, p.sealedFollowups = "", ""
	if atts != "" {
		_ = json.Unmarshal([]byte(atts), &p.Attachments)
	}
	if fus != "" {
		_ = json.Unmarshal([]byte(fus), &p.Followups)
	}
	return nil
}

// PutPending holds a delegation for approval, inside the transaction that
// also records its replay row. It returns ErrPendingFull when the queue
// already holds maxTotal items, or maxPerPeer items from the same
// requester. A delegation already held under the same id is left as it is.
func (t *Tx) PutPending(p PendingItem, maxTotal, maxPerPeer int) error {
	if p.IX == "" || p.FromAID == "" || len(p.Delegate) == 0 {
		return fmt.Errorf("%w: pending item needs an id, a requester and the delegation", ErrBadInput)
	}
	var exists int
	err := t.tx.QueryRow(`SELECT 1 FROM pending WHERE ix=?`, p.IX).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var total, mine int
	if err := t.tx.QueryRow(`SELECT COUNT(*), COALESCE(SUM(from_aid = ?), 0) FROM pending`, p.FromAID).Scan(&total, &mine); err != nil {
		return err
	}
	if maxTotal > 0 && total >= maxTotal {
		return fmt.Errorf("%w: %d held in total", ErrPendingFull, total)
	}
	if maxPerPeer > 0 && mine >= maxPerPeer {
		return fmt.Errorf("%w: %d held from %s", ErrPendingFull, mine, p.FromAID)
	}
	atts, err := json.Marshal(p.Attachments)
	if err != nil {
		return err
	}
	// The held item's data key commits with it (FS-L1); an approval hands
	// it to the interaction (TakePending).
	b, err := ensureDEK(t.tx, p.IX, t.now)
	if err != nil {
		return err
	}
	delegate, err := b.put(ColPendingDelegate, p.Delegate)
	if err != nil {
		return err
	}
	sealedAtts, err := b.putText(ColPendingAttachments, string(atts))
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(`INSERT INTO pending(`+pendingColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.IX, p.FromAID, p.ArrivedAt, int64(p.MsgTS), int64(p.KeyState), p.Capability, p.RequestCID, p.Bytes,
		delegate, p.KEL, p.Keys, p.ContextID, sealedAtts, []byte{}, p.MsgID, nilIfEmpty(p.PeerRK))
	return err
}

// GetPending reads one held item inside the transaction.
func (t *Tx) GetPending(ix string) (*PendingItem, error) {
	return getPending(t.tx, ix)
}

// GetPending returns one held item, or ErrNotFound.
func (s *Store) GetPending(ix string) (*PendingItem, error) {
	return getPending(s.db, ix)
}

func getPending(e execer, ix string) (*PendingItem, error) {
	p, err := scanPending(e.QueryRow(`SELECT `+pendingColumns+` FROM pending WHERE ix=?`, ix))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := openPending(e, p); err != nil {
		return nil, err
	}
	return p, nil
}

// AppendPendingFollowup adds a message to a held item, at most max per item.
func (t *Tx) AppendPendingFollowup(ix string, f PendingFollowup, max int) error {
	p, err := getPending(t.tx, ix)
	if err != nil {
		return err
	}
	if f.MsgID != "" {
		for _, prior := range p.Followups {
			if prior.MsgID == f.MsgID {
				return nil
			}
		}
	}
	if max > 0 && len(p.Followups) >= max {
		return ErrPendingFollowupsFull
	}
	p.Followups = append(p.Followups, f)
	plain, err := json.Marshal(p.Followups)
	if err != nil {
		return err
	}
	b, err := boxOf(t.tx, ix)
	if err != nil {
		return err
	}
	sealed, err := b.putText(ColPendingFollowups, string(plain))
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(`UPDATE pending SET followups=? WHERE ix=?`, sealed, ix)
	return err
}

// DeletePending removes a held item inside the transaction and reports
// whether it existed. Its data key goes with it (FS-L9) unless something
// stored still needs the key (ReleaseDEK); an approval uses TakePending,
// which keeps the key for the interaction.
func (t *Tx) DeletePending(ix string) (bool, error) {
	res, err := t.tx.Exec(`DELETE FROM pending WHERE ix=?`, ix)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if _, err := releaseDEK(t.tx, ix); err != nil {
		return false, err
	}
	return n > 0, nil
}

// DeletePending removes a held item and reports whether it existed.
func (s *Store) DeletePending(ix string) (bool, error) {
	var ok bool
	err := s.Update(func(t *Tx) error {
		var err error
		ok, err = t.DeletePending(ix)
		return err
	})
	return ok, err
}

// ListPending returns every held item, oldest first.
func (s *Store) ListPending() ([]*PendingItem, error) {
	return s.queryPending(`SELECT ` + pendingColumns + ` FROM pending ORDER BY arrived_at, ix`)
}

// ExpiredPending returns the held items that arrived before cutoff (unix ms).
func (s *Store) ExpiredPending(cutoff int64) ([]*PendingItem, error) {
	return s.queryPending(`SELECT `+pendingColumns+` FROM pending WHERE arrived_at < ? ORDER BY arrived_at`, cutoff)
}

// queryPending reads the held items q selects. One whose data key is gone
// (deleted with it since the query ran) is left out.
func (s *Store) queryPending(q string, args ...any) ([]*PendingItem, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	var items []*PendingItem
	for rows.Next() {
		p, err := scanPending(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, p)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	out := items[:0]
	for _, p := range items {
		switch err := openPending(s.db, p); {
		case errors.Is(err, ErrContentGone):
			continue
		case err != nil:
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
