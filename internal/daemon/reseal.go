package daemon

// reseal.go is the reseal engine (engine §3): when an attempt seals a new
// version of a queued message before it transmits (resealNeeded, pure), and
// the sealing itself (sealVersion, A7): which of the recipient's keys, the
// grant the message carries, and every field that makes one version differ
// from another — never the message's identity, which every version carries
// unchanged (ENG-R7, SI-82).

import (
	"context"
	"errors"
	"fmt"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// resealNeeded is engine ENG-R2 with the suppressions of ENG-R3: whether
// the attempt at row seals a new version before it transmits. cur is this
// node's key state now (keyCursor).
//
//   - T0: the row was never sealed (cur_ver 0, ENG-Q3).
//   - T2: the hub's last answer was 409 kid_stale (last_error says so, and
//     it survives a restart).
//   - S2: a frozen row is not sealed. S3, the gate, and S4, once an attempt,
//     are the attempt's (outboxStep, transmitOutbox).
//
// v030-todo(ENG-1b): T1 fsResealDue(row.EpochSealed, now), T3 and T4
// kelResealDue(row.KELSealed, cur), and S1: no version sealed for the hub
// while one is queued there (ENG-R3).
func resealNeeded(row RowView, vers []VerView, now uint64, cur KeyCursor) bool {
	switch {
	case row.Frozen:
		return false
	case row.CurVer == 0:
		return true
	case lastErrorCode(row.LastError) == codeKidStale:
		return true
	}
	return false
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
// ENG-R11); its grant is what grantFor gives the recipient with this
// message (ENG-K5); its mid, ts and exp are the row's.
func (d *Daemon) sealVersion(ctx context.Context, row *interactions.OutboxRow, body []byte, refresh bool) (*sealedVersion, error) {
	now := d.nowMS()
	set, err := d.sealKeys(ctx, row.ToAID, row.IX, refresh)
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
		exp: row.Exp, grant: grant}, set, now)
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

// ixKeysForSeal is sealIXKeys for task ix now.
//
// v030-todo(ENG-1b): the store's own key and the recorded peer_rk.
func (d *Daemon) ixKeysForSeal(ix string) (sealIXKeys, error) {
	return sealIXKeys{}, nil
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
