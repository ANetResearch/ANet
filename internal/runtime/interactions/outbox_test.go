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

// The retry loop reads due rows by id; a row deleted in a transaction that
// rolls back is still there, and one deleted in a committed transaction
// goes with the message metadata written beside it.
func TestOutboxIDsAndTransactionalDelete(t *testing.T) {
	s := open(t)
	due, _ := s.EnqueueOutbox(interactions.OutboxItem{IX: "ix_1", ToAID: "peer", Type: "anet.message/1", Body: []byte("a"), NextAt: 10})
	later, _ := s.EnqueueOutbox(interactions.OutboxItem{IX: "ix_1", ToAID: "peer", Type: "anet.message/1", Body: []byte("b"), NextAt: 20})
	if ids, err := s.DueOutboxIDs(15, 10); err != nil || len(ids) != 1 || ids[0] != due {
		t.Fatalf("due ids = %v (%v), want [%d]", ids, err, due)
	}
	if err := s.Put("ix_1", interactions.RoleOutbound, "peer", "goal", "", nil); err != nil {
		t.Fatal(err)
	}
	seq, _, err := s.AddMessageRecord(interactions.MessageRecord{InteractionID: "ix_1", SenderAID: "me",
		Kind: interactions.MsgText, Body: "goal", MsgID: "m1", Metadata: []byte(`{"a2a.messageId":"c1","k":"v"}`)})
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := s.Update(func(tx *interactions.Tx) error {
		if err := tx.DeleteOutbox(due); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := s.GetOutbox(due); err != nil {
		t.Fatal("a delete that rolled back removed the row")
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		if err := tx.MergeMessageMeta("ix_1", seq, map[string]any{"a2a.messageId": nil}); err != nil {
			return err
		}
		return tx.DeleteOutbox(due)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOutbox(due); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("deleted row: %v", err)
	}
	if _, err := s.GetOutbox(later); err != nil {
		t.Fatal(err)
	}
	m, err := s.MessageByMsgID("ix_1", "m1")
	if err != nil || m.Metadata != `{"k":"v"}` {
		t.Fatalf("message after the merge = %+v (%v)", m, err)
	}
}
