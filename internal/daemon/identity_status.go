package daemon

// identity_status.go is what the node says about its identity (KEL §2.8,
// §4.3.1, D-3, D-15): /identity/status, the identity object of /status
// (which MCP node_status passes on: the same shape, KEL M-2), the identity
// and recovery lines of the status summary (ux §4.7), and the refusal a
// send gets while the identity is stopped or deactivated. Every sentence is
// a reason-table sentence chosen here once, so the CLI, MCP and the local
// A2A interface say the same words (SI-79).
//
// ReadIdentityStatus says the same from the data directory alone, for
// doctor and `anet identity status` while the daemon is not running.

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// The modes of last_rotation (KEL §2.8, §2.10).
const (
	rotationWitness   = "witness"
	rotationScheduled = "scheduled"
	rotationEmergency = "emergency"
)

// IdentityStatus is /identity/status (KEL §2.8) and the identity object of
// /status and MCP node_status (KEL M-2). The control plane's binary values
// are standard base64 (core CORE-E5), which encoding/json writes for
// Commitment.
type IdentityStatus struct {
	AID  string `json:"aid"`
	Name string `json:"name"`
	// State is the row of KEL §4.3.1 (ux UX-R5); Reason its sentence key,
	// Message the sentence and Next its command.
	State   string `json:"state"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Next    string `json:"next,omitempty"`
	// KeySeq is the seq of the latest event in the identity file, the
	// pending one included, and Commitment its key 6 (null for a dip):
	// `anet identity rotate` derives key_seq + 1 from the recovery seed and
	// checks it against Commitment before it asks the daemon anything (KEL
	// L-3).
	KeySeq     uint64 `json:"key_seq"`
	Commitment []byte `json:"commitment"`
	// LastRotation is the latest rot the witness accepted; nil before the
	// first.
	LastRotation *IdentityRotation `json:"last_rotation"`
	// Witness is the witness hub; nil while there is none.
	Witness *IdentityWitness `json:"witness"`
	// Notice is the notice that confirms the tip now; nil when none does.
	Notice *IdentityNotice `json:"notice"`
	// Pending is the key event being published; nil when none is.
	Pending *IdentityPending `json:"pending"`
	// OldWitnessUnacked are the former witnesses that have not confirmed
	// the hub switch yet (KEL D-33): never null.
	OldWitnessUnacked []IdentityOldWitness `json:"old_witness_unacked"`
	// Recovery is whether the identity has a recovery passphrase (KEL
	// D-15): with the reminder while it has none.
	Recovery recoveryView `json:"recovery"`

	// said is the state's sentence; rec the recovery attribute.
	said a2ashape.Said
	rec  RecoveryState
}

// IdentityRotation is last_rotation: when (RFC 3339) and how.
type IdentityRotation struct {
	At   string `json:"at"`
	Mode string `json:"mode"`
}

// IdentityWitness is the witness hub: its URL as this node reaches it, and
// its AID.
type IdentityWitness struct {
	URL string `json:"url"`
	AID string `json:"aid"`
}

// IdentityNotice is the notice that confirms the tip: when the hub observed
// it (RFC 3339) and how old it is, in seconds.
type IdentityNotice struct {
	ObservedAt string `json:"observed_at"`
	AgeS       uint64 `json:"age_s"`
}

// IdentityPending is the key event being published: its kind (witness, rot,
// dip) and since when (RFC 3339).
type IdentityPending struct {
	Kind  string `json:"kind"`
	Since string `json:"since"`
}

// IdentityOldWitness is a former witness the KEL is still pushed to.
type IdentityOldWitness struct {
	URL     string `json:"url"`
	Since   string `json:"since"`
	NextTry string `json:"next_try"`
}

// RecoveryReminder is the sentence key of the recovery reminder the status
// carries ("" when none): identity.unprotected or its .resume variant. It
// is what doctor's identity.recovery says (ux §5.2).
func (s IdentityStatus) RecoveryReminder() string { return s.rec.reminderKey(s.State) }

// FillSummary sets the identity and recovery lines of the status summary
// (ux §4.7): the state, its sentence, and whether the identity has a
// recovery passphrase. a2ashape.StatusSummary decides which lines show.
func (s IdentityStatus) FillSummary(in *a2ashape.StatusInput) {
	in.IdentityState = s.State
	said := s.said
	in.Identity = &said
	in.Recovery, in.RecoveryResume = s.rec.State, s.rec.Resume
}

// identityName is the name of the identity this data directory holds, as
// `anet id` calls it ({name} in the identity sentences).
func (d *Daemon) identityName() string {
	return IdentityNameForDir(d.layout.Root)
}

// identitySaid is the sentence of state st: its key and arguments.
func (d *Daemon) identitySaid(st IdentityState, in identityInputs, lang a2ashape.Language, now time.Time) a2ashape.Said {
	if st.State == IdentityNoHub {
		// No witness yet: the join's sentence says it (ux UX-C9).
		v := d.viewOfJoin(d.joinSnapshot(), now, lang)
		return a2ashape.Said{Key: v.key, Args: v.args}
	}
	args := a2ashape.Args{"now": now}
	switch st.Reason {
	case reasonIdentityOK:
		args["name"], args["aid"], args["hub"] = d.identityName(), d.AID(), d.witnessURL(in.File.Witness)
		args["ago"] = time.Duration(noticeAge(in.Notice, in.Now)) * time.Millisecond
	case reasonConfirmAging, reasonNoticeStale:
		args["dur"] = time.Duration(unconfirmedFor(in)) * time.Millisecond
	case reasonPending:
		args["hub"], args["aid"] = d.witnessURL(in.File.Witness), d.AID()
	case reasonStopped:
		if n := d.identityName(); n != "" {
			args["name"] = n
		}
	}
	return a2ashape.Said{Key: st.Reason, Args: args}
}

// unconfirmedFor is how long, in ms, the hub has not confirmed the key this
// node signs under: since the notice that confirms the tip, or, without
// one, since the tip's own event (the key state began unconfirmed).
func unconfirmedFor(in identityInputs) uint64 {
	since := in.File.TipTS
	if in.Notice.confirms(in.File) {
		since = in.Notice.Body.ObservedAt
	}
	if in.Now <= since {
		return 0
	}
	return in.Now - since
}

// witnessURL is how this node reaches the witness hub of AID witness: the
// join record's hub when it is that hub, else the configured hub, else the
// AID itself.
func (d *Daemon) witnessURL(witness string) string {
	if r := d.joinSnapshot(); r != nil && r.HubAID == witness && r.Hub != "" {
		return r.Hub
	}
	if u := d.config().HubURL; u != "" {
		return u
	}
	return witness
}

// identityStatus is /identity/status now, in lang.
func (d *Daemon) identityStatus(lang a2ashape.Language) IdentityStatus {
	in := d.identityInputs()
	now := time.UnixMilli(int64(in.Now))
	st := identityStateOf(in)
	said := d.identitySaid(st, in, lang, now)
	rec := d.recoveryState()
	if st.State == IdentityOK && d.idOps.hubRecordInvalid.Load() {
		// The witness's record of this identity did not verify at the last
		// check (KEL D-27 row a): the identity is not affected, the hub is
		// suspect, and that is what the state says until a check finds
		// otherwise.
		st.Reason = reasonHubRecordInvalid
		said = a2ashape.Said{Key: reasonHubRecordInvalid, Args: a2ashape.Args{"hub": d.witnessURL(in.File.Witness)}}
	}
	s := IdentityStatus{State: st.State, Reason: said.Key, Message: a2ashape.Sentence(said.Key, lang, said.Args),
		Next: a2ashape.Next(said.Key, lang, said.Args), OldWitnessUnacked: d.oldWitnessUnacked(),
		Recovery: rec.view(st.State, lang), said: said, rec: rec}
	if d.self == nil {
		return s
	}
	s.AID, s.Name = d.AID(), d.identityName()
	if kel := d.self.KEL(); len(kel) > 0 {
		tip := kel[len(kel)-1].Event
		s.KeySeq, s.Commitment = tip.Seq, tip.NextDigest
	}
	s.LastRotation = d.lastRotationOf(d.self.Accepted())
	if w := in.File.Witness; w != "" {
		s.Witness = &IdentityWitness{URL: d.witnessURL(w), AID: w}
	}
	if in.Notice.confirms(in.File) {
		s.Notice = &IdentityNotice{ObservedAt: rfc3339(in.Notice.Body.ObservedAt), AgeS: noticeAge(in.Notice, in.Now) / 1000}
	}
	if p, ok := d.self.Pending(); ok {
		s.Pending = &IdentityPending{Kind: p.Kind, Since: rfc3339(p.Since)}
	}
	return s
}

// IdentityStatus is /identity/status in this process's language.
func (d *Daemon) IdentityStatus() IdentityStatus { return d.identityStatus(a2ashape.Lang()) }

// lastRotation is the latest rot of an accepted KEL: when, and how it was
// made — the witness designation (an agent's first rot), an emergency rot,
// or a planned one. nil before the first. A recover's rot reads as
// emergency here: only this node's evidence says recover
// (lastRotationOf).
func lastRotation(kel []identity.SignedEvent) *IdentityRotation {
	for i := len(kel) - 1; i > 0; i-- {
		e := kel[i].Event
		if e.Type != identity.Rotation {
			continue
		}
		mode := rotationScheduled
		switch {
		case e.Emergency:
			mode = rotationEmergency
		case i == 1 && e.Toad != nil && *e.Toad == identity.ToadAgent:
			mode = rotationWitness
		}
		return &IdentityRotation{At: rfc3339(e.Timestamp), Mode: mode}
	}
	return nil
}

// lastRotationOf is lastRotation, with the mode this node's evidence chain
// records for that rot when it records one (a recover's, KEL §2.10).
func (d *Daemon) lastRotationOf(kel []identity.SignedEvent) *IdentityRotation {
	r := lastRotation(kel)
	if r == nil {
		return nil
	}
	for i := len(kel) - 1; i > 0; i-- {
		if kel[i].Event.Type == identity.Rotation {
			if mode := d.rotationEvidence(kel[i].Event.Seq); mode != "" {
				r.Mode = mode
			}
			break
		}
	}
	return r
}

// reasonHubRecordInvalid is the state sentence of a witness record that
// does not verify (KEL D-27 row a; ux §1.4 E3).
const reasonHubRecordInvalid = "identity.hub_record_invalid"

// rfc3339 is a unix ms time as the control plane writes times.
func rfc3339(ms uint64) string {
	return time.UnixMilli(int64(ms)).UTC().Format(time.RFC3339)
}

// hIdentityStatus is POST /identity/status (KEL §2.8): read only, {} in.
func (d *Daemon) hIdentityStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, d.IdentityStatus())
}

// identitySendRefusal is the refusal of a send while the identity is
// stopped or deactivated (KEL D-3): for good, so the send fails at once and
// nothing is queued. nil otherwise.
func (d *Daemon) identitySendRefusal() *a2ashape.Refusal {
	switch st := d.identityState(); st.State {
	case IdentityStopped:
		var args a2ashape.Args
		if n := d.identityName(); n != "" {
			args = a2ashape.Args{"name": n}
		}
		return a2ashape.Refuse(reasonStopped, args)
	case IdentityDeactivated:
		return a2ashape.Refuse(reasonDeactivated, nil)
	}
	return nil
}

// ErrNoIdentity is a data directory without an identity file.
var ErrNoIdentity = errors.New("anet: this data directory has no identity yet")

// ReadIdentityStatus is /identity/status as the data directory says it,
// without the daemon (KEL L-7; doctor): the identity file, the latest
// notice (key_state_notice.cbor) and the join record, read and never
// written. What only the running daemon knows — the hub's last
// key_state_notice_error, a refusal of the pending event — is not in it,
// so a hub's quarantine reads as notice_stale here.
func ReadIdentityStatus(l Layout, lang a2ashape.Language) (IdentityStatus, error) {
	b, err := os.ReadFile(l.IdentityPath())
	if errors.Is(err, os.ErrNotExist) {
		return IdentityStatus{}, ErrNoIdentity
	}
	if err != nil {
		return IdentityStatus{}, err
	}
	if identityFileIsV02(b) || identityFileTagIsOlder(b) {
		return IdentityStatus{}, oldDataDirRefusal()
	}
	if identityFileIsNewer(b) {
		return IdentityStatus{}, newerDataDirRefusal()
	}
	c, err := identity.Restore(b)
	if err != nil {
		return IdentityStatus{}, invalidIdentityFileRefusal(l.IdentityPath())
	}
	d := &Daemon{layout: l, self: c, started: time.Now()}
	if n, err := readKeyStateNoticeFile(l); err == nil && n != nil {
		// The notice is a file the hub's answer was saved in; it counts only
		// when it verifies for this KEL against the hub KEL this data
		// directory holds (KEL C-51), read from the store without opening it
		// for writing. One that does not verify is no notice.
		hubKELs := func(aid string) ([]identity.SignedEvent, bool) { return offlineHubKEL(l, aid) }
		if identity.VerifyNotice(n.Wire, c.Accepted(), hubKELs, uint64(time.Now().UnixMilli())) == nil {
			d.idGate.noteNotice(n, "")
		}
	}
	if r, err := loadJoinRecord(l); err == nil {
		d.join.rec = r
	}
	return d.identityStatus(lang), nil
}
