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
// senders this node has not accepted anything from are not kept at all
// (a refusal is told in the delivery token, engine ENG-V15), so inbound
// traffic from new AIDs cannot displace or rewrite a stored row.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
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
// pinned reason, or "" for an unpinned row. A hub's KEL (pin "hub") is
// pinned by the stricter rule of the federation hub KEL set (pinHubKEL).
func (d *Daemon) pinPeerKEL(aid string, kel []identity.SignedEvent, pin string) ([]identity.SignedEvent, error) {
	if pin == interactions.PinHub {
		use, _, err := d.pinHubKEL(aid, kel)
		return use, err
	}
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
	return d.ix.UpdatePeerIdentity(aid, d.peerUpdate(aid, kel, keys, pin, fetched, d.nowMS()))
}

// peerUpdate is notePeer's read-decide-write of aid's row at now, for
// UpdatePeerIdentity in a transaction of its own or in the receive
// pipeline's step 10 (recordSenderTx).
func (d *Daemon) peerUpdate(aid string, kel []identity.SignedEvent, keys *peerKeySet, pin string, fetched bool,
	now uint64) func(*interactions.PeerIdentity) (*interactions.PeerIdentity, error) {
	return func(cur *interactions.PeerIdentity) (*interactions.PeerIdentity, error) {
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
		keys := keys
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
	}
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
// yet it looks the peer up in the directory (resolvePeer, which names it to
// no hub) and verifies its KEL and key set with expectAID = aid. When the
// directory cannot answer, the peer is recorded on its first valid message
// instead (trust on first use, §3.8).
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
	if d.config().HubURL == "" {
		return fmt.Errorf("anet: no record of %s and no hub to fetch one from; it will be recorded on its first message", aid)
	}
	if m, v := d.resolvePeer(ctx, aid, resolveOpts{need: needKeySet, record: true, pin: reason}); v != verdictFound {
		return fmt.Errorf("anet: could not fetch %s's keys (it will be recorded on its first message): %w",
			aid, &peerResolveError{AID: aid, Verdict: v, Cause: m.Err})
	}
	return nil
}

// ---- the federation hub KEL set (KEL D-24)
//
// The notices a node checks — its own, every sender's (receive step 6½), a
// directory entry's (D-26, D-28) — are signed by hubs, each under its key
// history as this node holds it: the peer_identity rows pinned "hub"
// (hubKELs, which reads that table and nothing else). They are kept in step
// with this node's hub's GET /hub/kels, which lists the hub's own key
// history and those of every federation peer it pinned: every HubKELSync,
// and on demand, at most once a minute, when a notice names a witness this
// node does not hold as far as the notice needs (witness-unknown).
//
// A pinned hub KEL only grows: a candidate replaces it only when it extends
// it (identity.ExtendsKEL) and every event it adds is dated at most
// EventClockWindow after this node's clock (R-9). A strict prefix — a hub's
// history before a rotation, under which its retired key would still be
// current — is never taken; a fork keeps the pinned KEL and is counted
// (hub-kel-fork, which anet doctor's identity.witness reports). A KEL that
// is not a hub's (toad ≠ 0) is not pinned at all.

const (
	// hubKELSyncMS is HubKELSync (KEL §2.12): how often this node reads
	// its hub's /hub/kels. A code constant: nothing for an operator to tune.
	hubKELSyncMS uint64 = 10 * 60 * 1000
	// hubKELRefreshMS is the least time between two reads of /hub/kels,
	// the on-demand refresh included (KEL D-24: at most once a minute).
	hubKELRefreshMS uint64 = 60 * 1000
	// hubKELsMax bounds the hubs one /hub/kels answer is read for; the
	// rest are skipped and counted.
	hubKELsMax = 256
	// hubKELsPath is the hub's list of hub key histories (KEL H-16), read
	// on the anonymous side (hubapi.SideOf).
	hubKELsPath = "/hub/kels"
)

