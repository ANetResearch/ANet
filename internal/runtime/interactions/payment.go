package interactions

// payment.go writes the x402 columns of an interaction (A2A-DESIGN §4.1,
// §8): pay_state, pay_required, pay_auth_ids, pay_payload, pay_receipts and
// quote_expires_at.
//
// The payment columns are not the task state. A receipt that arrives for a
// task already canceled here is still a fact about money this node paid,
// so these writes do not refuse a terminal row; the callers decide when a
// write applies, and a compare on the current pay_state (From) is what
// makes "at most one payment in flight" hold under concurrent callers.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// maxPayReceipts bounds the stored receipt history. A requester sending
// malformed payments gets one failure receipt each; the list keeps the
// newest.
const maxPayReceipts = 64

// PayUpdate is one write of the payment columns. Nil fields are left as
// they are.
type PayUpdate struct {
	// From, when not empty, lists the pay_state values the row must hold
	// for the write to apply ("" is the no-quote state).
	From []string
	// State is the new pay_state.
	State *string
	// Required replaces pay_required (the x402 PaymentRequired JSON).
	Required []byte
	// QuoteExpiresAt replaces quote_expires_at (unix ms).
	QuoteExpiresAt *int64
	// AuthIDs replaces pay_auth_ids; AddAuthID appends one id not already
	// present.
	AuthIDs   []string
	AddAuthID string
	// Payload replaces pay_payload.
	Payload []byte
	// Receipts replaces pay_receipts (a JSON array); AddReceipt appends one
	// JSON object to it.
	Receipts   []byte
	AddReceipt []byte
}

// PayState is a pointer to s, for PayUpdate.State.
func PayState(s string) *string { return &s }

// SetPayment applies u to interaction id. applied is false when the row's
// pay_state is not in u.From; an absent row is ErrNotFound.
func (s *Store) SetPayment(id string, u PayUpdate) (applied bool, err error) {
	err = s.Update(func(tx *Tx) error {
		var e error
		applied, e = tx.SetPayment(id, u)
		return e
	})
	return applied, err
}

// SetPayment is Store.SetPayment inside the transaction.
func (t *Tx) SetPayment(id string, u PayUpdate) (bool, error) {
	cur, err := t.Get(id)
	if err != nil {
		return false, err
	}
	if len(u.From) > 0 {
		ok := false
		for _, f := range u.From {
			if cur.PayState == f {
				ok = true
				break
			}
		}
		if !ok {
			return false, nil
		}
	}
	state := cur.PayState
	if u.State != nil {
		state = *u.State
	}
	required := cur.PayRequired
	if u.Required != nil {
		required = u.Required
	}
	expires := cur.QuoteExpiresAt
	if u.QuoteExpiresAt != nil {
		expires = *u.QuoteExpiresAt
	}
	ids := cur.PayAuthIDs
	if u.AuthIDs != nil {
		ids = u.AuthIDs
	}
	if u.AddAuthID != "" {
		seen := false
		for _, a := range ids {
			if a == u.AddAuthID {
				seen = true
				break
			}
		}
		if !seen {
			ids = append(append([]string(nil), ids...), u.AddAuthID)
		}
	}
	idsJSON := ""
	if len(ids) > 0 {
		b, err := json.Marshal(ids)
		if err != nil {
			return false, err
		}
		idsJSON = string(b)
	}
	payload := cur.PayPayload
	if u.Payload != nil {
		payload = u.Payload
	}
	receipts := cur.PayReceipts
	if u.Receipts != nil {
		receipts = u.Receipts
	}
	if u.AddReceipt != nil {
		var list []json.RawMessage
		if len(receipts) > 0 {
			if err := json.Unmarshal(receipts, &list); err != nil {
				return false, fmt.Errorf("interactions: stored receipts of %s: %w", id, err)
			}
		}
		list = append(list, json.RawMessage(u.AddReceipt))
		if len(list) > maxPayReceipts {
			list = list[len(list)-maxPayReceipts:]
		}
		b, err := json.Marshal(list)
		if err != nil {
			return false, err
		}
		receipts = b
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = t.tx.Exec(`UPDATE interaction SET pay_state=?, pay_required=?, quote_expires_at=?, pay_auth_ids=?,
	   pay_payload=?, pay_receipts=?, updated_at=? WHERE id=?`,
		state, required, expires, idsJSON, payload, receipts, now, id)
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListPayState returns the interactions in role whose pay_state is one of
// states, oldest first. active keeps only non-terminal ones.
func (s *Store) ListPayState(role Role, states []string, active bool) ([]*Interaction, error) {
	if len(states) == 0 {
		return nil, nil
	}
	ph := make([]string, len(states))
	args := []any{string(role)}
	for i, st := range states {
		ph[i] = "?"
		args = append(args, st)
	}
	q := `SELECT ` + ixColumns + ` FROM interaction WHERE role=? AND pay_state IN (` + strings.Join(ph, ",") + `)`
	if active {
		q += ` AND state NOT IN ` + terminalSQL
	}
	q += ` ORDER BY seq`
	return s.query(q, args...)
}
