package daemon

// receive.go is the receive pipeline for sealed envelopes (A2A-DESIGN §3.6).
// Every envelope, from the hub mailbox or from a transport module, passes
// through receiveEnvelope, and nothing reaches the interaction store any
// other way.
//
//	step 0   (transport modules only) daemon-wide rate limit before Open — transport.go
//	step 1-4 outer checks, key lookup, HPKE open, inner decode         seal.Open
//	step 5   time window                                                 seal.CheckTime
//	step 6   sender KEL: inner vs stored, extension rule, no network     resolveSenderKEL
//	step 7   signature under the resolved KEL, rotation grace            seal.VerifyInnerSig
//	step 8   attached key set: advisory, high-water rule                 seal.VerifyInnerKeys
//	         (from, mid) lock, refused list and replay table: a duplicate
//	         is not judged again                                         duplicate
//	step 9   authorization by type (no writes)                           authorize*
//	step 10  business write and replay row in one transaction, then
//	         side effects                                                ingest* (delegation.go)
//
// Failure classes. Permanent (P): the envelope is acknowledged and dropped,
// and the reason counted; the same bytes would fail the same way again.
// Temporary (T): a store error, the transport rate limit, a message that
// arrived before the delegation it belongs to; the envelope is not
// acknowledged, so the hub delivers it again and a p2p sender falls back to
// the hub. A temporary failure becomes permanent once the message has
// expired.
//
// Every value a handler acts on comes from the authenticated inner message:
// the sender is inner.from as proven by the signature in step 7, and the
// interaction is inner.ix. Nothing is taken from transport metadata.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
)

// rxClass is the outcome class of one envelope.
type rxClass uint8

const (
	rxAccepted  rxClass = iota + 1 // processed; acknowledge
	rxDropped                      // permanent refusal or duplicate; acknowledge
	rxTransient                    // temporary refusal; do not acknowledge
)

// rxResult is what the pipeline decided about one envelope.
type rxResult struct {
	class  rxClass
	reason string
}

// ack reports whether the transport should acknowledge the delivery.
func (r rxResult) ack() bool { return r.class != rxTransient }

func accepted() rxResult { return rxResult{class: rxAccepted} }

// Counter names beyond the seal.Reason* strings. They are stable: tests and
// operators read them from /status (ReceiveStats).
const (
	dropBadTransport  = "bad-transport-encoding" // the hub or peer frame did not carry decodable envelope bytes
	dropKELFork       = "kel-fork"               // inner KEL forks from the stored KEL
	dropNotAccepting  = "not-accepting"          // delegation refused by the inbound policy (rows 1 and 6 of §5.2)
	dropBadBody       = "bad-body"               // inner body does not decode as its type
	dropIXMismatch    = "ix-mismatch"            // DelegateReq.InteractionID != inner.ix
	dropBadTaskDoc    = "bad-taskdoc"            // the TaskDoc does not verify under the resolved KEL
	dropSignerIsOther = "signer-mismatch"        // the TaskDoc signer is not inner.from
	dropIXCollision   = "ix-collision"           // delegation for an ix this node holds in another role or with another peer
	dropUnknownIX     = "unknown-ix"             // status/result, or an expired message, for an ix this node does not hold
	dropNotPeer       = "not-peer"               // sender is not the interaction's peer
	dropWrongRole     = "wrong-role"             // status/result for an interaction this node did not start
	dropUnknownType   = "unknown-type"           // inner type this node does not implement
	dropBadStatus     = "bad-status-state"       // StatusMsg with a state outside the six defined
	dropResultRefused = "result-refused"         // ResultResp whose receipt does not verify
	dropDuplicate     = "duplicate"              // (from, mid) already handled
	dropRefusedReplay = "refused-replay"         // (from, mid) refused before: the in-memory list or the refused table
	dropRefusedFloor  = "refused-floor"          // delegation at or below a refused-table floor, not in the replay table
	dropExpiredWait   = "expired-while-waiting"  // a temporary failure outlived the message's exp
	dropEmptyIX       = "empty-ix"               // a message type that needs an interaction id carries none

	dropRefusedPrefix      = "refused-"             // + an anet.reason: a delegation the inbound policy refused
	dropDenied             = "denied"               // message, status or result from a peer on the deny list
	dropNotAllowed         = "not-allowed"          // message on a trust=peer interaction from a peer no longer allowed
	dropPublicCapText      = "public-cap-text"      // text on a public capability call; only cancel, end_request and payment are taken
	dropAfterTerminal      = "input-after-terminal" // new input for an interaction in a terminal state
	dropEndRequestIgnored  = "end-request-ignored"  // end_request from the provider, or after a terminal state
	dropCancelFromProvider = "cancel-from-provider" // cancel sent by the provider; the provider sends status canceled
	dropEndAccept          = "end-accept-dropped"   // wire-1 end_accept; the provider completes on its own
	dropPendingCollision   = "pending-collision"    // delegation for an ix held in the approval queue for another peer

	keysInvalid  = "keys-invalid"  // attached key set did not verify (advisory; message still processed)
	keysFork     = "keys-fork"     // attached or fetched key set differs from the stored one at the same seq
	keysRollback = "keys-rollback" // attached or fetched key set is older than the stored one

	noticeTaskNotFound = "notice-task-not-found" // TaskNotFound status sent
	noticeSent         = "notice-sent"           // a refusal or pending notice sent
	noticeSuppressed   = "notice-rate-limited"   // a notice not sent: rate limit or no keys
	resendSuppressed   = "resend-rate-limited"   // a redelivered delegation's answer not re-sent: rate limit

	transientStore       = "t-store"          // interactions store error
	transientP2PRate     = "t-p2p-rate-limit" // step 0
	transientUnknownIX   = "t-unknown-ix"     // message before its delegation, inside the wait window
	transientReplayCheck = "t-replay-read"    // replay table read failed
)

