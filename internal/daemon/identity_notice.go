package daemon

// identity_notice.go is this node's own witness notice (KEL D-50): the
// statement, signed by the witness hub, that the KEL it holds for this node
// ends at the tip this node signs under. Every envelope that carries the KEL
// carries the notice (inner key 11), and the outbound gate stays closed
// while no fresh one confirms the tip (KEL §4.3.1 row 5).
//
// The hub hands it out with every /register and /relay/poll answer, or says
// why it cannot (key_state_notice_error). What comes in is checked here, in
// full, before the gate sees it: the gate itself compares only the AID, the
// seq, the head and the witness (identityGateInputs.noteNotice), so a notice
// that did not pass identity.VerifyNotice must never reach it. The latest
// one is kept in key_state_notice.cbor for the next start and for doctor.

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// keyStateNoticeName is the file of the latest notice, in the data
// directory (KEL §2.9).
const keyStateNoticeName = "key_state_notice.cbor"

// keyStateNoticePath is <data>/key_state_notice.cbor.
func (l Layout) keyStateNoticePath() string { return filepath.Join(l.Root, keyStateNoticeName) }

// maxKeyStateNoticeFile bounds the file: a notice of at most
// seal.MaxNoticeBytes and a time.
const maxKeyStateNoticeFile = seal.MaxNoticeBytes + 32

// keyStateNoticeFile is key_state_notice.cbor (core §2.6).
//
//anet:wire KeyStateNoticeFile
type keyStateNoticeFile struct {
	Notice  []byte `cbor:"1,keyasint"` // coredet(NoticeWire), as the hub sent it
	SavedAt uint64 `cbor:"2,keyasint"` // unix ms
}

// errKeyStateNoticeFile is a key_state_notice.cbor this binary does not
// read.
var errKeyStateNoticeFile = errors.New("anet: key_state_notice.cbor is not valid")

// encodeKeyStateNoticeFile is the content of key_state_notice.cbor.
func encodeKeyStateNoticeFile(wire []byte, savedAt uint64) ([]byte, error) {
	if len(wire) == 0 || len(wire) > seal.MaxNoticeBytes || savedAt == 0 {
		return nil, fmt.Errorf("%w: a notice of %d bytes saved at %d", errKeyStateNoticeFile, len(wire), savedAt)
	}
	return coredet.Marshal(keyStateNoticeFile{Notice: wire, SavedAt: savedAt})
}

// decodeKeyStateNoticeFile reads key_state_notice.cbor strictly (core
// CORE-E3): the two keys and no other, a notice of 1 to
// seal.MaxNoticeBytes bytes in NoticeWire form, a time, and the canonical
// encoding of all that. It returns the notice with its decoded body; what
// the notice states is checked by the caller (loadKeyStateNotice).
func decodeKeyStateNoticeFile(b []byte) (*keyStateNotice, uint64, error) {
	if len(b) > maxKeyStateNoticeFile {
		return nil, 0, fmt.Errorf("%w: %d bytes", errKeyStateNoticeFile, len(b))
	}
	var f keyStateNoticeFile
	if err := coredet.UnmarshalStrict(b, &f); err != nil {
		return nil, 0, fmt.Errorf("%w: %v", errKeyStateNoticeFile, err)
	}
	switch {
	case len(f.Notice) == 0 || len(f.Notice) > seal.MaxNoticeBytes:
		return nil, 0, fmt.Errorf("%w: a notice of %d bytes", errKeyStateNoticeFile, len(f.Notice))
	case f.SavedAt == 0:
		return nil, 0, fmt.Errorf("%w: no saved_at", errKeyStateNoticeFile)
	}
	if re, err := coredet.Marshal(f); err != nil || !bytes.Equal(re, b) {
		return nil, 0, fmt.Errorf("%w: not in its canonical encoding", errKeyStateNoticeFile)
	}
	body, _, err := identity.DecodeNotice(f.Notice)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", errKeyStateNoticeFile, err)
	}
	return &keyStateNotice{Wire: f.Notice, Body: body}, f.SavedAt, nil
}

