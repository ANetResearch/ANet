package daemon

// identity_recovery.go is whether the identity has a recovery passphrase
// (KEL D-15): an attribute beside the identity's state, not a state of its
// own. It closes no gate and refuses no send; it decides whether the one
// reminder identity.unprotected (or its .resume variant) is shown, in the
// same words wherever the identity is described: /identity/status and the
// /hub-register answer, the recovery line of the status summary, doctor's
// identity.recovery, MCP node_status and the anet.identity_note of every
// task this node projects.
//
// Nothing is stored to say it. An identity that joined without a terminal
// has its recovery seed in the data directory, under a key that is there
// too (KEL C-36), and the daemon wrote the commitment that seed makes,
// local_commit (identity file key 7), when the join began (KEL D-11). As
// long as that commitment is the identity's next key or its current one,
// whoever copies the data directory can rotate or sign as the identity; once
// two rotations have moved both past it (`anet identity protect`, KEL D-16),
// it is neither. The daemon never opens the recovery file or its key (KEL
// D-2, SI-38): it compares public digests only.

import (
	"bytes"
	"crypto/sha256"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// The values of /identity/status.recovery.state (KEL §2.8, ux UX-R5).
const (
	// RecoveryNone: the identity has not begun to join a hub, so it has no
	// recovery seed yet.
	RecoveryNone = "none"
	// RecoveryUnprotected: the recovery seed is in the data directory,
	// without a passphrase (KEL D-11).
	RecoveryUnprotected = "unprotected"
	// RecoveryProtected: the recovery seed is outside the data directory,
	// under a passphrase.
	RecoveryProtected = "protected"
)

// The sentence keys of the reminder (ux §1.4 E3).
const (
	reasonUnprotected       = "identity.unprotected"
	reasonUnprotectedResume = "identity.unprotected.resume"
)

// RecoveryState is the recovery attribute of the identity (KEL D-15).
type RecoveryState struct {
	// State is RecoveryNone, RecoveryUnprotected or RecoveryProtected.
	State string
	// Resume is set with RecoveryUnprotected when the first of the two
	// rotations of `anet identity protect` is done and the second is not
	// (KEL D-16): the reminder is then identity.unprotected.resume.
	Resume bool
}

// recoveryFacts is what recoveryStateOf reads of the identity file (KEL
// §2.9): key 7 and the tip of the accepted KEL (key 1).
type recoveryFacts struct {
	// LocalCommit is key 7, local_commit; nil when the identity joined with
	// a passphrase.
	LocalCommit []byte
	// WitnessPending is the accepted tip naming no witness yet (KEL R-4).
	WitnessPending bool
	// TipNext is key 6 of the accepted tip, the next key it commits; nil
	// for a dip.
	TipNext []byte
	// TipKeyDigest is SHA-256 of the accepted tip's key (keys[0]): the
	// commitment the event revealed.
	TipKeyDigest []byte
}

// recoveryFactsOf reads c's identity file. A nil controller has no facts.
func recoveryFactsOf(c *identity.Controller) recoveryFacts {
	if c == nil {
		return recoveryFacts{}
	}
	kel := c.Accepted()
	f := recoveryFacts{LocalCommit: c.LocalCommit()}
	f.WitnessPending = kelFacts(c.AID(), kel).Witness == ""
	if n := len(kel); n > 0 {
		tip := kel[n-1].Event
		f.TipNext = tip.NextDigest
		if len(tip.Keys) == 1 {
			h := sha256.Sum256(tip.Keys[0])
			f.TipKeyDigest = h[:]
		}
	}
	return f
}

// recoveryStateOf is KEL D-15, in order:
//
//  1. the witness is pending and there is no witness_rot (the identity has
//     not begun to join): none;
//  2. local_commit is set and equals witness_rot.next_digest while the
//     witness is pending, the accepted tip's key 6, or the digest of the
//     accepted tip's key (the first of protect's two rotations done, the
//     second not: Resume): unprotected;
//  3. otherwise: protected.
//
// It is protected because of what the KEL shows, never because of a flag a
// crash could leave half-written: a protect interrupted between its two
// rotations still reads unprotected (Resume).
func recoveryStateOf(f recoveryFacts, witnessRot *WitnessRot) RecoveryState {
	if f.WitnessPending && witnessRot == nil {
		return RecoveryState{State: RecoveryNone}
	}
	if len(f.LocalCommit) > 0 {
		switch {
		case f.WitnessPending && bytes.Equal(f.LocalCommit, witnessRot.NextDigest):
			return RecoveryState{State: RecoveryUnprotected}
		case len(f.TipNext) > 0 && bytes.Equal(f.LocalCommit, f.TipNext):
			return RecoveryState{State: RecoveryUnprotected}
		case len(f.TipKeyDigest) > 0 && bytes.Equal(f.LocalCommit, f.TipKeyDigest):
			return RecoveryState{State: RecoveryUnprotected, Resume: true}
		}
	}
	return RecoveryState{State: RecoveryProtected}
}

// reminderKey is the sentence key of the reminder r calls for, "" when it
// calls for none: only an unprotected identity is reminded, and not once it
// is stopped or deactivated (nothing it could do with a passphrase then;
// ux §1.4 E3).
func (r RecoveryState) reminderKey(identityState string) string {
	if r.State != RecoveryUnprotected || identityState == IdentityStopped || identityState == IdentityDeactivated {
		return ""
	}
	if r.Resume {
		return reasonUnprotectedResume
	}
	return reasonUnprotected
}

// view is the recovery object of /identity/status, MCP node_status and the
// /hub-register answer (KEL §2.8): the state, and while there is a reminder
// its sentence and the command it names.
func (r RecoveryState) view(identityState string, lang a2ashape.Language) recoveryView {
	v := recoveryView{State: r.State}
	if key := r.reminderKey(identityState); key != "" {
		v.Message, v.Next = a2ashape.Sentence(key, lang, nil), a2ashape.Next(key, lang, nil)
	}
	return v
}

// recoveryState is the identity's recovery attribute now.
func (d *Daemon) recoveryState() RecoveryState {
	var rot *WitnessRot
	if r := d.joinSnapshot(); r != nil {
		rot = r.WitnessRot
	}
	return recoveryStateOf(recoveryFactsOf(d.self), rot)
}

// identityNote is anet.identity_note of a task this node projects (KEL
// D-15, core §2.13): the reminder's sentence in lang while the identity has
// no recovery passphrase, else "". It is the node's own note to its own
// clients; it never travels to a peer (CORE-K17).
func (d *Daemon) identityNote(lang a2ashape.Language) string {
	if d.self == nil {
		return ""
	}
	key := d.recoveryState().reminderKey(d.identityState().State)
	if key == "" {
		return ""
	}
	return a2ashape.Sentence(key, lang, nil)
}
