package daemon

// seal_send.go is the single place an outgoing daemon-to-daemon message is
// built (A2A-DESIGN §3.5):
//
//  1. resolve the recipient's encryption key set: the stored one while it
//     verifies and holds a usable key, else the one resolvePeer finds in
//     the directory without naming the recipient to the hub
//     (peer_resolve.go, sealed-sender SSR-D15), verified with expectAID =
//     the recipient (C0) and against any stored KEL for it (§3.8);
//  2. build the inner message with this node's full KEL and current signed
//     key set, a 16-byte message id and exp = ts + 14 days, sign it and
//     seal it (seal.Seal). A message this node records is sealed under the
//     id it was recorded with (newWireMID), and a retry-queue row sealed
//     late under the deadline it was queued with (sealOpts);
//  3. hand the envelope to the transport list, p2p first, hub last. Every
//     transport receives the same bytes.
//
// There is no unsealed path. A recipient whose keys cannot be resolved is
// an error, not a plaintext send.

import (
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

// relaySend seals one message to toAID and makes one attempt to deliver
// it, with nothing persisted. Messages on an interaction do not use it:
// every one goes through the retry queue (queueSendAs, an internal note item), which
// seals with the same functions and delivers with deliverEnvelope.
//
// The recipient record is pinned "outbound" when the interaction is one
// this node started: that relationship is this node's decision and the
// record must outlive eviction pressure while it exists.
func (d *Daemon) relaySend(ctx context.Context, toAID, typ, interactionID string, body []byte) error {
	env, err := d.sealEnvelope(ctx, toAID, typ, interactionID, body)
	if err != nil {
		return err
	}
	return d.deliverEnvelope(ctx, toAID, env)
}

// sealEnvelope builds and seals one message and returns the envelope bytes
// without sending them. A caller that must retry the same logical message
// (the result retry queue, §4.2) stores these bytes and passes them to
// deliverEnvelope on every attempt, so a retry is never a second message.
//
// A reply on a public or public_cap interaction is sealed to the key set
// kept on the interaction (A2A-DESIGN §3.8): that requester is not recorded
// in peer_identity, and a hub fetch here would record it. Only a
// peer_identity row that already exists (the requester is also a named
// peer) is used in its place.
func (d *Daemon) sealEnvelope(ctx context.Context, toAID, typ, interactionID string, body []byte) ([]byte, error) {
	return d.sealEnvelopeOpts(ctx, toAID, typ, interactionID, body, sealOpts{})
}

// sealOpts fixes what sealWith otherwise chooses itself.
type sealOpts struct {
	// mid is the inner message id; nil mints a fresh one. A message this
	// node records under its wire id (an internal note item) is sealed under the id it
	// was recorded with.
	mid []byte
	// exp is the inner exp (unix ms); 0 is ts + 14 days. A retry-queue row
	// sealed late keeps the deadline it was queued with.
	exp uint64
	// pin is the peer_identity pin for a recipient resolved here. Empty
	// derives it from the interaction row; a new delegation, whose row is
	// written after it is sealed, names it.
	pin string
}

// sealEnvelopeOpts is sealEnvelope with the message id, exp or pin fixed.
func (d *Daemon) sealEnvelopeOpts(ctx context.Context, toAID, typ, interactionID string, body []byte, o sealOpts) ([]byte, error) {
	pin := o.pin
	ix, ixErr := d.ix.Get(interactionID)
	if ixErr == nil && ix.Role == interactions.RoleOutbound {
		pin = interactions.PinOutbound
	}
	if ixErr == nil && ix.Role == interactions.RoleInbound &&
		(ix.Trust == interactions.TrustPublic || ix.Trust == interactions.TrustPublicCap) {
		now := d.nowMS()
		if ks, ok := interactionKeySet(ix, now); ok {
			return d.sealWithOpts(toAID, typ, interactionID, body, ks.set, o)
		}
		if row, err := d.ix.PeerIdentity(toAID); err == nil {
			if ks, ok := d.storedKeySet(row, now); ok {
				return d.sealWithOpts(toAID, typ, interactionID, body, ks.set, o)
			}
		}
		return nil, fmt.Errorf("anet: %s: %w: the requester's key set on this interaction is gone or expired", toAID, errNoRecipientKeys)
	}
	keys, err := d.recipientKeys(ctx, toAID, pin)
	if err != nil {
		return nil, err
	}
	return d.sealWithOpts(toAID, typ, interactionID, body, keys.set, o)
}

// sealWith seals to a key set the caller already resolved.
func (d *Daemon) sealWith(toAID, typ, interactionID string, body []byte, set *seal.EncKeySet) ([]byte, error) {
	return d.sealWithOpts(toAID, typ, interactionID, body, set, sealOpts{})
}

// sealWithOpts is sealWith with the message id or exp fixed.
func (d *Daemon) sealWithOpts(toAID, typ, interactionID string, body []byte, set *seal.EncKeySet, o sealOpts) ([]byte, error) {
	if toAID == d.AID() {
		return nil, fmt.Errorf("anet: refusing to seal a message to this node itself")
	}
	now := d.nowMS()
	mid, exp := o.mid, o.exp
	if mid == nil {
		mid = seal.NewMID()
	}
	if exp == 0 {
		exp = now + messageLifetimeMS
	}
	if exp <= now {
		return nil, fmt.Errorf("anet: %s to %s: the message expired before it could be sealed", typ, toAID)
	}
	key, err := seal.SelectKey(set, now)
	if err != nil {
		return nil, fmt.Errorf("anet: %s: %w: %v", toAID, errNoRecipientKeys, err)
	}
	// One key state signs, is the KEL carried and is what the notice
	// confirms: a rotation built meanwhile does not split them (KEL-8a,
	// engine ENG-K17).
	s := d.self.Snapshot()
	seq, err := identity.SeqOf(s)
	if err != nil {
		return nil, err
	}
	kel, err := identity.MarshalKEL(s.KEL())
	if err != nil {
		return nil, err
	}
	own := d.enc.SignedSet()
	if len(own) == 0 {
		return nil, fmt.Errorf("anet: this node has no encryption key set to attach")
	}
	// The witness notice for this KEL; seal v2 carries one exactly when it
	// carries the KEL (C-52).
	notice := []byte{}
	if n := d.tipNotice(s); n != nil {
		notice = n.Wire
	}
	// v030-todo(ENG-1b): with no notice confirming the tip the gate is
	// closed and nothing is sealed (ENG-K13); until the engine reads the
	// gate an envelope goes with an empty notice, which receivers accept
	// until they check step 6½ (ENG-1c).
	inner := &seal.SealedInner{
		From:        d.AID(),
		KeyStateSeq: seq,
		To:          toAID,
		Type:        typ,
		IX:          interactionID,
		MID:         mid,
		TS:          now,
		Exp:         exp,
		Body:        body,
		KEL:         kel,
		Notices:     [][]byte{notice},
		Keys:        own,
	}
	env, _, err := seal.Seal(inner, key, s.Sign, seal.SealOpts{})
	if err != nil {
		return nil, fmt.Errorf("anet: seal %s to %s: %w", typ, toAID, err)
	}
	// v030-todo(ENG-1a): the credential is set per attempt from accessFor
	// (ENG-I2); until then every envelope travels as a first contact.
	if env, err = seal.SetAccess(env, &seal.Access{Kind: seal.AccessKindSession}); err != nil {
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
