package daemon

// outbox_queue.go puts messages in the outbound queue (engine §2.2). Every
// message this node sends to a peer on an interaction goes through it — a
// delegation, a conversation message, a cancel, a payment, every status and
// result a provider sends, the notices after the fact (ENG-Q1) — in the
// transaction of the state change that produced it, and is attempted at
// once after (ENG-Q8). The queue keeps the body under the task's data key;
// nothing is sealed here (ENG-Q3): every version is sealed by an attempt,
// with the keys and the credential of its moment.

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// wireSend is one message for queueSendAs.
type wireSend struct {
	to, typ, ix string
	// kind is the row kind (rowKind*); "" takes it from the body
	// (rowKindOf).
	kind string
	body []byte
	// mid is the inner message id; nil mints one. A message this node also
	// records passes the id it recorded (newWireMID), so the msg_id on its
	// row is the id on the wire (an internal note item).
	mid []byte
	// pin is the peer_identity pin of a recipient looked up before its row
	// is written (strict).
	pin string
	// lastErr is what the row starts with (strictRecipient).
	lastErr string
	// strict looks the recipient up before anything is written (ENG-Q5): a
	// recipient no hub knows, or whose key history or keys do not verify,
	// is refused at once and nothing is recorded; one whose hub cannot be
	// asked is queued, waiting on home_unreachable. A new delegation is
	// strict.
	strict bool
}

// queueSend queues body to toAID, recording write in the same transaction;
// if write returns an error nothing is queued. The caller then attempts it
// (deliverQueued).
func (d *Daemon) queueSend(ctx context.Context, toAID, typ, ix string, body []byte,
	write func(tx *interactions.Tx) error) (int64, error) {
	return d.queueSendAs(ctx, wireSend{to: toAID, typ: typ, ix: ix, body: body}, write)
}

