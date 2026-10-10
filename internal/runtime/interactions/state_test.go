package interactions_test

import (
	"errors"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// IsTerminal is the Go side of the terminal set; the guarded SQL writes
// carry the same set as a literal. This pins both to the four states of
// A2A-DESIGN §4.1.
func TestTerminalStates(t *testing.T) {
	want := map[interactions.State]bool{
		interactions.StateSubmitted:     false,
		interactions.StateWorking:       false,
		interactions.StateInputRequired: false,
		interactions.StateCompleted:     true,
		interactions.StateFailed:        true,
		interactions.StateCanceled:      true,
		interactions.StateRejected:      true,
	}
	s := open(t)
	for st, term := range want {
		if st.IsTerminal() != term {
			t.Errorf("%s: IsTerminal = %v, want %v", st, st.IsTerminal(), term)
		}
		id := "ix_" + string(st)
		if err := s.Put(id, interactions.RoleOutbound, "did:anet:p", "g", "", nil); err != nil {
			t.Fatal(err)
		}
		if st != interactions.StateSubmitted {
			if _, err := s.SetState(id, st, interactions.CauseLocalEnded.When(st)); err != nil {
				t.Fatal(err)
			}
		}
		// The SQL guard must agree: a write after a terminal state is refused.
		changed, err := s.SetState(id, interactions.StateWorking, interactions.CauseNotTerminal)
		if err != nil {
			t.Fatal(err)
		}
		if st == interactions.StateWorking {
			continue // same state, no change either way
		}
		if changed == term {
			t.Errorf("%s: SetState(working) changed=%v, want %v", st, changed, !term)
		}
	}
}

// A terminal state is never overwritten, a result is not stored on a
// canceled interaction, and state_seq advances on every real change and
// not on a repeated one (a waiter compares sequence numbers, C35).
func TestStateWritesAreGuarded(t *testing.T) {
	s := open(t)
	const id = "ix_g"
	if err := s.Put(id, interactions.RoleInbound, "did:anet:r", "g", "", nil); err != nil {
		t.Fatal(err)
	}
	ix0, _ := s.Get(id)
	if ix0.State != interactions.StateSubmitted || ix0.StateSeq != 1 || ix0.StateAt == 0 {
		t.Fatalf("new interaction: state %s seq %d at %d", ix0.State, ix0.StateSeq, ix0.StateAt)
	}
	if ch, err := s.SetState(id, interactions.StateWorking, interactions.CauseNotTerminal); err != nil || !ch {
		t.Fatalf("submitted→working: changed=%v err=%v", ch, err)
	}
	if ch, err := s.SetState(id, interactions.StateWorking, interactions.CauseNotTerminal); err != nil || ch {
		t.Fatalf("working→working must not count as a change: changed=%v err=%v", ch, err)
	}
	ix1, _ := s.Get(id)
	if ix1.StateSeq != 2 {
		t.Fatalf("state_seq = %d after one change, want 2", ix1.StateSeq)
	}
	if ch, err := s.SetState(id, interactions.StateCanceled, interactions.CauseProviderEnd); err != nil || !ch {
		t.Fatalf("working→canceled: changed=%v err=%v", ch, err)
	}
	// SetResult after cancel leaves the interaction canceled with no receipt.
	err := s.SetResult(id, []byte("late"), "cid", []byte("RECEIPT"), interactions.VerificationVerified, interactions.CauseProviderEnd)
	if !errors.Is(err, interactions.ErrTerminal) {
		t.Fatalf("SetResult on canceled: err = %v, want ErrTerminal", err)
	}
	if ch, err := s.SetState(id, interactions.StateCompleted, interactions.CauseProviderEnd); err != nil || ch {
		t.Fatalf("canceled→completed: changed=%v err=%v", ch, err)
	}
	ix2, _ := s.Get(id)
	if ix2.State != interactions.StateCanceled || len(ix2.Receipt) != 0 || ix2.StateSeq != 3 {
		t.Fatalf("after refused writes: state %s receipt %q seq %d", ix2.State, ix2.Receipt, ix2.StateSeq)
	}
	// An absent row is ErrNotFound, not a silent no-op.
	if _, err := s.SetState("ix_absent", interactions.StateWorking, interactions.CauseNotTerminal); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("SetState on absent row: %v", err)
	}
	if err := s.SetFailed("ix_absent", nil, interactions.CauseProviderEnd); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("SetFailed on absent row: %v", err)
	}
}

