package daemon

// reseal.go is the reseal engine (engine §3): when an attempt seals a new
// version of a queued message before it transmits (resealNeeded, pure), and
// the sealing itself (sealVersion, A7): which of the recipient's keys, the
// grant the message carries, and every field that makes one version differ
// from another — never the message's identity, which every version carries
// unchanged (ENG-R7, SI-82).

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// resealNeeded is engine ENG-R2 with the suppressions of ENG-R3: whether
// the attempt at row seals a new version before it transmits. now is the
// sender's clock; cur is this node's key state now (keyCursor). Every input
// is stored or read now, so the answer is the same after a restart.
//
//   - T0: the row was never sealed (cur_ver 0, ENG-Q3).
//   - T1: the epoch key the version is sealed to is past its delivery
//     deadline, not_after + D (fsResealDue, FS-O1): a hub refuses it as
//     kid_stale, and a recipient may have deleted the key (SI-29).
//   - T2: the hub's last answer was 409 kid_stale (last_error says so, and
//     it survives a restart).
//   - T3: the version was signed under another key state than this node's
//     now (kelResealDue, KEL D-51(a)): after a rotation nothing goes out
//     under the old key.
//   - T4: the notice it carries is over NoticeResealAge old and this node
//     holds a newer one (kelResealDue, KEL D-51(b)). Without a newer one (the
//     witness out of reach) a new version would carry the same notice, so
//     none is made; the gate caps the notice's age instead (ENG-K17).
//   - S1: a version the hub holds queued is neither sent again nor sealed
//     again for the hub (ENG-D6). outboxStep keeps it: needsTransmit is
//     false while a version is queued or not asked about yet, so the only
//     transmission then is the direct bypass of ENG-D7 — and that one is
//     judged by the triggers like any (a direct recipient takes it at
//     arrival now, not at the hub's acceptance), its new version going
//     direct only. resealNeeded is therefore not asked to hold it back.
//   - S2: a frozen row is not sealed. S3, the gate, is sealMessage's
//     (innerFor) as well as the attempt's; S4, once an attempt and three
//     kid_stale retries an hour, is transmitOutbox's.
func resealNeeded(row RowView, vers []VerView, now uint64, cur KeyCursor) bool {
	switch {
	case row.Frozen:
		return false
	case row.CurVer == 0:
		return true
	case lastErrorCode(row.LastError) == codeKidStale:
		return true
	}
	return fsResealDue(row.EpochSealed, now) || kelResealDue(row.KELSealed, cur)
}

// sealedVersion is one sealing of a queued message (A7): the envelope
// without outer key 7 (the credential is set for every attempt, ENG-I2),
// its ack_key, what the version records about the keys it was sealed under
// (ENG-S3), and the capability its grant gives, which is written with the
// version (SSR-D10).
type sealedVersion struct {
	env      []byte
	ackKey   seal.AckKey
	kid      []byte
	kidNA    uint64
	kss      uint64
	noticeAt uint64
	grant    *grantPlan
}

// sealVersion is A7: a new version of row, whose body is body. Its key is
// the recipient's (sealKeys; from the directory when refresh, trigger T2,
// ENG-R11); its layers and rk are what the task's interaction keys give now
// (ixKeysForSeal: a single-layer task becomes a double-layer one once the
// peer announced its rk, ENG-K10); its grant is what grantFor gives the
// recipient with this message (ENG-K5); its mid, ts, exp, ix, type and body
// are the row's, the same in every version (ENG-R7).
func (d *Daemon) sealVersion(ctx context.Context, row *interactions.OutboxRow, body []byte, refresh bool) (*sealedVersion, error) {
	now := d.nowMS()
	set, err := d.sealKeys(ctx, row.ToAID, row.IX, refresh)
	if err != nil {
		return nil, err
	}
	ixk, err := d.ixKeysForSeal(row.IX)
	if err != nil {
		return nil, err
	}
	plan, err := d.grantForMessage(row.ToAID, capMsg{Kind: capMsgKindOf(row.Type, row.Kind), IX: row.IX}, now)
	if err != nil {
		return nil, err
	}
	var grant []byte
	if plan != nil {
		grant = plan.Grant
	}
	sv, err := d.sealMessage(sealMsg{to: row.ToAID, typ: row.Type, ix: row.IX, body: body, mid: row.MID, ts: row.TS,
		exp: row.Exp, grant: grant, rk: ixk.own}, set, seal.SealOpts{PeerRK: ixk.peer}, now)
	if err != nil {
		return nil, err
	}
	sv.grant = plan
	return sv, nil
}

