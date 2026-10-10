package daemon

// seal_send.go is the single place an outgoing daemon-to-daemon message is
// built (A2A-DESIGN §3.5):
//
//  1. resolve the recipient's encryption key set: the stored one while it
//     verifies and holds a usable key, else the one resolvePeer finds in
//     the directory without naming the recipient to the hub
//     (peer_resolve.go, sealed-sender SSR-D15), verified with expectAID =
//     the recipient (C0) and against any stored KEL for it (§3.8);
//  2. build the inner message with this node's full KEL, the witness notice
//     confirming its tip and its current signed key set (innerFor), sign it
//     and seal it (seal.Seal), without outer key 7.
//
// A queued message is sealed by the outbound engine (reseal.go, A7), which
// sets its credential for every attempt (ENG-I2); a one-shot envelope by the
// one-shot sender (oneshot.go), the only place outer key 8 is set (engine
// ENG-I1). Nothing is sealed while the outbound gate is closed, or while no
// witness notice confirms the key state the message would be signed under
// (engine ENG-K13, ENG-R3 S3). sealWith seals a refusal notice that is not
// queued, once, as a first contact (sendNotice).
//
// There is no unsealed path. A recipient whose keys cannot be resolved is
// an error, not a plaintext send.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// messageLifetimeMS is exp - ts of every message this node sends: the hub's
// undelivered-message TTL (A2A-DESIGN §3.5 step 2, §3.7). A message that
// cannot reach its recipient within it is not delivered at all, rather than
// delivered long after the sender stopped waiting.
const messageLifetimeMS = 14 * 24 * 3600 * 1000

// errNoRecipientKeys is returned when no encryption key can be resolved for
// a recipient. Nothing is sent.
var errNoRecipientKeys = errors.New("no usable encryption key for the recipient")

// sealMsg is one inner message to seal: its recipient, type, interaction
// and body, the identity every version of it shares (mid, ts, exp, ENG-R7),
// the grant it carries (inner key 14, nil for none) and this node's rk on
// the task (inner key 15, nil for none: FS-X4).
type sealMsg struct {
	to, typ, ix string
	body, mid   []byte
	ts, exp     uint64
	grant       []byte
	rk          *seal.RK
	// bare leaves out this node's KEL, its notice and its key set (inner
	// keys 10, 11 and 12): a preview frame carries none of them (core
	// CORE-K5, G7).
	bare bool
}

// errNoTipNotice is why nothing is sealed when no witness notice confirms
// the key state a message would be signed under: a rotation was built
// between the gate's reading and the Snapshot. The gate is closed at its
// next reading, and the message waits for it like any other.
var errNoTipNotice = fmt.Errorf("%w: no witness notice confirms the key state to sign under", errGateClosed)

// innerFor builds m's inner message at now under one Snapshot of this
// node's key state, which it returns to sign with: one Snapshot signs, is
// the KEL carried and is what the notice confirms, so a rotation built
// meanwhile does not split them (KEL-8a, engine ENG-K17). The notice's
// observed_at is the version's sealed_notice_at (ENG-S3); 0 for a bare
// message.
//
// It builds nothing while the outbound gate is closed (ENG-K13, ENG-R3 S3:
// no signature under a key that may be leaking), nor when no notice
// confirms the Snapshot's tip (tipNotice): either is an error that is
// errGateClosed, and the caller waits for the gate.
func (d *Daemon) innerFor(m sealMsg, now uint64) (*seal.SealedInner, identity.Signer, uint64, error) {
	if m.to == d.AID() {
		return nil, nil, 0, fmt.Errorf("anet: refusing to seal a message to this node itself")
	}
	if m.exp <= now {
		return nil, nil, 0, fmt.Errorf("anet: %s to %s: the message expired before it could be sealed", m.typ, m.to)
	}
	if g := d.identityGate(); !g.Open {
		return nil, nil, 0, &gateClosedError{state: g.State}
	}
	s := d.self.Snapshot()
	n := d.tipNotice(s)
	if n == nil {
		return nil, nil, 0, errNoTipNotice
	}
	seq, err := identity.SeqOf(s)
	if err != nil {
		return nil, nil, 0, err
	}
	inner := &seal.SealedInner{
		From:        d.AID(),
		KeyStateSeq: seq,
		To:          m.to,
		Type:        m.typ,
		IX:          m.ix,
		MID:         m.mid,
		TS:          m.ts,
		Exp:         m.exp,
		Body:        m.body,
		Grant:       m.grant,
		RK:          m.rk,
	}
	if m.bare {
		return inner, s, 0, nil
	}
	if inner.KEL, err = identity.MarshalKEL(s.KEL()); err != nil {
		return nil, nil, 0, err
	}
	if inner.Keys = d.enc.SignedSet(); len(inner.Keys) == 0 {
		return nil, nil, 0, fmt.Errorf("anet: this node has no encryption key set to attach")
	}
	// The witness notice for this KEL; seal v2 carries one exactly when it
	// carries the KEL (C-52).
	inner.Notices = [][]byte{n.Wire}
	return inner, s, n.Body.ObservedAt, nil
}

