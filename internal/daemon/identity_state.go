package daemon

// identity_state.go is the identity's state table and the outbound gate
// (KEL §4.3.1, D-1), the reseal trigger of the key state (KEL D-51) and the
// arrival time of an envelope (KEL D-20): the pure functions the outbound
// engine and the receive pipeline are built on (engine §0.2), and the thin
// shell that reads their inputs from the running daemon.
//
// How the engine calls them:
//
//   - identityGate() is the one gate (engine ENG-K13, SI-84). While it is
//     closed the daemon hands no envelope to any transport (outbox rows and
//     one-shot envelopes alike), seals nothing, and sends no relayauth-signed
//     request outside the join and the publication sequences; only the
//     anonymous status queries go on. Its State is also what
//     anet.delivery_wait shows for a row it holds back (KEL D-4): publishing
//     is rotation_pending, notice_stale is notice_stale. It is read at the
//     moment it is used, never cached in a row.
//   - kelResealDue(row.KELSealed, d.keyCursor()) decides triggers T3 and T4
//     for a row about to be transmitted (engine ENG-R2, ENG-R4). The row's
//     sealed_kss and sealed_notice_at are the two KELSealed columns; the
//     engine writes them when it seals a version: the key_state_seq the
//     signature was made under and the observed_at of the notice it carried.
//   - A version is sealed with one Snapshot s of the controller: s signs,
//     s.KEL() is the KEL it carries, and tipNotice(s) the notice (inner key
//     11). When tipNotice(s) is nil the engine does not seal in this attempt
//     (a rotation was built after the gate was read).
//   - The sender's key state of a received envelope is judged only through
//     identity.SenderKeyUsable (KEL C-13), never through RetiredUsableAt or a
//     grace computed here: receive step 7 calls seal.VerifyInnerSig(inner,
//     preimage, the merged KEL, arrival), which calls it; a held task
//     approved later calls it with the arrival saved at step 10 (KEL D-25).
//     arrival is hubArrival(accepted_at, inner.ts, now) for an envelope from
//     the hub mailbox and now for a direct delivery (KEL D-20); step 6½,
//     seal.CheckNotice(inner, the KEL the envelope carried, hubKELs,
//     arrival), takes the same value. An emergency rot or a dip the merge of
//     step 6 brings in is identity.MergedEmergency(old, merged) (KEL D-44).

import (
	"sync"
	"sync/atomic"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"
)

// The age limits of this node's own notice (KEL §2.12), unix ms. Neither is
// configurable.
const (
	// NoticeResealAge is how old the notice a queued version carries may get
	// before the version is sealed again with a newer one (KEL D-51 (b)).
	NoticeResealAge uint64 = 3 * 60 * 60 * 1000
	// NoticeStopLine is how old this node's latest notice may get before the
	// gate closes (KEL D-52). A recipient refuses a notice older than
	// identity.NoticeMaxAge when the envelope arrives; the node stops 10
	// minutes short of that, so an envelope handed over just before the stop
	// line still arrives in time.
	NoticeStopLine uint64 = identity.NoticeMaxAge - 10*60*1000
)

// A version resealed for its notice's age carries a notice the gate still
// lets out: the reseal age is under the stop line (the constant underflows,
// and the build fails, otherwise).
const _ uint64 = NoticeStopLine - NoticeResealAge - 1

// The identity states (KEL §4.3.1; /identity/status.state, ux UX-R5). The
// gate is open in IdentityOK alone.
const (
	// IdentityNoHub: the KEL names no witness yet (the icp alone, R-4): the
	// first join has not finished.
	IdentityNoHub = "no_hub"
	// IdentityPublishing: a key event is pending (IdentityFile key 4) and the
	// witness has not accepted it yet.
	IdentityPublishing = "publishing"
	// IdentityOK: the witness confirmed the KEL tip less than NoticeStopLine
	// ago.
	IdentityOK = "ok"
	// IdentityNoticeStale: no notice confirms the KEL tip, or the latest one is
	// older than NoticeStopLine.
	IdentityNoticeStale = "notice_stale"
	// IdentityStopped: the identity was rotated or recovered elsewhere (KEL
	// D-27); for good.
	IdentityStopped = "stopped"
	// IdentityDeactivated: this node deactivated the identity (KEL D-46); for
	// good.
	IdentityDeactivated = "deactivated"
)

// The sentence keys of the identity states besides the join's (ux §1.4 E2,
// E3).
const (
	reasonRotationPending = "rotation_pending"
	reasonNoticeStale     = "notice_stale"
	reasonQuarantine      = "identity.quarantine"
	reasonConfirmAging    = "identity.confirm_aging"
	reasonIdentityOK      = "identity.ok"
)

