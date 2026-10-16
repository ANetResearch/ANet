//go:build !no_x402

package daemon

import (
	"context"
	"testing"
	"time"
)

// HPP §4.3.8: the account state (probation, trial) comes from the kernel's
// own hub client, with or without a payment module.
func TestBalanceCarriesTheAccountState(t *testing.T) {
	srv := newFakeHub(t)
	d := registered(t, srv.URL, "newcomer")
	grantOn(srv.URL, d.AID(), 40)
	h := fakeHubAt(t, srv.URL)
	until := uint64(time.Now().Add(48 * time.Hour).UnixMilli())
	h.mu.Lock()
	h.acctExtra = map[string]map[string]any{d.AID(): {"probation_until": until, "trial": "pending"}}
	h.mu.Unlock()
	ctx := context.Background()

	out, err := d.Balance(ctx)
	if err != nil || out["balance"] != int64(40) || out["probation_until"] != until || out["trial"] != "pending" {
		t.Fatalf("with the module: %v, %v", out, err)
	}
	// No payment module: the account state still answers; the balance is
	// the module's, and says so.
	d.mu.Lock()
	d.pay = nil
	d.mu.Unlock()
	out, err = d.Balance(ctx)
	if err != nil || out["balance"] != moduleAbsent || out["probation_until"] != until || out["trial"] != "pending" {
		t.Fatalf("without the module: %v, %v", out, err)
	}
	// An account admitted from a list has neither member.
	h.mu.Lock()
	h.acctExtra = nil
	h.mu.Unlock()
	if out, err = d.Balance(ctx); err != nil {
		t.Fatal(err)
	} else if _, ok := out["trial"]; ok {
		t.Errorf("an admitted account shows a trial: %v", out)
	}
}