// unknownIXWait is how long a message for an interaction this node does not
// know is treated as early rather than wrong (§3.6 step 9). The delegation
// and a follow-up can take different paths (p2p and hub) and arrive out of
// order; within the window the message stays in the mailbox.
const unknownIXWait = 10 * time.Minute

// rxMsg is an envelope that passed steps 1 to 8.
type rxMsg struct {
	from, typ, ix string
	mid           []byte
	ts, exp       uint64
	ksn           uint64 // the sender's key state seq in the inner message
	body          []byte
	// kel is the sender KEL resolved in step 6; the signature and every
	// nested signed object are checked against it.
	kel []identity.SignedEvent
	// kelUpdate is the KEL to store after acceptance: the inner KEL when
	// it extends the stored one or nothing is stored, else nil.
	kelUpdate []identity.SignedEvent
	// keysUpdate is the attached key set when it passed step 8 and is newer
	// than the stored one; noticeKeys is any verified set usable to reply.
	keysUpdate, noticeKeys *peerKeySet

	// Filled by step 9.
	existing *interactions.Interaction
	dr       *delegation.DelegateReq
	td       *tsir.TaskDoc
	tdBytes  []byte
	// requestCID is the CID of tdBytes: what a delegation asks for. A
	// delegation for an interaction this node holds must carry the same.
	requestCID string
	cm         *delegation.ChatMsg
	rr         *delegation.ResultResp
	sm         *delegation.StatusMsg

	// The inbound policy decision for a delegation (§5.2): the trust it is
	// accepted under, or hold for the approval queue. release frees the
	// admission slot of a public capability call.
	trust   string
	hold    bool
	release func()
	// pendingRoute marks a message for a delegation held in the approval
	// queue.
	pendingRoute bool
	// refusalStored marks a delegation whose refusal is in the refused
	// table already (recordRefusal).
	refusalStored bool
}

// receiveEnvelope runs steps 1 to 10 on one envelope.
func (d *Daemon) receiveEnvelope(ctx context.Context, env []byte) rxResult {
	now := d.nowMS()

	// Steps 1-4.
	op, err := seal.Open(env, d.AID(), d.enc)
	if err != nil {
		return d.drop(reasonOf(err), err)
	}
	in := &op.Inner

	// Step 5.
	if err := seal.CheckTime(in, now); err != nil {
		return d.drop(reasonOf(err), err)
	}
	if d.refused.has(replayKey(in.From, in.MID)) {
		return d.drop(dropRefusedReplay, nil)
	}

	// Step 6.
	use, update, row, res := d.resolveSenderKEL(in.From, op.KEL)
	if res != nil {
		return *res
	}

	// Step 7.
	if err := seal.VerifyInnerSig(in, op.Preimage, use, now, d.rotationGrace()); err != nil {
		return d.drop(reasonOf(err), err)
	}

	m := &rxMsg{from: in.From, typ: in.Type, ix: in.IX, mid: in.MID, ts: in.TS, exp: in.Exp,
		ksn: in.KeyStateSeq, body: in.Body, kel: use, kelUpdate: update}

	// Step 8: advisory. A key set that fails here does not affect the
	// message; the authenticity of the message was decided in step 7.
	if signed, set, kerr := seal.VerifyInnerKeys(in, use, now); kerr != nil {
		d.count(keysInvalid)
	} else {
		ks := &peerKeySet{signed: in.Keys, set: set}
		switch seal.DecideHighWater(seenOf(row), set.Seq, signed.Set) {
		case seal.Replace:
			m.keysUpdate, m.noticeKeys = ks, ks
		case seal.Same:
			m.noticeKeys = ks
		case seal.Fork:
			d.count(keysFork)
		case seal.Ignore:
			d.count(keysRollback)
		}
	}
	if m.noticeKeys == nil {
		if ks, ok := d.storedKeySet(row, now); ok {
			m.noticeKeys = ks
		}
	}

	// Steps 9 and 10 for one (from, mid) at a time, and a message handled
	// or refused before is not judged again [redteam:F26]. Two copies of
	// one envelope (the hub's and a p2p peer's, or a redelivery racing the
	// first) meet here: the second waits for the first to be decided and
	// committed, then finds it in the refused list or the replay table. It
	// takes no admission slot, sends no refusal and runs nothing the first
	// did not; a delegation's duplicate goes to the redelivery path.
	key := replayKey(m.from, m.mid)
	unlock := d.rxLocks.lock(key)
	defer unlock()
	if m.typ == seal.TypeDelegate && m.ix != "" {
		// The same delegation sealed twice (two message ids) is one
		// decision too: the second copy finds the interaction the first
		// created, and is a redelivery of it.
		unlockIX := d.rxIXLocks.lock(m.from + "\x00" + m.ix)
		defer unlockIX()
	}
	if d.refused.has(key) {
		return d.drop(dropRefusedReplay, nil)
	}
	// A delegation refused before is refused again, without a reply,
	// after a restart or after the in-memory list forgot it: the policy
	// may have changed since, and the requester was told `rejected`
	// [redteam:F5].
	var floor uint64
	if m.typ == seal.TypeDelegate {
		exact, f, err := d.ix.Refused(m.from, m.mid)
		if err != nil {
			return d.expireTransient(d.transient(transientReplayCheck, err), m, now)
		}
		if exact {
			d.refused.add(key)
			return d.drop(dropRefusedReplay, nil)
		}
		floor = f
	}
	seen, err := d.ix.ReplaySeen(m.from, m.mid)
	if err != nil {
		return d.expireTransient(d.transient(transientReplayCheck, err), m, now)
	}
	if seen {
		return d.expireTransient(d.duplicate(ctx, m), m, d.nowMS())
	}
	if m.ts <= floor {
		// Older than refusals of this sender's that the table let go of
		// (refused.go): it may be one of them.
		return d.drop(dropRefusedFloor, nil)
	}

	// Step 9.
	if res := d.authorize(m, now); res != nil {
		if res.class == rxDropped {
			d.noteRefusedEnvelope(m)
		}
		return d.expireTransient(*res, m, now)
	}

	// Step 10.
	out := d.process(ctx, m)
	if m.release != nil {
		// Admitted in step 9 but not executed (a store error, a duplicate):
		// the admission slot is returned.
		m.release()
		m.release = nil
	}
	return d.expireTransient(out, m, d.nowMS())
}

