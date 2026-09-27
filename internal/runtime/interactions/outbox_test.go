package interactions_test

import (
	"errors"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The same body queued again for the same interaction and type is the row
// already there; another type, another interaction or a delivered row make
// a new one.
func TestTheSameMessageIsQueuedOnce(t *testing.T) {
	s := open(t)
	it := interactions.OutboxItem{IX: "ix_1", ToAID: "peer", Type: "anet.result/1", Body: []byte("answer")}
	first, err := s.EnqueueOutbox(it)
	if err != nil {
		t.Fatal(err)
	}
	var again int64
	if err := s.Update(func(tx *interactions.Tx) error {
		var err error
		again, err = tx.EnqueueOutbox(interactions.OutboxItem{IX: "ix_1", ToAID: "peer", Type: "anet.result/1",
			Envelope: []byte("sealed"), Digest: interactions.OutboxDigest([]byte("answer"))})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("the same body queued again made row %d, want %d", again, first)
	}
	other, _ := s.EnqueueOutbox(interactions.OutboxItem{IX: "ix_1", ToAID: "peer", Type: "anet.status/1", Body: []byte("answer")})
	elsewhere, _ := s.EnqueueOutbox(interactions.OutboxItem{IX: "ix_2", ToAID: "peer", Type: "anet.result/1", Body: []byte("answer")})
	if other == first || elsewhere == first || other == elsewhere {
		t.Fatalf("rows %d %d %d, want three", first, other, elsewhere)
	}
	if n, _ := s.OutboxLen(); n != 3 {
		t.Fatalf("%d rows, want 3", n)
	}
	if err := s.DeleteOutbox(first); err != nil {
		t.Fatal(err)
	}
	if next, _ := s.EnqueueOutbox(it); next == first {
		t.Fatal("a delivered row's id was returned for a new message")
	}
}

// A row keeps its message id and deadline; a row without a deadline (from
// before they were kept) is given 14 days from when it was queued.
func TestAnOutboxRowKeepsItsIDAndDeadline(t *testing.T) {
	s := open(t)
	mid := []byte("0123456789abcdef")
	id, err := s.EnqueueOutbox(interactions.OutboxItem{IX: "ix_1", ToAID: "peer", Type: "anet.message/1",
		Body: []byte("hi"), MID: mid, Exp: 12345})
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.GetOutbox(id)
	if err != nil || string(it.MID) != string(mid) || it.Exp != 12345 || it.Deadline() != 12345 || len(it.Digest) != 32 {
		t.Fatalf("row = %+v (%v)", it, err)
	}
	legacy, _ := s.EnqueueOutbox(interactions.OutboxItem{IX: "ix_1", ToAID: "peer", Type: "anet.message/1", Body: []byte("old")})
	it, _ = s.GetOutbox(legacy)
	if it.Deadline() != uint64(it.CreatedAt)+interactions.OutboxLifetimeMS {
		t.Fatalf("legacy deadline %d, created %d", it.Deadline(), it.CreatedAt)
	}
}

// A message is found by the id its sender recorded it under.
func TestAMessageIsFoundByItsID(t *testing.T) {
	s := open(t)
	if err := s.Put("ix_1", interactions.RoleOutbound, "peer", "goal", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddMessageRecord(interactions.MessageRecord{InteractionID: "ix_1", SenderAID: "me",
		Kind: interactions.MsgEndRequest, MsgID: "abcd"}); err != nil {
		t.Fatal(err)
	}
	m, err := s.MessageByMsgID("ix_1", "abcd")
	if err != nil || m.Kind != interactions.MsgEndRequest {
		t.Fatalf("message = %+v (%v)", m, err)
	}
	if _, err := s.MessageByMsgID("ix_1", "nope"); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := s.MessageByMsgID("ix_2", "abcd"); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("another interaction: %v", err)
	}
}

// A late result written in a transaction that rolls back is not stored.
func TestALateResultRollsBackWithItsTransaction(t *testing.T) {
	s := open(t)
	if err := s.Put("ix_1", interactions.RoleOutbound, "peer", "goal", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetState("ix_1", interactions.StateCanceled); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	err := s.Update(func(tx *interactions.Tx) error {
		wrote, err := tx.SetLateResult("ix_1", []byte("R"), "cid", []byte("RC"), interactions.VerificationVerified)
		if err != nil || !wrote {
			t.Fatalf("late result in tx: %v %v", wrote, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if ix, _ := s.Get("ix_1"); len(ix.Receipt) != 0 {
		t.Fatal("a rolled-back late result was stored")
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		_, err := tx.SetLateResult("ix_1", []byte("R"), "cid", []byte("RC"), interactions.VerificationVerified)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ix, _ := s.Get("ix_1"); string(ix.Receipt) != "RC" || ix.State != interactions.StateCanceled {
		t.Fatalf("late result = %+v", ix)
	}
}