// sealMessage builds m's inner message (innerFor) and seals it to the
// usable key of set at now, under o: o.PeerRK seals a second layer to the
// peer's rk (FS-X5); o.Exp is outer key 8, which only the one-shot sender
// sets, equal to m.exp (engine ENG-I1, TestOnlyOneShotSealsWithExp) — a
// queued message never carries it. The envelope has no outer key 7. What
// the version records about its keys comes with it (ENG-S3).
func (d *Daemon) sealMessage(m sealMsg, set *seal.EncKeySet, o seal.SealOpts, now uint64) (*sealedVersion, error) {
	key, err := seal.SelectKey(set, now)
	if err != nil {
		return nil, fmt.Errorf("anet: %s: %w: %v", m.to, errNoRecipientKeys, err)
	}
	inner, s, noticeAt, err := d.innerFor(m, now)
	if err != nil {
		return nil, err
	}
	env, ack, err := seal.Seal(inner, key, s.Sign, o)
	if err != nil {
		return nil, fmt.Errorf("anet: seal %s to %s: %w", m.typ, m.to, err)
	}
	return &sealedVersion{env: env, ackKey: ack, kid: bytes.Clone(key.KID), kidNA: key.NotAfter, kss: inner.KeyStateSeq,
		noticeAt: noticeAt}, nil
}

// sealWith seals one message that is not queued to a key set the caller
// resolved: a fresh message id, ts now, exp 14 days on, no grant, and the
// first-contact credential in outer key 7 (it is sent once, sendNotice).
//
// v030-todo(ENG-1c): the refusal notices are the last envelopes neither
// queued nor one-shot (engine ENG-Q1, ENG-V15): sendNotice and
// deliverEnvelope go with replyRejected and sendNoticeStatus, and sealWith
// with them (the tests' sealFrom can seal through sealMessage).
func (d *Daemon) sealWith(toAID, typ, interactionID string, body []byte, set *seal.EncKeySet) ([]byte, error) {
	now := d.nowMS()
	sv, err := d.sealMessage(sealMsg{to: toAID, typ: typ, ix: interactionID, body: body, mid: seal.NewMID(), ts: now,
		exp: now + messageLifetimeMS}, set, seal.SealOpts{}, now)
	if err != nil {
		return nil, err
	}
	clear(sv.ackKey[:])
	env, err := seal.SetAccess(sv.env, &seal.Access{Kind: seal.AccessKindSession})
	if err != nil {
		return nil, fmt.Errorf("anet: seal %s to %s: %w", typ, toAID, err)
	}
	return env, nil
}

// newWireMID mints the inner message id of a message this node records,
// with the msg_id it is recorded under: the id in hex (an internal note item). The
// receiver records the same hex of the same id, so both sides name a
// message alike, and a projection's messageId is the same on both.
func newWireMID() (mid []byte, msgID string) {
	mid = seal.NewMID()
	return mid, hex.EncodeToString(mid)
}

