package daemon

import (
	"sync"

	"github.com/ANetResearch/ANetCore/identity"
)

// peerKELs remembers the key histories this node has already verified.
//
// The daemon learns a peer's KEL every time it accepts a delegation: the
// request carries it, and the signature is checked against it before
// anything else happens. It then threw that away. Nothing needed it — until
// a module did.
//
// A shared blackboard has to answer "whose key signed this contribution",
// and it cannot ask the hub: the hub stores KELs at registration but does
// not publish them, so the only KEL this node can trust is one it verified
// itself. Remembering those is both the cheapest source and the most
// defensible one — this node vouches for exactly the peers it has actually
// talked to, and for nobody it has not.
//
// Bounded on purpose. A node that has spoken to thousands of peers should
// not hold thousands of key histories forever, and the eviction is crude
// because the consequence of a miss is mild: a contribution from a peer we
// have forgotten is refused, and the peer delegates again.
type peerKELs struct {
	mu   sync.RWMutex
	kels map[string][]identity.SignedEvent
	// order records insertion sequence for eviction.
	order []string
	limit int
}

const defaultPeerKELLimit = 512

func newPeerKELs() *peerKELs {
	return &peerKELs{kels: map[string][]identity.SignedEvent{}, limit: defaultPeerKELLimit}
}

// remember stores a KEL the caller has already verified.
//
// Verified is the precondition, not a hope: this is called from the
// delegation ingest path after the signature check, and calling it anywhere
// else would put an unverified key history where callers expect a trusted
// one.
func (p *peerKELs) remember(aid string, kel []identity.SignedEvent) {
	if aid == "" || len(kel) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, seen := p.kels[aid]; !seen {
		p.order = append(p.order, aid)
		if len(p.order) > p.limit {
			oldest := p.order[0]
			p.order = p.order[1:]
			delete(p.kels, oldest)
		}
	}
	// Always overwrite: a later KEL is a longer key history, and refusing
	// to update it would make a peer that rotated its key permanently
	// unverifiable here.
	p.kels[aid] = kel
}

// resolve returns a verified KEL for an AID.
func (p *peerKELs) resolve(aid string) ([]identity.SignedEvent, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	kel, ok := p.kels[aid]
	return kel, ok
}

func (p *peerKELs) len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.kels)
}