// queueSendAs is queueSend with the message id, kind or pin fixed, or
// strict (engine §2.2): the message's identity is fixed now — mid, ts now,
// exp ts + 14 days (ENG-Q2) — and every version carries it. The same
// message queued twice is one row (ENG-Q4).
func (d *Daemon) queueSendAs(ctx context.Context, s wireSend, write func(tx *interactions.Tx) error) (int64, error) {
	if s.strict {
		code, err := d.strictRecipient(ctx, s.to, s.pin)
		if err != nil {
			return 0, err
		}
		s.lastErr = code
	}
	var id int64
	var fresh bool
	err := d.ix.Update(func(tx *interactions.Tx) error {
		// The row goes in ahead of the write that produced it when the task
		// exists already: a terminal write then sees it, and keeps the
		// peer's key material the row may have to be sealed with (engine
		// ENG-S7). A task the write makes comes first.
		exists, err := tx.Exists(s.ix)
		if err != nil {
			return err
		}
		if exists {
			if id, fresh, err = d.enqueueTx(tx, s); err != nil {
				return err
			}
		}
		if write != nil {
			if err := write(tx); err != nil {
				return err
			}
		}
		if !exists {
			id, fresh, err = d.enqueueTx(tx, s)
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	if fresh {
		d.noteDelivery(s.ix)
	}
	return id, nil
}

// enqueueTx queues s in tx (engine §2.2): its identity is fixed now — mid,
// ts now, exp ts + 14 days (ENG-Q2) — and every version carries it; the
// same message queued twice is one row (ENG-Q4, fresh false). The caller
// calls noteDelivery after the commit.
func (d *Daemon) enqueueTx(tx *interactions.Tx, s wireSend) (id int64, fresh bool, err error) {
	if s.mid == nil {
		s.mid = seal.NewMID()
	}
	if s.kind == "" {
		s.kind = rowKindOf(s.typ, s.body)
	}
	now := d.nowMS()
	return tx.EnqueueOutbox(interactions.OutboxItem{IX: s.ix, ToAID: s.to, Type: s.typ, Kind: s.kind, MID: s.mid,
		Body: s.body, TS: now, Exp: now + messageLifetimeMS, LastError: s.lastErr})
}

// strictRecipient is ENG-Q5 for a new delegation to `to`, before anything
// is written: the recipient's key history and keys are looked up and
// checked (resolvePeer), and strictOutcome says what that means.
func (d *Daemon) strictRecipient(ctx context.Context, to, pin string) (string, error) {
	_, err := d.recipientKeys(ctx, to, pin)
	if err == nil {
		return "", nil
	}
	return strictOutcome(err)
}

// strictOutcome is ENG-Q5's reading of a failed lookup of a new
// delegation's recipient. No hub knows it: recipient_unknown; what the
// directory holds does not verify: recipient_unverifiable — both refusals
// of the reason table, nothing recorded, nothing sent (ENG-U5). The hub
// cannot be asked, or answers in part: the row is queued all the same and
// starts waiting on home_unreachable, which is returned (KEL D-26); a
// recipient whose keys are all past their time waits the same way, on
// -no_keys. Anything else (the store) is an error.
func strictOutcome(err error) (string, error) {
	switch resolveVerdictOf(err) {
	case verdictNotFound:
		return "", a2ashape.Refuse(reasonRecipientUnknown, nil)
	case verdictUnverifiable:
		return "", a2ashape.Refuse(reasonRecipientUnverifiable, nil)
	case verdictUnavailable:
		return lastErrorOf(reasonHomeUnreachable), nil
	}
	if errors.Is(err, errNoRecipientKeys) {
		return errNoKeys, nil
	}
	return "", err
}

// The codes of a strict send (ENG-Q5, ux §1.4 G).
const (
	reasonRecipientUnknown      = "recipient_unknown"
	reasonRecipientUnverifiable = "recipient_unverifiable"
	reasonHomeUnreachable       = "home_unreachable"
)

// rowKindOf is the row kind of a body of type typ (engine ENG-O1–O3): the
// ChatMsg kind of a message — a text carrying a payment is a payment — and
// the state of a status. A notice after the fact names its kind itself
// (queueNotice).
func rowKindOf(typ string, body []byte) string {
	switch typ {
	case seal.TypeMessage:
		cm, err := delegation.UnmarshalChatMsg(body)
		if err != nil {
			return ""
		}
		if cm.Kind == delegation.ChatText && hasPaymentStatus(cm.Metadata) {
			return rowKindPayment
		}
		return cm.Kind
	case seal.TypeStatus:
		sm, err := delegation.UnmarshalStatusMsg(body)
		if err != nil {
			return ""
		}
		return sm.State
	}
	return ""
}

// queueNotice queues one of the three notices after the fact about a
// delegation this node held or refused (ENG-Q1, Q7): pending_approval,
// pending_expired or operator_rejected, a status of kind. It goes only
// under a credential the requester gave this node for the task or as its
// peer (ENG-K2), and only within the notice limit: without either it is
// not queued at all, and counted. The loop sends it. It reports whether it
// was queued.
func (d *Daemon) queueNotice(to, ix, kind string, body []byte) bool {
	now := d.nowMS()
	choice, err := d.accessChoiceFor(to, ix, false, accessNotice, now)
	if err != nil || !choice.Send {
		d.count(cntNoticeNoGrant)
		return false
	}
	if !d.notices.allow(to, now) {
		d.count(noticeSuppressed)
		return false
	}
	ctx, cancel := context.WithTimeout(d.ctx, hubCallTimeoutShort)
	defer cancel()
	if _, err := d.queueSendAs(ctx, wireSend{to: to, typ: seal.TypeStatus, ix: ix, kind: kind, body: body}, nil); err != nil {
		log.Printf("anet: %s: queue the %s notice to %s: %v", ix, kind, to, err)
		return false
	}
	d.count(noticeSent)
	d.kickOutbox()
	return true
}

// noticeBody is the status a notice after the fact carries: its state, a
// text and anet.reason (the requester's node words it).
func noticeBody(state, text, reason string, now uint64) ([]byte, error) {
	var meta map[string]any
	if reason != "" {
		meta = map[string]any{a2ashape.KeyReason: reason}
	}
	return noticeBodyMeta(state, text, meta, now)
}

// noticeBodyMeta is noticeBody with the metadata given.
func noticeBodyMeta(state, text string, meta map[string]any, now uint64) ([]byte, error) {
	var mb []byte
	if len(meta) > 0 {
		var err error
		if mb, err = json.Marshal(meta); err != nil {
			return nil, err
		}
	}
	return (&delegation.StatusMsg{State: state, Text: text, Metadata: mb, At: now}).Marshal()
}