// noteRefusedEnvelope remembers an envelope step 9 refused for good: in the
// in-memory list, which a replay meets before any decryption work is
// repeated, and for a delegation also in the refused table, which outlives
// a restart and the list's eviction [redteam:F5]. A refusal the requester
// is told of was written before it was told (recordRefusal); for one that
// is not answered a table write that fails is logged, and the list still
// covers this process.
func (d *Daemon) noteRefusedEnvelope(m *rxMsg) {
	d.refused.add(replayKey(m.from, m.mid))
	if m.typ != seal.TypeDelegate || m.refusalStored {
		return
	}
	if err := d.ix.RecordRefused(m.from, m.mid, m.ts, m.exp); err != nil {
		log.Printf("anet: record the refused delegation %s from %s: %v", m.ix, m.from, err)
	}
}

// recordRefusal writes a delegation's refusal to the refused table before
// the requester is told `rejected` [redteam:F5]: a refusal the requester
// was told of must outlive a restart and the in-memory list, or the same
// envelope could be judged again later and run. One that cannot be written
// is not told either: the envelope is not acknowledged (T) and is judged
// again when it comes back, as if this delivery had not happened.
func (d *Daemon) recordRefusal(m *rxMsg) *rxResult {
	if err := d.ix.RecordRefused(m.from, m.mid, m.ts, m.exp); err != nil {
		r := d.transient(transientStore, fmt.Errorf("record the refusal of %s: %w", m.ix, err))
		return &r
	}
	m.refusalStored = true
	return nil
}

// duplicate handles an envelope whose (from, mid) the replay table holds:
// it was accepted before, and nothing is decided about it again. A
// delegation may mean the requester never got the answer: when the
// interaction it opened is still this sender's and the envelope carries the
// same request (authorizeRedelivery), redeliveredDelegate re-sends the
// answer or finishes work a crash interrupted. Anything else is
// acknowledged and dropped, without a reply.
func (d *Daemon) duplicate(ctx context.Context, m *rxMsg) rxResult {
	if m.typ != seal.TypeDelegate {
		return d.drop(dropDuplicate, nil)
	}
	if res := d.parseDelegate(m); res != nil {
		return *res
	}
	ix, res := d.lookupIX(m.ix)
	if res != nil {
		return *res
	}
	if ix == nil || ix.Role != interactions.RoleInbound || ix.PeerAID != m.from {
		// Held for approval, refused on approval, or not a delegation
		// this node took: nothing to re-send and nothing to run.
		return d.drop(dropDuplicate, nil)
	}
	if res := d.authorizeRedelivery(m, ix); res != nil {
		return *res
	}
	// A re-run the daemon's stop cut short is not acknowledged (SI-10).
	if !d.redeliveredDelegate(ctx, m) {
		return d.transient(transientStopping, d.ctx.Err())
	}
	return d.drop(dropDuplicate, nil)
}

// reasonOf maps an error from the seal package to its counter name.
func reasonOf(err error) string {
	if r := seal.ReasonOf(err); r != "" {
		return r
	}
	return "open-failed"
}

// expireTransient turns a temporary failure into a permanent one once the
// message has expired: the hub would otherwise hold it until its TTL and
// every later delivery would fail step 5 anyway.
func (d *Daemon) expireTransient(r rxResult, m *rxMsg, now uint64) rxResult {
	if r.class == rxTransient && now > m.exp {
		return d.drop(dropExpiredWait, nil)
	}
	return r
}

// resolveSenderKEL is step 6. It makes no network request: the KEL comes
// from the message or from the persisted peer record.
//
//   - the inner KEL extends the stored one (or nothing is stored): use the
//     inner KEL and store it after acceptance;
//   - the stored KEL extends the inner one: use the stored KEL, so a
//     signature by a key the stored history retired is judged as retired;
//   - they fork: refuse.
//
// With nothing stored the inner KEL is trusted on first use (§21 item 6).
func (d *Daemon) resolveSenderKEL(from string, inner []identity.SignedEvent) (
	use, update []identity.SignedEvent, row *interactions.PeerIdentity, res *rxResult) {
	aid, err := replayedAID(inner)
	if err != nil {
		r := d.drop(seal.ReasonBadKEL, err)
		return nil, nil, nil, &r
	}
	if aid != from {
		r := d.drop(seal.ReasonFromMismatch, fmt.Errorf("KEL replays to %s, inner from %s", aid, from))
		return nil, nil, nil, &r
	}
	row, err = d.ix.PeerIdentity(from)
	if errors.Is(err, interactions.ErrNotFound) {
		return inner, inner, nil, nil
	}
	if err != nil {
		r := d.transient(transientStore, err)
		return nil, nil, nil, &r
	}
	var stored []identity.SignedEvent
	if len(row.KEL) > 0 {
		if stored, err = identity.UnmarshalKEL(row.KEL); err != nil {
			stored = nil
		}
	}
	use, replace, err := mergeKEL(stored, inner)
	if err != nil {
		r := d.drop(dropKELFork, err)
		return nil, nil, nil, &r
	}
	if replace {
		update = inner
	}
	return use, update, row, nil
}

