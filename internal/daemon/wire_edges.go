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
//   - follow-ups held with an approval-queue item are recorded by kind
//     (A2A-DESIGN §5.3).

import (
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
	if ix.PayState == interactions.PaySubmitted || d.untakenPayment(ix) != nil {
		// Not executed: the payment's outcome is not known yet, or the
		// payment was not taken yet, and startPayments presents it again
		// (A2A-DESIGN §8.3).
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
		// before it starts, so this one stopped in between, unless it is
		// waiting for a payment; a short one is run again by the
		// redelivery of its delegation.
		if long && ix.PayState != interactions.PayRequired && ix.PayState != interactions.PayFailed {
			return leftoverInterrupted
		}
		return leftoverKeep
	case interactions.StateWorking:
		if resolvable && !long && (ix.PayState == interactions.PayNone || ix.PayState == interactions.PayCompleted) {
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

// skippedPaidKeep forgets the reported ids not in current: tasks that have
// ended, or whose peer is no longer denied.
func (d *Daemon) skippedPaidKeep(current map[string]bool) {
	d.peerLists.skippedPaid.Range(func(k, _ any) bool {
		if id, _ := k.(string); !current[id] {
			d.peerLists.skippedPaid.Delete(k)
		}
		return true
	})
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