// sealIXKeys is what a sealing on a task carries of its interaction keys
// (engine §3.2, FS-X4–X6): own, the rk of this node's key on the task while
// it holds one (inner key 15; none once it is deleted); peer, the rk the
// peer announced on the task, which seals a second layer to it (none: one
// layer).
type sealIXKeys struct {
	own, peer *seal.RK
}

// ixKeysForSeal is sealIXKeys for task ix now: this node's own key while
// the store holds it (FS-X4), and the rk the peer announced on the task when
// the task records one (FS-X5). A message that is not on a task (a price
// card's, an id that names no task) has neither: one layer, no rk.
func (d *Daemon) ixKeysForSeal(ix string) (sealIXKeys, error) {
	var out sealIXKeys
	if ix == "" {
		return out, nil
	}
	own, err := d.ix.OwnRK(ix)
	if err != nil {
		return out, fmt.Errorf("anet: %s: read this node's interaction key: %w", ix, err)
	}
	if own != nil {
		out.own = &seal.RK{Suite: own.Suite, Pub: bytes.Clone(own.Pub)}
	}
	row, err := d.ix.Get(ix)
	switch {
	case errors.Is(err, interactions.ErrNotFound):
		return out, nil
	case err != nil:
		return out, fmt.Errorf("anet: %s: read the task: %w", ix, err)
	case len(row.PeerRK) == 0:
		return out, nil
	}
	if out.peer, err = storedRK(row.PeerRK); err != nil {
		return out, fmt.Errorf("anet: %s: the peer's rk on record: %w", ix, err)
	}
	return out, nil
}

// storedRK reads an rk the store recorded as coredet(RK) (FS-R4): exactly
// the canonical encoding of {1: suite, 2: pub}, suite 1 and a 32-byte
// public key, as the receive path took it from the peer's message. Anything
// else is an error: a second layer is never sealed to a key read loosely,
// nor dropped to one layer because the record is damaged.
func storedRK(b []byte) (*seal.RK, error) {
	var rk seal.RK
	if err := coredet.UnmarshalStrict(b, &rk); err != nil {
		return nil, err
	}
	if re, err := coredet.Marshal(&rk); err != nil || !bytes.Equal(re, b) {
		return nil, errors.New("not the canonical encoding of an rk")
	}
	if rk.Suite != seal.SuiteX25519 || len(rk.Pub) != storedRKPubLen {
		return nil, fmt.Errorf("suite %d with a %d-byte key, want suite %d with %d bytes", rk.Suite, len(rk.Pub),
			seal.SuiteX25519, storedRKPubLen)
	}
	return &rk, nil
}

// storedRKPubLen is the public key length of suite 1 (X25519), the only
// suite of an interaction key (FS-C10).
const storedRKPubLen = 32

// newOwnIXKey makes, in memory, this node's interaction key for task ix
// with peer (FS-X1, FS-X2): before the transaction that records the task,
// which stores it with putOwnIXKeyTx. The caller wipes it (wipeIXKey) once
// that transaction has ended, committed or not: the stored copy is the
// only one kept, and a failed transaction leaves none (FS-X1).
func newOwnIXKey(ix, peer string) (*seal.IXKey, error) {
	k, err := seal.GenerateIXKey(seal.SuiteX25519, ix, peer)
	if err != nil {
		return nil, fmt.Errorf("anet: %s: make the interaction key: %w", ix, err)
	}
	return k, nil
}