// authorize is step 9: decide whether the sender may send this message for
// this interaction. It writes nothing. A refusal that owes the sender an
// answer (TaskNotFound) sends it here, rate limited and never retried.
func (d *Daemon) authorize(m *rxMsg, now uint64) *rxResult {
	switch m.typ {
	case seal.TypeDelegate:
		return d.authorizeDelegate(m)
	case seal.TypeMessage:
		return d.authorizeMessage(m, now)
	case seal.TypeStatus, seal.TypeResult:
		return d.authorizeReply(m)
	default:
		r := d.drop(dropUnknownType, fmt.Errorf("type %q", m.typ))
		return &r
	}
}

// lookupIX reads the interaction named by the message. A missing row is
// (nil, nil); a store error is returned as a temporary result.
func (d *Daemon) lookupIX(id string) (*interactions.Interaction, *rxResult) {
	ix, err := d.ix.Get(id)
	if errors.Is(err, interactions.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		r := d.transient(transientStore, err)
		return nil, &r
	}
	return ix, nil
}

// authorizeDelegate checks an anet.delegate/1: the body is a DelegateReq for
// inner.ix whose TaskDoc verifies under the resolved KEL at the message time
// and is signed by inner.from; inner.ix is not an interaction this node
// holds in another role or with another peer; and the inbound policy
// (§5.2) accepts, holds or refuses it. A refusal is answered here, rate
// limited, and nothing is written.
func (d *Daemon) authorizeDelegate(m *rxMsg) *rxResult {
	if res := d.parseDelegate(m); res != nil {
		return res
	}
	ix, res := d.lookupIX(m.ix)
	if res != nil {
		return res
	}
	if ix != nil && (ix.Role != interactions.RoleInbound || ix.PeerAID != m.from) {
		// Another party's interaction id, or ours for a task we started:
		// executing it would let a peer attach work to an interaction it
		// is not part of. No reply, so the collision reveals nothing.
		r := d.drop(dropIXCollision, fmt.Errorf("%s is %s with %s", m.ix, ix.Role, ix.PeerAID))
		return &r
	}
	if ix != nil {
		return d.authorizeRedelivery(m, ix)
	}
	if held, err := d.ix.GetPending(m.ix); err == nil {
		if held.FromAID != m.from {
			r := d.drop(dropPendingCollision, nil)
			return &r
		}
		if held.RequestCID != m.requestCID {
			// The held request is what the operator is shown and approves;
			// another TaskDoc under its id is not added to it.
			r := d.drop(dropIXCollision, fmt.Errorf("%s is held for request %s, this one is %s",
				m.ix, held.RequestCID, m.requestCID))
			return &r
		}
		// Already held: the same delegation again. Nothing to decide.
		r := d.drop(dropDuplicate, nil)
		return &r
	} else if !errors.Is(err, interactions.ErrNotFound) {
		r := d.transient(transientStore, err)
		return &r
	}
	capID, args, _ := capabilityCall(m.td)
	argsLen := 0
	if capID != "" {
		if b, err := json.Marshal(args); err == nil {
			argsLen = len(b)
		}
	}
	dec := d.decideDelegate(m.from, capID, argsLen)
	switch dec.action {
	case actRefuse:
		if res := d.recordRefusal(m); res != nil {
			return res
		}
		d.noteRefused(m.from, m.ix, m.typ, dec.reason, capID)
		d.replyRejected(m, dec.reason, dec.retryAfterMS)
		reason := dropRefusedPrefix + dec.reason
		if dec.reason == reasonNotAccepting {
			reason = dropNotAccepting
		}
		r := d.drop(reason, nil)
		return &r
	case actHold:
		m.hold = true
	default:
		m.trust, m.release = dec.trust, dec.release
	}
	return nil
}

// parseDelegate decodes and verifies an anet.delegate/1 body: a DelegateReq
// for inner.ix whose TaskDoc verifies under the resolved KEL at the message
// time and is signed by inner.from. It fills m.dr, m.td, m.tdBytes and
// m.requestCID.
func (d *Daemon) parseDelegate(m *rxMsg) *rxResult {
	dr, err := delegation.UnmarshalDelegateReq(m.body)
	if err != nil {
		r := d.drop(dropBadBody, err)
		return &r
	}
	if m.ix == "" || dr.InteractionID != m.ix {
		r := d.drop(dropIXMismatch, fmt.Errorf("body names %q, envelope %q", dr.InteractionID, m.ix))
		return &r
	}
	signer, td, tdBytes, err := delegation.VerifyDelegateReqWithKEL(dr, m.kel, m.ts)
	if err != nil {
		r := d.drop(dropBadTaskDoc, err)
		return &r
	}
	if signer != m.from {
		r := d.drop(dropSignerIsOther, fmt.Errorf("TaskDoc signed by %s, envelope from %s", signer, m.from))
		return &r
	}
	requestCID, err := anetcid.Sum(tdBytes)
	if err != nil {
		r := d.drop(dropBadTaskDoc, err)
		return &r
	}
	m.dr, m.td, m.tdBytes, m.requestCID = dr, td, tdBytes, requestCID
	return nil
}

