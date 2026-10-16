package daemon

// receive.go is the receive pipeline v3 (engine §4). Every envelope, from the
// hub mailbox (relay.go, pollOnce) or from a transport module (transport.go,
// inbound.Receive), passes through receiveEnvelopeVia, and nothing reaches
// the interaction store any other way.
//
//	step 0    (direct) rate limit by the capability it came under, before Open  transport.go
//	step 0a   (direct) wait for start-up to finish                             transport.go
//	step 1    the outer; 1a (hub) the capability it came under, checked again  seal.ParseOuter
//	step 2-4  key lookup, layer 1, the Wrap and layer 2 (3a), the inner        seal.Open
//	step 4a   the keys its type must and must not carry                        kindKeysOK
//	step 5    time window; 5a the refused list                                 seal.CheckTime
//	step 6    sender KEL: inner vs stored, extension and R-9, no network       resolveSenderKEL
//	step 6½   the witness notice of the KEL it carries                         seal.CheckNotice
//	step 7    signature under the merged KEL at its arrival                    seal.VerifyInnerSig
//	step 7½   the capability's grantee                                         grantMisuse
//	step 8    attached key set and grant: advisory                             seal.VerifyInnerKeys
//	step 8½   (from, mid) lock, refused list and table, replay table           duplicate
//	step 9    authorization by type (no writes)                                authorize*
//	step 10   business writes and replay row in one transaction                ingest* (delegation.go)
//	step 11   the delivery token and the ack                                   answerEnvelope, rxTokenFor
//
// Result classes (engine §4.3). A: step 10 committed it, or it is a copy of
// one that did, or it was sealed to an interaction this node closed; it is
// acknowledged and answered tok(1). P: the same bytes would fail the same
// way again; acknowledged, and answered tok(2) with a note once the first
// layer opened. T: a store error, a rate limit, start-up, a witness this node
// does not know yet, and over a direct transport anything that did not open;
// not acknowledged, no token, so the hub delivers it again and a direct
// sender goes to the hub. A temporary failure becomes permanent once the
// message has expired. Before the first layer opens nothing knows the
// envelope's ack_key, and nothing is answered with a token (ENG-V4); a
// one-shot envelope (outer key 8) is never answered with one (ENG-V6).
//
// Every value a handler acts on comes from the authenticated inner message:
// the sender is inner.from as proven by the signature in step 7, and the
// interaction is inner.ix. Nothing is taken from transport metadata. An
// immediate refusal sends nothing: it is told in the token's note (ENG-V15).

import (
	"context"
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
	// step is the step that decided the envelope (rxVerdict.Step).
	step rxStep
	// note is the note a refusal is given where its step chose one: the
	// inbound policy's (noteReasonFor), or the reason a refusal of the same
	// envelope was recorded with (rxVerdict.Note).
	note seal.Note
	// dack is the delivery token the envelope is answered with, or nil
	// for none (DLV-R-6b). Only the engine sets it, once rxTokenFor has
	// decided the envelope earns one and MakeDAck has made it (engine
	// ENG-V11, V12). The hub path puts it in the envelope's /relay/ack
	// item (ackItem, relay.go), the direct path in its ack frame.
	dack *rxDAck
}

// rxDAck is a delivery token as the receive path hands it on: the handle
// the sender asks its hub about, and the dack itself (DLV §2.2, core §2.7).
type rxDAck struct {
	handle [seal.HandleLen]byte
	dack   [seal.DAckLen]byte
}

// ack reports whether the transport should acknowledge the delivery.
func (r rxResult) ack() bool { return r.class != rxTransient }

func accepted() rxResult { return rxResult{class: rxAccepted, step: rxStep10} }

// at is r decided by step s.
func (r rxResult) at(s rxStep) rxResult {
	r.step = s
	return r
}

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
	dropUnknownIX     = "unknown-ix"             // message, status or result for an ix this node does not hold
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

	dropRefusedPrefix        = "refused-"               // + the code: a delegation the inbound policy or its admission refused
	dropDenied               = "denied"                 // message, status or result from a peer on the deny list
	dropNotAllowed           = "not-allowed"            // message on a trust=peer interaction from a peer no longer allowed
	dropPublicCapText        = "public-cap-text"        // text on a public capability call; only cancel, end_request and payment are taken
	dropAfterTerminal        = "input-after-terminal"   // new input for an interaction in a terminal state
	dropEndRequestIgnored    = "end-request-ignored"    // end_request from the provider
	dropCancelFromProvider   = "cancel-from-provider"   // cancel sent by the provider; the provider sends status canceled
	dropPendingCollision     = "pending-collision"      // delegation for an ix held in the approval queue for another peer
	dropPendingFollowupsFull = "pending-followups-full" // a follow-up to a held delegation that has as many as it may hold

	keysInvalid  = "keys-invalid"  // attached key set did not verify (advisory; message still processed)
	keysFork     = "keys-fork"     // attached or fetched key set differs from the stored one at the same seq
	keysRollback = "keys-rollback" // attached or fetched key set is older than the stored one

	noticeSent       = "notice-sent"         // a notice after the fact queued (queueNotice)
	noticeSuppressed = "notice-rate-limited" // a notice after the fact not queued: rate limit
	resendSuppressed = "resend-rate-limited" // a redelivered delegation's answer not hurried: rate limit

	// redeliveryNotAuthorized counts a redelivered delegation whose side
	// effects were not taken: the sender may no longer send on its
	// interaction, or the copy is not the request it was accepted for. It
	// is answered tok(1) all the same (engine ENG-V18).
	redeliveryNotAuthorized = "redelivery-not-authorized"
	// accessInvalidAtRx counts a kind-1 credential on a mailbox envelope that
	// does not check here (step 1a): the envelope goes on as one under no
	// capability, and the capability is not revoked.
	accessInvalidAtRx = "access-invalid-at-rx"
	// The one-shot envelopes of step 9 (engine §4.5).
	previewRejected  = "preview-rejected"  // a preview frame not taken
	cardGetNoGrant   = "card-get-no-grant" // a card request without the one-shot grant to answer under: not answered
	cardGetAnswered  = "card-get-answered" // a card request answered
	cardRejected     = "card-rejected"     // a card answer not taken
	cancelTombEvict  = "cancel-tombstone-evicted"
	cancelTombstoned = "cancel-tombstone" // a cancel for a task this node does not hold, kept as a tombstone

	transientStore       = "t-store"          // interactions store error
	transientP2PRate     = "t-p2p-rate-limit" // step 0
	transientReplayCheck = "t-replay-read"    // replay table read failed
	// transientDirectUnopened: an envelope a transport module delivered
	// that this node cannot open (steps 1-3); the sender is sent to the hub.
	transientDirectUnopened = "t-direct-unopened"
	// transientNotReady: a direct delivery that arrived during start-up and
	// could not wait for it to finish (awaitReady).
	transientNotReady = "t-not-ready"
)

