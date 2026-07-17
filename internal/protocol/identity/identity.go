// Package identity implements the P1 Identity & Key plane: AID + KEL (Key Event Log) +
// KeyState resolution, with KERI-style pre-rotation.
//
// Normative source: design3/spec arch-03 §5.1 (M5.1.1–M5.1.4). Core invariants:
//   - An AID (Autonomic Identifier) MUST NOT equal a key: AID = multihash(canonical
//     inception event); the current key evolves through a signed, pre-rotation-committed,
//     append-only KEL. The AID is stable across rotation.
//   - A verifier resolves a DID to a current key by REPLAYING the KEL, not by decoding
//     the DID string, producing KeyState(current_keys, threshold, key_state_seq, status).
//   - Pre-rotation: a stolen current key cannot rotate to an attacker key, because the
//     next key is pre-committed only as a digest (rot.keys MUST hash-match prior next_digest
//     and rot is signed by the PRIOR current key).
//   - legacy did:key is a single-event KEL (embedded key = icp.keys[0]).
//
// Baseline scope: single-key threshold (multi-sig / witnesses are fields here but the
// MTI logic is single-key); event types icp/rot/dip. Delegation (drt/dip-delegated) and
// witness receipts are a follow-up.
package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/ANetResearch/ANet/internal/protocol/anetcid"
	"github.com/ANetResearch/ANet/internal/protocol/coredet"
)

// EventType is a KEL event type (arch-03 §5.1 M5.1.1).
type EventType string

const (
	Inception    EventType = "icp"
	Rotation     EventType = "rot"
	Interaction  EventType = "ixn"
	Deactivation EventType = "dip"
	Delegation   EventType = "drt" // delegate a device/host key to this AID (arch-03 M5.1.5; anrp VAL-11 mode b)
)

// Status values for a resolved KeyState (arch-03 §5.1 / §4 KeyState).
//   - StatusActive: this key-state is the live tip, or (for an intermediate state) was the
//     tip at its seq and has not yet been retired by a later event.
//   - StatusRotated: a LATER rot/dip event supersedes this key-state's key; objects
//     time-stamped at/after that later event's Timestamp are REVOKED_KEY (arch-03 §5.1
//     M5.1.3), but objects from before it stay valid (grace).
//   - StatusDeactivated: the AID's terminal state (dip event).
const (
	StatusActive      = "active"
	StatusRotated     = "rotated"
	StatusDeactivated = "deactivated"
)

// KeyEvent is one KEL entry. The CBOR int-key map (keyasint) is the Signable preimage;
// the detached signature is carried beside it in SignedEvent, never inside the preimage.
type KeyEvent struct {
	AID        string    `cbor:"1,keyasint,omitempty"` // empty in the icp preimage that derives the AID
	Seq        uint64    `cbor:"2,keyasint"`
	Prev       string    `cbor:"3,keyasint,omitempty"` // prev event_id (CID); empty for icp
	Type       EventType `cbor:"4,keyasint"`
	Keys       [][]byte  `cbor:"5,keyasint"`           // current public keys (Ed25519 raw, 32B)
	NextDigest []byte    `cbor:"6,keyasint,omitempty"` // sha256(next public key) — pre-rotation commitment
	Threshold  uint32    `cbor:"7,keyasint"`
	Timestamp  uint64    `cbor:"8,keyasint,omitempty"` // event time (unix-millis); 0 = unknown (arch-03 §5.1 M5.1.3)
}

// SignedEvent is a KEL entry with its detached signature and computed event id.
type SignedEvent struct {
	Event   KeyEvent `cbor:"1,keyasint"`
	Sig     []byte   `cbor:"2,keyasint"` // Ed25519 over the event's CoreDet-CBOR preimage, by the controlling key
	EventID string   `cbor:"3,keyasint"` // CID of the preimage
}

// MarshalKEL / UnmarshalKEL serialize a KEL (the ordered slice of signed key events) to
// deterministic CoreDet-CBOR bytes so it can be persisted or carried on the wire. The wrapper fields
// are tagged keyasint; the per-event CID/signature preimages come from KeyEvent.Signable and are
// unaffected, so a round-tripped KEL re-derives identical AIDs/key-states (verified by Replay).
func MarshalKEL(kel []SignedEvent) ([]byte, error) { return coredet.Marshal(kel) }