// Peer key material held on a public interaction is removed when the
// interaction ends (A2A-DESIGN §3.8).
func TestPeerKeysAreDeletedAtTerminal(t *testing.T) {
	s := open(t)
	if err := s.Create(interactions.New{ID: "ix_p", Role: interactions.RoleInbound, PeerAID: "did:anet:r",
		Goal: "net.echo", Trust: interactions.TrustPublicCap, IsCapability: true,
		PeerKEL: []byte("KEL"), PeerKeys: []byte("KEYS")}); err != nil {
		t.Fatal(err)
	}
	ix, _ := s.Get("ix_p")
	if string(ix.PeerKEL) != "KEL" || string(ix.PeerKeys) != "KEYS" || ix.Trust != interactions.TrustPublicCap || !ix.IsCapability {
		t.Fatalf("created row: %+v", ix)
	}
	if err := s.SetResult("ix_p", []byte("{}"), "cid", []byte("R"), interactions.VerificationVerified, interactions.CauseProviderEnd); err != nil {
		t.Fatal(err)
	}
	ix, _ = s.Get("ix_p")
	if len(ix.PeerKEL) != 0 || len(ix.PeerKeys) != 0 {
		t.Fatalf("peer keys kept after completion: kel %q keys %q", ix.PeerKEL, ix.PeerKeys)
	}
}

