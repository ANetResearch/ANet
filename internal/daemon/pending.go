package daemon

// pending.go is the approval queue of the approve policy (A2A-DESIGN §5.3).
//
// A delegation from a peer that is neither denied nor allowed is held in the
// pending table (interactions/pending.go) in the same transaction as its
// replay row, and the requester is told status{submitted,
// anet.inbound=pending_approval}. Nothing in the daemon lists, answers or
// executes a held delegation; follow-up messages for it are appended to the
// held item (at most pendingFollowupsMax) and a cancel removes it.
//
// Approval is a human decision. The control route that approves is
// bearer-only; the CLI command that calls it (`anet inbound approve`)
// reads a confirmation from /dev/tty and refuses without one. The daemon
// cannot tell whether a caller holding the control token went through a
// TTY (§21 item 13).
//
// On approval the delegation is verified again against the requester's
// current KEL, with the key-state rule of §3.6 step 7 evaluated now, so a
// key the requester retired while the item waited is not honored past the
// rotation grace. An approved item becomes an interaction with
// trust=approved and follows the ordinary execution path.
//
// A held item that is not decided within ttl_hours expires and the
// requester is told status{rejected, anet.reason=pending_expired}.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// PendingView is a held delegation as the control plane (and MCP
// inbound_pending) reports it: metadata only. The task's goal and body are
// not included (A2A-DESIGN §5.3).
type PendingView struct {
	InteractionID string `json:"interaction_id"`
	Requester     string `json:"requester"`
	ArrivedAt     int64  `json:"arrived_at"`
	Bytes         int64  `json:"bytes"`
	RequestCID    string `json:"request_cid"`
	Capability    string `json:"capability,omitempty"`
	Attachments   int    `json:"attachments"`
	Followups     int    `json:"followups"`
	ExpiresAt     int64  `json:"expires_at"`
}

// ErrNotPending is returned when no held delegation has the given id.
var ErrNotPending = errors.New("anet: no held delegation with that id")

// holdDelegate is step 10 for a delegation the approve policy holds.
func (d *Daemon) holdDelegate(m *rxMsg) rxResult {
	in := d.config().inbound()
	requestCID, err := anetcid.Sum(m.tdBytes)
	if err != nil {
		return d.drop(dropBadTaskDoc, err)
	}
	stripped := *m.dr
	stripped.Attachments = nil
	var atts []interactions.PendingAttachment
	for _, a := range m.dr.Attachments {
		atts = append(atts, interactions.PendingAttachment{Name: a.Name, Mime: a.Mime, Size: a.Size, CID: a.CID})
	}
	delegateBytes, err := stripped.Marshal()
	if err != nil {
		return d.drop(dropBadBody, err)
	}
	kelBytes, err := identity.MarshalKEL(m.kel)
	if err != nil {
		return d.drop(dropBadBody, err)
	}
	var keys []byte
	if m.noticeKeys != nil {
		keys = m.noticeKeys.signed
	}
	capID, _, _ := capabilityCall(m.td)
	item := interactions.PendingItem{
		IX: m.ix, FromAID: m.from, ArrivedAt: int64(d.nowMS()), MsgTS: m.ts, KeyState: m.ksn,
		Capability: capID, RequestCID: requestCID, Bytes: int64(len(m.body)),
		Delegate: delegateBytes, KEL: kelBytes, Keys: keys, ContextID: m.dr.ContextID, Attachments: atts,
	}
	res := d.commitRx(m, func(tx *interactions.Tx) error {
		if err := tx.PutPending(item, in.Pending.MaxTotal, in.Pending.MaxPerPeer); err != nil {
			if errors.Is(err, interactions.ErrPendingFull) {
				return rxPermanent(dropRefusedPrefix+reasonPendingFull, err)
			}
			return err
		}
		return nil
	})
	switch {
	case res.class == rxAccepted:
		d.sendNoticeStatus(m, delegation.StateSubmitted, "this node holds the task for its operator's approval",
			map[string]any{"anet.inbound": "pending_approval"})
	case res.reason == dropRefusedPrefix+reasonPendingFull:
		d.refused.add(replayKey(m.from, m.mid))
		d.noteRefused(m.from, m.ix, m.typ, reasonPendingFull, capID)
		d.replyRejected(m, reasonPendingFull, 0)
	}
	return res
}