// UnmarshalKEL is the inverse of MarshalKEL.
func UnmarshalKEL(b []byte) ([]SignedEvent, error) {
	var kel []SignedEvent
	if err := coredet.Unmarshal(b, &kel); err != nil {
		return nil, err
	}
	return kel, nil
}

// controllerExport is the on-disk form of a Controller: the two ed25519 private-key SEEDS (the
// current signing key + the pre-committed next key) and the public KEL — the minimum to reconstruct
// a fully-functional Controller (Sign + Rotate). It contains PRIVATE KEYS: store 0600, never publish.
//
// Pre-rotation tradeoff: persisting the NEXT seed alongside the current one means a reader of this
// blob obtains both the current key AND its committed successor — so for THIS stored controller the
// pre-rotation guarantee (a stolen current key can't rotate to an attacker key) does not hold. This
// is inherent to persisting a rotatable controller; if the threat model warrants, encrypt the blob at
// rest (the package's SealTo/Open is available).
type controllerExport struct {
	CurSeed []byte        `cbor:"1,keyasint"`
	NxtSeed []byte        `cbor:"2,keyasint"`
	KEL     []SignedEvent `cbor:"3,keyasint"`
}

// Export serializes the Controller (private-key seeds + KEL) to CoreDet-CBOR for durable storage so a
// daemon can reload its identity across restarts. The bytes contain PRIVATE KEYS.
func (c *Controller) Export() ([]byte, error) {
	return coredet.Marshal(controllerExport{CurSeed: c.cur.Seed(), NxtSeed: c.nxt.Seed(), KEL: c.kel})
}

// Restore reconstructs a Controller from Export bytes: it re-derives the AID from the inception
// preimage, replays+verifies the KEL, and checks the stored current private key matches the current
// key state — so a corrupt/torn file is rejected rather than yielding a mis-signing controller.
func Restore(b []byte) (*Controller, error) {
	var e controllerExport
	if err := coredet.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	if len(e.CurSeed) != ed25519.SeedSize || len(e.NxtSeed) != ed25519.SeedSize {
		return nil, errors.New("identity: bad private-key seed length")
	}
	if len(e.KEL) == 0 {
		return nil, errors.New("identity: empty KEL")
	}
	states, err := Replay(e.KEL)
	if err != nil {
		return nil, fmt.Errorf("identity: restore replay: %w", err)
	}
	// AID = CID of the inception preimage — re-derived, not trusted from the file.
	pre, err := preimage(e.KEL[0].Event)
	if err != nil {
		return nil, err
	}
	aid, err := anetcid.Sum(pre)
	if err != nil {
		return nil, err
	}
	if aid != e.KEL[0].EventID {
		return nil, errors.New("identity: inception EventID does not match its preimage")
	}
	cur := ed25519.NewKeyFromSeed(e.CurSeed)
	last := states[len(states)-1]
	// A deactivated AID must not be reconstituted as a signer (it has no live key state, and its dip
	// head carries no pre-rotation commitment to validate nxt against).
	if last.Status != StatusActive {
		return nil, fmt.Errorf("identity: cannot restore a non-active identity (status %q)", last.Status)
	}
	if len(last.CurrentKeys) != 1 || !bytes.Equal(last.CurrentKeys[0], cur.Public().(ed25519.PublicKey)) {
		return nil, errors.New("identity: current key does not match the KEL head (corrupt store)")
	}
	// The next (pre-rotation) seed MUST match the digest the KEL head committed to, else the first
	// Rotate would emit an event the KEL's own pre-rotation gate rejects — silently bricking the
	// identity. Validate it here so a corrupt/torn NxtSeed is rejected at restore, not at rotation.
	nxt := ed25519.NewKeyFromSeed(e.NxtSeed)
	head := e.KEL[len(e.KEL)-1].Event
	if !bytes.Equal(nextDigest(nxt.Public().(ed25519.PublicKey)), head.NextDigest) {
		return nil, errors.New("identity: next key does not match the KEL head's pre-rotation commitment (corrupt store)")
	}
	return &Controller{
		aid: aid,
		cur: cur,
		nxt: nxt,
		kel: e.KEL,
	}, nil
}

