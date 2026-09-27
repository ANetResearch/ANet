package daemon

// wire_edges.go holds the edge rules of the receive and execution paths
// that are not the main line of any one of them (B3-02):
//
//   - a capability call cut off by the daemon stopping records nothing and
//     is not acknowledged (SI-10: a cancellation is a temporary failure);
//   - startup recovery and redelivery of calls an earlier process left
//     open (A2A-DESIGN §3.6);
//   - the deny list does not stop the delivery of work this node paid for,
//     and a deny reports the paid work it left running once (0017 Q10);
//   - a cancel left pending because a payment was in flight is carried out
//     when the payment does not go through (0017 Q3);
//   - follow-ups held with an approval-queue item are recorded by kind
//     (A2A-DESIGN §5.3).

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// transientStopping counts envelopes left unacknowledged because the daemon
// stopped while their capability call ran.
const transientStopping = "t-stopping"

// cutOffByStop reports whether an invocation's outcome is the daemon's
// stopping rather than the call's own: the daemon context is done and the
// call did not report an effect that happened. A provider that honors its
// context returns an error, or a FAILED effect, when it is stopped; either
// would otherwise be delivered as the call's answer, a definite failure
// for work that was only interrupted. Such a call records nothing and stays
// open: a short call runs again when its delegation is redelivered, and a
// long one is reported interrupted at the next start. An effect that
// happened (OK, UNVERIFIED) is recorded even while stopping.
func (d *Daemon) cutOffByStop(err error, eff effect.Effect) bool {
	if d.ctx.Err() == nil {
		return false
	}
	if err != nil {
		return true
	}
	switch eff.Status {
	case effect.OK, effect.Unverified:
		return false
	}
	return true
}

// leftover is what to do with an inbound capability call an earlier process
// left open with no result.
type leftover uint8

const (
	leftoverKeep        leftover = iota // not ours to decide now
	leftoverRerun                       // run it again (a short call)
	leftoverInterrupted                 // report failed, anet.reason=interrupted
)

// leftoverAction classifies an open inbound capability call with no result
// (see recoverInterrupted).
func (d *Daemon) leftoverAction(ix *interactions.Interaction, capID string) leftover {
	if !ix.IsCapability || ix.IsTerminal() || len(ix.Receipt) > 0 {
		return leftoverKeep
	}
	switch ix.PayState {
	case interactions.PayNone, interactions.PayCompleted:
	default:
		// Priced work runs only once paid (A2A-DESIGN §8.3), so this one
		// has not run: its payment is being settled or was received and
		// not taken (startPayments presents it again), or its quote waits
		// (the quote sweep ends the task when it lapses). Reporting it
		// interrupted would say an effect might have happened when none
		// did.
		return leftoverKeep
	}
	resolvable, long := false, false
	if d.providers != nil {
		if p, ok := d.providers.Resolve(capID); ok {
			resolvable = true
			_, long = invokeBound(p, capID)
		}
	}
	switch ix.State {
	case interactions.StateSubmitted:
		// Recorded and never marked working. A long call is marked working
		// before it starts, so this one stopped in between. A short one is
		// run again by the redelivery of its delegation, which was not
		// acknowledged — except an approved one: its delegation was
		// acknowledged when it was held, and nothing will come again.
		if long {
			return leftoverInterrupted
		}
		if resolvable && ix.Trust == interactions.TrustApproved {
			return leftoverRerun
		}
		return leftoverKeep
	case interactions.StateWorking:
		if resolvable && !long {
			return leftoverRerun
		}
		return leftoverInterrupted
	}
	return leftoverKeep
}

// leftByEarlierProcess reports whether ix was created before this process
// opened the store. One created since may simply not have started yet.
func (d *Daemon) leftByEarlierProcess(ix *interactions.Interaction) bool {
	at, err := time.Parse(time.RFC3339Nano, ix.CreatedAt)
	return err == nil && at.Before(d.started)
}

// storedCall reads the capability id and arguments back from an
// interaction's stored request; the goal stands in for an id it cannot
// read.
func storedCall(ix *interactions.Interaction) (string, map[string]any) {
	if td, err := decodeTaskDoc(ix.RequestDoc); err == nil {
		if c, args, ok := capabilityCall(td); ok {
			return c, args
		}
	}
	return ix.Goal, map[string]any{}
}

// paidWork reports whether ix is a task this node started and has paid, or
// is paying, for. Its provider's status and result are taken even when the
// provider has since been denied: the deny sweep leaves such a task open,
// and the work is delivered (0017 Q10).
func paidWork(ix *interactions.Interaction) bool {
	return ix != nil && ix.Role == interactions.RoleOutbound &&
		(ix.PayState == interactions.PaySubmitted || ix.PayState == interactions.PayCompleted)
}