// readKeyStateNoticeFile reads l's key_state_notice.cbor; nil and no error
// when there is none.
func readKeyStateNoticeFile(l Layout) (*keyStateNotice, error) {
	b, err := os.ReadFile(l.keyStateNoticePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	n, _, err := decodeKeyStateNoticeFile(b)
	return n, err
}

// hubKELs is the hub KELs this node holds, by AID, for checking its own
// notices (KEL C-51 step 5): the rows of peer_identity pinned as a hub's
// (pinHubIdentity writes them at every hub it signs a request for, and only
// an extension replaces one). It reads the table alone, never the network.
// KEL-11 keeps the same table in step with the hub's /hub/kels (KEL D-24).
func (d *Daemon) hubKELs(hubAID string) ([]identity.SignedEvent, bool) {
	if d.ix == nil {
		return nil, false
	}
	row, err := d.ix.PeerIdentity(hubAID)
	if err != nil || row.PinnedReason != interactions.PinHub {
		return nil, false
	}
	return d.peerKEL(hubAID)
}

// verifyOwnNotice checks wire as a notice about this node's accepted KEL,
// at arrival (KEL C-51): decoded strictly, about the accepted tip, from the
// witness the KEL names, signed under that hub's KEL as this node holds it,
// and fresh at arrival.
func (d *Daemon) verifyOwnNotice(wire []byte, arrival uint64) (*keyStateNotice, error) {
	if len(wire) == 0 || len(wire) > seal.MaxNoticeBytes {
		return nil, fmt.Errorf("a notice of %d bytes", len(wire))
	}
	if d.self == nil {
		return nil, errors.New("no identity")
	}
	if err := identity.VerifyNotice(wire, d.self.Accepted(), d.hubKELs, arrival); err != nil {
		return nil, err
	}
	body, _, err := identity.DecodeNotice(wire)
	if err != nil {
		return nil, err
	}
	return &keyStateNotice{Wire: append([]byte(nil), wire...), Body: body}, nil
}

// noticeErrKind names the step of KEL C-51 a notice failed, for logging
// each kind once.
func noticeErrKind(err error) string {
	for _, k := range []struct {
		err  error
		name string
	}{
		{identity.ErrNoticeInvalid, "invalid"}, {identity.ErrNoticeMissing, "missing"},
		{identity.ErrNoticeMismatch, "mismatch"}, {identity.ErrNoticeUnknownWitness, "unknown"},
		{identity.ErrNoticeStale, "stale"},
	} {
		if errors.Is(err, k.err) {
			return k.name
		}
	}
	return "other"
}

// The values of key_state_notice_error this node knows (KEL H-13).
var knownNoticeErrors = map[string]bool{noticeErrQuarantine: true, noticeErrNotAdmitted: true, noticeErrNotWitness: true}

// noteHubNotice takes what a /register or /relay/poll answer said of this
// node's key state (KEL D-50): key_state_notice (base64url of the notice's
// bytes, hub wire 3) or key_state_notice_error. A notice is recorded only
// once it passed identity.VerifyNotice for this node's accepted KEL, now; a
// newer one replaces the one held, and is written to key_state_notice.cbor.
// An answer with neither, a notice that does not pass, or an error this node
// does not know changes nothing.
func (d *Daemon) noteHubNotice(notice, noticeErr string) {
	switch {
	case notice != "":
		if len(notice) > base64.RawURLEncoding.EncodedLen(seal.MaxNoticeBytes) {
			// Longer than any notice can be: not decoded at all.
			d.logOnce("own-notice-form", "anet: the hub sent a witness notice of %d characters, longer than any notice", len(notice))
			return
		}
		wire, err := hubapi.DecodeB64(notice, -1)
		if err != nil {
			d.logOnce("own-notice-form", "anet: the hub sent a witness notice that is not base64url: %v", err)
			return
		}
		n, err := d.verifyOwnNotice(wire, d.nowMS())
		if err != nil {
			// Not recorded: the gate stays as it was, and the next answer
			// brings another. A notice that does not verify is the hub's
			// fault or an attack; either way it must not open the gate.
			d.logOnce("own-notice:"+noticeErrKind(err), "anet: the witness notice the hub sent is not kept: %v", err)
			return
		}
		if !n.confirms(identityFactsOf(d.self)) {
			// The tip moved on while this answer was checked: a notice
			// about the one before would only close the gate again.
			return
		}
		if d.idGate.noteNotice(n, "") {
			d.saveKeyStateNotice()
		}
	case knownNoticeErrors[noticeErr]:
		d.idGate.noteNotice(nil, noticeErr)
	}
}

// saveKeyStateNotice writes the notice held to key_state_notice.cbor. The
// writes are serialized and each writes the notice held at that moment, so
// the file never goes back to an older one.
func (d *Daemon) saveKeyStateNotice() {
	d.idGate.saveMu.Lock()
	defer d.idGate.saveMu.Unlock()
	n, _, _ := d.idGate.read()
	if n == nil {
		return
	}
	b, err := encodeKeyStateNoticeFile(n.Wire, d.nowMS())
	if err == nil {
		err = writeFileAtomic(d.layout.keyStateNoticePath(), b, 0o600)
	}
	if err != nil {
		log.Printf("anet: save %s: %v", keyStateNoticeName, err)
	}
}

// loadKeyStateNotice reads key_state_notice.cbor at start (KEL D-50). The
// file is read strictly and the notice in it is checked again, as of when
// the hub observed it: it must be about this node's accepted tip and signed
// by its witness under a key of that hub's KEL that was usable then. Its
// age is the gate's to judge, so an old notice is kept (the gate stays
// closed, and the state says how long the hub has not confirmed the key).
// Anything else is ignored, and the next hub answer brings a notice anyway.
func (d *Daemon) loadKeyStateNotice() {
	n, err := readKeyStateNoticeFile(d.layout)
	if err != nil {
		log.Printf("anet: %s is not used: %v", keyStateNoticeName, err)
		return
	}
	if n == nil {
		return
	}
	if now := d.nowMS(); n.Body.ObservedAt > now && n.Body.ObservedAt-now > identity.EventClockWindow {
		log.Printf("anet: %s is not used: it was observed in the future", keyStateNoticeName)
		return
	}
	checked, err := d.verifyOwnNotice(n.Wire, n.Body.ObservedAt)
	if err != nil {
		log.Printf("anet: %s is not used: %v", keyStateNoticeName, err)
		return
	}
	d.idGate.noteNotice(checked, "")
}

// StoredNotice is the notice key_state_notice.cbor in l holds, as it travels
// (NoticeWire bytes), when it states the tip of kel: what this node's
// envelopes carry in inner key 11 while it lasts. nil when the file is
// missing, unreadable, or about another key state. Its signature and age are
// not checked here (tools that seal as the node, anetfixture).
func StoredNotice(l Layout, kel []identity.SignedEvent) []byte {
	n, err := readKeyStateNoticeFile(l)
	if err != nil || n == nil || len(kel) == 0 {
		return nil
	}
	f := kelFacts(n.Body.AID, kel)
	if states, err := identity.Replay(kel); err != nil || states[len(states)-1].AID != n.Body.AID {
		return nil
	}
	if !n.confirms(f) {
		return nil
	}
	return n.Wire
}