func nextDigest(pub ed25519.PublicKey) []byte {
	h := sha256.Sum256(pub)
	return h[:]
}

// preimage returns the CoreDet-CBOR Signable of an event (excludes signature).
func preimage(e KeyEvent) ([]byte, error) { return coredet.Marshal(e) }

// Controller holds the private key material and the KEL it has produced. It is the
// signing side; verifiers use only the public KEL ([]SignedEvent) via Replay.
type Controller struct {
	aid string
	cur ed25519.PrivateKey
	nxt ed25519.PrivateKey
	kel []SignedEvent
}

// Incept creates a new identity: generates the current key K0 and the pre-committed next
// key K1, builds the icp event (AID = CID of its preimage), and self-signs it with K0.
func Incept() (*Controller, error) {
	_, k0, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	_, k1, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	icp := KeyEvent{
		Seq:        0,
		Type:       Inception,
		Keys:       [][]byte{k0.Public().(ed25519.PublicKey)},
		NextDigest: nextDigest(k1.Public().(ed25519.PublicKey)),
		Threshold:  1,
	}
	pre, err := preimage(icp) // AID-deriving preimage has empty AID field
	if err != nil {
		return nil, err
	}
	aid, err := anetcid.Sum(pre)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(k0, pre)
	c := &Controller{aid: aid, cur: k0, nxt: k1}
	c.kel = append(c.kel, SignedEvent{Event: icp, Sig: sig, EventID: aid})
	return c, nil
}

// AID returns the stable identifier (CID of the inception event).
func (c *Controller) AID() string { return c.aid }

// DID returns the did:anet form of the AID.
func (c *Controller) DID() string { return "did:anet:" + c.aid }

// KEL returns the public key event log (safe to publish).
func (c *Controller) KEL() []SignedEvent { return c.kel }

// CurrentSeq is the key_state_seq the controller signs objects under.
func (c *Controller) CurrentSeq() uint64 { return c.kel[len(c.kel)-1].Event.Seq }

// CurrentPrivateKey returns the controller's current signing private key. A caller MAY use it as a
// libp2p host key so the transport peer_id derives from the owner's current KEL key (anrp VAL-11 mode
// (a) name→endpoint binding). CAUTION: this couples the transport identity to the anet signing key —
// a key rotation then changes the peer_id, and the key is reused across the anet-object and transport
// (noise) protocols. The caller owns that tradeoff (it is the price of VAL-11 mode (a)).
func (c *Controller) CurrentPrivateKey() ed25519.PrivateKey { return c.cur }

// Sign signs an object preimage with the current key, returning the signature and the
// key_state_seq a verifier MUST select (anti-downgrade, arch-03 §5.1 M5.1.2).
func (c *Controller) Sign(objPreimage []byte) (sig []byte, keyStateSeq uint64) {
	return ed25519.Sign(c.cur, objPreimage), c.CurrentSeq()
}

// Rotate advances the KEL: the pre-committed next key K(n) becomes current, a fresh next
// key K(n+1) is pre-committed, and the rot event is signed by the PRIOR current key. ts is
// the event time in unix-millis (0 = unknown); it stamps the rot event so the revocation
// gate can apply the grace window (arch-03 §5.1 M5.1.3).
// Delegate appends a drt event delegating hostPub (a device/host key, e.g. a libp2p transport key) to
// this AID, signed by the current key. The delegated key is recorded in the key-state's DelegatedKeys
// (it does not become a signing key) — it authenticates a delegated endpoint for anrp VAL-11 mode (b).
func (c *Controller) Delegate(hostPub ed25519.PublicKey, ts uint64) error {
	if len(hostPub) != ed25519.PublicKeySize {
		return errors.New("identity: delegated key must be a 32-byte ed25519 public key")
	}
	last := c.kel[len(c.kel)-1]
	if last.Event.Type == Deactivation {
		return errors.New("identity: cannot delegate from a deactivated AID")
	}
	drt := KeyEvent{
		AID:  c.aid,
		Seq:  last.Event.Seq + 1,
		Prev: last.EventID,
		Type: Delegation,
		Keys: [][]byte{append([]byte(nil), hostPub...)},
		// Carry the pre-rotation commitment forward: a delegation does NOT change the signing keys, so it
		// re-commits the SAME next key. The KEL head must always advertise the live commitment, else
		// Restore (which validates head.NextDigest) and a later Rotate (whose pre-rotation gate reads the
		// IMMEDIATELY-prior event's next_digest) would both reject a post-delegation KEL.
		NextDigest: nextDigest(c.nxt.Public().(ed25519.PublicKey)),
		Threshold:  1,
		Timestamp:  ts,
	}
	pre, err := preimage(drt)
	if err != nil {
		return err
	}
	id, err := anetcid.Sum(pre)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(c.cur, pre) // signed by the current key
	c.kel = append(c.kel, SignedEvent{Event: drt, Sig: sig, EventID: id})
	return nil
}

