package daemon

import (
	"errors"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/a2acard"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// forgetCardMarks drops node self's cached marks, as a restart does.
func forgetCardMarks(self string) {
	cardMarks.mu.Lock()
	defer cardMarks.mu.Unlock()
	for k := range cardMarks.m {
		if strings.HasPrefix(k, self+"\x00") {
			delete(cardMarks.m, k)
		}
	}
}

// B3-09: the card high water of a peer with a peer_identity row survives a
// restart — the same card again is admitted, a second card under the same
// seq is counted as a fork and refused without replacing the mark, an older
// card is refused — and a stranger's card writes no row.
func TestTheCardHighWaterIsPersistedForKnownPeers(t *testing.T) {
	d := newCardDaemon(t, "Consumer", "")
	self := d.AID()
	const known, stranger = "did:anet:known-peer", "did:anet:stranger"
	if err := d.ix.UpdatePeerIdentity(known, func(*interactions.PeerIdentity) (*interactions.PeerIdentity, error) {
		return &interactions.PeerIdentity{PinnedReason: interactions.PinOutbound}, nil
	}); err != nil {
		t.Fatal(err)
	}
	card := func(aid string, seq uint64, body byte) *a2acard.Verified {
		return &a2acard.Verified{AID: aid, Seq: seq, PayloadHash: [32]byte{body}}
	}
	mark := func() (uint64, byte) {
		p, err := d.ix.PeerIdentity(known)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.CardHash) != 32 {
			return p.CardSeq, 0
		}
		return p.CardSeq, p.CardHash[0]
	}

	if err := d.admitCardMark(card(known, 5, 1)); err != nil {
		t.Fatalf("first card: %v", err)
	}
	if seq, h := mark(); seq != 5 || h != 1 {
		t.Fatalf("stored mark %d/%d, want 5/1", seq, h)
	}
	forgetCardMarks(self) // a restart
	if err := d.admitCardMark(card(known, 5, 1)); err != nil {
		t.Fatalf("the same card after a restart: %v", err)
	}
	forks := cardMarks.forkCount(self)
	forgetCardMarks(self)
	if err := d.admitCardMark(card(known, 5, 2)); !a2acard.IsCode(err, a2acard.CodeSeqFork) {
		t.Fatalf("another card under seq 5 after a restart: %v, want a fork", err)
	}
	if cardMarks.forkCount(self) != forks+1 {
		t.Fatal("the fork was not counted")
	}
	if seq, h := mark(); seq != 5 || h != 1 {
		t.Fatalf("a fork replaced the mark: %d/%d", seq, h)
	}
	forgetCardMarks(self)
	if err := d.admitCardMark(card(known, 4, 1)); !a2acard.IsCode(err, a2acard.CodeSeqRollback) {
		t.Fatalf("an older card after a restart: %v, want a rollback", err)
	}
	if err := d.admitCardMark(card(known, 6, 3)); err != nil {
		t.Fatalf("a newer card: %v", err)
	}
	if seq, h := mark(); seq != 6 || h != 3 {
		t.Fatalf("stored mark %d/%d, want 6/3", seq, h)
	}

	// A mark the cache holds above the row's (admitted while the row did
	// not hold it) is written when that card is seen again, so a restart
	// keeps it.
	if err := cardMarks.admit(self, card(known, 8, 4)); err != nil {
		t.Fatal(err)
	}
	if err := d.admitCardMark(card(known, 8, 4)); err != nil {
		t.Fatalf("the cached card again: %v", err)
	}
	if seq, h := mark(); seq != 8 || h != 4 {
		t.Fatalf("stored mark %d/%d, want the cached 8/4", seq, h)
	}

	// A row that kept a seq but no hash still refuses an older card, and
	// records the hash of a card under its seq.
	const legacy = "did:anet:legacy-peer"
	if err := d.ix.UpdatePeerIdentity(legacy, func(*interactions.PeerIdentity) (*interactions.PeerIdentity, error) {
		return &interactions.PeerIdentity{PinnedReason: interactions.PinOutbound, CardSeq: 5}, nil
	}); err != nil {
		t.Fatal(err)
	}
	forgetCardMarks(self)
	if err := d.admitCardMark(card(legacy, 4, 1)); !a2acard.IsCode(err, a2acard.CodeSeqRollback) {
		t.Fatalf("an older card than a hashless row's seq: %v, want a rollback", err)
	}
	if err := d.admitCardMark(card(legacy, 5, 2)); err != nil {
		t.Fatalf("the card under a hashless row's seq: %v", err)
	}
	if p, err := d.ix.PeerIdentity(legacy); err != nil || p.CardSeq != 5 || len(p.CardHash) != 32 || p.CardHash[0] != 2 {
		t.Fatalf("hashless row after its card: %+v %v", p, err)
	}

	// A stranger: held in memory only.
	if err := d.admitCardMark(card(stranger, 7, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ix.PeerIdentity(stranger); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("a stranger's card wrote a peer_identity row: %v", err)
	}
	if err := d.admitCardMark(card(stranger, 6, 1)); !a2acard.IsCode(err, a2acard.CodeSeqRollback) {
		t.Fatalf("a stranger's older card in the same process: %v, want a rollback", err)
	}
}
