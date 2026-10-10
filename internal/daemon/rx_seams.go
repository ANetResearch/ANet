package daemon

// rx_seams.go is where the receive pipeline v3 meets what other packages
// deliver after it (engine ENG-G3, §0.2): each seam has the signature the
// pipeline calls and, until its package replaces it, the stub behavior the
// engine fixes (no preview frame is taken, every card answer is refused,
// nothing more is written for a refused result, the hub KEL set is not
// refreshed). The interaction key lookup of step 3a (FS-7) is the store's.
// The reply grant of anet.card.get/1 is kept here for the one-shot sender
// (ENG-1b), which seals the answer.

import (
	"context"
	"fmt"
	"sync"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// ixKeys is the lookup of this node's interaction keys that receive step 3a
// opens a second layer with (seal.Open's ixLookup; FS-R1): the store's
// IXKeyByKID. It answers (nil, nil) for a key this node does not hold (the
// envelope is sealed-to-closed-interaction) and an error when the store
// cannot be read (the envelope is temporary, ENG-V8). Each call is a lookup
// of its own: the caller hands it to one Open and then to wipeLookup.
func (d *Daemon) ixKeys() seal.IXKeyLookup {
	if l := d.testIXLookup(); l != nil {
		return l
	}
	return &storeIXKeys{s: d.ix}
}

// storeIXKeys is ixKeys over the interaction store. It keeps every key it
// handed out, so that wipe zeroes their private halves once Open is done
// (FS-X8). It is never printed: a key reaches no log through it (SI-34).
type storeIXKeys struct {
	s    *interactions.Store
	gave []*seal.IXKey
}

// IXKey is seal.IXKeyLookup: this node's key whose kid is ikid, as the
// store holds it (interactions.IXKey to seal.IXKey, field for field), or
// (nil, nil); a read error is returned as it is, which Open passes on
// wrapped and without a seal reason.
func (l *storeIXKeys) IXKey(ikid []byte) (*seal.IXKey, error) {
	k, err := l.s.IXKeyByKID(ikid)
	if err != nil {
		return nil, fmt.Errorf("anet: read the interaction key: %w", err)
	}
	if k == nil {
		return nil, nil
	}
	sk := &seal.IXKey{IX: k.IX, PeerAID: k.PeerAID, Suite: k.Suite, Pub: k.Pub, Private: k.Priv}
	l.gave = append(l.gave, sk)
	return sk, nil
}

// wipe zeroes the private halves of the keys l handed out.
func (l *storeIXKeys) wipe() {
	for _, k := range l.gave {
		wipeIXKey(k)
	}
	l.gave = nil
}

// wipeLookup wipes what the interaction key lookup l handed out, when it is
// the store's (a test's own lookup keeps its keys).
func wipeLookup(l seal.IXKeyLookup) {
	if w, ok := l.(*storeIXKeys); ok {
		w.wipe()
	}
}

// previewArrived is receive step 9 for a stream_preview frame (engine §4.5;
// CUX-P14): UX-8's previewAccept decides, an accepted frame is kept in
// memory and published (EventPreview), and nothing is written to the store.
// It reports whether the frame was taken; one that was not is dropped
// without a word (preview-rejected).
//
// v030-todo(UX-8): previewAccept and the preview cache; until then no frame
// is taken.
func (d *Daemon) previewArrived(m *rxMsg) bool { return false }

// cardAnswerArrived is receive step 9 for an anet.card/1 (engine §4.5; HPP
// PAY-C13, C14): taken only for an ix this node has a card fetch pending
// for, verified (verifyExtCard) against kel, the sender's KEL as steps 6 and
// 7 resolved it, written to peer_ext_card outside any receive transaction,
// then PAY-D5's wake-up. It reports whether the card was taken.
//
// v030-todo(PAY-11): the requester's half of the card exchange; until then
// every answer is refused (verifyExtCard refuses, ENG-G3).
func (d *Daemon) cardAnswerArrived(from, ix string, kel []identity.SignedEvent, resp *delegation.CardResp) bool {
	return false
}

// resultRefused is CUX-R7's terminal transaction: before a result refused
// for result-refused or reply-mismatch is answered with tok(2), the task
// is failed (anet.reason = result_refused) through the terminal write, in a
// transaction of its own. An error leaves the envelope temporary: not
// acknowledged and no token, so its refusal is told only once it is written
// (as ENG-V12 for a delegation).
//
// v030-todo(UX-3): the terminal write; until then nothing is written.
func (d *Daemon) resultRefused(m *rxMsg, reason string) error { return nil }

// refreshHubKELs is receive step 6½'s refresh for a notice under a witness
// this node does not hold the KEL of as far as the notice needs
// (witness-unknown; KEL D-24): the federation hub KEL set is fetched again
// from this node's hub, at most once a minute. It reports whether the set
// may have changed, so the notice is checked again.
//
// v030-todo(KEL-11): /hub/kels on demand; until then nothing is refreshed and
// witness-unknown stays temporary.
func (d *Daemon) refreshHubKELs(ctx context.Context) bool { return false }

// cardReplyGrants keeps, by (requester, ix), the one-shot capability an
// anet.card.get/1 brought for its answer (SS §4.3.2 rule 0): the only
// credential the anet.card/1 answering it is sealed under (ENG-K2, SS §4.3.3
// row card). It is not written to caps_held (SSR-W8): it lives here until
// the answer takes it or the capability expires.
type cardReplyGrants struct {
	mu sync.Mutex
	m  map[string]interactions.HeldCap
}

// cardReplyGrantLimit bounds the grants kept; past it the ones that expire
// first go.
const cardReplyGrantLimit = 1024

// put keeps h, the reply grant of a card request from `from` on ix.
func (g *cardReplyGrants) put(from, ix string, h interactions.HeldCap, now uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]interactions.HeldCap{}
	}
	for k, v := range g.m {
		if v.CapExp <= now {
			delete(g.m, k)
		}
	}
	for len(g.m) >= cardReplyGrantLimit {
		var first string
		var exp uint64
		for k, v := range g.m {
			if first == "" || v.CapExp < exp {
				first, exp = k, v.CapExp
			}
		}
		delete(g.m, first)
	}
	g.m[from+"\x00"+ix] = h
}

// take returns and forgets the reply grant for a card answer to `to` on ix,
// when one is kept and has not expired.
func (g *cardReplyGrants) take(to, ix string, now uint64) (interactions.HeldCap, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := to + "\x00" + ix
	h, ok := g.m[k]
	delete(g.m, k)
	if !ok || h.CapExp <= now {
		return interactions.HeldCap{}, false
	}
	return h, true
}

// cardReplyGrant is the credential the one-shot sender seals an anet.card/1
// to `to` on ix under (ENG-1b's accessFor, one-shot mode): the capability
// the request brought, taken once. ok false: none, and the answer is not
// sent (ENG-K2).
func (d *Daemon) cardReplyGrant(to, ix string) (interactions.HeldCap, bool) {
	return d.cardGrants.take(to, ix, d.nowMS())
}