// The values of key_state_notice_error a hub answers instead of a notice
// (KEL H-13), and the hub's reason for refusing a key event whose ts is off
// its clock (KEL §2.7).
const (
	noticeErrQuarantine  = "quarantine"
	noticeErrNotAdmitted = "not_admitted"
	hubKELTSOutOfWindow  = "kel_ts_out_of_window"
)

// IdentityState is a row of the state table (KEL §4.3.1): the state and the
// sentence key that says it (ux §1.4 E2, E3; the hub row's key in
// IdentityNoHub, F).
type IdentityState struct {
	State  string
	Reason string
}

// GateState is the outbound gate (KEL D-1): Open in IdentityOK alone, and
// the state and sentence key it was decided from.
type GateState struct {
	Open   bool
	State  string
	Reason string
}

// gateOf is the gate of an identity state.
func gateOf(s IdentityState) GateState {
	return GateState{Open: s.State == IdentityOK, State: s.State, Reason: s.Reason}
}

// identityFacts is what the state table reads of the identity file (KEL
// §2.9): key 6, whether key 4 holds a pending event, and the tip of key 1.
type identityFacts struct {
	// State is IdentityFile key 6: identity.StateOK, StateStopped or
	// StateDeactivated.
	State string
	// Pending is IdentityFile key 4 present: an event being published.
	Pending bool
	// AID, TipSeq and TipID are the identity and the seq and event id of the
	// last event the witness accepted.
	AID    string
	TipSeq uint64
	TipID  string
	// Witness is the witness hub the accepted KEL names (key 9 of its last
	// rot); "" while the witness is pending (KEL R-4).
	Witness string
}

// identityFactsOf reads c. A nil controller gives the zero facts, which the
// table takes for a stopped identity.
//
// It reads the pending event before the accepted KEL and the state last: an
// Adopt between the reads then shows as still publishing, and a stop as
// stopped, so a race can close the gate early but never open it early.
func identityFactsOf(c *identity.Controller) identityFacts {
	if c == nil {
		return identityFacts{}
	}
	_, pending := c.Pending()
	f := kelFacts(c.AID(), c.Accepted())
	f.Pending = pending
	f.State = c.State()
	return f
}

// kelFacts is the tip of kel, the KEL of aid, and the witness it names: key
// 9 of its last establishment event, so a hub switch names the new hub.
func kelFacts(aid string, kel []identity.SignedEvent) identityFacts {
	f := identityFacts{AID: aid}
	if n := len(kel); n > 0 {
		f.TipSeq, f.TipID = kel[n-1].Event.Seq, kel[n-1].EventID
	}
	for i := len(kel) - 1; i >= 0; i-- {
		if e := kel[i].Event; e.Type == identity.Inception || e.Type == identity.Rotation {
			if len(e.Witnesses) > 0 {
				f.Witness = e.Witnesses[0]
			}
			break
		}
	}
	return f
}

// keyStateNotice is this node's latest witness notice (KEL D-50): the
// NoticeWire bytes its witness hub sent, which a sealed version carries in
// inner key 11, and their body (identity.DecodeNotice of Wire).
type keyStateNotice struct {
	Wire []byte
	Body identity.NoticeBody
}

// confirms reports whether n is a notice about the tip f names: the same AID,
// seq and event id, signed for the witness the KEL names. Any other notice
// says nothing about the key state this node signs under: a recipient refuses
// it (witness-mismatch, witness-invalid), so it counts as no notice.
func (n *keyStateNotice) confirms(f identityFacts) bool {
	return n != nil && f.Witness != "" && n.Body.AID == f.AID && n.Body.Seq == f.TipSeq &&
		n.Body.Head == f.TipID && n.Body.Witness == f.Witness
}

// identityInputs are the inputs of the state table (KEL §4.3.1).
type identityInputs struct {
	// File is what the table reads of the identity file.
	File identityFacts
	// Notice is this node's latest notice; nil when it holds none.
	Notice *keyStateNotice
	// NoticeErr is key_state_notice_error of the latest hub answer that
	// carried no notice (quarantine, not_admitted, not_witness), or "".
	NoticeErr string
	// PublishErr is the hub's reason for refusing the latest attempt to
	// publish the pending event (core §2.15.1), or "".
	PublishErr string
	// HubReason is the sentence key of the hub row (the join state, ux §1.4
	// F): what IdentityNoHub says, since a state without a witness is the
	// join's to explain (ux UX-C9). "" is no hub configured.
	HubReason string
	// Now is unix ms.
	Now uint64
}

