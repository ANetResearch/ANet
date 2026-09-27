package daemon

import (
	"context"
	"errors"
	"testing"
)

// SI-10 with a store failure, across paths — the case the joint run cannot
// stage, because a released daemon has no fault to inject (scripts/joint.sh
// names this test). The copy that arrives over p2p hits a store error: it
// must not be acknowledged, so the sender's peer reports the delivery failed
// and the same bytes come through the hub. That copy is then processed
// once, the capability runs once, and a third copy over p2p is a duplicate
// that is acknowledged and changes nothing.
func TestAStoreFailureOnTheP2PCopyThenRedeliveryThroughTheHubIsProcessedOnce(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "light.onoff@sim/lamp-1", map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())

	prov.rxFault = func(string) error { return errors.New("injected store failure") }
	err = prov.Inbound().Receive(ctx, env)
	prov.rxFault = nil
	if err == nil {
		t.Fatal("a delivery whose store write failed was acknowledged over p2p; the sender would not fall back")
	}
	if _, err := prov.ix.Get(id); err == nil {
		t.Fatal("the rolled-back interaction was stored")
	}
	if len(lamp.invoked) != 0 {
		t.Fatal("the capability ran although the delivery failed")
	}

	// The hub copy: the same bytes, the sender's fallback.
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("after the hub copy the capability ran %d times, want 1", n)
	}
	if _, err := prov.ix.Get(id); err != nil {
		t.Fatalf("the redelivered interaction was not stored: %v", err)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatalf("the hub copy was not acknowledged (%d left)", n)
	}

	// A late p2p copy of the same envelope.
	dups := counter(prov, dropDuplicate)
	msgs := countMsgs(t, prov, id)
	if err := prov.Inbound().Receive(ctx, env); err != nil {
		t.Fatalf("a duplicate over p2p was not acknowledged: %v", err)
	}
	if n := len(lamp.invoked); n != 1 {
		t.Fatalf("a late copy ran the capability again (%d runs)", n)
	}
	if counter(prov, dropDuplicate) != dups+1 {
		t.Fatalf("the late copy was not counted as a duplicate: %v", prov.ReceiveStats())
	}
	if got := countMsgs(t, prov, id); got != msgs {
		t.Fatalf("a late copy changed the conversation (%d → %d lines)", msgs, got)
	}
}