// Counters of the hub KEL set (ReceiveStats).
const (
	cntHubKELFork      = "hub-kel-fork"       // a hub KEL that forks from the pinned one; the pinned one is kept (D-24)
	cntHubKELNotHub    = "hub-kel-not-hub"    // a KEL offered as a hub's that is not one (toad ≠ 0); not pinned
	cntHubKELFuture    = "hub-kel-future"     // a hub KEL adding an event dated past now + 5 minutes (R-9); not pinned
	cntHubKELsBadEntry = "hub-kels-bad-entry" // a /hub/kels entry that does not decode or replay to its aid
)

// hubKELSyncEvery is HubKELSync as the sync loop's timer takes it; a test
// shortens it.
var hubKELSyncEvery = newKnob(time.Duration(hubKELSyncMS) * time.Millisecond)

// errNotAHubKEL is a KEL offered as a hub's whose witness mode is not a
// hub's (toad ≠ 0).
var errNotAHubKEL = errors.New("not a hub's key history (toad is not 0)")

// pinHubKEL pins kel as the key history of the hub aid (pinned reason
// "hub") by the extension rule, and returns the KEL to use for aid and
// whether the stored row changed. A KEL that does not replay to aid, or is
// not a hub's, is refused; a strict prefix of the pinned one leaves it as
// it is; a fork leaves it as it is, counted, and is an error wrapping
// identity.ErrKELFork; an extension that adds an event from the future
// (R-9) is refused.
func (d *Daemon) pinHubKEL(aid string, kel []identity.SignedEvent) ([]identity.SignedEvent, bool, error) {
	states, err := identity.Replay(kel)
	if err != nil {
		return nil, false, fmt.Errorf("KEL does not replay: %w", err)
	}
	if len(states) == 0 || states[len(states)-1].AID != aid {
		return nil, false, fmt.Errorf("KEL does not replay to %s", aid)
	}
	if states[len(states)-1].Toad != identity.ToadHub {
		d.count(cntHubKELNotHub)
		return nil, false, errNotAHubKEL
	}
	now := d.nowMS()
	var use []identity.SignedEvent
	changed := false
	err = d.ix.UpdatePeerIdentity(aid, func(cur *interactions.PeerIdentity) (*interactions.PeerIdentity, error) {
		use, changed = nil, false
		var stored []identity.SignedEvent
		if cur != nil && len(cur.KEL) > 0 {
			stored, _ = identity.UnmarshalKEL(cur.KEL)
		}
		replace := false
		switch err := identity.ExtendsKEL(stored, kel); {
		case len(stored) == 0:
			replace = true
		case err == nil:
			replace = len(kel) > len(stored)
		case errors.Is(err, identity.ErrKELRollback):
			// The pinned KEL is longer: what is offered is its past.
		default:
			d.count(cntHubKELFork)
			return nil, err
		}
		if replace {
			if err := identity.CheckNewEvents(stored, kel, now, nil); err != nil {
				d.count(cntHubKELFuture)
				return nil, err
			}
			use = kel
		} else {
			use = stored
		}
		next := cur
		if next == nil {
			next = &interactions.PeerIdentity{}
		}
		write := replace
		if replace {
			b, err := identity.MarshalKEL(kel)
			if err != nil {
				return nil, err
			}
			next.KEL, next.KELLen = b, len(kel)
		}
		if next.PinnedReason == "" {
			next.PinnedReason = interactions.PinHub
			write = true
		}
		if !write {
			return nil, nil
		}
		changed = replace
		return next, nil
	})
	if err != nil {
		return nil, false, err
	}
	return use, changed, nil
}

// hubKELsAnswer is the GET /hub/kels answer (core §2.12.2): every hub's
// AID and key history, base64url.
type hubKELsAnswer struct {
	Hubs []struct {
		AID string `json:"aid"`
		KEL string `json:"kel"`
	} `json:"hubs"`
}

// hubKELEntry is one hub of a /hub/kels answer, decoded.
type hubKELEntry struct {
	AID string
	KEL []identity.SignedEvent
}