// identityStateOf is the state table (KEL §4.3.1), the first row that holds
// from the top:
//
//	1  key 6 deactivated                          deactivated   identity_deactivated
//	2  key 6 stopped                              stopped       identity_stopped
//	3  the KEL tip's witness is pending           no_hub        the hub row's key
//	4  key 4 holds a pending event                publishing    rotation_pending; clock_skew when
//	                                                            the witness refused its ts
//	5  no notice confirms the tip, or it is       notice_stale  notice_stale; identity.quarantine or
//	   older than NoticeStopLine                                hub_pending_admission after the
//	                                                            hub's quarantine or not_admitted
//	6  the notice is older than NoticeResealAge   ok            identity.confirm_aging
//	7  otherwise                                  ok            identity.ok
//
// The gate is open in ok alone (gateOf). A key 6 value an identity file
// cannot hold fails closed, as stopped. A notice observed after now (the
// hub's clock ahead of this node's) has age 0.
func identityStateOf(in identityInputs) IdentityState {
	f := in.File
	switch {
	case f.State == identity.StateDeactivated:
		return IdentityState{IdentityDeactivated, reasonDeactivated}
	case f.State != identity.StateOK:
		return IdentityState{IdentityStopped, reasonStopped}
	case f.Witness == "":
		r := in.HubReason
		if r == "" {
			r = reasonNoHub
		}
		return IdentityState{IdentityNoHub, r}
	case f.Pending:
		if in.PublishErr == hubKELTSOutOfWindow {
			return IdentityState{IdentityPublishing, reasonClockSkew}
		}
		return IdentityState{IdentityPublishing, reasonRotationPending}
	}
	if !in.Notice.confirms(f) || noticeAge(in.Notice, in.Now) > NoticeStopLine {
		switch in.NoticeErr {
		case noticeErrQuarantine:
			return IdentityState{IdentityNoticeStale, reasonQuarantine}
		case noticeErrNotAdmitted:
			return IdentityState{IdentityNoticeStale, reasonPending}
		}
		return IdentityState{IdentityNoticeStale, reasonNoticeStale}
	}
	if noticeAge(in.Notice, in.Now) > NoticeResealAge {
		return IdentityState{IdentityOK, reasonConfirmAging}
	}
	return IdentityState{IdentityOK, reasonIdentityOK}
}

// noticeAge is now − observed_at, and 0 for a notice observed after now.
func noticeAge(n *keyStateNotice, now uint64) uint64 {
	if now <= n.Body.ObservedAt {
		return 0
	}
	return now - n.Body.ObservedAt
}

// KELSealed is what an outbox version records about the key state it was
// sealed under (engine ENG-S3): the outbox columns sealed_kss and
// sealed_notice_at. The engine's row view embeds it and hands it to
// kelResealDue.
type KELSealed struct {
	// SealedKSS is the key_state_seq the version's signature was made under.
	SealedKSS uint64
	// SealedNoticeAt is the observed_at of the notice the version carries.
	SealedNoticeAt uint64
}

// KeyCursor is this node's key state at the moment of an attempt (KEL D-51).
type KeyCursor struct {
	// KeyStateSeq is the key_state_seq this node signs under now.
	KeyStateSeq uint64
	// NoticeObservedAt is the observed_at of the notice confirming the KEL
	// tip; 0 when there is none.
	NoticeObservedAt uint64
	// Now is unix ms.
	Now uint64
}

// kelResealDue reports whether the key state makes row's current version
// unfit to transmit, so that the engine seals a new one first (KEL D-51):
//
//   - T3: it was signed under another key state than this node's now. Every
//     input is stored, so the answer survives a crash.
//   - T4: the notice it carries is more than NoticeResealAge old, and this
//     node holds a newer one. Without one (the hub out of reach) a new
//     version would carry the same notice, so none is made; the gate caps the
//     notice's age instead (KEL D-52).
func kelResealDue(row KELSealed, cur KeyCursor) bool {
	if row.SealedKSS != cur.KeyStateSeq {
		return true
	}
	return cur.Now > row.SealedNoticeAt && cur.Now-row.SealedNoticeAt > NoticeResealAge &&
		cur.NoticeObservedAt > row.SealedNoticeAt
}

// hubArrival is when an envelope taken from the hub mailbox counts as
// arrived (KEL D-20): min(now, max(accepted_at, inner.ts − 5 minutes)). The
// hub's accepted_at is taken, but never earlier than the clock skew before
// the sender's own time, and never later than now. A direct delivery
// arrives at now.
//
// accepted_at comes from this node's own hub and cannot be checked here: the
// judgements that use it (the planned-rotation grace, a notice's freshness)
// trust that hub; the sender's emergency retirement does not use it
// (identity.SenderKeyUsable).
func hubArrival(acceptedAt, innerTS, now uint64) uint64 {
	var floor uint64
	if innerTS > seal.ClockSkewMS {
		floor = innerTS - seal.ClockSkewMS
	}
	return min(max(acceptedAt, floor), now)
}