// authorizeRedelivery is step 9 for a delegation naming ix, an inbound
// interaction of the sender's: a redelivery of the delegation ix was
// accepted for, and of nothing else [redteam:F7]. The policy decided on
// that request, so only that request may come this way; another TaskDoc
// under the same ix is a collision, dropped without a reply. Revocation
// reaches existing interactions (§5.1): the deny list, and on a trust=peer
// interaction the allow list, as for a message. The interaction's trust is
// kept.
func (d *Daemon) authorizeRedelivery(m *rxMsg, ix *interactions.Interaction) *rxResult {
	if ix.RequestCID == "" || ix.RequestCID != m.requestCID {
		r := d.drop(dropIXCollision, fmt.Errorf("%s was accepted for request %s, this one is %s",
			m.ix, ix.RequestCID, m.requestCID))
		return &r
	}
	ps := d.readPeers()
	if ps.denied(m.from) {
		r := d.drop(dropDenied, nil)
		return &r
	}
	if ix.Trust == interactions.TrustPeer && !ps.allowed(m.from) {
		r := d.drop(dropNotAllowed, nil)
		return &r
	}
	m.existing, m.trust = ix, ix.Trust
	return nil
}

// authorizeMessage checks an anet.message/1: the interaction exists (or is
// held in the approval queue), its peer is the sender, in either role, and
// the sender is not denied. On a trust=peer interaction the sender must
// still be allowed; on a public_cap interaction only cancel, end_request
// and payment messages are taken. For an interaction this node does not
// hold, the message waits (not acknowledged) for unknownIXWait after its
// send time, in case the delegation is still on its way; after that the
// sender is told TaskNotFound.
//
// The deny list is read after the interaction, not before: a denied peer
// writing to an interaction this node does not hold is treated exactly as
// a stranger is — the same wait, the same TaskNotFound, the same notice
// limiter — so under closed it cannot tell it is denied (X2).
func (d *Daemon) authorizeMessage(m *rxMsg, now uint64) *rxResult {
	cm, err := delegation.UnmarshalChatMsg(m.body)
	if err != nil {
		r := d.drop(dropBadBody, err)
		return &r
	}
	if m.ix == "" {
		r := d.drop(dropEmptyIX, nil)
		return &r
	}
	ix, res := d.lookupIX(m.ix)
	if res != nil {
		return res
	}
	ps := d.readPeers()
	if ix == nil {
		held, err := d.ix.GetPending(m.ix)
		switch {
		case err == nil && held.FromAID == m.from:
			if ps.denied(m.from) {
				r := d.drop(dropDenied, nil)
				return &r
			}
			m.cm, m.pendingRoute = cm, true
			return nil
		case err != nil && !errors.Is(err, interactions.ErrNotFound):
			r := d.transient(transientStore, err)
			return &r
		}
		if int64(now)-int64(m.ts) <= unknownIXWait.Milliseconds() {
			r := d.transient(transientUnknownIX, nil)
			return &r
		}
		d.replyTaskNotFound(m)
		r := d.drop(dropUnknownIX, nil)
		return &r
	}
	if ps.denied(m.from) {
		r := d.drop(dropDenied, nil)
		return &r
	}
	if ix.PeerAID != m.from {
		r := d.drop(dropNotPeer, fmt.Errorf("%s belongs to %s", m.ix, ix.PeerAID))
		return &r
	}
	if ix.Role == interactions.RoleInbound && ix.Trust == interactions.TrustPeer && !ps.allowed(m.from) {
		r := d.drop(dropNotAllowed, nil)
		return &r
	}
	if ix.Trust == interactions.TrustPublicCap {
		switch {
		case cm.Kind == delegation.KindCancel, cm.Kind == delegation.ChatEndRequest:
		case cm.Kind == delegation.ChatText && publicCapPayment(cm.Metadata):
		default:
			r := d.drop(dropPublicCapText, nil)
			return &r
		}
	}
	m.cm, m.existing = cm, ix
	return nil
}

// publicCapPayment reports whether a message on a public_cap interaction is
// one of the payment messages it takes: x402.payment.status of
// payment-submitted or payment-rejected.
func publicCapPayment(meta []byte) bool {
	switch decodeMeta(meta)[x402a2a.KeyStatus] {
	case x402a2a.StatusSubmitted, x402a2a.StatusRejected:
		return true
	}
	return false
}

// authorizeReply checks anet.status/1 and anet.result/1: they answer an
// interaction this node started (role outbound) with the sender as its
// provider.
func (d *Daemon) authorizeReply(m *rxMsg) *rxResult {
	if m.typ == seal.TypeStatus {
		sm, err := delegation.UnmarshalStatusMsg(m.body)
		if err != nil {
			r := d.drop(dropBadBody, err)
			return &r
		}
		if !delegation.ValidStatusState(sm.State) {
			r := d.drop(dropBadStatus, fmt.Errorf("state %q", sm.State))
			return &r
		}
		m.sm = sm
	} else {
		rr, err := delegation.UnmarshalResultResp(m.body)
		if err != nil {
			r := d.drop(dropBadBody, err)
			return &r
		}
		m.rr = rr
	}
	ix, res := d.lookupIX(m.ix)
	if res != nil {
		return res
	}
	if d.readPeers().denied(m.from) && !paidWork(ix) {
		// Work this node paid for is delivered to it, denied or not
		// (0017 Q10): the deny sweep leaves such a task open.
		r := d.drop(dropDenied, nil)
		return &r
	}
	if ix == nil {
		r := d.drop(dropUnknownIX, nil)
		return &r
	}
	if ix.Role != interactions.RoleOutbound {
		r := d.drop(dropWrongRole, fmt.Errorf("%s is %s", m.ix, ix.Role))
		return &r
	}
	if ix.PeerAID != m.from {
		r := d.drop(dropNotPeer, fmt.Errorf("%s belongs to %s", m.ix, ix.PeerAID))
		return &r
	}
	m.existing = ix
	return nil
}

