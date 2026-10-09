package interactions_test

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// enqueueOutbox queues it in a transaction of its own and returns its row,
// filling in a message id and an exp (14 days on) when it has none.
func enqueueOutbox(t testing.TB, s *interactions.Store, it interactions.OutboxItem) int64 {
	t.Helper()
	id, err := tryEnqueueOutbox(s, it)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// tryEnqueueOutbox is enqueueOutbox returning the error.
func tryEnqueueOutbox(s *interactions.Store, it interactions.OutboxItem) (int64, error) {
	fillOutboxItem(&it)
	var id int64
	err := s.Update(func(tx *interactions.Tx) error {
		var err error
		id, _, err = tx.EnqueueOutbox(it)
		return err
	})
	return id, err
}

// fillOutboxItem gives it a message id, a ts and an exp when it has none.
func fillOutboxItem(it *interactions.OutboxItem) {
	if it.MID == nil {
		it.MID = make([]byte, 16)
		_, _ = rand.Read(it.MID)
	}
	now := uint64(time.Now().UnixMilli())
	if it.TS == 0 {
		it.TS = now
	}
	if it.Exp == 0 {
		it.Exp = it.TS + 14*24*3600*1000
	}
}
