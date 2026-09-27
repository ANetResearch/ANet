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
}

func (s *Store) migratePending() error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS pending (
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
		   attachments TEXT NOT NULL DEFAULT '',
		   followups TEXT NOT NULL DEFAULT ''
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_pending_from ON pending(from_aid)`,
		`CREATE INDEX IF NOT EXISTS idx_pending_arrived ON pending(arrived_at)`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("interactions: migrate pending: %w", err)
		}
	}
	return nil
}

const pendingColumns = `ix,from_aid,arrived_at,msg_ts,key_state,capability,request_cid,bytes,delegate,kel,keys,context_id,attachments,followups`

func scanPending(sc scanner) (*PendingItem, error) {
	var p PendingItem
	var ts, ks int64
	var atts, fus string
	if err := sc.Scan(&p.IX, &p.FromAID, &p.ArrivedAt, &ts, &ks, &p.Capability, &p.RequestCID, &p.Bytes,
		&p.Delegate, &p.KEL, &p.Keys, &p.ContextID, &atts, &fus); err != nil {
		return nil, err
	}
	p.MsgTS, p.KeyState = uint64(ts), uint64(ks)
	if atts != "" {
		_ = json.Unmarshal([]byte(atts), &p.Attachments)
	}
	if fus != "" {
		_ = json.Unmarshal([]byte(fus), &p.Followups)
	}
	return &p, nil
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
	_, err = t.tx.Exec(`INSERT INTO pending(`+pendingColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.IX, p.FromAID, p.ArrivedAt, int64(p.MsgTS), int64(p.KeyState), p.Capability, p.RequestCID, p.Bytes,
		p.Delegate, p.KEL, p.Keys, p.ContextID, string(atts), "")
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
	return p, err
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
	b, err := json.Marshal(p.Followups)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(`UPDATE pending SET followups=? WHERE ix=?`, string(b), ix)
	return err
}

// DeletePending removes a held item inside the transaction and reports
// whether it existed.
func (t *Tx) DeletePending(ix string) (bool, error) {
	res, err := t.tx.Exec(`DELETE FROM pending WHERE ix=?`, ix)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
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

func (s *Store) queryPending(q string, args ...any) ([]*PendingItem, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PendingItem
	for rows.Next() {
		p, err := scanPending(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