// identityGateInputs is what the gate reads besides the identity file and
// the join record: this node's latest notice, the latest key_state_notice_error,
// and the hub's latest refusal of the pending event.
//
// v030-todo(KEL-8): key_state_notice.cbor is loaded at start and the notice
// or its error is recorded from every register and poll answer (KEL D-50);
// v030-todo(KEL-9): the publication sequence records its refusals here.
type identityGateInputs struct {
	mu         sync.Mutex
	notice     *keyStateNotice
	noticeErr  string
	publishErr string
	// gateHook, when set, replaces the gate. Tests only, through
	// setIdentityGate (testhooks.go).
	gateHook atomic.Pointer[func() GateState]
}

// noteNotice records what the latest hub answer said of this node's key
// state: a notice (n, err "") or why it carried none (n nil, err its
// key_state_notice_error). An answer without a notice keeps the one held.
//
// The gate checks only that n is about this node's tip and witness, not its
// signature or its time: the caller records a notice that
// identity.VerifyNotice accepted for this node's KEL at arrival now, which
// also keeps out one observed more than the clock skew after now.
func (g *identityGateInputs) noteNotice(n *keyStateNotice, noticeErr string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n != nil {
		g.notice, g.noticeErr = n, ""
		return
	}
	g.noticeErr = noticeErr
}

// notePublishError records the hub's reason for refusing the pending event;
// "" once it is accepted.
func (g *identityGateInputs) notePublishError(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.publishErr = reason
}

func (g *identityGateInputs) read() (n *keyStateNotice, noticeErr, publishErr string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.notice, g.noticeErr, g.publishErr
}

// identityInputs reads the inputs of the state table now.
func (d *Daemon) identityInputs() identityInputs {
	in := identityInputs{File: identityFactsOf(d.self), Now: d.nowMS()}
	in.Notice, in.NoticeErr, in.PublishErr = d.idGate.read()
	if r := d.joinSnapshot(); r != nil {
		in.HubReason = r.Reason
	}
	return in
}

// identityState is the identity's state now (KEL §4.3.1).
func (d *Daemon) identityState() IdentityState {
	return identityStateOf(d.identityInputs())
}

// identityGate is the outbound gate now (KEL D-1; engine ENG-K13): the only
// one the engine consults. It is read where it is used, not kept.
func (d *Daemon) identityGate() GateState {
	if f := d.testIdentityGate(); f != nil {
		return f()
	}
	return gateOf(d.identityState())
}

// tipNotice is the notice a version signed by s carries in inner key 11, its
// observed_at the version's sealed_notice_at (KEL D-50, engine ENG-K17):
// this node's latest notice when it confirms the tip of s's KEL, else nil.
//
// s is the Snapshot the version is signed with, and its KEL is the one the
// version carries, so the signature, the KEL and the notice describe one key
// state. The gate is read before the Snapshot is taken, and a rotation built
// in between makes the controller sign under the new key at once; the notice
// held then confirms the old tip, and a recipient would refuse the version
// for good (witness-mismatch). So nil means: do not seal in this attempt. The
// gate is closed at its next read.
func (d *Daemon) tipNotice(s identity.Signer) *keyStateNotice {
	if s == nil {
		return nil
	}
	if n, _, _ := d.idGate.read(); n.confirms(kelFacts(s.AID(), s.KEL())) {
		return n
	}
	return nil
}

// keyCursor is the key state a version sealed now would record, for
// kelResealDue: the key_state_seq a Snapshot of the controller signs under,
// and the observed_at of tipNotice for that Snapshot (0 when there is none).
//
// identityGate and keyCursor are each a reading at one moment. A caller that
// seals afterwards takes its own Snapshot s, seals only when tipNotice(s) is
// not nil, and records identity.SeqOf(s) and that notice's observed_at: a
// rotation built in between leaves tipNotice(s) nil, and one adopted in
// between shows as T3 at the next attempt.
func (d *Daemon) keyCursor() KeyCursor {
	cur := KeyCursor{Now: d.nowMS()}
	if d.self == nil {
		return cur
	}
	s := d.self.Snapshot()
	cur.KeyStateSeq, _ = identity.SeqOf(s)
	if n := d.tipNotice(s); n != nil {
		cur.NoticeObservedAt = n.Body.ObservedAt
	}
	return cur
}