// routePendingMessage is step 10 for a message whose interaction is held:
// a cancel removes the held item; anything else is appended to it.
func (d *Daemon) routePendingMessage(m *rxMsg) rxResult {
	cm := m.cm
	if cm.Kind == delegation.KindCancel {
		return d.commitRx(m, func(tx *interactions.Tx) error {
			_, err := tx.DeletePending(m.ix)
			return err
		})
	}
	f := interactions.PendingFollowup{Kind: cm.Kind, Body: cm.Body, MsgID: cm.MsgID, At: int64(d.nowMS())}
	if len(cm.Metadata) > 0 && json.Valid(cm.Metadata) {
		f.Metadata = json.RawMessage(cm.Metadata)
	}
	return d.commitRx(m, func(tx *interactions.Tx) error {
		err := tx.AppendPendingFollowup(m.ix, f, pendingFollowupsMax)
		if errors.Is(err, interactions.ErrPendingFollowupsFull) {
			return rxPermanent("pending-followups-full", err)
		}
		return err
	})
}

// PendingList returns the held delegations, oldest first, metadata only.
func (d *Daemon) PendingList() ([]PendingView, error) {
	items, err := d.ix.ListPending()
	if err != nil {
		return nil, err
	}
	ttl := int64(d.config().inbound().Pending.TTLHours) * 3600 * 1000
	out := make([]PendingView, 0, len(items))
	for _, p := range items {
		out = append(out, PendingView{
			InteractionID: p.IX, Requester: p.FromAID, ArrivedAt: p.ArrivedAt, Bytes: p.Bytes,
			RequestCID: p.RequestCID, Capability: p.Capability, Attachments: len(p.Attachments),
			Followups: len(p.Followups), ExpiresAt: p.ArrivedAt + ttl,
		})
	}
	return out, nil
}