// rxMsg is an envelope that passed steps 1 to 8.
type rxMsg struct {
	from, typ, ix string
	mid           []byte
	ts, exp       uint64
	keySeq        uint64 // the sender's key state seq in the inner message
	body          []byte
	// arrival is when the envelope counts as arrived (KEL D-20): the hub's
	// accepted_at bounded by the sender's time and now, or now for a direct
	// delivery. A held delegation keeps it (KEL D-25).
	arrival uint64
	// kel is the sender KEL resolved in step 6; the signature and every
	// nested signed object are checked against it.
	kel []identity.SignedEvent
	// kelUpdate is the KEL to store after acceptance: the inner KEL when
	// it extends the stored one or nothing is stored, else nil.
	kelUpdate []identity.SignedEvent
	// known: the sender has a persistent peer_identity row (§3.8: written
	// only for peers this node dealt with in an authorized context).
	known bool
	// direct: it arrived over a transport module, not from the hub mailbox
	// (rxPath.direct).
	direct bool
	// oneShot: the outer carries key 8 (a preview frame or a card message).
	oneShot bool
	// keysUpdate is the attached key set when it passed step 8 and is newer
	// than the stored one; noticeKeys is any verified set usable to reply.
	keysUpdate, noticeKeys *peerKeySet
	// rk is the interaction key the sender announces on the task (inner key
	// 15) as the store records it, coredet(RK), or nil when it carries none
	// (FS-R4): compared with the task's peer_rk at step 9, recorded at step
	// 10 when the task has none yet.
	rk []byte
	// grant is the capability the message gives this node (inner key 14),
	// checked at step 8 against the attached key set and kept by the
	// step-10 transaction that commits the message, and by no other
	// (SSR-W8, grantToKeep); nil for none.
	grant *interactions.HeldCap

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
	// tombstone marks a cancel for a task this node does not hold: step 10
	// keeps it as a tombstone with its replay row (engine §4.5).
	tombstone bool
	// redeliveryDenied marks a delegation for an interaction of the
	// sender's that it may no longer send on: step 10 records its replay
	// row and does nothing else (engine ENG-V18).
	redeliveryDenied bool

	// ackKey is the envelope's ack_key, for the token committed makes; ackNow
	// is rxPath.ack: set on a direct delivery, called once by committed.
	ackKey seal.AckKey
	ackNow func(dack []byte)
}

// committed reports that step 10 has committed this message: a direct
// delivery is acknowledged now, with its tok(1), not after the side effects
// that follow (an internal note item, engine ENG-V2). Until this point every failure is
// still one the sender must hear about; after it, redelivering the envelope
// could only be a duplicate.
func (m *rxMsg) committed() {
	if m.ackNow == nil {
		return
	}
	var b []byte
	if tok := rxTokenFor(rxVerdict{Class: rxAccepted, Step: rxStep10, Layer1: true, OneShot: m.oneShot}); tok.Make {
		if dack, err := m.ackKey.MakeDAck(tok.Outcome, tok.Note); err == nil {
			b = dack[:]
		}
	}
	m.ackNow(b)
	m.ackNow = nil
}

// rxPath is how an envelope reached this node.
type rxPath struct {
	// direct: a transport module handed it over (module.Inbound, p2p), not
	// the hub mailbox.
	direct bool
	// acceptedAt is when the hub accepted a mailbox envelope (unix ms, the
	// row's accepted_at): its arrival (KEL D-20).
	acceptedAt uint64
	// issued is the capability this node issued that a direct delivery came
	// under, as step 0 checked it (nil: none).
	issued *interactions.IssuedCap
	// ack, when set, is called once step 10 has committed the envelope's
	// business writes and replay row, before the side effects that follow
	// the commit (running a capability, sending an answer), with the tok(1)
	// it is answered with. A direct delivery is acknowledged there (0017
	// Q29, inbound.Receive).
	ack func(dack []byte)
}

// receiveEnvelope runs the pipeline on one envelope from the hub mailbox
// that the hub accepted just now.
func (d *Daemon) receiveEnvelope(ctx context.Context, env []byte) rxResult {
	return d.receiveEnvelopeVia(ctx, env, rxPath{acceptedAt: d.nowMS()})
}

