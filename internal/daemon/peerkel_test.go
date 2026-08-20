package daemon

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
)

// fakeKEL is a stand-in key history: the cache's bookkeeping does not read
// the events, only counts and replaces them.
func fakeKEL() []identity.SignedEvent { return []identity.SignedEvent{{}} }

// A peer's key history is remembered when its delegation verifies, and only
// then. That bound is the point: this node vouches for peers it has talked
// to and for nobody else, because the hub stores KELs without publishing
// them.
func TestVerifiedPeerKELIsRemembered(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Bob", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}

	if _, ok := prov.peers.resolve(req.AID()); ok {
		t.Fatal("a peer we have never heard from must not resolve")
	}

	if _, err := req.Delegate(ctx, prov.AID(), "do a thing", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	kel, ok := prov.peers.resolve(req.AID())
	if !ok || len(kel) == 0 {
		t.Fatal("after verifying a delegation the peer's key history must be resolvable")
	}
}

// The cache is bounded: a node that has spoken to many peers must not hold
// every key history forever. A forgotten peer is refused and delegates
// again, which is why crude eviction is acceptable here.
func TestPeerKELCacheIsBounded(t *testing.T) {
	p := newPeerKELs()
	p.limit = 3
	for _, aid := range []string{"a", "b", "c", "d"} {
		p.remember(aid, fakeKEL())
	}
	if got := p.len(); got != 3 {
		t.Fatalf("cache holds %d, want the limit of 3", got)
	}
	if _, ok := p.resolve("a"); ok {
		t.Error("the oldest entry should have been evicted")
	}
	if _, ok := p.resolve("d"); !ok {
		t.Error("the newest entry must be present")
	}
}

// A peer that rotates its key must stay verifiable: the later, longer key
// history replaces the earlier one.
func TestPeerKELIsUpdatedNotFrozen(t *testing.T) {
	p := newPeerKELs()
	p.remember("a", fakeKEL())
	first, _ := p.resolve("a")
	p.remember("a", append(fakeKEL(), fakeKEL()...))
	second, _ := p.resolve("a")
	if len(second) <= len(first) {
		t.Fatalf("a rotated key history must replace the old one: %d then %d", len(first), len(second))
	}
	if p.len() != 1 {
		t.Fatalf("updating a peer must not add an entry, len=%d", p.len())
	}
}
