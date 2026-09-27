package daemon

// peerkel.go is this node's record of other nodes' identities
// (A2A-DESIGN §3.8): the key history (KEL) and encryption key set of each
// peer it has an authorized relationship with, persisted in the
// peer_identity table of interactions.db.
//
// Two rules make the record useful and not only a cache:
//
//   - A stored KEL is replaced only by a KEL that extends it
//     (identity.ExtendsKEL). A peer that rotated its key is followed; a hub
//     or a sender that presents a truncated KEL (the key history before a
//     rotation, under which a retired key is still current) does not move
//     this node back. That protection holds only for peers with a stored
//     row, and first contact is trust on first use (§21 item 6).
//   - A key set is replaced only under the consumer high-water rule on its
//     seq (seal.DecideHighWater).
//
// Rows are written only in authorized contexts: after the receive path
// accepted a message from the peer (§3.6 step 10), when this node initiates
// contact, when the operator pins a peer (PinPeer), for a hub's own KEL, and
// for KELs a module resolved to verify a signed object. KELs and key sets of
// senders this node has not accepted anything from are kept only in a small
// in-memory cache (strangers below), used to encrypt a refusal reply and for
// nothing else, so inbound traffic from new AIDs cannot displace or rewrite
// a stored row.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// replayedAID replays a KEL and returns the AID it describes.
func replayedAID(kel []identity.SignedEvent) (string, error) {
	states, err := identity.Replay(kel)
	if err != nil {
		return "", err
	}
	if len(states) == 0 {
		return "", errors.New("empty KEL")
	}
	return states[len(states)-1].AID, nil
}

// peerKEL returns the stored KEL for aid. A row whose KEL does not decode
// or does not replay to aid is treated as absent and reported in the log;
// it cannot have been written by this code.
func (d *Daemon) peerKEL(aid string) ([]identity.SignedEvent, bool) {
	row, err := d.ix.PeerIdentity(aid)
	if err != nil || len(row.KEL) == 0 {
		return nil, false
	}
	kel, err := identity.UnmarshalKEL(row.KEL)
	if err != nil {
		d.logOnce("peer-kel-corrupt:"+aid, "anet: stored KEL for %s does not decode: %v", aid, err)
		return nil, false
	}
	if got, err := replayedAID(kel); err != nil || got != aid {
		d.logOnce("peer-kel-corrupt:"+aid, "anet: stored KEL for %s does not replay to it", aid)
		return nil, false
	}
	return kel, true
}