// putOwnIXKeyTx stores k as this node's key for its task, in tx, the
// transaction that records the task (FS-X1, FS-X2): from then on every
// message this node seals on the task carries its rk (ixKeysForSeal,
// FS-X4), and the peer seals its own to it.
func putOwnIXKeyTx(tx *interactions.Tx, k *seal.IXKey) error {
	return tx.PutIXKey(interactions.IXKey{IX: k.IX, PeerAID: k.PeerAID, Suite: k.Suite, Pub: k.Pub, Priv: k.Private})
}

// wipeIXKey zeroes the private half of a key this node holds in memory
// (FS-X8).
func wipeIXKey(k *seal.IXKey) {
	if k != nil {
		clear(k.Private)
	}
}

// rkBytes is rk as the store records it (FS-R4): coredet(RK), the canonical
// encoding storedRK reads back. nil for none.
func rkBytes(rk *seal.RK) ([]byte, error) {
	if rk == nil {
		return nil, nil
	}
	return coredet.Marshal(rk)
}

// sealKeys is the recipient's key set a version is sealed to (engine §3.2):
// the one kept on a public or public_cap interaction this node serves
// (A2A-DESIGN §3.8), the held delegation's for a notice about one, the
// stored record's while it holds a usable key, else the directory's
// (recipientKeys); with refresh the directory's afresh (T2). An interaction
// this node started pins the recipient.
func (d *Daemon) sealKeys(ctx context.Context, to, ixID string, refresh bool) (*seal.EncKeySet, error) {
	if refresh {
		ks, err := d.refreshForKidStale(ctx, to)
		if err != nil {
			return nil, err
		}
		return ks.set, nil
	}
	now := d.nowMS()
	pin := ""
	ix, err := d.ix.Get(ixID)
	switch {
	case err == nil && ix.Role == interactions.RoleOutbound:
		pin = interactions.PinOutbound
	case err == nil && ix.Role == interactions.RoleInbound &&
		(ix.Trust == interactions.TrustPublic || ix.Trust == interactions.TrustPublicCap):
		if ks, ok := interactionKeySet(ix, now); ok {
			return ks.set, nil
		}
		if row, err := d.ix.PeerIdentity(to); err == nil {
			if ks, ok := d.storedKeySet(row, now); ok {
				return ks.set, nil
			}
		}
		return nil, fmt.Errorf("anet: %s: %w: the requester's key set on this interaction is gone or expired", to, errNoRecipientKeys)
	case errors.Is(err, interactions.ErrNotFound):
		// A notice about a delegation held for approval: the key set it
		// carried, while the held item is there.
		if set, ok := d.heldKeySet(ixID, to, now); ok {
			return set, nil
		}
	}
	keys, err := d.recipientKeys(ctx, to, pin)
	if err != nil {
		return nil, err
	}
	return keys.set, nil
}

// heldKeySet is the key set the held delegation ix of `to` carried, verified
// against the KEL it carried and usable now.
func (d *Daemon) heldKeySet(ixID, to string, now uint64) (*seal.EncKeySet, bool) {
	item, err := d.ix.GetPending(ixID)
	if err != nil || item.FromAID != to || len(item.Keys) == 0 {
		return nil, false
	}
	kel, err := identity.UnmarshalKEL(item.KEL)
	if err != nil {
		return nil, false
	}
	signed, err := seal.UnmarshalSignedEncKeySet(item.Keys)
	if err != nil {
		return nil, false
	}
	set, err := seal.VerifyEncKeySet(signed, to, kel, now)
	if err != nil {
		return nil, false
	}
	if _, err := seal.SelectKey(set, now); err != nil {
		return nil, false
	}
	return set, true
}