func (c *Controller) Rotate(ts uint64) error {
	last := c.kel[len(c.kel)-1]
	if last.Event.Type == Deactivation {
		return errors.New("identity: cannot rotate a deactivated AID")
	}
	prior := c.cur
	newCur := c.nxt
	_, newNxt, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	rot := KeyEvent{
		AID:        c.aid,
		Seq:        last.Event.Seq + 1,
		Prev:       last.EventID,
		Type:       Rotation,
		Keys:       [][]byte{newCur.Public().(ed25519.PublicKey)},
		NextDigest: nextDigest(newNxt.Public().(ed25519.PublicKey)),
		Threshold:  1,
		Timestamp:  ts,
	}
	pre, err := preimage(rot)
	if err != nil {
		return err
	}
	id, err := anetcid.Sum(pre)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(prior, pre) // signed by PRIOR current key (M5.1.1)
	c.kel = append(c.kel, SignedEvent{Event: rot, Sig: sig, EventID: id})
	c.cur, c.nxt = newCur, newNxt
	return nil
}

// Deactivate appends a dip event (signed by the current key), terminating the AID. ts is
// the event time in unix-millis (0 = unknown); it stamps the dip event so the revocation
// gate can apply the grace window (arch-03 §5.1 M5.1.3).
func (c *Controller) Deactivate(ts uint64) error {
	last := c.kel[len(c.kel)-1]
	if last.Event.Type == Deactivation {
		return errors.New("identity: already deactivated")
	}
	dip := KeyEvent{
		AID:       c.aid,
		Seq:       last.Event.Seq + 1,
		Prev:      last.EventID,
		Type:      Deactivation,
		Keys:      [][]byte{c.cur.Public().(ed25519.PublicKey)},
		Threshold: 1,
		Timestamp: ts,
	}
	pre, err := preimage(dip)
	if err != nil {
		return err
	}
	id, err := anetcid.Sum(pre)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(c.cur, pre)
	c.kel = append(c.kel, SignedEvent{Event: dip, Sig: sig, EventID: id})
	return nil
}

// KeyState is the verifier-derived trust state (arch-03 §4 KeyState).
type KeyState struct {
	AID         string
	CurrentKeys [][]byte
	Threshold   uint32
	KeyStateSeq uint64
	Status      string
	LastEventID string
	// DelegatedKeys are device/host keys this AID has delegated via drt events (arch-03 M5.1.5). They
	// do NOT sign anet objects (the owner's CurrentKeys do) — they authenticate a delegated endpoint
	// (anrp VAL-11 mode b: a NameRecord's host_key_sig must be by a key in this set).
	DelegatedKeys [][]byte
	// SupersededAt is the Timestamp (unix-millis) of the LATER rot/dip event that retired
	// this key-state, or 0 if this state is still the tip (or the superseding event carried
	// no timestamp). Used by the revocation gate's grace window (arch-03 §5.1 M5.1.3).
	SupersededAt uint64
}

