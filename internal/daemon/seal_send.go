package daemon

// seal_send.go is the single place an outgoing daemon-to-daemon message is
// built (A2A-DESIGN §3.5):
//
//  1. resolve the recipient's encryption key set: the stored one while it
//     verifies and holds a usable key, else POST {hub}/agents/keys:lookup
//     with the AID in the body (GET {hub}/agents/{aid}/keys from a hub
//     without that route), verified with expectAID = the recipient (C0)
//     and against any stored KEL for it (§3.8);
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
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// messageLifetimeMS is exp - ts of every message this node sends: the hub's
// undelivered-message TTL (A2A-DESIGN §3.5 step 2, §3.7). A message that
// cannot reach its recipient within it is not delivered at all, rather than
// delivered long after the sender stopped waiting.
const messageLifetimeMS = 14 * 24 * 3600 * 1000

// keysRevalidateMS is how often a stored recipient key set is re-checked
// against the hub. The stored set stays in use while the hub is
// unreachable or answers 404, until it holds no usable key (§3.5 step 1).
const keysRevalidateMS = 10 * 60 * 1000

// errNoRecipientKeys is returned when no encryption key can be resolved for
// a recipient. Nothing is sent.
var errNoRecipientKeys = errors.New("no usable encryption key for the recipient")

// relaySend seals one message to toAID and makes one attempt to deliver
// it, with nothing persisted. Messages on an interaction do not use it:
// every one goes through the retry queue (queueSendAs, 0017 Q5), which
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
	// node records under its wire id (0017 Q9) is sealed under the id it
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
	kel, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		return nil, err
	}
	own := d.enc.SignedSet()
	if len(own) == 0 {
		return nil, fmt.Errorf("anet: this node has no encryption key set to attach")
	}
	inner := &seal.SealedInner{
		From:        d.AID(),
		KeyStateSeq: d.self.CurrentSeq(),
		To:          toAID,
		Type:        typ,
		IX:          interactionID,
		MID:         mid,
		TS:          now,
		Exp:         exp,
		Body:        body,
		KEL:         kel,
		Keys:        own,
	}
	env, err := seal.Seal(inner, key, d.self.Sign)
	if err != nil {
		return nil, fmt.Errorf("anet: seal %s to %s: %w", typ, toAID, err)
	}
	return env, nil
}

// newWireMID mints the inner message id of a message this node records,
// with the msg_id it is recorded under: the id in hex (0017 Q9). The
// receiver records the same hex of the same id, so both sides name a
// message alike, and a projection's messageId is the same on both.
func newWireMID() (mid []byte, msgID string) {
	mid = seal.NewMID()
	return mid, hex.EncodeToString(mid)
}

// wireMsgID is the msg_id a received message is recorded under: the id the
// sender put in the message body when it did (a chat message carries one),
// otherwise the envelope's inner id in hex (0017 Q9).
func wireMsgID(bodyID string, mid []byte) string {
	if bodyID != "" {
		return bodyID
	}
	if len(mid) == 0 {
		return ""
	}
	return hex.EncodeToString(mid)
}

// recipientKeys is §3.5 step 1.
func (d *Daemon) recipientKeys(ctx context.Context, toAID, pin string) (*peerKeySet, error) {
	now := d.nowMS()
	row, err := d.ix.PeerIdentity(toAID)
	if err != nil && !errors.Is(err, interactions.ErrNotFound) {
		return nil, fmt.Errorf("anet: read peer record for %s: %w", toAID, err)
	}
	if ks, ok := d.storedKeySet(row, now); ok {
		if _, serr := seal.SelectKey(ks.set, now); serr == nil {
			if now-uint64(row.KeysCheckedAt) > keysRevalidateMS {
				d.revalidateKeys(toAID, pin)
			}
			if pin != "" && row.PinnedReason == "" {
				if err := d.notePeer(toAID, nil, nil, pin, false); err != nil {
					log.Printf("anet: pin %s: %v", toAID, err)
				}
			}
			return ks, nil
		}
	}
	hub := d.config().HubURL
	if hub == "" {
		return nil, fmt.Errorf("anet: %s: %w and no hub to ask", toAID, errNoRecipientKeys)
	}
	cctx, cancel := context.WithTimeout(ctx, hubCallTimeoutShort)
	defer cancel()
	return d.fetchRecipientKeys(cctx, hub, toAID, pin)
}