// receiveEnvelopeVia runs the pipeline on one envelope that arrived by path.
func (d *Daemon) receiveEnvelopeVia(ctx context.Context, env []byte, path rxPath) (result rxResult) {
	now := d.nowMS()

	// Step 1: the outer. Open parses it again; it is read first for what the
	// steps after it need before the inner opens: whether the envelope is
	// one-shot (outer key 8), and which capability of this node's it came
	// under.
	outer, err := seal.ParseOuter(env)
	if err != nil {
		return d.unopened(path, err)
	}
	oneShot := outer.Exp != nil
	// Step 1a (the hub path) or step 0 (direct): the capability this node
	// issued that the envelope came under and proved, nil for none. On the
	// hub path a credential that does not check here only means none; it is
	// not revoked (ENG-V7).
	issued := path.issued
	if !path.direct {
		var kind1 bool
		issued, _, kind1 = d.capUnder(outer, env, now)
		if kind1 && issued == nil {
			d.count(accessInvalidAtRx)
		}
	}

	// Steps 2 to 4. The private half of an interaction key the second layer
	// was opened with is wiped as soon as Open is done with it (FS-X8).
	lookup := d.ixKeys()
	op, err := seal.Open(env, d.AID(), d.enc, lookup)
	wipeLookup(lookup)
	var ack seal.AckKey
	layer1 := false
	if err == nil {
		ack, layer1 = op.AckKey, true
	} else {
		ack, layer1 = seal.AckKeyOf(err)
	}
	var m *rxMsg
	if layer1 {
		// Step 11: what the envelope is answered with (ENG-V11, V12).
		defer func() {
			result = d.answerEnvelope(result, ack, oneShot)
			clear(ack[:])
			if m != nil {
				clear(m.ackKey[:])
			}
		}()
	}
	if err != nil {
		reason := seal.ReasonOf(err)
		switch {
		case !layer1:
			return d.unopened(path, err)
		case reason == "":
			// The interaction key store could not be read: not a property of
			// the bytes, so neither a refusal nor a closed interaction
			// (ENG-V8).
			return d.transient(transientStore, err).at(rxStep3a)
		}
		step := rxStep4
		switch reason {
		case seal.ReasonBadWrap, seal.ReasonDecrypt, seal.ReasonSealedToClosedInteraction:
			step = rxStep3a
		}
		d.misused(issued, step, reason, oneShot, "")
		if reason == seal.ReasonSealedToClosedInteraction {
			// Sealed to an interaction this node has closed: what it carried
			// has nothing left to land on, and the sender is told it was
			// taken (tok(1); FS-R2, DLV T13).
			d.count(reason)
			return rxResult{class: rxAccepted, reason: reason, step: step}
		}
		return d.drop(reason, err).at(step)
	}
	in := &op.Inner

	// Step 4a: the keys its type must and must not carry.
	cm, preview := previewOf(in)
	if !kindKeysOK(op, preview) {
		d.misused(issued, rxStep4a, dropBadKindKeys, oneShot, in.Type)
		return d.drop(dropBadKindKeys, fmt.Errorf("type %s, outer key 8 %v, layers %d", in.Type, oneShot, op.Layers)).at(rxStep4a)
	}

	// Step 5.
	if err := seal.CheckTime(in, now); err != nil {
		return d.drop(reasonOf(err), err).at(rxStep5)
	}
	// Step 5a: a copy of an envelope refused before is answered as the
	// first was, before any work is spent on it again.
	if reason, refused := d.refused.get(replayKey(in.From, in.MID)); refused {
		return d.refusedReplay(rxStep5a, reason)
	}

	arrival := now
	if !path.direct {
		arrival = hubArrival(path.acceptedAt, in.TS, now)
	}

	// Step 6.
	use, update, row, res := d.resolveSenderKEL(in, op.KEL, preview, now)
	if res != nil {
		d.misused(issued, rxStep6, res.reason, oneShot, in.Type)
		return res.at(rxStep6)
	}

	// Step 6½: the witness notice of the KEL the envelope carries, whether
	// or not this node holds a record of the sender (KEL D-21). A preview
	// frame carries none (C-52).
	if !preview {
		if res := d.checkNotice(ctx, in, op.KEL, arrival); res != nil {
			d.misused(issued, rxStep6b, res.reason, oneShot, in.Type)
			return res.at(rxStep6b)
		}
	}

	// Step 7, at the arrival (KEL D-22).
	if err := seal.VerifyInnerSig(in, op.Preimage, use, arrival); err != nil {
		reason := reasonOf(err)
		d.misused(issued, rxStep7, reason, oneShot, in.Type)
		return d.drop(reason, err).at(rxStep7)
	}

	// Step 7½: a capability this node issued, used by someone other than
	// its grantee or, one-shot, for another interaction, is revoked; the
	// message goes on as one under no capability (SS §4.3.10).
	if issued != nil && grantMisuse(issued, in.From, in.IX) {
		if _, err := d.revokeMisused(issued.CapID); err != nil {
			log.Printf("anet: revoke a misused capability: %v", err)
		}
		issued = nil
	}

	rk, err := rkBytes(in.RK)
	if err != nil {
		return d.drop(seal.ReasonBadRK, err).at(rxStep4)
	}
	m = &rxMsg{from: in.From, typ: in.Type, ix: in.IX, mid: in.MID, ts: in.TS, exp: in.Exp,
		keySeq: in.KeyStateSeq, body: in.Body, arrival: arrival, kel: use, kelUpdate: update,
		ackNow: path.ack, ackKey: ack, known: row != nil, direct: path.direct, oneShot: oneShot, cm: cm, rk: rk}

	// Step 8: advisory. A key set that fails here does not affect the
	// message; the authenticity of the message was decided in step 7. A
	// preview frame carries neither a key set nor a grant (CORE-K5).
	var verifiedKeys *seal.EncKeySet
	if !preview {
		if signed, set, kerr := seal.VerifyInnerKeys(in, use, now); kerr != nil {
			d.count(keysInvalid)
		} else {
			verifiedKeys = set
			ks := &peerKeySet{signed: in.Keys, set: set}
			switch seal.DecideHighWater(seenAgainst(row, use, now, set.Seq), set.Seq, signed.Set) {
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
	}
	if m.noticeKeys == nil {
		if ks, ok := d.storedKeySet(row, now); ok {
			m.noticeKeys = ks
		}
	}
	// Step 8: the grant, checked against the key set this message carries.
	m.grant = d.grantToKeep(in, verifiedKeys, oneShot, now)

	// Steps 8½ to 10 for one (from, mid) at a time, and a message handled
	// or refused before is not judged again [redteam:F26]. Two copies of
	// one envelope (the hub's and a p2p peer's, or a redelivery racing the
	// first) meet here: the second waits for the first to be decided and
	// committed, then finds it in the refused list or the replay table. It
	// takes no admission slot and runs nothing the first did not; a
	// delegation's duplicate goes to the redelivery path.
	key := replayKey(m.from, m.mid)
	unlock := d.rxLocks.lock(key)
	defer func() {
		unlock()
		if f := d.testRxUnlockHook(); f != nil {
			f()
		}
	}()
	if m.typ == seal.TypeDelegate && m.ix != "" {
		// The same delegation sealed twice (two message ids) is one
		// decision too: the second copy finds the interaction the first
		// created, and is a redelivery of it.
		unlockIX := d.rxIXLocks.lock(m.from + "\x00" + m.ix)
		defer unlockIX()
	}
	// A refusal is made to stick before the locks are released, so the copy
	// waiting on them finds it [redteam:F5][redteam:F26].
	defer func() { result = d.stickRefusal(result, m) }()
	if reason, refused := d.refused.get(key); refused {
		return d.refusedReplay(rxStep8b, reason)
	}
	if oneShot {
		// A one-shot envelope is judged by its type alone: no refused table,
		// no replay row (engine §4.2 step 8½, §4.7).
		return d.receiveOneShot(ctx, m, preview, verifiedKeys, in, now)
	}
	// A delegation refused before is refused again, with the reason it was
	// first told, after a restart or after the in-memory list forgot it:
	// the policy may have changed since [redteam:F5].
	var floor uint64
	if m.typ == seal.TypeDelegate {
		exact, reason, f, err := d.ix.Refused(m.from, m.mid)
		if err != nil {
			return d.expireTransient(d.transient(transientReplayCheck, err).at(rxStep8b), m, now)
		}
		if exact {
			d.refused.add(key, reason)
			return d.refusedReplay(rxStep8b, reason)
		}
		floor = f
	}
	seen, err := d.ix.ReplaySeen(m.from, m.mid)
	if err != nil {
		return d.expireTransient(d.transient(transientReplayCheck, err).at(rxStep8b), m, now)
	}
	if seen {
		return d.expireTransient(d.duplicate(ctx, m), m, d.nowMS())
	}
	if m.ts <= floor {
		// Older than refusals of this sender's that the table let go of
		// (refused.go): it may be one of them.
		return d.drop(dropRefusedFloor, nil).at(rxStep8b)
	}

	// Step 9.
	if ares := d.authorize(m, now); ares != nil {
		return d.expireTransient(ares.at(rxStep9), m, now)
	}

	// Step 10.
	out := d.process(ctx, m)
	if out.class == rxAccepted {
		// The round a requester's message opened, and whether it asked
		// for previews: read from the message as it arrived, before
		// anet.* is stripped for storage (CUX-P0).
		d.notePreviewRound(m)
	}
	if m.release != nil {
		// Admitted in step 9 but not executed (a store error, a duplicate):
		// the admission slot is returned.
		m.release()
		m.release = nil
	}
	if out.step == 0 {
		out.step = rxStep10
	}
	return d.expireTransient(out, m, d.nowMS())
}

// unopened is the answer to an envelope whose first layer did not open
// (steps 1 to 3; engine ENG-V4). From the hub mailbox it is wrong for good:
// the mailbox is this node's, and the hub checked the outer to. Over a direct
// transport it may only be someone else's [redteam:F22]: the address the
// sender dialled can lead to another node (a stale or reused address,
// another identity behind the same host and port). Acknowledged there, the
// sender would count it delivered and never try the hub, where the real
// recipient's mailbox is, and the message would be lost; not acknowledged,
// it goes through the hub, which refuses it there if it is malformed after
// all. Neither is answered with a token: none can be made before layer 1.
func (d *Daemon) unopened(path rxPath, err error) rxResult {
	if path.direct {
		return d.transient(transientDirectUnopened, fmt.Errorf("%s: %w", reasonOf(err), err)).at(rxStep1)
	}
	if seal.ReasonOf(err) == seal.ReasonUnknownKey {
		d.unknownKeyDrops.note(d.nowMS())
	}
	return d.drop(reasonOf(err), err).at(rxStep1)
}

// hourCounts counts events in the last 24 hours, by the hour they fell in
// (unix ms); in memory, bounded, and as old as the process at most.
type hourCounts struct {
	mu    sync.Mutex
	hours [24]struct{ hour, n uint64 }
}

// countHourMS is the width of one of hourCounts' buckets.
const countHourMS = 3_600_000

// note counts one event at now.
func (c *hourCounts) note(now uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := now / countHourMS
	b := &c.hours[h%24]
	if b.hour != h {
		b.hour, b.n = h, 0
	}
	b.n++
}

// last24h is the events in the 24 hours up to now.
func (c *hourCounts) last24h(now uint64) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := now / countHourMS
	var n uint64
	for _, b := range c.hours {
		if b.hour+24 > h && b.hour <= h {
			n += b.n
		}
	}
	return n
}

// unknownKeyDrops24h is how many envelopes from the hub mailbox were dropped
// in the last 24 hours as sealed to an epoch key this node does not hold
// (engine §4.2 step 2): /status privacy's unknown_key_drops_24h (FS §2.10).
// A direct delivery that fails the same way is temporary and not counted.
func (d *Daemon) unknownKeyDrops24h() uint64 { return d.unknownKeyDrops.last24h(d.nowMS()) }

// refusedReplay answers a copy of an envelope refused before with the reason
// the first was refused with, and no wait (engine ENG-V13).
func (d *Daemon) refusedReplay(step rxStep, reason string) rxResult {
	r := d.drop(dropRefusedReplay, nil).at(step)
	r.note = seal.Note{Reason: reason}
	return r
}

// stickRefusal makes a refusal of an authenticated message by step 9 or 10
// stick before it is told (engine ENG-V12, V13), while the envelope's locks
// are still held: a delegation's in the refused table, every one in the
// in-memory list, with the note it is told. One that cannot be written is
// not told: the envelope is temporary and judged again when it comes back
// [redteam:F5]. A result refused for its receipt or its reply first fails
// the task (CUX-R7).
func (d *Daemon) stickRefusal(res rxResult, m *rxMsg) rxResult {
	if m.oneShot || res.class != rxDropped || res.step < rxStep9 || res.reason == dropDuplicate {
		return res
	}
	tok := rxTokenFor(rxVerdict{Class: res.class, Step: res.step, Reason: res.reason, Layer1: true, Note: res.note})
	if res.reason == dropResultRefused || res.reason == dropReplyMismatch {
		if err := d.resultRefused(m, res.reason); err != nil {
			return d.expireTransient(d.transient(transientStore, fmt.Errorf("fail %s for its refused result: %w", m.ix, err)).at(res.step), m, d.nowMS())
		}
	}
	if m.typ == seal.TypeDelegate {
		if err := d.ix.RecordRefused(m.from, m.mid, m.ts, m.exp, tok.Note.Reason); err != nil {
			return d.expireTransient(d.transient(transientStore, fmt.Errorf("record the refusal of %s: %w", m.ix, err)).at(res.step), m, d.nowMS())
		}
	}
	d.refused.add(replayKey(m.from, m.mid), tok.Note.Reason)
	return res
}

// answerEnvelope is step 11: what an envelope whose first layer opened is
// answered with (engine ENG-V11, V12): rxTokenFor decides the token, and the
// envelope's ack_key makes it. A refusal it answers was made to stick first
// (stickRefusal).
func (d *Daemon) answerEnvelope(res rxResult, ack seal.AckKey, oneShot bool) rxResult {
	tok := rxTokenFor(rxVerdict{Class: res.class, Step: res.step, Reason: res.reason, OneShot: oneShot, Layer1: true, Note: res.note})
	if !tok.Make || res.dack != nil {
		return res
	}
	dack, err := ack.MakeDAck(tok.Outcome, tok.Note)
	if err != nil {
		log.Printf("anet: receive: make the delivery token: %v", err)
		return res
	}
	res.dack = &rxDAck{handle: ack.Handle(), dack: dack}
	return res
}

// duplicate handles an envelope whose (from, mid) the replay table holds: it
// was taken before, it is answered tok(1) whatever follows (engine ENG-V13,
// V17), and nothing is decided about it again. A delegation may mean the
// requester never got the answer: when the interaction it opened is still
// the sender's, the envelope carries the request it was accepted for, and
// the sender may still send on it, redeliveredDelegate hurries the answer
// or finishes work a crash interrupted (ENG-V18). Otherwise nothing follows,
// counted redelivery-not-authorized.
func (d *Daemon) duplicate(ctx context.Context, m *rxMsg) rxResult {
	dup := d.drop(dropDuplicate, nil).at(rxStep8b)
	if m.typ != seal.TypeDelegate {
		return dup
	}
	if res := d.parseDelegate(m); res != nil {
		d.count(redeliveryNotAuthorized)
		return dup
	}
	ix, res := d.lookupIX(m.ix)
	if res != nil {
		return res.at(rxStep8b)
	}
	if ix == nil || ix.Role != interactions.RoleInbound || ix.PeerAID != m.from {
		// Held for approval, refused on approval, or not a delegation
		// this node took: nothing to hurry and nothing to run.
		return dup
	}
	if interactions.RKConflict(ix.PeerRK, m.rk) {
		// A copy of what was taken, under another rk than the task holds:
		// answered as the copy it is, and nothing follows it (FS-R5,
		// ENG-V18).
		d.count(dropRKMismatch)
		return dup
	}
	if !d.mayRedeliver(m, ix) {
		d.count(redeliveryNotAuthorized)
		return dup
	}
	// Over a direct transport the duplicate is acknowledged first, with its
	// token, and the side effects follow (an internal note item): a crash in between
	// leaves the call to startup recovery, as for a first delivery.
	if m.ackNow != nil {
		m.committed()
		d.redeliveredDelegate(ctx, m)
		return dup
	}
	// A re-run the daemon's stop cut short is not acknowledged (SI-10).
	if !d.redeliveredDelegate(ctx, m) {
		return d.transient(transientStopping, d.ctx.Err()).at(rxStep10)
	}
	return dup
}

// reasonOf maps an error from the seal package to its counter name.
func reasonOf(err error) string {
	if r := seal.ReasonOf(err); r != "" {
		return r
	}
	return "open-failed"
}

// expireTransient turns a temporary failure into a permanent one once the
// message has expired (engine ENG-V5): the hub would otherwise hold it
// until its TTL and every later delivery would fail step 5 anyway.
func (d *Daemon) expireTransient(r rxResult, m *rxMsg, now uint64) rxResult {
	if r.class == rxTransient && now > m.exp {
		return d.drop(dropExpiredWait, nil).at(rxStep5)
	}
	return r
}

// capUnder is the capability this node issued that an envelope came under,
// when its credential (outer key 7, kind 1) checks: issued by this node
// under its current mailbox key, not revoked, not expired, and the holder's
// proof over the envelope's digest verifies (p2pRxBucket). It returns the
// row (nil: none, or a credential that does not check), the rate bucket a
// direct delivery is charged to, and whether the envelope named a
// capability at all. It reads caps_issued once; a store error is no
// capability.
func (d *Daemon) capUnder(outer *seal.SealedEnvelope, env []byte, now uint64) (*interactions.IssuedCap, rxBucketKey, bool) {
	a := outer.Access
	if a == nil || a.Kind != seal.AccessKindCap {
		return nil, rxBucketKey{}, false
	}
	capID := capIDOf(a)
	if capID == nil || d.mbox == nil {
		return nil, rxBucketKey{}, true
	}
	issued, err := d.ix.IssuedCap(capID)
	if err != nil {
		return nil, rxBucketKey{}, true
	}
	mbox, ok := d.mbox.Current()
	if !ok {
		return nil, rxBucketKey{}, true
	}
	digest, err := seal.EnvDigest(env)
	if err != nil {
		return nil, rxBucketKey{}, true
	}
	k := p2pRxBucket(a, digest, issued, d.AID(), mbox, now)
	if k.Class == 0 {
		return nil, k, true
	}
	return issued, k, true
}

// misused revokes issued, the capability an envelope came under, when the
// envelope failed at step for a reason its holder cannot meet by accident
// (capMisuse; SS §4.3.8 item 2, engine ENG-V7). A preview frame's failure
// never does (CUX-P20): before step 4 decoded its type, a one-shot envelope
// may be one.
func (d *Daemon) misused(issued *interactions.IssuedCap, step rxStep, reason string, oneShot bool, typ string) {
	if issued == nil || !capMisuse(step, reason) {
		return
	}
	if oneShot && (typ == "" || typ == seal.TypeMessage) {
		return
	}
	if _, err := d.revokeMisused(issued.CapID); err != nil {
		log.Printf("anet: revoke a misused capability: %v", err)
	}
}

// previewOf decodes the ChatMsg of an anet.message/1 (nil when it does not
// decode: step 9 refuses it as bad-body) and reports whether it is a
// stream_preview frame (core CORE-K5).
func previewOf(in *seal.SealedInner) (*delegation.ChatMsg, bool) {
	if in.Type != seal.TypeMessage {
		return nil, false
	}
	cm, err := delegation.UnmarshalChatMsg(in.Body)
	if err != nil {
		return nil, false
	}
	return cm, cm.Kind == delegation.ChatStreamPreview
}

// kindKeysOK is step 4a (core CORE-K3, K5, G7, G11): which keys a message
// type must and must not carry, which the seal layer leaves to the daemon.
//
//   - a preview frame: no KEL, notice, key set or grant (10, 11, 12, 14);
//     outer key 8; two layers, with rk;
//   - anet.card.get/1 and anet.card/1: the KEL, its notice and the key set
//     (the requester checks the card under the provider's KEL, and the
//     provider answers to the requester's keys); outer key 8; one layer;
//   - everything else: the KEL, its notice and the key set; no outer key 8.
func kindKeysOK(op *seal.Opened, preview bool) bool {
	in := &op.Inner
	oneShot := op.Outer.Exp != nil
	carries := in.KEL != nil && len(in.Notices) > 0 && len(in.Keys) > 0
	switch {
	case preview:
		return in.KEL == nil && len(in.Notices) == 0 && in.Keys == nil && len(in.Grant) == 0 &&
			oneShot && op.Layers == 2 && in.RK != nil
	case in.Type == delegation.TypeCardGet, in.Type == delegation.TypeCard:
		return carries && oneShot && op.Layers == 1
	}
	return carries && !oneShot
}

// resolveSenderKEL is step 6. It makes no network request: the KEL comes
// from the message or from the persisted peer record.
//
//   - the inner KEL extends the stored one (or nothing is stored): use the
//     inner KEL and store it after acceptance — once its new events pass
//     R-9 (none dated more than five minutes after this node's clock;
//     kel-from-future, KEL D-23);
//   - the stored KEL extends the inner one: use the stored KEL, so a
//     signature by a key the stored history retired is judged as retired;
//   - they fork: refuse.
//
// With nothing stored the inner KEL is trusted on first use (§21 item 6). A
// preview frame carries no KEL (CORE-K5): the sender's stored one is used,
// from its peer_identity row or, for the requester of a public task, the
// one kept on the interaction.
func (d *Daemon) resolveSenderKEL(in *seal.SealedInner, inner []identity.SignedEvent, preview bool, now uint64) (
	use, update []identity.SignedEvent, row *interactions.PeerIdentity, res *rxResult) {
	from := in.From
	if preview {
		return d.storedSenderKEL(in)
	}
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
	if err != nil && !errors.Is(err, interactions.ErrNotFound) {
		r := d.transient(transientStore, err)
		return nil, nil, nil, &r
	}
	if errors.Is(err, interactions.ErrNotFound) {
		row = nil
	}
	var stored []identity.SignedEvent
	if row != nil && len(row.KEL) > 0 {
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
		if err := identity.CheckNewEvents(stored, inner, now, nil); err != nil {
			r := d.drop(seal.ReasonKELFromFuture, err)
			return nil, nil, nil, &r
		}
		update = inner
	}
	return use, update, row, nil
}

// storedSenderKEL is step 6 for a preview frame: the KEL this node holds for
// its sender.
func (d *Daemon) storedSenderKEL(in *seal.SealedInner) (use, update []identity.SignedEvent, row *interactions.PeerIdentity, res *rxResult) {
	row, err := d.ix.PeerIdentity(in.From)
	switch {
	case err == nil && len(row.KEL) > 0:
		if kel, kerr := identity.UnmarshalKEL(row.KEL); kerr == nil {
			return kel, nil, row, nil
		}
	case err != nil && !errors.Is(err, interactions.ErrNotFound):
		r := d.transient(transientStore, err)
		return nil, nil, nil, &r
	}
	if ix, rerr := d.lookupIX(in.IX); rerr != nil {
		return nil, nil, nil, rerr
	} else if ix != nil && ix.PeerAID == in.From && len(ix.PeerKEL) > 0 {
		if kel, kerr := identity.UnmarshalKEL(ix.PeerKEL); kerr == nil {
			return kel, nil, nil, nil
		}
	}
	r := d.drop(seal.ReasonBadKEL, fmt.Errorf("no KEL held for %s", in.From))
	return nil, nil, nil, &r
}

// checkNotice is step 6½ (KEL C-55, D-21): the notice must confirm the tip
// of the KEL the envelope carries, at its arrival, under the hub KELs this
// node holds. A witness whose KEL this node does not hold as far as the
// notice needs (witness-unknown) has the hub KEL set refreshed once and the
// notice checked again; still unknown, the envelope is temporary (T, no
// token): the refresh that would know the witness may come later.
func (d *Daemon) checkNotice(ctx context.Context, in *seal.SealedInner, kel []identity.SignedEvent, arrival uint64) *rxResult {
	err := seal.CheckNotice(in, kel, d.hubKELs, arrival)
	if seal.ReasonOf(err) == seal.ReasonWitnessUnknown && d.refreshHubKELs(ctx) {
		err = seal.CheckNotice(in, kel, d.hubKELs, arrival)
	}
	switch reason := seal.ReasonOf(err); {
	case err == nil:
		return nil
	case reason == seal.ReasonWitnessUnknown:
		r := d.transient(reason, err)
		return &r
	default:
		r := d.drop(reasonOf(err), err)
		return &r
	}
}

// authorize is step 9: decide whether the sender may send this message for
// this interaction. It writes nothing and sends nothing (ENG-V15).
func (d *Daemon) authorize(m *rxMsg, now uint64) *rxResult {
	var res *rxResult
	switch m.typ {
	case seal.TypeDelegate:
		res = d.authorizeDelegate(m, now)
	case seal.TypeMessage:
		res = d.authorizeMessage(m)
	case seal.TypeStatus, seal.TypeResult:
		res = d.authorizeReply(m)
	default:
		r := d.drop(dropUnknownType, fmt.Errorf("type %q", m.typ))
		return &r
	}
	if res != nil {
		return res
	}
	return d.authorizeRK(m)
}

// authorizeRK is step 9's comparison of the rk a message carries with the
// one its task holds from the peer (FS-R4): a different one is refused,
// rk-mismatch, before anything is written; the peer announced one key for
// the task and keeps it (there is no change of rk within a task, FS §4.5.5).
// A message that carries none declares nothing, and a task with none yet
// records the first at step 10. A message for a delegation held for
// approval is compared with the rk the held delegation carried, which the
// approval makes the task's.
func (d *Daemon) authorizeRK(m *rxMsg) *rxResult {
	var recorded []byte
	switch {
	case len(m.rk) == 0:
		return nil
	case m.existing != nil:
		recorded = m.existing.PeerRK
	case m.pendingRoute:
		held, err := d.ix.GetPending(m.ix)
		if errors.Is(err, interactions.ErrNotFound) {
			return nil
		}
		if err != nil {
			r := d.transient(transientStore, err)
			return &r
		}
		recorded = held.PeerRK
	}
	if interactions.RKConflict(recorded, m.rk) {
		r := d.drop(dropRKMismatch, fmt.Errorf("%s: the rk is not the one the peer announced on the task", m.ix))
		return &r
	}
	return nil
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
// holds in another role or with another peer, nor one its requester
// canceled before it arrived (a tombstone); and the inbound policy (§5.2)
// accepts, holds or refuses it. A refusal writes nothing here: step 11
// records it and answers it with the note noteReasonFor gives (engine §4.5).
func (d *Daemon) authorizeDelegate(m *rxMsg, now uint64) *rxResult {
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
		// is not part of.
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
	// Its requester canceled it before it arrived (DLV T11): refused, and
	// not run.
	if tomb, err := d.ix.HasCancelTomb(m.from, m.ix, now); err != nil {
		r := d.transient(transientStore, err)
		return &r
	} else if tomb {
		r := d.drop(dropCanceled, nil)
		return &r
	}
	form := readDelegateForm(m.td, m.dr.Attachments...)
	// The task binding (core §2.4.3, CORE-K7): a delegation without it, a text task or a capability
	// call alike, is a bad request (step 2 of CUX-Q1a, after the policy). The receipt this node
	// would sign for it could not carry the requester's commitments.
	if delegation.CheckTaskBinding(m.td) != nil {
		form.bad = true
	}
	dec := d.decideDelegate(m.from, form)
	switch dec.action {
	case actRefuse:
		d.noteRefused(m.from, m.ix, m.typ, dec.reason, form.capID)
		reason := dropRefusedPrefix + dec.reason
		if dec.reason == reasonNotAccepting {
			reason = dropNotAccepting
		}
		r := d.drop(reason, nil)
		r.note = refusalNote(dec.reason, dec.retryAfterMS, now)
		return &r
	case actHold:
		m.hold = true
	default:
		m.trust, m.release = dec.trust, dec.release
	}
	return nil
}

// refusalNote is the note a delegation refused with code is answered with
// (noteReasonFor, CUX-Q1).
func refusalNote(code string, retryAfterMS int64, now uint64) seal.Note {
	reason, retryS := noteReasonFor(AdmitVerdict{Refusal: code, RetryAfterMS: retryAfterMS, Now: now})
	return seal.Note{Reason: reason, RetryAfterS: retryS}
}

// parseDelegate decodes and verifies an anet.delegate/1 body: a DelegateReq
// for inner.ix whose TaskDoc verifies under the resolved KEL at the message
// time and is signed by inner.from. It fills m.dr, m.td, m.tdBytes and
// m.requestCID. A body that does not decode — an unknown member, a TaskDoc
// that does not decode or holds no task — is bad-body (delegation.ErrBody,
// CUX §4.1); bad-taskdoc is a TaskDoc whose signature does not verify.
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
		reason := dropBadTaskDoc
		if errors.Is(err, delegation.ErrBody) {
			reason = dropBadBody
		}
		r := d.drop(reason, err)
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
// under the same ix is a collision. Revocation reaches existing
// interactions (§5.1): a sender on the deny list, or no longer allowed on a
// trust=peer interaction, has its copy taken as what it is — the
// delegation this node accepted, answered tok(1) — and nothing more done
// for it (engine ENG-V18). The interaction's trust is kept.
func (d *Daemon) authorizeRedelivery(m *rxMsg, ix *interactions.Interaction) *rxResult {
	if ix.RequestCID == "" || ix.RequestCID != m.requestCID {
		r := d.drop(dropIXCollision, fmt.Errorf("%s was accepted for request %s, this one is %s",
			m.ix, ix.RequestCID, m.requestCID))
		return &r
	}
	m.existing, m.trust = ix, ix.Trust
	if !d.mayRedeliver(m, ix) {
		d.count(redeliveryNotAuthorized)
		m.redeliveryDenied = true
	}
	return nil
}

// mayRedeliver reports whether the sender of a redelivered delegation for
// ix, the interaction it was accepted for, may still have it acted on: the
// same request, a sender not on the deny list, and on a trust=peer
// interaction one still allowed [redteam:F7].
func (d *Daemon) mayRedeliver(m *rxMsg, ix *interactions.Interaction) bool {
	if m.requestCID == "" || m.requestCID != ix.RequestCID {
		return false
	}
	ps := d.readPeers()
	if ps.denied(m.from) {
		return false
	}
	if ix.Trust == interactions.TrustPeer && !ps.allowed(m.from) {
		return false
	}
	return true
}

// authorizeMessage checks an anet.message/1: the interaction exists (or is
// held in the approval queue), its peer is the sender, in either role, and
// the sender is not denied. On a trust=peer interaction the sender must
// still be allowed; on a public_cap interaction only cancel, end_request
// and payment messages are taken.
//
// A message for an interaction this node does not hold is refused at once,
// task_not_found, whoever sent it and by whichever path (engine ENG-V10):
// no message waits in the mailbox for its delegation. The sender's outbox
// does not send a follow-up before its delegation is confirmed (ENG-O2), so
// an honest one does not arrive first. A cancel for such an interaction is
// the exception: it is taken, and kept as a tombstone, so the delegation it
// cancels is refused if it arrives after it (DLV T11).
//
// The deny list is read after the interaction, not before: a denied peer
// writing to an interaction this node does not hold is answered exactly as
// a stranger is, so under closed it cannot tell it is denied (X2).
func (d *Daemon) authorizeMessage(m *rxMsg) *rxResult {
	cm := m.cm
	if cm == nil {
		var err error
		if cm, err = delegation.UnmarshalChatMsg(m.body); err != nil {
			r := d.drop(dropBadBody, err)
			return &r
		}
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
		if cm.Kind == delegation.KindCancel {
			m.cm, m.tombstone = cm, true
			return nil
		}
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
		// (an internal note item): the deny sweep leaves such a task open.
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
	if m.rr != nil && m.rr.Reply != nil && ix.IsCapability {
		// Only a text task's result carries a reply (CUX-R1); the decoder
		// cannot tell, this node knows the task.
		r := d.drop(dropBadBody, fmt.Errorf("a capability task's result carries a reply"))
		return &r
	}
	m.existing = ix
	return nil
}

// receiveOneShot is steps 8½ to 10 of a one-shot envelope (engine §4.5): a
// preview frame, a card request or a card answer. None creates or changes an
// interaction, none is held for approval, none has a replay row, and none is
// answered with a token (ENG-V6); a mailbox row carrying one is acknowledged
// with its id alone. Whatever happens to it is counted.
func (d *Daemon) receiveOneShot(ctx context.Context, m *rxMsg, preview bool, keys *seal.EncKeySet,
	in *seal.SealedInner, now uint64) rxResult {
	switch {
	case preview:
		// A frame's rk is compared with the task's, and never recorded
		// (FS-R4): a task with no rk of the peer's yet compares nothing.
		ix, res := d.lookupIX(m.ix)
		if res != nil {
			return res.at(rxStep9)
		}
		if ix != nil && ix.PeerAID == m.from && interactions.RKConflict(ix.PeerRK, m.rk) {
			return d.drop(dropRKMismatch, fmt.Errorf("%s: a preview frame under another rk", m.ix)).at(rxStep9)
		}
		m.existing = ix
		if !d.previewArrived(m) {
			return d.drop(previewRejected, nil).at(rxStep9)
		}
		return rxResult{class: rxAccepted, reason: "preview", step: rxStep9}
	case m.typ == delegation.TypeCardGet:
		if _, err := delegation.UnmarshalCardGet(m.body); err != nil {
			return d.drop(dropBadBody, err).at(rxStep9)
		}
		return d.answerCardRequest(ctx, m, in, keys, now)
	case m.typ == delegation.TypeCard:
		resp, err := delegation.UnmarshalCardResp(m.body)
		if err != nil {
			return d.drop(dropBadBody, err).at(rxStep9)
		}
		if !d.cardAnswerArrived(m.from, m.ix, m.kel, resp) {
			return d.drop(cardRejected, nil).at(rxStep9)
		}
		return rxResult{class: rxAccepted, reason: "card", step: rxStep9}
	}
	// Step 4a lets no other type through with outer key 8.
	return d.drop(dropBadKindKeys, nil).at(rxStep9)
}

// answerCardRequest is a provider's answer to anet.card.get/1 (engine §4.5;
// HPP PAY-C8, C9): the card answerCardGet gives the sender, sent once under
// the one-shot capability the request brought for it (SS §4.3.2 rule 0) and
// under no other (ENG-K2): a request without one is not answered. The deny
// list and strangers are treated alike (answerCardGet). Nothing is written:
// no interaction, no approval queue, no replay row. The answer goes in the
// background, so a slow hub or peer does not hold the mailbox round or the
// direct ack.
func (d *Daemon) answerCardRequest(ctx context.Context, m *rxMsg, in *seal.SealedInner, keys *seal.EncKeySet,
	now uint64) rxResult {
	grant, ok := d.cardRequestGrant(in, keys, now)
	if !ok {
		return d.drop(cardGetNoGrant, nil).at(rxStep9)
	}
	body, err := func() ([]byte, error) {
		resp := d.answerCardGet(m.from)
		return resp.Marshal()
	}()
	if err != nil {
		log.Printf("anet: answer a card request from %s: %v", m.from, err)
		return d.drop(cardGetNoGrant, nil).at(rxStep9)
	}
	d.cardGrants.put(m.from, m.ix, grant, now)
	from, ix := m.from, m.ix
	if f := d.testCardAnswerHook(); f != nil {
		f(from, ix, body)
		d.count(cardGetAnswered)
		return rxResult{class: rxAccepted, reason: cardGetAnswered, step: rxStep9}
	}
	d.goBackground(func() {
		sctx, cancel := context.WithTimeout(d.ctx, hubCallTimeoutShort)
		defer cancel()
		d.oneShotSend(sctx, oneShotCard, from, ix, body)
	})
	d.count(cardGetAnswered)
	return rxResult{class: rxAccepted, reason: cardGetAnswered, step: rxStep9}
}

// cardRequestGrant is the reply capability a card request brought (inner key
// 14): checked against the key set it carries, one-shot for the request's
// own interaction, contact class (core G11). It is held in memory for the
// answer only, never in caps_held (SSR-W8).
func (d *Daemon) cardRequestGrant(in *seal.SealedInner, keys *seal.EncKeySet, now uint64) (interactions.HeldCap, bool) {
	if len(in.Grant) == 0 || keys == nil {
		return interactions.HeldCap{}, false
	}
	g, body, err := seal.CheckGrant(in, keys, now)
	if err != nil {
		d.countErr(cntGrantInvalid, err)
		return interactions.HeldCap{}, false
	}
	h, err := interactions.HeldFromGrant(g, body, now)
	if err != nil {
		d.countErr(cntGrantInvalid, err)
		return interactions.HeldCap{}, false
	}
	if h.Scope != interactions.CapScopeIX || h.IX != in.IX || h.Class != seal.CapClassContact {
		return interactions.HeldCap{}, false
	}
	return h, true
}

// process is step 10: run the type's handler, under the (from, mid) lock
// receiveEnvelope holds, for a message the replay table did not hold when
// the lock was taken (each handler's transaction claims the replay row, so
// a copy under another message id is still recorded once). The sender's KEL
// and key set are recorded in the handler's transaction (commitRx,
// recordSenderTx).
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
	d.noteLivePeer(m.from)
	if m.typ == seal.TypeDelegate {
		d.kickTurns()
	}
	return res
}

// recordSenderTx writes the sender's KEL and key set in the step-10
// transaction of an accepted message (engine §4.7 item 7), only in the
// authorized contexts of A2A-DESIGN §3.8: the interaction is one this node
// started (pinned "outbound"), the sender is on the allow or trust list
// (pinned "allow" or "trust"), or the interaction was approved by the
// operator. The requester of a public or public_cap interaction is not
// recorded in peer_identity; its key material is kept on the interaction
// instead. A held delegation's sender is recorded in the pending row, and a
// cancel kept as a tombstone has no interaction: nothing is written for
// either. What the write voids or revokes — the capabilities the sender
// signed under an older mailbox epoch, and on its emergency rotation every
// capability the two gave each other (ENG-K4) — goes in the same
// transaction (interactions.UpdatePeerIdentity, SetPeerKeys).
func (d *Daemon) recordSenderTx(tx *interactions.Tx, m *rxMsg, ps peerSets) error {
	if m.hold || m.pendingRoute || m.tombstone {
		return nil
	}
	ix, err := tx.Get(m.ix)
	if errors.Is(err, interactions.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	pin, authorized := "", false
	switch {
	case ix.Role == interactions.RoleOutbound:
		pin, authorized = interactions.PinOutbound, true
	case ps.trusted(m.from):
		pin, authorized = interactions.PinTrust, true
	case ps.allowed(m.from):
		pin, authorized = interactions.PinAllow, true
	case ix.Trust == interactions.TrustApproved:
		authorized = true
	}
	if authorized {
		if m.kelUpdate == nil && m.keysUpdate == nil && pin == "" {
			return nil
		}
		return tx.UpdatePeerIdentity(m.from, d.peerUpdate(m.from, m.kelUpdate, m.keysUpdate, pin, false, d.nowMS()))
	}
	if (ix.Trust == interactions.TrustPublic || ix.Trust == interactions.TrustPublicCap) && !ix.IsTerminal() &&
		(m.kelUpdate != nil || m.keysUpdate != nil) {
		var kel, keys []byte
		if m.kelUpdate != nil {
			if kel, err = identity.MarshalKEL(m.kelUpdate); err != nil {
				return err
			}
		}
		if m.keysUpdate != nil {
			keys = m.keysUpdate.signed
		}
		return tx.SetPeerKeys(m.ix, kel, keys)
	}
	return nil
}

// recordPeerRKTx records m's rk as the peer's for its task, in the step-10
// transaction (engine §4.7 item 3, FS-R4), when the task has none yet. A
// task the message does not reach — a delegation held for approval (the held
// row keeps it), a cancel kept as a tombstone, a task deleted meanwhile —
// records nothing. One that holds another rk is a permanent refusal,
// rk-mismatch, and the transaction rolls back with every write it held.
func recordPeerRKTx(tx *interactions.Tx, m *rxMsg) error {
	if len(m.rk) == 0 || m.ix == "" || m.hold || m.pendingRoute || m.tombstone {
		return nil
	}
	switch err := tx.RecordPeerRK(m.ix, m.rk); {
	case err == nil, errors.Is(err, interactions.ErrNotFound):
		return nil
	case errors.Is(err, interactions.ErrRKMismatch):
		return rxPermanent(dropRKMismatch, err)
	default:
		return err
	}
}

// errReplayDuplicate aborts a transaction whose replay row already exists.
var errReplayDuplicate = errors.New("replay row exists")

// rxPermanentErr aborts a step-10 transaction for a reason that will not
// change on redelivery. commitRx rolls the transaction back and reports it
// as a permanent refusal with the given counter, not as a store error.
type rxPermanentErr struct {
	reason string
	note   seal.Note
	err    error
}

func (e *rxPermanentErr) Error() string { return e.reason + ": " + e.err.Error() }
func (e *rxPermanentErr) Unwrap() error { return e.err }

// rxPermanent returns an error a business write returns from inside
// commitRx to refuse the message permanently.
func rxPermanent(reason string, err error) error { return &rxPermanentErr{reason: reason, err: err} }

// commitRx runs a message's business writes and its replay row in one
// SQLite transaction (engine §4.7). Either all are stored or none is: a
// store error leaves no replay row behind, so the redelivered message is
// processed, and a crash after the commit leaves all, so it is not
// processed twice. With them, in the same transaction: the grant the
// message carries (item 4), the implicit confirmation of this node's
// delegation to the sender (item 2) and the sender's KEL and key set (item
// 7).
func (d *Daemon) commitRx(m *rxMsg, fn func(*interactions.Tx) error) rxResult {
	implicit := 0
	ps := d.readPeers()
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
		// The peer's rk for the task, the first time it announces one; a
		// different one than the task holds, which a copy racing this one
		// recorded since step 9 read it, refuses the message (ENG §4.7
		// item 3, FS-R4).
		if err := recordPeerRKTx(tx, m); err != nil {
			return err
		}
		// The grant goes with the message it came in (ENG §4.7 item 4).
		if m.grant != nil {
			if _, err := tx.PutHeldGrant(*m.grant); err != nil {
				return err
			}
		}
		// A message from the peer of a task this node asked for confirms
		// its delegation, in the same transaction (ENG §4.7 item 2,
		// ENG-C2).
		if implicit, err = d.confirmImplicitTx(tx, m.ix, m.from); err != nil {
			return err
		}
		if err := d.recordSenderTx(tx, m, ps); err != nil {
			return err
		}
		if fault := d.testRxFault(); fault != nil {
			return fault(m.typ)
		}
		return nil
	})
	var perm *rxPermanentErr
	switch {
	case err == nil:
		m.committed()
		d.implicitConfirmed(m.ix, implicit)
		return accepted()
	case errors.Is(err, errReplayDuplicate):
		return d.drop(dropDuplicate, nil)
	case errors.As(err, &perm):
		r := d.drop(perm.reason, perm.err)
		r.note = perm.note
		return r
	default:
		return d.transient(transientStore, err)
	}
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

// tryLock takes key's lock only when nobody holds it.
func (k *keyedLocks) tryLock(key string) (func(), bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = map[string]*keyedLock{}
	}
	if l := k.m[key]; l != nil {
		return nil, false
	}
	l := &keyedLock{n: 1}
	l.mu.Lock()
	k.m[key] = l
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		l.n--
		if l.n == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}, true
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

// noticeLimiter bounds the notices after the fact (a held task's approval
// wait, expiry or rejection; queueNotice, engine ENG-Q7): per peer and
// daemon-wide, at most 6 a peer an hour and 60 a minute. A notice over
// either limit is not queued. An immediate refusal is no notice: it is told
// in its delivery token (ENG-V15).
type noticeLimiter struct {
	mu     sync.Mutex
	global tokenBucket
	peers  map[string]*tokenBucket
	order  []string
	// perPeerPerHour and globalPerMin are the rates; zero means the
	// constants noticePerPeerPerHour and noticeGlobalPerMin, which are all
	// a daemon runs with (core G10: inbound.reject_notice is gone). Only a
	// test sets others.
	perPeerPerHour, globalPerMin int
}

// configure sets the rates (a test's) and resets the buckets.
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

// resendLimiter bounds the answers hurried for redelivered delegations
// (engine ENG-V18, C33) [redteam:F29], the way noticeLimiter bounds the
// notices: per (peer, interaction) and daemon-wide. A redelivery means the
// requester may not have the answer, and one hurry of its queue is what
// that needs; every further redelivery of the same answered delegation — a
// hub or a requester replaying it to spend this node's hub send budget — is
// acknowledged and does not hurry it again until the bucket refills. The
// keys are bounded; the oldest is forgotten first.
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
// expired, with the clock skew tolerance as margin, and the cancel
// tombstones that have.
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
	// A tombstone's exp already allows for the clock skew (DLV-ST-7).
	if _, err := d.ix.PurgeCancelTombs(d.nowMS()); err != nil {
		log.Printf("anet: purge cancel tombstones: %v", err)
	}
}