// encodedPeerKEL returns a peer's stored key history, base64, or "" if this
// node holds none. Empty is the honest answer: this node vouches for the
// peers it has actually checked and for nobody else.
func (d *Daemon) encodedPeerKEL(aid string) string {
	kel, ok := d.peerKEL(aid)
	if !ok {
		return ""
	}
	b, err := identity.MarshalKEL(kel)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// mergeKEL applies the extension rule to a stored row and a candidate KEL
// for the same AID. It returns the KEL to use (the longer of the two when
// one extends the other), whether the candidate should replace the stored
// one, and an error wrapping identity.ErrKELFork when they disagree.
func mergeKEL(stored, candidate []identity.SignedEvent) (use []identity.SignedEvent, replace bool, err error) {
	if len(stored) == 0 {
		return candidate, true, nil
	}
	switch err := identity.ExtendsKEL(stored, candidate); {
	case err == nil:
		return candidate, len(candidate) > len(stored), nil
	case errors.Is(err, identity.ErrKELRollback):
		return stored, false, nil
	default:
		return nil, false, err
	}
}

// pinPeerKEL records a KEL this node obtained in an authorized context
// (a hub's identity, a recipient's keys fetched to contact it) and returns
// the KEL to use for aid. The KEL must replay to aid (C0). pin is the
// pinned reason, or "" for an unpinned row.
func (d *Daemon) pinPeerKEL(aid string, kel []identity.SignedEvent, pin string) ([]identity.SignedEvent, error) {
	got, err := replayedAID(kel)
	if err != nil {
		return nil, fmt.Errorf("KEL does not replay: %w", err)
	}
	if got != aid {
		return nil, fmt.Errorf("KEL replays to %s, not %s", got, aid)
	}
	var use []identity.SignedEvent
	err = d.ix.UpdatePeerIdentity(aid, func(cur *interactions.PeerIdentity) (*interactions.PeerIdentity, error) {
		var stored []identity.SignedEvent
		if cur != nil && len(cur.KEL) > 0 {
			if stored, err = identity.UnmarshalKEL(cur.KEL); err != nil {
				stored = nil
			}
		}
		u, replace, merr := mergeKEL(stored, kel)
		if merr != nil {
			return nil, merr
		}
		use = u
		next := cur
		if next == nil {
			next = &interactions.PeerIdentity{}
		}
		if replace {
			b, err := identity.MarshalKEL(u)
			if err != nil {
				return nil, err
			}
			next.KEL, next.KELLen = b, len(u)
		}
		if pin != "" && next.PinnedReason == "" {
			next.PinnedReason = pin
		}
		if cur != nil && !replace && (pin == "" || cur.PinnedReason != "") {
			return nil, nil // nothing to write
		}
		return next, nil
	})
	if err != nil {
		return nil, err
	}
	return use, nil
}

// peerKeySet is a verified encryption key set of a peer.
type peerKeySet struct {
	signed []byte // SignedEncKeySet encoding
	set    *seal.EncKeySet
}

// seenOf returns the high-water mark stored in a row, or nil.
func seenOf(row *interactions.PeerIdentity) *seal.Seen {
	if row == nil || len(row.KeySet) == 0 {
		return nil
	}
	s, err := seal.UnmarshalSignedEncKeySet(row.KeySet)
	if err != nil {
		return nil
	}
	return &seal.Seen{Seq: row.KeySetSeq, Set: s.Set}
}

// storedKeySet decodes and re-verifies the key set stored for aid against
// the stored KEL at now. A stored set is re-verified on every use because
// both inputs of the check move: the keys expire, and a KEL rotation makes
// a set signed under the old key state no longer a current statement.
func (d *Daemon) storedKeySet(row *interactions.PeerIdentity, now uint64) (*peerKeySet, bool) {
	if row == nil || len(row.KeySet) == 0 || len(row.KEL) == 0 {
		return nil, false
	}
	kel, err := identity.UnmarshalKEL(row.KEL)
	if err != nil {
		return nil, false
	}
	signed, err := seal.UnmarshalSignedEncKeySet(row.KeySet)
	if err != nil {
		return nil, false
	}
	set, err := seal.VerifyEncKeySet(signed, row.AID, kel, now)
	if err != nil {
		return nil, false
	}
	return &peerKeySet{signed: row.KeySet, set: set}, true
}

// notePeer writes what an accepted message or a completed key fetch taught
// this node about aid: a KEL (extension rule) and a key set (high-water
// rule). kel may be nil to leave the KEL alone; keys may be nil. It is
// called only from authorized contexts (see the file comment).
func (d *Daemon) notePeer(aid string, kel []identity.SignedEvent, keys *peerKeySet, pin string, fetched bool) error {
	if len(kel) > 0 {
		// The record is keyed by AID; a KEL stored under an AID it does not
		// replay to would vouch for another identity's signatures (C0).
		if got, err := replayedAID(kel); err != nil || got != aid {
			return fmt.Errorf("anet: refusing to record a KEL for %s that does not replay to it", aid)
		}
	}
	now := d.nowMS()
	return d.ix.UpdatePeerIdentity(aid, func(cur *interactions.PeerIdentity) (*interactions.PeerIdentity, error) {
		if cur == nil && len(kel) == 0 {
			// A record starts with a KEL; nothing else can be checked
			// without one.
			return nil, nil
		}
		next := cur
		if next == nil {
			next = &interactions.PeerIdentity{}
		}
		write := cur == nil
		if len(kel) > 0 {
			var stored []identity.SignedEvent
			if cur != nil && len(cur.KEL) > 0 {
				stored, _ = identity.UnmarshalKEL(cur.KEL)
			}
			u, replace, err := mergeKEL(stored, kel)
			if err != nil {
				// A fork here means the stored row changed between the
				// receive path's check and this write. The stored row
				// wins, and so does its key set: keys was verified
				// against the candidate KEL, which the stored history
				// does not accept.
				d.count(dropKELFork)
				replace = false
				keys = nil
			}
			if replace {
				b, err := identity.MarshalKEL(u)
				if err != nil {
					return nil, err
				}
				next.KEL, next.KELLen = b, len(u)
				write = true
			}
		}
		if keys != nil {
			switch seal.DecideHighWater(seenOf(cur), keys.set.Seq, mustSetBytes(keys.signed)) {
			case seal.Replace:
				next.KeySet, next.KeySetSeq = keys.signed, keys.set.Seq
				next.KeysCheckedAt = int64(now)
				write = true
			case seal.Same:
				if fetched {
					next.KeysCheckedAt = int64(now)
					write = true
				}
			case seal.Fork:
				d.count(keysFork)
			case seal.Ignore:
				d.count(keysRollback)
			}
		}
		if pin != "" && next.PinnedReason == "" {
			next.PinnedReason = pin
			write = true
		}
		if !write {
			return nil, nil
		}
		return next, nil
	})
}

// mustSetBytes returns the Set field of a SignedEncKeySet encoding, or nil.
func mustSetBytes(signed []byte) []byte {
	s, err := seal.UnmarshalSignedEncKeySet(signed)
	if err != nil {
		return nil
	}
	return s.Set
}

// PinPeer marks aid as a peer the operator chose (reason "allow" or
// "trust"), so its record is never evicted. When this node holds no record
// yet it fetches the peer's KEL and key set from the hub and verifies them
// with expectAID = aid. When the hub cannot answer, the peer is recorded on
// its first valid message instead (trust on first use, §3.8).
func (d *Daemon) PinPeer(ctx context.Context, aid, reason string) error {
	if aid == "" {
		return fmt.Errorf("anet: name the peer to pin")
	}
	if reason != interactions.PinAllow && reason != interactions.PinTrust {
		return fmt.Errorf("anet: pin reason must be %q or %q", interactions.PinAllow, interactions.PinTrust)
	}
	if row, err := d.ix.PeerIdentity(aid); err == nil {
		if row.PinnedReason == reason {
			return nil
		}
		return d.ix.UpdatePeerIdentity(aid, func(cur *interactions.PeerIdentity) (*interactions.PeerIdentity, error) {
			if cur == nil {
				return nil, nil
			}
			cur.PinnedReason = reason
			return cur, nil
		})
	} else if !errors.Is(err, interactions.ErrNotFound) {
		return err
	}
	hub := d.config().HubURL
	if hub == "" {
		return fmt.Errorf("anet: no record of %s and no hub to fetch one from; it will be recorded on its first message", aid)
	}
	if _, err := d.fetchRecipientKeys(ctx, hub, aid, reason); err != nil {
		return fmt.Errorf("anet: could not fetch %s's keys (it will be recorded on its first message): %w", aid, err)
	}
	return nil
}

// strangers holds the KEL and key set of senders this node has not
// accepted anything from, for encrypting a refusal reply to them. It is
// bounded and in memory only; it never feeds ResolveKEL, the high-water
// marks or peer_identity.
type strangerCache struct {
	mu    sync.Mutex
	limit int
	order []string
	sets  map[string]*peerKeySet
}

const strangerCacheLimit = 256

func (c *strangerCache) put(aid string, ks *peerKeySet) {
	if ks == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sets == nil {
		c.sets = map[string]*peerKeySet{}
	}
	if c.limit == 0 {
		c.limit = strangerCacheLimit
	}
	if _, ok := c.sets[aid]; !ok {
		c.order = append(c.order, aid)
		if len(c.order) > c.limit {
			delete(c.sets, c.order[0])
			c.order = c.order[1:]
		}
	}
	c.sets[aid] = ks
}

func (c *strangerCache) get(aid string) (*peerKeySet, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ks, ok := c.sets[aid]
	return ks, ok
}