// lookupKeys asks the hub for aid's key set with aid in the request body
// (POST /agents/keys:lookup), not in the path. The lookup is made from this
// node's address, unauthenticated, before the first message to a peer and
// every ten minutes of a conversation; a reverse proxy in front of the hub
// that logs request lines would otherwise keep "this address wrote to aid"
// on the hub host long after the message was deleted [redteam:F3].
//
// A hub that predates the route answers from its mux, a 405 (or a 404 from
// a proxy) without the hub's JSON error; only then is the old
// GET /agents/{aid}/keys asked. The route's own 404 ("no key set for aid")
// carries a JSON error and is returned as it is.
func (d *Daemon) lookupKeys(ctx context.Context, hub, aid string, out *hubapi.KeysResponse) error {
	err := d.hubPost(ctx, hub, hubapi.KeysLookupPath, hubapi.KeysLookupRequest{AID: aid}, out)
	var he *hubError
	if errors.As(err, &he) && he.msg == "" &&
		(he.code == http.StatusMethodNotAllowed || he.code == http.StatusNotFound) {
		return d.hubGet(ctx, hub, "/agents/"+url.PathEscape(aid)+"/keys", nil, out)
	}
	return err
}

// fetchRecipientKeys asks the hub for aid's key set and KEL and verifies
// them itself. The hub is not trusted with either: the KEL must replay to
// aid and extend any KEL stored for aid, and the key set must be signed by
// the current key of that KEL and name aid (seal.VerifyEncKeySet with
// expectAID = aid). A hub that serves another AID's valid key set in place
// of the recipient's is refused here.
func (d *Daemon) fetchRecipientKeys(ctx context.Context, hub, aid, pin string) (*peerKeySet, error) {
	var out hubapi.KeysResponse
	if err := d.lookupKeys(ctx, hub, aid, &out); err != nil {
		return nil, fmt.Errorf("anet: fetch %s's encryption keys: %w", aid, err)
	}
	rawSet, err1 := base64.StdEncoding.DecodeString(out.KeySet)
	rawKEL, err2 := base64.StdEncoding.DecodeString(out.KEL)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("anet: hub served unreadable keys for %s", aid)
	}
	signed, err := seal.UnmarshalSignedEncKeySet(rawSet)
	if err != nil {
		return nil, fmt.Errorf("anet: hub served an undecodable key set for %s: %w", aid, err)
	}
	kel, err := seal.ParseKEL(rawKEL)
	if err != nil {
		return nil, fmt.Errorf("anet: hub served an undecodable KEL for %s: %w", aid, err)
	}
	row, err := d.ix.PeerIdentity(aid)
	if err != nil && !errors.Is(err, interactions.ErrNotFound) {
		return nil, err
	}
	var stored []identity.SignedEvent
	if row != nil && len(row.KEL) > 0 {
		stored, _ = identity.UnmarshalKEL(row.KEL)
	}
	use, _, err := mergeKEL(stored, kel)
	if err != nil {
		d.count(dropKELFork)
		return nil, fmt.Errorf("anet: hub served a KEL for %s that forks from the one this node holds: %w", aid, err)
	}
	now := d.nowMS()
	set, err := seal.VerifyEncKeySet(signed, aid, use, now)
	if err != nil {
		return nil, fmt.Errorf("anet: hub served keys for %s that do not verify: %w", aid, err)
	}
	ks := &peerKeySet{signed: rawSet, set: set}
	switch seal.DecideHighWater(seenOf(row), set.Seq, signed.Set) {
	case seal.Ignore:
		d.count(keysRollback)
		return nil, fmt.Errorf("anet: hub served key set seq %d for %s, older than the stored one: %w", set.Seq, aid, errNoRecipientKeys)
	case seal.Fork:
		d.count(keysFork)
		return nil, fmt.Errorf("anet: hub served a different key set for %s at the stored seq %d", aid, set.Seq)
	}
	if err := d.notePeer(aid, use, ks, pin, true); err != nil {
		return nil, fmt.Errorf("anet: record %s's keys: %w", aid, err)
	}
	return ks, nil
}

// revalidating guards the background key revalidation per AID.
type revalidating struct {
	mu sync.Mutex
	m  map[string]bool
}

// revalidateKeys refreshes a stored key set from the hub in the background.
// Failures are not errors: the stored set stays in use until it holds no
// usable key.
func (d *Daemon) revalidateKeys(aid, pin string) {
	hub := d.config().HubURL
	if hub == "" {
		return
	}
	d.reval.mu.Lock()
	if d.reval.m == nil {
		d.reval.m = map[string]bool{}
	}
	if d.reval.m[aid] {
		d.reval.mu.Unlock()
		return
	}
	d.reval.m[aid] = true
	d.reval.mu.Unlock()
	started := d.goBackground(func() {
		defer func() {
			d.reval.mu.Lock()
			delete(d.reval.m, aid)
			d.reval.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(d.ctx, hubCallTimeoutShort)
		defer cancel()
		if _, err := d.fetchRecipientKeys(ctx, hub, aid, pin); err != nil && ctx.Err() == nil {
			d.logOnce("revalidate:"+aid, "anet: revalidating %s's keys: %v (using the stored set)", aid, err)
		}
	})
	if !started {
		d.reval.mu.Lock()
		delete(d.reval.m, aid)
		d.reval.mu.Unlock()
	}
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
