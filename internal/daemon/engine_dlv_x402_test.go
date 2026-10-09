//go:build !no_x402

package daemon

// DLV T1 with a paid call: the payment message is one of the messages an
// empty error from the direct path must not confirm.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// emptyErrorP2P is a direct path whose every send is handed over and
// answered with an empty error frame: no token, ever (DLV T1).
type emptyErrorP2P struct{ sends atomic.Int32 }

func (p *emptyErrorP2P) Name() string                           { return "p2p-empty-error" }
func (p *emptyErrorP2P) Reachable(context.Context, string) bool { return true }
func (p *emptyErrorP2P) Send(context.Context, string, []byte) ([]byte, bool, error) {
	p.sends.Add(1)
	return nil, true, errors.New("")
}

// DLV T1 (ENG-C1): a direct path that answers every send with an empty
// error and no token. The delegation, the result, the payment and a
// cancel all reach their peers through the hub and are confirmed through
// /relay/status; the requester's task does not stay working.
func TestDLVT1_AnEmptyErrorConfirmsNothingAndTheHubCarriesAll(t *testing.T) {
	work := &meteredWork{price: 30}
	hubURL, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	reqP2P, provP2P := &emptyErrorP2P{}, &emptyErrorP2P{}
	req.RegisterTransport(reqP2P)
	prov.RegisterTransport(provP2P)
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req, prov, req)
	if rix := getIX(t, req, id); rix.State != interactions.StateCompleted || rix.PayState != interactions.PayCompleted {
		t.Fatalf("requester: %s / %q", rix.State, rix.PayState)
	}
	// A text task, canceled once it was taken.
	text, err := req.Delegate(ctx, prov.AID(), "to be canceled", nil)
	if err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov)
	if _, err := req.CancelTask(ctx, text); err != nil {
		t.Fatal(err)
	}
	poll(t, prov)
	if st := stateOf(t, prov, text); st != interactions.StateCanceled {
		t.Fatalf("the cancel did not reach the provider: %s", st)
	}
	// Everything confirmed through the hub, nothing through the direct
	// path.
	for i := 0; i < 3; i++ {
		settleDelivery(t, req, prov)
		settleDelivery(t, prov, req)
	}
	if rows := append(allOutboxRows(t, req), allOutboxRows(t, prov)...); len(rows) != 0 {
		t.Fatalf("rows left: %+v", rows)
	}
	if reqP2P.sends.Load() == 0 || provP2P.sends.Load() == 0 {
		t.Fatal("setup: the direct path was never tried")
	}
	if n := counter(req, cntDAckConfirmedP2P) + counter(prov, cntDAckConfirmedP2P); n != 0 {
		t.Fatalf("%d confirmations through a path that gave no token", n)
	}
	if n := counter(req, cntDAckConfirmedHub) + counter(prov, cntDAckConfirmedHub); n == 0 {
		t.Fatal("nothing was confirmed through /relay/status")
	}
	_ = hubURL
}