// process is step 10: run the type's handler, under the (from, mid) lock
// receiveEnvelope holds, for a message the replay table did not hold when
// the lock was taken (each handler's transaction claims the replay row, so
// a copy under another message id is still recorded once). After an
// accepted message the sender's KEL and key set are recorded, because
// every message that reaches this point is from the peer of an interaction
// this node holds or accepted.
func (d *Daemon) process(ctx context.Context, m *rxMsg) rxResult {
	var res rxResult
	switch m.typ {
	case seal.TypeDelegate:
		res = d.ingestDelegate(ctx, m)
	case seal.TypeMessage:
		res = d.ingestMessage(ctx, m)
	case seal.TypeResult:
		res = d.ingestResult(ctx, m)
	case seal.TypeStatus:
		res = d.ingestStatus(ctx, m)
	default:
		res = d.drop(dropUnknownType, nil)
	}
	if res.class != rxAccepted {
		return res
	}
	d.recordSenderIdentity(m)
	d.noteLivePeer(m.from)
	if m.typ == seal.TypeDelegate {
		d.kickAutoReply()
	}
	return res
}

// recordSenderIdentity writes the sender's KEL and key set after an
// accepted message, only in the authorized contexts of A2A-DESIGN §3.8:
// the interaction is one this node started (pinned "outbound"), the sender
// is on the allow or trust list (pinned "allow" or "trust"), or the
// interaction was approved by the operator. The requester of a public or
// public_cap interaction is not recorded in peer_identity; its key material
// is kept on the interaction instead. A held delegation's sender is
// recorded in the pending row, and nothing is written for it here.
func (d *Daemon) recordSenderIdentity(m *rxMsg) {
	if m.hold || m.pendingRoute {
		return
	}
	ix, err := d.ix.Get(m.ix)
	if err != nil {
		return
	}
	pin, authorized := "", false
	switch {
	case ix.Role == interactions.RoleOutbound:
		pin, authorized = interactions.PinOutbound, true
	default:
		ps := d.readPeers()
		switch {
		case ps.trusted(m.from):
			pin, authorized = interactions.PinTrust, true
		case ps.allowed(m.from):
			pin, authorized = interactions.PinAllow, true
		case ix.Trust == interactions.TrustApproved:
			authorized = true
		}
	}
	if authorized {
		if m.kelUpdate != nil || m.keysUpdate != nil || pin != "" {
			if err := d.notePeer(m.from, m.kelUpdate, m.keysUpdate, pin, false); err != nil {
				log.Printf("anet: record %s's identity: %v", m.from, err)
			}
		}
		return
	}
	if (ix.Trust == interactions.TrustPublic || ix.Trust == interactions.TrustPublicCap) && !ix.IsTerminal() &&
		(m.kelUpdate != nil || m.keysUpdate != nil) {
		var kel, keys []byte
		if m.kelUpdate != nil {
			kel, _ = identity.MarshalKEL(m.kelUpdate)
		}
		if m.keysUpdate != nil {
			keys = m.keysUpdate.signed
		}
		if err := d.ix.SetPeerKeys(m.ix, kel, keys); err != nil {
			log.Printf("anet: %s: record the requester's keys: %v", m.ix, err)
		}
	}
}

// errReplayDuplicate aborts a transaction whose replay row already exists.
var errReplayDuplicate = errors.New("replay row exists")

// rxPermanentErr aborts a step-10 transaction for a reason that will not
// change on redelivery. commitRx rolls the transaction back and reports it
// as a permanent refusal with the given counter, not as a store error.
type rxPermanentErr struct {
	reason string
	err    error
}

func (e *rxPermanentErr) Error() string { return e.reason + ": " + e.err.Error() }
func (e *rxPermanentErr) Unwrap() error { return e.err }

// rxPermanent returns an error a business write returns from inside
// commitRx to refuse the message permanently.
func rxPermanent(reason string, err error) error { return &rxPermanentErr{reason: reason, err: err} }

// commitRx runs a message's business writes and its replay row in one
// SQLite transaction (§3.6 step 10). Either both are stored or neither is:
// a store error leaves no replay row behind, so the redelivered message is
// processed, and a crash after the commit leaves both, so it is not
// processed twice.
func (d *Daemon) commitRx(m *rxMsg, fn func(*interactions.Tx) error) rxResult {
	err := d.ix.Update(func(tx *interactions.Tx) error {
		claimed, err := tx.ClaimReplay(m.from, m.mid, m.exp)
		if err != nil {
			return err
		}
		if !claimed {
			return errReplayDuplicate
		}
		if fn != nil {
			if err := fn(tx); err != nil {
				return err
			}
		}
		if fault := d.testRxFault(); fault != nil {
			return fault(m.typ)
		}
		return nil
	})
	var perm *rxPermanentErr
	switch {
	case err == nil:
		return accepted()
	case errors.Is(err, errReplayDuplicate):
		return d.drop(dropDuplicate, nil)
	case errors.As(err, &perm):
		return d.drop(perm.reason, perm.err)
	default:
		return d.transient(transientStore, err)
	}
}

// replyTaskNotFound tells an authenticated sender that the interaction its
// message names does not exist here (A2A TaskNotFound), rate limited per
// peer and daemon-wide, with the same limiter as refusal notices. It needs
// the sender's key set from this message or from the stored record;
// without one nothing is sent. The sender may be a stranger: its key set
// goes to the bounded in-memory cache that exists for such replies, never
// to peer_identity.
func (d *Daemon) replyTaskNotFound(m *rxMsg) {
	if d.sendNoticeStatus(m, delegation.StateFailed, "this node has no task "+m.ix,
		map[string]any{"anet.a2aError": "TaskNotFoundError"}) {
		d.count(noticeTaskNotFound)
	}
}