// ApprovePending turns a held delegation into an interaction with
// trust=approved, after verifying it again (see the file comment). The
// requester's KEL and key set are recorded in peer_identity: approval is an
// authorized context (A2A-DESIGN §3.8).
func (d *Daemon) ApprovePending(ixID string) (*interactions.Interaction, error) {
	item, err := d.ix.GetPending(ixID)
	if errors.Is(err, interactions.ErrNotFound) {
		return nil, ErrNotPending
	}
	if err != nil {
		return nil, err
	}
	dr, td, tdBytes, kel, keys, err := d.reverifyPending(item)
	if err != nil {
		return nil, fmt.Errorf("anet: %s no longer verifies, not approved: %w", ixID, err)
	}
	requestCID, err := anetcid.Sum(tdBytes)
	if err != nil {
		return nil, err
	}
	capID, _, isCap := capabilityCall(td)
	goal := delegation.TaskGoal(td)
	var goalSeq int64
	err = d.ix.Update(func(tx *interactions.Tx) error {
		ok, err := tx.DeletePending(ixID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotPending
		}
		if _, err := tx.Get(ixID); err == nil {
			return fmt.Errorf("anet: interaction %s already exists", ixID)
		} else if !errors.Is(err, interactions.ErrNotFound) {
			return err
		}
		if err := tx.Create(interactions.New{ID: ixID, Role: interactions.RoleInbound, PeerAID: item.FromAID,
			Goal: goal, RequestCID: requestCID, RequestDoc: tdBytes, ContextID: item.ContextID,
			Trust: interactions.TrustApproved, IsCapability: isCap, TaskNonce: taskNonce(td)}); err != nil {
			return err
		}
		goalSeq, err = tx.AddMessage(ixID, item.FromAID, interactions.MsgText, goal)
		if err != nil {
			return err
		}
		for _, f := range item.Followups {
			switch f.Kind {
			case delegation.ChatText:
				if _, _, err := tx.AddMessageRecord(interactions.MessageRecord{InteractionID: ixID,
					SenderAID: item.FromAID, Kind: interactions.MsgText, Body: f.Body, MsgID: f.MsgID,
					Metadata: f.Metadata}); err != nil {
					return err
				}
				if _, err := tx.SetState(ixID, interactions.StateWorking); err != nil {
					return err
				}
			case delegation.ChatEndRequest:
				if _, err := tx.AddMessage(ixID, item.FromAID, interactions.MsgEndRequest, ""); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	d.publishMessage(ixID, goalSeq, interactions.MsgText)
	if err := d.notePeer(item.FromAID, kel, keys, "", false); err != nil {
		log.Printf("anet: record %s's identity after approval: %v", item.FromAID, err)
	}
	d.recordReceived(ixID, item.FromAID, interactions.TrustApproved, requestCID, capID)
	d.recordPolicyChange("inbound.pending", ixID, "approved", map[string]any{"requester_aid": item.FromAID})
	d.publishState(ixID)
	if isCap {
		_, args, _ := capabilityCall(td)
		d.goBackground(func() { d.runCapabilityCall(ixID, capID, args, dr.Payment, nil) })
	} else {
		d.kickAutoReply()
		for _, f := range item.Followups {
			if f.Kind == delegation.ChatEndRequest {
				d.goBackground(func() {
					cctx, cancel := context.WithTimeout(d.ctx, hubCallTimeout)
					defer cancel()
					if err := d.CompleteTask(cctx, ixID); err != nil {
						log.Printf("anet: %s: complete on the held end request: %v", ixID, err)
					}
				})
				break
			}
		}
	}
	return d.ix.Get(ixID)
}

// reverifyPending checks a held delegation again at approval time.
func (d *Daemon) reverifyPending(item *interactions.PendingItem) (*delegation.DelegateReq,
	*tsir.TaskDoc, []byte, []identity.SignedEvent, *peerKeySet, error) {
	dr, err := delegation.UnmarshalDelegateReq(item.Delegate)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	carried, err := identity.UnmarshalKEL(item.KEL)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	aid, err := replayedAID(carried)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if aid != item.FromAID {
		return nil, nil, nil, nil, nil, fmt.Errorf("held KEL replays to %s, not %s", aid, item.FromAID)
	}
	var stored []identity.SignedEvent
	if row, rerr := d.ix.PeerIdentity(item.FromAID); rerr == nil && len(row.KEL) > 0 {
		stored, _ = identity.UnmarshalKEL(row.KEL)
	} else if rerr != nil && !errors.Is(rerr, interactions.ErrNotFound) {
		return nil, nil, nil, nil, nil, rerr
	}
	kel, _, err := mergeKEL(stored, carried)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	signer, td, tdBytes, err := delegation.VerifyDelegateReqWithKEL(dr, kel, item.MsgTS)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if signer != item.FromAID {
		return nil, nil, nil, nil, nil, fmt.Errorf("TaskDoc signed by %s", signer)
	}
	if dr.Envelope != nil {
		if err := d.keyStateUsable(kel, dr.Envelope.KeyStateSeq); err != nil {
			return nil, nil, nil, nil, nil, err
		}
	}
	var keys *peerKeySet
	if len(item.Keys) > 0 {
		if signed, kerr := seal.UnmarshalSignedEncKeySet(item.Keys); kerr == nil {
			if set, verr := seal.VerifyEncKeySet(signed, item.FromAID, kel, d.nowMS()); verr == nil {
				keys = &peerKeySet{signed: item.Keys, set: set}
			}
		}
	}
	return dr, td, tdBytes, kel, keys, nil
}

// keyStateUsable applies the key-state rule of §3.6 step 7 at the current
// time: a signature by the active key state is usable; one by a retired key
// state only within the rotation grace after its retirement.
func (d *Daemon) keyStateUsable(kel []identity.SignedEvent, ksn uint64) error {
	states, err := identity.Replay(kel)
	if err != nil {
		return err
	}
	if ksn >= uint64(len(states)) {
		return fmt.Errorf("key state %d beyond the KEL", ksn)
	}
	ks := states[ksn]
	if ks.Status == identity.StatusActive {
		return nil
	}
	grace := uint64(d.rotationGrace() / time.Millisecond)
	if now := d.nowMS(); ks.SupersededAt == 0 || (now > ks.SupersededAt && now-ks.SupersededAt > grace) {
		return fmt.Errorf("signed by key state %d, retired at %d, beyond the rotation grace", ksn, ks.SupersededAt)
	}
	return nil
}

// RejectPending removes a held delegation and tells the requester
// status{rejected, anet.reason=operator_rejected}.
func (d *Daemon) RejectPending(ixID string) error {
	item, err := d.ix.GetPending(ixID)
	if errors.Is(err, interactions.ErrNotFound) {
		return ErrNotPending
	}
	if err != nil {
		return err
	}
	ok, err := d.ix.DeletePending(ixID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotPending
	}
	d.recordPolicyChange("inbound.pending", ixID, "rejected", map[string]any{"requester_aid": item.FromAID})
	d.tellHeldRequester(item, reasonOperatorRejected)
	return nil
}

// expirePending rejects held items older than the TTL.
func (d *Daemon) expirePending() { d.expirePendingAt(int64(d.nowMS())) }

// expirePendingAt is expirePending with the current time given (unix ms).
func (d *Daemon) expirePendingAt(now int64) {
	ttl := int64(d.config().inbound().Pending.TTLHours) * 3600 * 1000
	cutoff := now - ttl
	items, err := d.ix.ExpiredPending(cutoff)
	if err != nil {
		log.Printf("anet: read expired held delegations: %v", err)
		return
	}
	for _, item := range items {
		ok, err := d.ix.DeletePending(item.IX)
		if err != nil || !ok {
			continue
		}
		d.noteRefused(item.FromAID, item.IX, seal.TypeDelegate, reasonPendingExpired, item.Capability)
		d.tellHeldRequester(item, reasonPendingExpired)
	}
}

// tellHeldRequester sends status{rejected} to the requester of a held item,
// sealed to the key set the delegation carried. It is attempted once and
// not queued; it goes through the same per-peer and daemon-wide limit as
// every other refusal.
func (d *Daemon) tellHeldRequester(item *interactions.PendingItem, reason string) {
	if len(item.Keys) == 0 || !d.notices.allow(item.FromAID, d.nowMS()) {
		d.count(noticeSuppressed)
		return
	}
	kel, err := identity.UnmarshalKEL(item.KEL)
	if err != nil {
		return
	}
	signed, err := seal.UnmarshalSignedEncKeySet(item.Keys)
	if err != nil {
		return
	}
	set, err := seal.VerifyEncKeySet(signed, item.FromAID, kel, d.nowMS())
	if err != nil {
		return
	}
	meta, _ := json.Marshal(map[string]any{"anet.reason": reason})
	body, err := (&delegation.StatusMsg{State: delegation.StateRejected,
		Text: "this node did not accept the task: " + reason, Metadata: meta, At: d.nowMS()}).Marshal()
	if err != nil {
		return
	}
	d.count(noticeSent)
	d.goBackground(func() {
		env, err := d.sealWith(item.FromAID, seal.TypeStatus, item.IX, body, set)
		if err != nil {
			return
		}
		cctx, cancel := context.WithTimeout(d.ctx, hubCallTimeoutShort)
		defer cancel()
		if err := d.deliverEnvelope(cctx, item.FromAID, env); err != nil {
			d.logOnce("notice:"+item.FromAID, "anet: refusal notice to %s not delivered: %v", item.FromAID, err)
		}
	})
}