// wireMsgID is the msg_id a received message is recorded under: the id the
// sender put in the message body when it did (a chat message carries one),
// otherwise the envelope's inner id in hex (an internal note item).
func wireMsgID(bodyID string, mid []byte) string {
	if bodyID != "" {
		return bodyID
	}
	if len(mid) == 0 {
		return ""
	}
	return hex.EncodeToString(mid)
}

// recipientKeys is §3.5 step 1: the stored key set while it verifies and
// holds a key usable now — fetched again in the background once a day for
// an active contact (SSR-D18) — else the directory's (resolvePeer, which
// records it): bypassing every cache when the stored set has no usable key
// left (SSR-D18). A recipient the directory does not hold, or holds only
// unverifiably, is a *peerResolveError; so is a directory out of reach,
// which a send retries (hubUnreachable).
func (d *Daemon) recipientKeys(ctx context.Context, toAID, pin string) (*peerKeySet, error) {
	now := d.nowMS()
	row, err := d.ix.PeerIdentity(toAID)
	if err != nil && !errors.Is(err, interactions.ErrNotFound) {
		return nil, fmt.Errorf("anet: read peer record for %s: %w", toAID, err)
	}
	if ks, ok := d.storedKeySet(row, now); ok {
		if _, serr := seal.SelectKey(ks.set, now); serr == nil {
			d.recheckActiveContact(row, pin, now)
			if pin != "" && row.PinnedReason == "" {
				d.pinIfUnpinned(toAID, pin)
			}
			return ks, nil
		}
	}
	m, v := d.resolvePeer(ctx, toAID, resolveOpts{need: needKeys, bypass: row != nil, record: true, pin: pin})
	if v != verdictFound {
		return nil, fmt.Errorf("anet: fetch %s's encryption keys: %w", toAID, &peerResolveError{AID: toAID, Verdict: v, Cause: m.Err})
	}
	return m.Keys, nil
}

// sendNotice seals and delivers a refusal reply (a status message) to a
// sender whose key set is in the stranger cache. It runs in the background,
// is attempted once and never retried or queued: a refusal must not delay
// or block anything else (A2A-DESIGN §2 X2). It never fetches keys from the
// hub and never writes peer_identity.
func (d *Daemon) sendNotice(toAID, interactionID string, body []byte) {
	keys, ok := d.strangers.get(toAID)
	if !ok || keys.set == nil {
		return
	}
	d.goBackground(func() {
		env, err := d.sealWith(toAID, seal.TypeStatus, interactionID, body, keys.set)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(d.ctx, hubCallTimeoutShort)
		defer cancel()
		if err := d.deliverEnvelope(ctx, toAID, env); err != nil {
			d.logOnce("notice:"+toAID, "anet: refusal notice to %s not delivered: %v", toAID, err)
		}
	})
}

// nowMS is the daemon's clock in unix milliseconds. Tests replace it with
// setClock.
func (d *Daemon) nowMS() uint64 {
	if clock := d.testClock(); clock != nil {
		return clock()
	}
	return uint64(time.Now().UnixMilli())
}

// interactionKeySet verifies the key set kept on a public or public_cap
// interaction against the KEL kept with it, and returns it when it holds a
// usable key.
func interactionKeySet(ix *interactions.Interaction, now uint64) (*peerKeySet, bool) {
	if len(ix.PeerKEL) == 0 || len(ix.PeerKeys) == 0 {
		return nil, false
	}
	kel, err := identity.UnmarshalKEL(ix.PeerKEL)
	if err != nil {
		return nil, false
	}
	signed, err := seal.UnmarshalSignedEncKeySet(ix.PeerKeys)
	if err != nil {
		return nil, false
	}
	set, err := seal.VerifyEncKeySet(signed, ix.PeerAID, kel, now)
	if err != nil {
		return nil, false
	}
	if _, err := seal.SelectKey(set, now); err != nil {
		return nil, false
	}
	return &peerKeySet{signed: ix.PeerKeys, set: set}, true
}