// Replay validates a KEL and returns the cumulative KeyState after each event (index i =
// state as-of seq i). It enforces: icp first; monotonic seq; prev linkage; pre-rotation
// hash match; and the signing rule (icp/dip self-signed by current; rot by prior current).
func Replay(kel []SignedEvent) ([]KeyState, error) {
	if len(kel) == 0 {
		return nil, errors.New("identity: empty KEL")
	}
	states := make([]KeyState, 0, len(kel))
	var prior KeyState
	for i, se := range kel {
		e := se.Event
		pre, err := preimage(e)
		if err != nil {
			return nil, err
		}
		switch {
		case i == 0:
			if e.Type != Inception || e.Seq != 0 || e.Prev != "" {
				return nil, errors.New("identity: first event must be icp seq 0 no-prev")
			}
			aid, err := anetcid.Sum(pre)
			if err != nil {
				return nil, err
			}
			if e.AID != "" && e.AID != aid {
				return nil, errors.New("identity: icp AID mismatch")
			}
			if len(e.Keys) != 1 || !ed25519.Verify(e.Keys[0], pre, se.Sig) {
				return nil, errors.New("identity: icp not self-signed by its key")
			}
			prior = KeyState{AID: aid, CurrentKeys: e.Keys, Threshold: e.Threshold,
				KeyStateSeq: 0, Status: StatusActive, LastEventID: aid}
		default:
			if e.AID != prior.AID {
				return nil, errors.New("identity: event AID mismatch")
			}
			if e.Seq != prior.KeyStateSeq+1 {
				return nil, fmt.Errorf("identity: non-monotonic seq %d after %d", e.Seq, prior.KeyStateSeq)
			}
			if e.Prev != prior.LastEventID {
				return nil, errors.New("identity: broken prev linkage")
			}
			if prior.Status != StatusActive {
				return nil, errors.New("identity: event after terminal state")
			}
			id, err := anetcid.Sum(pre)
			if err != nil {
				return nil, err
			}
			switch e.Type {
			case Rotation:
				if len(e.Keys) != 1 {
					return nil, errors.New("identity: rot needs one key (baseline)")
				}
				// pre-rotation: revealed key MUST hash-match prior next_digest
				if !bytesEqual(nextDigest(e.Keys[0]), priorNextDigest(kel, i)) {
					return nil, errors.New("identity: rot key does not match pre-committed digest")
				}
				// rot signed by the PRIOR current key
				if !ed25519.Verify(prior.CurrentKeys[0], pre, se.Sig) {
					return nil, errors.New("identity: rot not signed by prior current key")
				}
				prior = KeyState{AID: prior.AID, CurrentKeys: e.Keys, Threshold: e.Threshold,
					KeyStateSeq: e.Seq, Status: StatusActive, LastEventID: id, DelegatedKeys: prior.DelegatedKeys}
			case Deactivation:
				if !ed25519.Verify(prior.CurrentKeys[0], pre, se.Sig) {
					return nil, errors.New("identity: dip not signed by current key")
				}
				prior = KeyState{AID: prior.AID, CurrentKeys: prior.CurrentKeys, Threshold: prior.Threshold,
					KeyStateSeq: e.Seq, Status: StatusDeactivated, LastEventID: id, DelegatedKeys: prior.DelegatedKeys}
			case Interaction:
				if !ed25519.Verify(prior.CurrentKeys[0], pre, se.Sig) {
					return nil, errors.New("identity: ixn not signed by current key")
				}
				prior.KeyStateSeq = e.Seq
				prior.LastEventID = id
			case Delegation:
				// drt commits a delegated device/host key (does NOT change the owner's signing keys),
				// signed by the owner's current key (arch-03 M5.1.5).
				if len(e.Keys) != 1 {
					return nil, errors.New("identity: drt needs one delegated key")
				}
				if !ed25519.Verify(prior.CurrentKeys[0], pre, se.Sig) {
					return nil, errors.New("identity: drt not signed by current key")
				}
				prior.KeyStateSeq = e.Seq
				prior.LastEventID = id
				prior.DelegatedKeys = append(append([][]byte(nil), prior.DelegatedKeys...), e.Keys[0])
			default:
				return nil, fmt.Errorf("identity: unsupported event type %q", e.Type)
			}
		}
		states = append(states, prior)
	}
	// Post-pass (arch-03 §5.1 M5.1.3): mark each key-state that a LATER rot/dip supersedes.
	// State i is superseded by event i+1 when that event rotates the key (rot) or terminates
	// the AID (dip) — in both cases the key current at seq i is retired by event i+1. The
	// superseding event's Timestamp seeds the grace window in VerifyObject. (An ixn does not
	// change the key, so it does not supersede.) The terminal dip's own state keeps
	// StatusDeactivated; intermediate retired states become StatusRotated.
	for i := 0; i+1 < len(states); i++ {
		next := kel[i+1].Event
		if next.Type == Rotation || next.Type == Deactivation {
			if states[i].Status == StatusActive {
				states[i].Status = StatusRotated
			}
			states[i].SupersededAt = next.Timestamp
		}
	}
	return states, nil
}

