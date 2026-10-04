package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// testToken is a delivery token as the engine hands it to the receive path:
// the ack_key's handle and a dack of outcome o.
func testToken(t *testing.T, o seal.Outcome, n seal.Note) (seal.AckKey, *rxDAck) {
	t.Helper()
	var k seal.AckKey
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatal(err)
	}
	dk, err := k.MakeDAck(o, n)
	if err != nil {
		t.Fatal(err)
	}
	return k, &rxDAck{handle: k.Handle(), dack: dk}
}

// TestPollAckItems (DLV-R-6a): the /relay/ack item of each outcome class.
// Accepted (A) and refused for good (P) get one: with handle and dack when
// the receive path made a token — the hub stores it for the sender, who
// verifies it under the envelope's ack_key — and with the id alone
// otherwise, counted dack-none. A temporary refusal (T) gets none, with or
// without a token.
func TestPollAckItems(t *testing.T) {
	d := &Daemon{}
	kA, tokA := testToken(t, seal.OutcomeProcessed, seal.Note{})
	kP, tokP := testToken(t, seal.OutcomeRefused, seal.Note{Reason: seal.NoteBusy, RetryAfterS: 30})
	for _, c := range []struct {
		name    string
		r       rxResult
		item    bool
		key     *seal.AckKey
		outcome seal.Outcome
	}{
		{"A with a token", rxResult{class: rxAccepted, dack: tokA}, true, &kA, seal.OutcomeProcessed},
		{"A without a token", rxResult{class: rxAccepted}, true, nil, 0},
		{"P with a token", rxResult{class: rxDropped, reason: dropNotAccepting, dack: tokP}, true, &kP, seal.OutcomeRefused},
		{"P without a token", rxResult{class: rxDropped, reason: dropDuplicate}, true, nil, 0},
		{"T", rxResult{class: rxTransient, reason: transientStore}, false, nil, 0},
		{"T with a token", rxResult{class: rxTransient, reason: transientStore, dack: tokA}, false, nil, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			none := counter(d, dackNone)
			it, ok := d.ackItem(41, c.r)
			if ok != c.item {
				t.Fatalf("item %v, want %v", ok, c.item)
			}
			if !ok {
				if it != (hubapi.RelayAckItem{}) || counter(d, dackNone) != none {
					t.Fatalf("a held row gave %+v and counted dack-none", it)
				}
				return
			}
			if it.ID != 41 {
				t.Fatalf("id %d", it.ID)
			}
			if c.key == nil {
				if it.Handle != "" || it.DAck != "" || counter(d, dackNone) != none+1 {
					t.Fatalf("no token: %+v, dack-none %d → %d", it, none, counter(d, dackNone))
				}
				return
			}
			if counter(d, dackNone) != none {
				t.Fatal("a row with a token counted dack-none")
			}
			h, err := hubapi.DecodeB64(it.Handle, hubapi.HandleLen)
			if want := c.key.Handle(); err != nil || string(h) != string(want[:]) {
				t.Fatalf("handle %q (%v)", it.Handle, err)
			}
			dk, err := hubapi.DecodeB64(it.DAck, hubapi.DAckLen)
			if err != nil {
				t.Fatal(err)
			}
			if _, o, _, _, ok := seal.VerifyDAck([]seal.AckKey{*c.key}, dk); !ok || o != c.outcome {
				t.Fatalf("the item's dack verifies %v as %d, want %d", ok, o, c.outcome)
			}
		})
	}

	// Through pollOnce: an accepted delegation (A) and a mailbox row that is
	// no envelope (P) are acknowledged in one request, each by its id alone —
	// the receive path makes no token yet (engine ENG-1c) — and counted.
	srv, req, prov := registeredPair(t)
	prov.stopRelayLoop()
	ctx := context.Background()
	if _, err := req.Delegate(ctx, prov.AID(), "summarize this", nil); err != nil {
		t.Fatal(err)
	}
	junk := pushJunk(t, srv.URL, prov.AID(), 1)
	h := fakeHubAt(t, srv.URL)
	ids := mailboxIDsFor(srv.URL, prov.AID())
	none := counter(prov, dackNone)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	reqs := ackRequestsOf(h)
	if len(reqs) != 1 || len(reqs[0]) != 2 || reqs[0][0] != (hubapi.RelayAckItem{ID: ids[0]}) || reqs[0][1] != (hubapi.RelayAckItem{ID: junk[0]}) {
		t.Fatalf("ack requests %+v, want one with ids %v and nothing else", reqs, ids)
	}
	if n := counter(prov, dackNone) - none; n != 2 {
		t.Fatalf("dack-none +%d, want 2", n)
	}
	if left := mailboxIDsFor(srv.URL, prov.AID()); len(left) != 0 {
		t.Fatalf("left in the mailbox: %v", left)
	}
}