// skippedPaidNew marks ids as reported left running after a deny and
// returns those not reported before.
func (d *Daemon) skippedPaidNew(ids []string) []string {
	var fresh []string
	for _, id := range ids {
		if _, seen := d.peerLists.skippedPaid.LoadOrStore(id, struct{}{}); !seen {
			fresh = append(fresh, id)
		}
	}
	return fresh
}

// skippedPaidReported is the set of ids reported so far.
func (d *Daemon) skippedPaidReported() map[string]bool {
	out := map[string]bool{}
	d.peerLists.skippedPaid.Range(func(k, _ any) bool {
		if id, _ := k.(string); id != "" {
			out[id] = true
		}
		return true
	})
	return out
}

// skippedPaidForget forgets the ids of reported that are not in current:
// tasks that have ended, or whose peer is no longer denied. An id reported
// after reported was read is kept.
func (d *Daemon) skippedPaidForget(reported, current map[string]bool) {
	for id := range reported {
		if !current[id] {
			d.peerLists.skippedPaid.Delete(id)
		}
	}
}

// denyExtra is the detail of an anet.policy.changed for a deny: the
// interactions canceled, and those left running because a payment was
// submitted or settled on them (0017 Q10).
func denyExtra(canceled, skippedPaid []string) map[string]any {
	extra := map[string]any{"canceled": canceled}
	if len(skippedPaid) > 0 {
		extra["skipped_paid"] = skippedPaid
	}
	return extra
}

// carryOutRequestedCancel cancels a task this node started whose cancel was
// sent while its payment was submitted, once the provider says the payment
// did not go through (a payment-failed, or a new quote). The cancel was
// left pending only because paid work is not canceled (A2A-DESIGN §4.2,
// 0017 Q3); with no payment taken the task can be canceled, and that is
// what the requester asked for, rather than an automatic payment again or
// a task waiting for another decision. It reports whether a cancel was
// pending.
func (d *Daemon) carryOutRequestedCancel(ctx context.Context, ixID string) bool {
	ix, err := d.ix.Get(ixID)
	if err != nil || ix.Role != interactions.RoleOutbound || ix.IsTerminal() ||
		ix.PayState == interactions.PaySubmitted || ix.PayState == interactions.PayCompleted {
		return false
	}
	msgs, err := d.ix.Messages(ixID)
	if err != nil {
		return false
	}
	requested := false
	for _, m := range msgs {
		// A cancel of an unpaid task ends it, so one on a task still open
		// was sent while a payment was in flight.
		if m.Kind == interactions.MsgCancel && m.SenderAID == d.AID() {
			requested = true
			break
		}
	}
	if !requested {
		return false
	}
	log.Printf("anet: %s: the payment did not go through; carrying out the cancel sent while it was pending", ixID)
	if _, err := d.CancelTask(ctx, ixID); err != nil && !errors.Is(err, ErrNotCancelable) {
		log.Printf("anet: %s: carry out the pending cancel: %v", ixID, err)
	}
	return true
}

// heldMessage is a follow-up stored when its approval-queue item was
// approved, to be published after the commit.
type heldMessage struct {
	seq  int64
	kind string
}

// addHeldFollowup records one follow-up of an approved item the way the
// receive path records the same message on a live interaction
// (ingestMessage): a text with x402.payment.status is a payment message,
// kept as metadata only on a capability call, and the state moves as the
// message moves it. The seq is 0 when nothing was stored.
func addHeldFollowup(tx *interactions.Tx, ixID, from string, isCap bool, f interactions.PendingFollowup) (heldMessage, error) {
	var rec interactions.MessageRecord
	switch f.Kind {
	case delegation.ChatText:
		kind, body := interactions.MsgText, f.Body
		if hasPaymentStatus(f.Metadata) {
			kind = interactions.MsgPayment
			if isCap {
				body = ""
			}
		}
		rec = interactions.MessageRecord{InteractionID: ixID, SenderAID: from, Kind: kind, Body: body,
			MsgID: f.MsgID, Metadata: f.Metadata}
	case delegation.ChatEndRequest:
		rec = interactions.MessageRecord{InteractionID: ixID, SenderAID: from,
			Kind: interactions.MsgEndRequest, MsgID: f.MsgID}
	default:
		return heldMessage{}, nil
	}
	seq, stored, err := tx.AddMessageRecord(rec)
	if err != nil || !stored {
		return heldMessage{}, err
	}
	if next := stateOnMessage(true, rec.Kind, f.Metadata, ""); next != "" {
		if _, err := tx.SetState(ixID, next); err != nil {
			return heldMessage{}, err
		}
	}
	return heldMessage{seq: seq, kind: rec.Kind}, nil
}