// rotationGrace is the §3.6 step 7 grace for signatures by a key state a
// rotation retired, from config (rotation_grace, a Go duration), default
// one hour.
func (d *Daemon) rotationGrace() time.Duration {
	if s := d.config().RotationGrace; s != "" {
		if g, err := time.ParseDuration(s); err == nil && g >= 0 {
			return g
		}
	}
	return seal.DefaultRotationGrace
}

// --- counters, locks and small bounded structures ---

// drop counts a permanent refusal and returns its result.
func (d *Daemon) drop(reason string, err error) rxResult {
	d.countErr(reason, err)
	return rxResult{class: rxDropped, reason: reason}
}

// transient counts a temporary refusal and returns its result.
func (d *Daemon) transient(reason string, err error) rxResult {
	d.countErr(reason, err)
	return rxResult{class: rxTransient, reason: reason}
}

// rxCounters counts receive outcomes by reason.
type rxCounters struct {
	mu sync.Mutex
	m  map[string]uint64
}

// count increments a reason counter.
func (d *Daemon) count(reason string) { d.countErr(reason, nil) }

// countErr increments a reason counter and logs the 1st, 10th, 100th, …
// occurrence with the detail, so a flood of one kind of bad envelope costs
// a handful of log lines rather than one each.
func (d *Daemon) countErr(reason string, err error) {
	d.rxStats.mu.Lock()
	if d.rxStats.m == nil {
		d.rxStats.m = map[string]uint64{}
	}
	d.rxStats.m[reason]++
	n := d.rxStats.m[reason]
	d.rxStats.mu.Unlock()
	if n == 1 || n == 10 || n == 100 || n == 1000 || n%10000 == 0 {
		if err != nil {
			log.Printf("anet: receive: %s (count %d): %v", reason, n, err)
		} else {
			log.Printf("anet: receive: %s (count %d)", reason, n)
		}
	}
}

// ReceiveStats returns the receive outcome counters: every drop reason,
// every temporary refusal and every notice sent or suppressed since start.
func (d *Daemon) ReceiveStats() map[string]uint64 {
	d.rxStats.mu.Lock()
	defer d.rxStats.mu.Unlock()
	out := make(map[string]uint64, len(d.rxStats.m))
	for k, v := range d.rxStats.m {
		out[k] = v
	}
	return out
}

// logOnce logs a line once per key per process, for conditions that
// repeat per message. The key set is bounded; when full it is cleared.
func (d *Daemon) logOnce(key, format string, args ...any) {
	d.logOnceMu.Lock()
	if d.logOnceSeen == nil || len(d.logOnceSeen) > 1024 {
		d.logOnceSeen = map[string]bool{}
	}
	seen := d.logOnceSeen[key]
	d.logOnceSeen[key] = true
	d.logOnceMu.Unlock()
	if !seen {
		log.Printf(format, args...)
	}
}

// replayKey is the in-process key of a message: sender and message id.
func replayKey(from string, mid []byte) string { return from + "\x00" + string(mid) }

// keyedLocks is a mutex per key, created on demand and dropped when unused.
// It serializes the handling of one message when two copies arrive at
// once, one from the hub and one over p2p.
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*keyedLock
}

type keyedLock struct {
	mu sync.Mutex
	n  int
}

func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyedLock{}
	}
	l := k.m[key]
	if l == nil {
		l = &keyedLock{}
		k.m[key] = l
	}
	l.n++
	k.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		l.n--
		if l.n == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// boundedSet remembers up to limit keys, forgetting the oldest first. The
// refused-envelope list is one: a replay of a refused envelope is dropped
// at step 5, before its KEL and signature are checked again. It is a cache.
// For a delegation the refused table (interactions/refused.go) is what
// keeps a replay that outlived its entry refused [redteam:F5]; for other
// types a replay that outlived it is judged again, and a message's step 9
// writes nothing and sends at most a rate-limited TaskNotFound.
type boundedSet struct {
	mu    sync.Mutex
	limit int
	order []string
	m     map[string]bool
}

const refusedLimit = 4096

func (s *boundedSet) add(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]bool{}
	}
	if s.limit == 0 {
		s.limit = refusedLimit
	}
	if s.m[k] {
		return
	}
	s.m[k] = true
	s.order = append(s.order, k)
	if len(s.order) > s.limit {
		delete(s.m, s.order[0])
		s.order = s.order[1:]
	}
}

func (s *boundedSet) has(k string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[k]
}

// tokenBucket is a rate limiter over the daemon clock (unix ms).
type tokenBucket struct {
	perMS  float64 // tokens added per millisecond
	burst  float64
	tokens float64
	last   uint64
	primed bool
}

func newBucket(perSecond, burst float64) tokenBucket {
	return tokenBucket{perMS: perSecond / 1000, burst: burst}
}