// ListPage is newest state change first with a stable cursor (R02 D8: the
// old list returned the oldest N rows).
func TestListPageIsNewestFirstWithCursor(t *testing.T) {
	s := open(t)
	clock := int64(1_000_000)
	s.SetClock(func() int64 { return clock })
	for i := 0; i < 250; i++ {
		clock++
		id := "ix_" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if err := s.Put(id, interactions.RoleInbound, "did:anet:r", "g", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	// Touch the oldest row: it must move to the head.
	clock++
	if _, err := s.SetState("ix_aa", interactions.StateWorking, interactions.CauseNotTerminal); err != nil {
		t.Fatal(err)
	}
	var seen []string
	f := interactions.ListFilter{Role: interactions.RoleInbound, Limit: 100}
	for {
		p, err := s.ListPage(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, ix := range p.Items {
			seen = append(seen, ix.ID)
		}
		if p.Next == "" {
			break
		}
		f.Cursor = p.Next
	}
	if len(seen) != 250 {
		t.Fatalf("paged %d rows, want 250", len(seen))
	}
	if seen[0] != "ix_aa" {
		t.Fatalf("head = %s, want the most recently changed ix_aa", seen[0])
	}
	uniq := map[string]bool{}
	for _, id := range seen {
		if uniq[id] {
			t.Fatalf("%s listed twice", id)
		}
		uniq[id] = true
	}
	if n, err := s.Count(interactions.ListFilter{Role: interactions.RoleInbound, Active: true}); err != nil || n != 250 {
		t.Fatalf("count active = %d (%v)", n, err)
	}
	if _, err := s.ListPage(interactions.ListFilter{Cursor: "garbage"}); !errors.Is(err, interactions.ErrBadCursor) {
		t.Fatalf("bad cursor: %v", err)
	}
}

// The approval queue enforces its total and per-peer limits inside the
// transaction, keeps at most the configured number of follow-ups, and an
// item is found by age for expiry.
func TestPendingLimits(t *testing.T) {
	s := open(t)
	put := func(ix, from string, at int64) error {
		return s.Update(func(tx *interactions.Tx) error {
			return tx.PutPending(interactions.PendingItem{IX: ix, FromAID: from, ArrivedAt: at, Delegate: []byte("D")}, 3, 2)
		})
	}
	if err := put("p1", "did:anet:a", 1); err != nil {
		t.Fatal(err)
	}
	if err := put("p2", "did:anet:a", 2); err != nil {
		t.Fatal(err)
	}
	if err := put("p3", "did:anet:a", 3); !errors.Is(err, interactions.ErrPendingFull) {
		t.Fatalf("third item from one peer: %v, want ErrPendingFull", err)
	}
	if err := put("p1", "did:anet:a", 9); err != nil {
		t.Fatalf("re-holding an item already held must be a no-op: %v", err)
	}
	if err := put("p4", "did:anet:b", 4); err != nil {
		t.Fatal(err)
	}
	if err := put("p5", "did:anet:c", 5); !errors.Is(err, interactions.ErrPendingFull) {
		t.Fatalf("fourth item in total: %v, want ErrPendingFull", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Update(func(tx *interactions.Tx) error {
			return tx.AppendPendingFollowup("p1", interactions.PendingFollowup{Kind: "text", Body: "more", MsgID: string(rune('x' + i))}, 2)
		}); err != nil {
			t.Fatal(err)
		}
	}
	err := s.Update(func(tx *interactions.Tx) error {
		return tx.AppendPendingFollowup("p1", interactions.PendingFollowup{Kind: "text", Body: "one too many", MsgID: "z"}, 2)
	})
	if !errors.Is(err, interactions.ErrPendingFollowupsFull) {
		t.Fatalf("third follow-up: %v", err)
	}
	p, _ := s.GetPending("p1")
	if len(p.Followups) != 2 {
		t.Fatalf("follow-ups = %d", len(p.Followups))
	}
	old, _ := s.ExpiredPending(3)
	if len(old) != 2 || old[0].IX != "p1" || old[1].IX != "p2" {
		t.Fatalf("expired = %v", old)
	}
	if ok, _ := s.DeletePending("p1"); !ok {
		t.Fatal("delete held item")
	}
	if _, err := s.GetPending("p1"); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("deleted item still found: %v", err)
	}
}

// The outbox survives closing and reopening the store: it is the part of
// the retry queue that lives on disk, versions and their ack_keys included.
func TestOutboxSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := enqueueOutbox(t, s, interactions.OutboxItem{IX: "ix_o", ToAID: "did:anet:r", Type: "anet.result/1",
		Body: []byte("BODY"), MID: []byte("0123456789abcdef"), TS: 1, Exp: 99999})
	if err := s.Update(func(tx *interactions.Tx) error {
		if _, _, err := tx.SealOutboxVersion(id, interactions.OutboxSeal{AckKey: []byte("K1"), Envelope: []byte("ENV")}); err != nil {
			return err
		}
		return tx.EndOutboxAttempt(id, interactions.OutboxEnd{At: 7, Handed: []uint64{1}, Attempts: 3, NextAt: 12345,
			LastError: "-network"})
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	due, err := s2.DueOutboxIDs(12345, 10)
	if err != nil || len(due) != 1 || due[0] != id {
		t.Fatalf("due after reopen = %v (%v)", due, err)
	}
	row, vers, err := s2.OutboxState(id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Attempts != 3 || row.Exp != 99999 || row.LastError != "-network" || row.HandedAt != 7 || row.CurVer != 1 ||
		len(vers) != 1 || string(vers[0].AckKey) != "K1" || vers[0].HandedAt != 7 {
		t.Fatalf("row after reopen: %+v %+v", row, vers)
	}
	body, env, err := s2.OutboxContent(id)
	if err != nil || string(body) != "BODY" || string(env) != "ENV" {
		t.Fatalf("content after reopen: %q %q %v", body, env, err)
	}
	if early, _ := s2.DueOutboxIDs(12344, 10); len(early) != 0 {
		t.Fatalf("row due before its time: %v", early)
	}
}