// decodeHubKELs reads a /hub/kels answer: each entry's KEL strict
// base64url (core CORE-E5), parsed (seal.ParseKEL), replaying to the AID
// it is listed under. Entries that do not are skipped (counted by the
// caller), as are those past hubKELsMax; an answer that is not the JSON
// object is an error.
func decodeHubKELs(body []byte) (out []hubKELEntry, skipped int, err error) {
	var a hubKELsAnswer
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, 0, fmt.Errorf("anet: the hub's /hub/kels answer does not decode: %w", err)
	}
	if a.Hubs == nil {
		return nil, 0, errors.New("anet: the hub's /hub/kels answer has no hubs")
	}
	for i, h := range a.Hubs {
		if i >= hubKELsMax {
			skipped += len(a.Hubs) - i
			break
		}
		raw, err := hubapi.DecodeB64(h.KEL, -1)
		if err != nil || !identity.ValidAID(h.AID) {
			skipped++
			continue
		}
		kel, err := seal.ParseKEL(raw)
		if err != nil {
			skipped++
			continue
		}
		if got, err := replayedAID(kel); err != nil || got != h.AID {
			skipped++
			continue
		}
		out = append(out, hubKELEntry{AID: h.AID, KEL: kel})
	}
	return out, skipped, nil
}

// syncHubKELs reads this node's hub's /hub/kels and pins every hub KEL it
// lists by pinHubKEL. It reports whether a pinned KEL changed. Without a
// hub it does nothing. It does not bound itself: refreshHubKELs and the
// sync loop do.
func (d *Daemon) syncHubKELs(ctx context.Context) (bool, error) {
	hub := strings.TrimRight(d.config().HubURL, "/")
	if hub == "" || d.ix == nil {
		return false, nil
	}
	cctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
	defer cancel()
	var raw json.RawMessage
	if err := d.hubGet(cctx, hub, hubKELsPath, nil, &raw); err != nil {
		return false, err
	}
	entries, skipped, err := decodeHubKELs(raw)
	if err != nil {
		return false, err
	}
	for range skipped {
		d.count(cntHubKELsBadEntry)
	}
	changed := false
	for _, e := range entries {
		_, ch, err := d.pinHubKEL(e.AID, e.KEL)
		if err != nil {
			d.logOnce("hub-kel:"+e.AID, "anet: the key history %s lists for hub %s is not taken: %v", hub, e.AID, err)
			continue
		}
		changed = changed || ch
	}
	return changed, nil
}

// refreshHubKELs reads the hub KEL set again for a notice under a witness
// this node does not hold the KEL of as far as the notice needs
// (witness-unknown; KEL D-24): at most once a minute, whatever asks, and a
// second caller while a read is in progress waits for it. It reports
// whether a pinned KEL changed, so the notice is worth checking again.
func (d *Daemon) refreshHubKELs(ctx context.Context) bool {
	changed, _ := d.peers.hubKELFlights.do("", func() (bool, error) {
		now := d.nowMS()
		if !d.peers.mayReadHubKELs(now, hubKELRefreshMS) {
			return false, nil
		}
		changed, err := d.syncHubKELs(ctx)
		if err != nil {
			d.logOnce("hub-kels-refresh", "anet: reading the hub key histories: %v", err)
		}
		return changed, nil
	})
	return changed
}

// hubKELSyncLoop reads the hub KEL set every HubKELSync (KEL D-24), the
// first time one period after start: until then the on-demand refresh
// covers a witness this node does not hold yet.
func (d *Daemon) hubKELSyncLoop(ctx context.Context) {
	t := time.NewTicker(hubKELSyncEvery.get())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if d.config().HubURL == "" {
				continue
			}
			_, _ = d.peers.hubKELFlights.do("", func() (bool, error) {
				d.peers.mayReadHubKELs(d.nowMS(), 0)
				if _, err := d.syncHubKELs(ctx); err != nil {
					d.logOnce("hub-kels-sync", "anet: reading the hub key histories: %v", err)
				}
				return false, nil
			})
		}
	}
}

// offlineHubKEL is hubKELs for a data directory with no running daemon: the hub's KEL as the store
// holds it, read without writing, replayed to the AID it is stored under.
func offlineHubKEL(l Layout, aid string) ([]identity.SignedEvent, bool) {
	raw, ok, err := interactions.HubKELIn(l.InteractionsDir(), aid)
	if err != nil || !ok {
		return nil, false
	}
	kel, err := identity.UnmarshalKEL(raw)
	if err != nil {
		return nil, false
	}
	if got, err := replayedAID(kel); err != nil || got != aid {
		return nil, false
	}
	return kel, true
}