func (b *tokenBucket) allow(now uint64) bool {
	if !b.primed {
		b.tokens, b.last, b.primed = b.burst, now, true
	}
	if now > b.last {
		b.tokens += float64(now-b.last) * b.perMS
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// p2pLimiter is step 0 of §3.6 for envelopes that arrive through a
// transport module: one daemon-wide bucket, checked before any decryption.
// The per-connection half of step 0 is enforced by the peer process, the
// only party that sees connections (tools/anetpeer).
//
// The rate is of the same order as the hub's per-sender budget (20/s, burst
// 200). A sender over it is not refused outright: its delivery is not
// acknowledged and it falls back to the hub, which applies its own
// per-sender limit.
type p2pLimiter struct {
	mu sync.Mutex
	b  tokenBucket
}

const (
	p2pRatePerSecond = 20
	p2pBurst         = 100
)

var errP2PRateLimited = errors.New("anet: inbound transport rate limit reached; use the hub")

func (l *p2pLimiter) allow(now uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.b.primed && l.b.burst == 0 {
		l.b = newBucket(p2pRatePerSecond, p2pBurst)
	}
	return l.b.allow(now)
}

// noticeLimiter bounds refusal notices: per peer and daemon-wide, with the
// defaults of the inbound reject_notice block (A2A-DESIGN §5.1:
// per_peer_per_hour 6, global_per_min 60). A notice over either limit is
// dropped silently; notices never queue and never delay other work.
type noticeLimiter struct {
	mu     sync.Mutex
	global tokenBucket
	peers  map[string]*tokenBucket
	order  []string
	// perPeerPerHour and globalPerMin are the configured rates
	// (inbound.reject_notice); zero means the defaults.
	perPeerPerHour, globalPerMin int
}

// configure sets the rates and resets the buckets.
func (l *noticeLimiter) configure(perPeerPerHour, globalPerMin int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.perPeerPerHour, l.globalPerMin = perPeerPerHour, globalPerMin
	l.peers, l.order = nil, nil
}

const (
	noticePerPeerPerHour = 6
	noticeGlobalPerMin   = 60
	noticePeerLimit      = 4096
)

func (l *noticeLimiter) allow(peer string, now uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	perPeer, global := l.perPeerPerHour, l.globalPerMin
	if perPeer <= 0 {
		perPeer = noticePerPeerPerHour
	}
	if global <= 0 {
		global = noticeGlobalPerMin
	}
	if l.peers == nil {
		l.peers = map[string]*tokenBucket{}
		l.global = newBucket(float64(global)/60.0, float64(global))
	}
	b := l.peers[peer]
	if b == nil {
		nb := newBucket(float64(perPeer)/3600.0, float64(perPeer))
		b = &nb
		l.peers[peer] = b
		l.order = append(l.order, peer)
		if len(l.order) > noticePeerLimit {
			delete(l.peers, l.order[0])
			l.order = l.order[1:]
		}
	}
	if !b.allow(now) {
		return false
	}
	return l.global.allow(now)
}

// resendLimiter bounds the answers re-sent for redelivered delegations
// (§3.6 step 10, C33) [redteam:F29], the way noticeLimiter bounds refusal
// notices: per (peer, interaction) and daemon-wide. A redelivery means the
// requester may not have the answer, and one re-send is what that needs;
// every further redelivery of the same answered delegation — a hub or a
// requester replaying it to spend this node's hub send budget — is
// acknowledged and not answered again until the bucket refills. The keys
// are bounded; the oldest is forgotten first.
type resendLimiter struct {
	mu     sync.Mutex
	global tokenBucket
	keys   map[string]*tokenBucket
	order  []string
}

const (
	resendBurst        = 2       // re-sends of one answer at once
	resendEveryMS      = 300_000 // then one per five minutes
	resendGlobalPerMin = 60      // daemon-wide, as the notice default
	resendKeyLimit     = 4096
)

func (l *resendLimiter) allow(peer, ix string, now uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.keys == nil {
		l.keys = map[string]*tokenBucket{}
		l.global = newBucket(resendGlobalPerMin/60.0, resendGlobalPerMin)
	}
	k := peer + "\x00" + ix
	b := l.keys[k]
	if b == nil {
		nb := newBucket(1000.0/resendEveryMS, resendBurst)
		b = &nb
		l.keys[k] = b
		l.order = append(l.order, k)
		if len(l.order) > resendKeyLimit {
			delete(l.keys, l.order[0])
			l.order = l.order[1:]
		}
	}
	if !b.allow(now) {
		return false
	}
	return l.global.allow(now)
}

// purgeReplay deletes replay rows and refused rows of messages that have
// expired, with the clock skew tolerance as margin.
func (d *Daemon) purgeReplay() {
	cutoff := d.nowMS()
	if cutoff > seal.ClockSkewMS {
		cutoff -= seal.ClockSkewMS
	}
	if _, err := d.ix.PurgeReplay(cutoff); err != nil {
		log.Printf("anet: purge replay rows: %v", err)
	}
	if _, err := d.ix.PurgeRefused(cutoff); err != nil {
		log.Printf("anet: purge refused rows: %v", err)
	}
}

// receiveMaintenance runs the periodic housekeeping of the receive path
// every hour until ctx ends: expired replay rows, key ring rotation and
// publication, and the public_cap retention sweep. New runs the first purge
// itself; the key ring is maintained at start by setupKeyRing and published
// by the registration refresh; the first retention sweep runs here, off the
// start path.
func (d *Daemon) receiveMaintenance(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	m := time.NewTicker(time.Minute)
	defer m.Stop()
	d.pruneRetention() // at start too: a node restarted more often than hourly still sweeps
	for {
		select {
		case <-ctx.Done():
			d.flushInboundSummary(true)
			return
		case <-t.C:
			d.purgeReplay()
			d.maintainKeyRing()
			d.pruneRetention() // daily in effect: the cutoff moves once a day (evidence_mode.go)
		case <-m.C:
			// The inbound policy's periodic work (inbound.go, pending.go):
			// the refusal and acceptance summaries, expiry of held
			// delegations, and interactions of peers added to the deny list
			// outside the CLI.
			d.flushInboundSummary(false)
			d.expirePending()
			d.revocationSweep()
		}
	}
}
