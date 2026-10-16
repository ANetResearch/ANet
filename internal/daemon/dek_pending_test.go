package daemon

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// FS-L1 and FS-L9 through the approval queue: the interaction an approval creates keeps the data
// key its held delegation had; a held delegation the operator rejects, the requester cancels or the
// TTL expires takes its data key with it — once the notices about it that are queued for the
// requester, sealed from what the key keeps, have left the queue (engine ENG-S7, E-T27).
func TestHeldDelegationDataKeyFollowsTheApprovalQueue(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	s := registered(t, srv.URL, "stranger")
	setPolicy(t, prov, PolicyApprove)
	prov.notices.configure(100, 100)
	hold := func(goal string) string {
		t.Helper()
		id, err := s.Delegate(ctx, prov.AID(), goal, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := prov.ix.GetPending(id); err != nil {
			t.Fatalf("%s is not held: %v", id, err)
		}
		return id
	}
	keyGone := func(what, id string) {
		t.Helper()
		if rows := outboxRows(t, prov, id); len(rows) > 0 {
			if _, err := prov.ix.MACKey(id); err != nil {
				t.Errorf("%s: the data key went while %d notices are queued (%v)", what, len(rows), err)
			}
		}
		// The requester takes the notices and the provider hears so.
		waitUntil(t, what+": the notices confirmed", func() bool {
			settleDelivery(t, prov, s)
			return len(outboxRows(t, prov, id)) == 0
		})
		if _, err := prov.ix.MACKey(id); !errors.Is(err, interactions.ErrContentGone) {
			t.Errorf("%s: the held item's data key is still there (%v)", what, err)
		}
	}

	id := hold("approved under the key it was held with")
	// The pending_approval notice leaves the queue first: a queued row of
	// the task keeps its data key by itself (ENG-S7), and the approval must
	// be what carries the key over.
	waitUntil(t, "the pending_approval notice confirmed", func() bool {
		settleDelivery(t, prov, s)
		return len(outboxRows(t, prov, id)) == 0
	})
	held, err := prov.ix.MACKey(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ApprovePending(id); err != nil {
		t.Fatal(err)
	}
	got, err := prov.ix.MACKey(id)
	if err != nil || !bytes.Equal(got, held) {
		t.Fatalf("the approved interaction's data key is not the held item's (%v)", err)
	}
	if ix, err := prov.ix.Get(id); err != nil || ix.Goal != "approved under the key it was held with" {
		t.Fatalf("approved interaction: %+v %v", ix, err)
	}

	rejected := hold("rejected")
	if err := prov.RejectPending(rejected); err != nil {
		t.Fatal(err)
	}
	keyGone("rejected", rejected)

	canceled := hold("canceled")
	if _, err := s.CancelTask(ctx, canceled); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.ix.GetPending(canceled); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("a canceled held item is still held: %v", err)
	}
	keyGone("canceled", canceled)

	expired := hold("expired")
	prov.expirePendingAt(int64(prov.nowMS()) + 73*3600*1000)
	if _, err := prov.ix.GetPending(expired); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("an expired held item is still held: %v", err)
	}
	keyGone("expired", expired)
}