// A row the receive path holds back (T: the refusal could not be stored,
// so the requester is not told) is not acknowledged and stays in the
// mailbox; once storage recovers, it is refused for good and acknowledged.
func TestAHeldRowIsNotAcked(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov") // refuses every delegation
	req := registered(t, srv.URL, "req")
	recovered := refusedTableFault(t, prov)
	if _, err := req.Delegate(ctx, prov.AID(), "a task", nil); err != nil {
		t.Fatal(err)
	}
	queued := mailboxIDsFor(srv.URL, prov.AID())
	if len(queued) != 1 {
		t.Fatalf("queued %v", queued)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	h := fakeHubAt(t, srv.URL)
	if reqs := ackRequestsOf(h); len(reqs) != 0 {
		t.Fatalf("a held row was acknowledged: %+v", reqs)
	}
	recovered()
	for i := 0; i < 3 && len(mailboxIDsFor(srv.URL, prov.AID())) > 0; i++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	reqs := ackRequestsOf(h)
	if len(reqs) != 1 || len(reqs[0]) != 1 || reqs[0][0] != (hubapi.RelayAckItem{ID: queued[0]}) {
		t.Fatalf("after recovery: %+v", reqs)
	}
}

// T42, the daemon's part: a round of 2500 decided rows is acknowledged in
// three requests of at most 1000 items (DLV-W-14), in mailbox order.
func TestPollOnceAcksInRequestsOf1000(t *testing.T) {
	srv := newFakeHub(t)
	prov := registered(t, srv.URL, "prov")
	ids := pushJunk(t, srv.URL, prov.AID(), 2500)
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	h.pollAll = true
	h.mu.Unlock()
	none := counter(prov, dackNone)
	if err := prov.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs := ackRequestsOf(h)
	if len(reqs) != 3 || len(reqs[0]) != 1000 || len(reqs[1]) != 1000 || len(reqs[2]) != 500 {
		sizes := make([]int, len(reqs))
		for i, r := range reqs {
			sizes[i] = len(r)
		}
		t.Fatalf("ack requests of %v items, want 1000, 1000, 500", sizes)
	}
	k := 0
	for _, r := range reqs {
		for _, it := range r {
			if it != (hubapi.RelayAckItem{ID: ids[k]}) {
				t.Fatalf("item %d: %+v, want id %d alone", k, it, ids[k])
			}
			k++
		}
	}
	if n := counter(prov, dackNone) - none; n != 2500 {
		t.Fatalf("dack-none +%d, want 2500", n)
	}
	if left := mailboxIDsFor(srv.URL, prov.AID()); len(left) != 0 {
		t.Fatalf("%d rows left", len(left))
	}
}

// One ack request that fails leaves only its own rows in the mailbox
// (DLV-R-6a): the other requests of the round are still sent, the poll
// reports the failure, and the rows left are read again by a round that
// goes back, refused the same way and acknowledged.
func TestAFailedAckRequestLeavesOnlyItsRows(t *testing.T) {
	srv := newFakeHub(t)
	prov := registered(t, srv.URL, "prov")
	ids := pushJunk(t, srv.URL, prov.AID(), 2500)
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	h.pollAll, h.ackFail = true, 2
	h.mu.Unlock()
	ctx := context.Background()
	err := prov.pollOnce(ctx)
	var he *hubError
	if !errors.As(err, &he) || he.code != 503 {
		t.Fatalf("pollOnce: %v, want the failed request's 503", err)
	}
	left := mailboxIDsFor(srv.URL, prov.AID())
	if len(left) != 1000 || left[0] != ids[1000] || left[999] != ids[1999] {
		t.Fatalf("left %d rows (%v…), want the second request's 1000", len(left), left[:min(3, len(left))])
	}
	if reqs := ackRequestsOf(h); len(reqs) != 3 {
		t.Fatalf("%d ack requests, want all 3 sent", len(reqs))
	}
	for i := 0; i < relayHeadEvery+2 && len(mailboxIDsFor(srv.URL, prov.AID())) > 0; i++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if left := mailboxIDsFor(srv.URL, prov.AID()); len(left) != 0 {
		t.Fatalf("%d rows still left after the rounds that go back", len(left))
	}
}

// pushJunk queues n rows for aid that are no envelope (a P on the hub
// path) and returns their ids.
func pushJunk(t *testing.T, srvURL, aid string, n int) []int64 {
	t.Helper()
	h := fakeHubAt(t, srvURL)
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]int64, n)
	for i := range ids {
		h.nextID++
		ids[i] = h.nextID
		h.mailbox = append(h.mailbox, &fakeHubMsg{id: h.nextID, toAID: aid, payload: []byte("not an envelope"),
			createdAt: time.Now().UTC().Format(time.RFC3339Nano)})
	}
	return ids
}

// ackRequestsOf is the items of every /relay/ack request the fake took.
func ackRequestsOf(h *fakeHub) [][]hubapi.RelayAckItem {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]hubapi.RelayAckItem(nil), h.ackRequests...)
}
