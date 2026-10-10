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
//     confirming its tip and its current signed key set, sign it and seal
//     it (seal.Seal), without outer key 7.
//
// A queued message is sealed by the outbound engine (reseal.go, A7), which
// sets its credential for every attempt (ENG-I2). sealWith seals a message
// that is not queued, once, as a first contact (sendNotice).
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
// and the grant it carries (inner key 14, nil for none).
type sealMsg struct {
	to, typ, ix string
	body, mid   []byte
	ts, exp     uint64
	grant       []byte
}

// sealMessage builds m's inner message and seals it to the usable key of
// set at now: one Snapshot of this node's key state signs, is the KEL
// carried and is what the notice confirms, so a rotation built meanwhile
// does not split them (KEL-8a, engine ENG-K17). The envelope has no outer
// key 7. What the version records about its keys comes with it (ENG-S3).
func (d *Daemon) sealMessage(m sealMsg, set *seal.EncKeySet, now uint64) (*sealedVersion, error) {
	if m.to == d.AID() {
		return nil, fmt.Errorf("anet: refusing to seal a message to this node itself")
	}
	if m.exp <= now {
		return nil, fmt.Errorf("anet: %s to %s: the message expired before it could be sealed", m.typ, m.to)
	}
	key, err := seal.SelectKey(set, now)
	if err != nil {
		return nil, fmt.Errorf("anet: %s: %w: %v", m.to, errNoRecipientKeys, err)
	}
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
	var noticeAt uint64
	if n := d.tipNotice(s); n != nil {
		notice, noticeAt = n.Wire, n.Body.ObservedAt
	}
	// v030-todo(ENG-1b): with no notice confirming the tip the gate is
	// closed and nothing is sealed (ENG-K13); a rotation built between the
	// gate's reading and this Snapshot still seals with an empty notice,
	// which receivers accept until they check step 6½ (ENG-1c).
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
		KEL:         kel,
		Notices:     [][]byte{notice},
		Keys:        own,
		Grant:       m.grant,
	}
	env, ack, err := seal.Seal(inner, key, s.Sign, seal.SealOpts{})
	if err != nil {
		return nil, fmt.Errorf("anet: seal %s to %s: %w", m.typ, m.to, err)
	}
	return &sealedVersion{env: env, ackKey: ack, kid: bytes.Clone(key.KID), kidNA: key.NotAfter, kss: seq, noticeAt: noticeAt}, nil
}

// sealWith seals one message that is not queued to a key set the caller
// resolved: a fresh message id, ts now, exp 14 days on, no grant, and the
// first-contact credential in outer key 7. Only tests call it (sealFrom):
// the refusal notices it sealed are gone (engine ENG-V15).
func (d *Daemon) sealWith(toAID, typ, interactionID string, body []byte, set *seal.EncKeySet) ([]byte, error) {
	now := d.nowMS()
	sv, err := d.sealMessage(sealMsg{to: toAID, typ: typ, ix: interactionID, body: body, mid: seal.NewMID(), ts: now,
		exp: now + messageLifetimeMS}, set, now)
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
