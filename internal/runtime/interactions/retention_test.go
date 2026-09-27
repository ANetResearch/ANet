package interactions_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// PruneTerminal deletes finished interactions of one trust class older than
// the cutoff, with their messages and attachments, and nothing else: not a
// newer one, not an open one, not another trust class, not one whose
// result is still waiting in the retry queue, and not one whose payment is
// submitted and not settled.
func TestPruneTerminalDeletesOnlyOldFinishedCallsOfTheClass(t *testing.T) {
	s := open(t)
	const old, recent, cutoff = int64(1_000_000), int64(9_000_000), int64(5_000_000)
	mk := func(id, trust string, finishAt int64, finish bool) {
		t.Helper()
		s.SetClock(func() int64 { return finishAt })
		if err := s.Create(interactions.New{ID: id, Role: interactions.RoleInbound, PeerAID: "did:anet:caller",
			Goal: "call", Trust: trust, IsCapability: true, PeerKEL: []byte("kel"), PeerKeys: []byte("keys")}); err != nil {
			t.Fatal(err)
		}
		seq, err := s.AddMessage(id, "did:anet:caller", interactions.MsgText, "the caller's arguments")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddAttachment(id, seq, interactions.Attachment{Name: "a.txt", Size: 1, CID: "c", Data: []byte("x")}); err != nil {
			t.Fatal(err)
		}
		if finish {
			if err := s.SetResult(id, []byte("the result"), "cid", []byte("r"), interactions.VerificationVerified); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("ix_old", interactions.TrustPublicCap, old, true)
	mk("ix_recent", interactions.TrustPublicCap, recent, true)
	mk("ix_open", interactions.TrustPublicCap, old, false)
	mk("ix_peer", interactions.TrustPeer, old, true)
	mk("ix_queued", interactions.TrustPublicCap, old, true)
	mk("ix_unsettled", interactions.TrustPublicCap, old, true)
	if _, err := s.SetPayment("ix_unsettled", interactions.PayUpdate{
		State: interactions.PayState(interactions.PaySubmitted), Payload: []byte("signed authorization")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueOutbox(interactions.OutboxItem{IX: "ix_queued", ToAID: "did:anet:caller",
		Type: "anet.result/1", Body: []byte("b"), NextAt: old, CreatedAt: old}); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneTerminal(interactions.TrustPublicCap, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n.Interactions != 1 || n.Messages != 1 || n.Attachments != 1 {
		t.Fatalf("pruned %+v, want exactly ix_old with its one message and one attachment", n)
	}
	if _, err := s.Get("ix_old"); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("ix_old still stored: %v", err)
	}
	if msgs, _ := s.Messages("ix_old"); len(msgs) != 0 {
		t.Fatalf("messages of ix_old kept: %d", len(msgs))
	}
	if atts, _ := s.Attachments("ix_old"); len(atts) != 0 {
		t.Fatalf("attachments of ix_old kept: %d", len(atts))
	}
	for _, id := range []string{"ix_recent", "ix_open", "ix_peer", "ix_queued", "ix_unsettled"} {
		if _, err := s.Get(id); err != nil {
			t.Errorf("%s was deleted: %v", id, err)
		}
		if msgs, _ := s.Messages(id); len(msgs) != 1 {
			t.Errorf("%s: %d messages left", id, len(msgs))
		}
	}
	// A second sweep with the same cutoff finds nothing.
	if n, err := s.PruneTerminal(interactions.TrustPublicCap, cutoff); err != nil || n.Interactions != 0 {
		t.Fatalf("second sweep: %+v %v", n, err)
	}
	if _, err := s.PruneTerminal("", cutoff); !errors.Is(err, interactions.ErrBadInput) {
		t.Fatalf("a sweep without a trust class: %v", err)
	}
}

// A sweep with more rows than one batch deletes them all, over several
// transactions, and counts every batch.
func TestPruneTerminalDeletesInBatches(t *testing.T) {
	defer interactions.SetPruneBatch(2)()
	s := open(t)
	s.SetClock(func() int64 { return 1000 })
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("ix_%d", i)
		if err := s.Create(interactions.New{ID: id, Role: interactions.RoleInbound, PeerAID: "did:anet:caller",
			Goal: "call", Trust: interactions.TrustPublicCap, IsCapability: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddMessage(id, "did:anet:caller", interactions.MsgText, "args"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetResult(id, []byte("r"), "cid", nil, interactions.VerificationVerified); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneTerminal(interactions.TrustPublicCap, 2000)
	if err != nil || n.Interactions != 5 || n.Messages != 5 {
		t.Fatalf("pruned %+v %v, want all five with their messages", n, err)
	}
	if list, err := s.List(interactions.RoleInbound, "", 0, 0); err != nil || len(list) != 0 {
		t.Fatalf("left %d interactions (%v)", len(list), err)
	}
}