// priorNextDigest returns the next_digest committed by the event before index i.
func priorNextDigest(kel []SignedEvent, i int) []byte { return kel[i-1].Event.NextDigest }

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// VerifyObject verifies a signed object against a KEL (arch-03 §5.1 M5.1.2 + M5.1.3):
// resolve KEL → select the key at the declared key_state_seq (anti-downgrade) → verify
// signature → revocation gate. Returns a typed-reason error on failure.
//
// msgTime is the object's binding time in unix-millis (e.g. an ALP datagram's msg_time).
// The revocation gate is time-aware (M5.1.3): a key valid at signing time stays valid for
// objects time-stamped BEFORE its retirement (grace), and is REVOKED_KEY for objects
// time-stamped at/after the LATER rot/dip event that superseded it. When msgTime==0 the
// time is unknown, so the gate falls back to the conservative rule: reject only an object
// claiming the terminal (deactivated) key-state seq.
func VerifyObject(kel []SignedEvent, signerAID string, keyStateSeq, msgTime uint64, objPreimage, sig []byte) error {
	states, err := Replay(kel)
	if err != nil {
		return &VErr{"UNKNOWN_AID", err.Error()}
	}
	final := states[len(states)-1]
	if final.AID != signerAID {
		return &VErr{"UNKNOWN_AID", "KEL AID does not match signer_aid"}
	}
	if keyStateSeq >= uint64(len(states)) {
		return &VErr{"KEY_STATE_DOWNGRADE", "declared key_state_seq beyond KEL"}
	}
	ks := states[keyStateSeq]
	if len(ks.CurrentKeys) == 0 {
		return &VErr{"UNKNOWN_AID", "no current key at declared key_state_seq"}
	}
	if !ed25519.Verify(ks.CurrentKeys[0], objPreimage, sig) {
		return &VErr{"INVALID_SIGNATURE", "signature does not verify under key at key_state_seq"}
	}
	// Revocation gate (arch-03 §5.1 M5.1.3).
	if msgTime == 0 {
		// Unknown time: conservative fallback — reject only an object claiming the terminal
		// deactivated key-state seq.
		if final.Status == StatusDeactivated && keyStateSeq == final.KeyStateSeq {
			return &VErr{"REVOKED_KEY", "AID deactivated at this key_state_seq"}
		}
		return nil
	}
	// Time-aware: a key-state retired by a later rot/dip (StatusRotated/StatusDeactivated) is honored
	// ONLY for objects bound BEFORE the retirement.
	//   - retirement carried a timestamp (SupersededAt != 0): honor iff msgTime < SupersededAt.
	//   - retirement carried NO timestamp (SupersededAt == 0): we cannot establish the object predates
	//     it, so REJECT conservatively — otherwise a compromised rotated-away key could sign new objects
	//     with a future msg_time and slip past the gate, defeating the point of rotation. (This subsumes
	//     the terminal-deactivated rule.)
	if ks.Status != StatusActive {
		if ks.SupersededAt == 0 || msgTime >= ks.SupersededAt {
			return &VErr{"REVOKED_KEY", "key_state_seq retired by a later rotation/deactivation (no grace window establishes the object predates it)"}
		}
	}
	return nil
}

// VErr is a typed verification error (arch-03 §5.1 M5.1.4 #6).
type VErr struct {
	Reason string
	Detail string
}

func (e *VErr) Error() string { return e.Reason + ": " + e.Detail }