// receiveMaintenance runs the periodic housekeeping of the receive path
// until ctx ends: every hour, expired replay rows; every minute, the
// inbound policy's periodic work. New runs the first purge itself. The
// key ring's maintenance period has a loop of its own (keyMaintenanceLoop).
func (d *Daemon) receiveMaintenance(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	m := time.NewTicker(time.Minute)
	defer m.Stop()
	for {
		select {
		case <-ctx.Done():
			d.flushInboundSummary(true)
			return
		case <-t.C:
			d.purgeReplay()
		case <-m.C:
			// The inbound policy's periodic work (inbound.go, pending.go):
			// the refusal and acceptance summaries, expiry of held
			// delegations, and interactions of peers added to the deny list
			// outside the CLI.
			d.flushInboundSummary(false)
			d.expirePending()
			d.revocationSweep()
			d.sweepCallFiles()
		}
	}
}

// keyMaintenanceLoop runs a maintenance period every S until ctx ends
// (keyMaintenance, FS-K4): the key ring, its deletions under the clock gate
// and its publication, the interaction keys past close_after and the
// public_cap retention sweep. It starts with the hard-bound sweeps at the
// level of the process's first period (setupKeyRing), so a node restarted
// more often than every period still sweeps; New runs that period itself,
// and the registration at start publishes the set.
//
// A period takes the mailbox poll's lock, so it runs between two poll
// rounds of the relay loop, serial with the receive path of the hub
// mailbox; a node without a hub has no poll to wait for. The loop is its
// own because a poll moving a large envelope holds that lock for minutes,
// and the rest of the receive path's housekeeping (receiveMaintenance)
// does not wait with it.
func (d *Daemon) keyMaintenanceLoop(ctx context.Context) {
	k := time.NewTicker(keyMaintainEvery.get())
	defer k.Stop()
	d.sweepHardBound(d.startLevel)
	for {
		select {
		case <-ctx.Done():
			return
		case <-k.C:
			d.keyMaintenance()
		}
	}
}
