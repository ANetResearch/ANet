//go:build !no_x402

package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// /redeem signs only to the hub the operator confirmed (pay_to, which
// `anet redeem` takes from /payments/status {"hub": true} and shows): a
// request without it is refused, one naming another AID is refused, and
// after the node moves to another hub the AID confirmed for the old one is
// refused too — each before anything is signed. [mut] drop the comparison
// in module/x402 Redeem, or key its hub cache by nothing → red.
func TestRedeemIsSignedOnlyToTheConfirmedHub(t *testing.T) {
	one, two := newFakeHub(t), newFakeHub(t)
	d := registered(t, one.URL, "redeemer")
	d.mu.Lock()
	d.cfg.HubURL = one.URL
	d.mu.Unlock()
	grantOn(one.URL, d.AID(), 40)
	token, err := loadOrGenControlToken(d.layout)
	if err != nil {
		t.Fatal(err)
	}
	p := newPlaneFor(t, d, token)
	post := func(path, body string) (int, map[string]any) {
		t.Helper()
		resp, raw := p.req(t, "POST", path, body, p.bearer)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}

	code, st := post("/payments/status", `{"hub":true}`)
	if code != http.StatusOK || st["hub"] != one.URL || st["hub_aid"] != hubAIDOf(one.URL) {
		t.Fatalf("/payments/status {hub}: %d %v", code, st)
	}
	if code, st := post("/payments/status", ``); code != http.StatusOK || st["hub_aid"] != nil {
		t.Fatalf("/payments/status without a body: %d %v", code, st)
	}

	for _, c := range []struct {
		body   string
		code   int
		reason string
	}{
		{`{"amount":3,"reference":"r-none"}`, http.StatusBadRequest, "pay_to_required"},
		{`{"amount":3,"reference":"r-other","pay_to":"` + hubAIDOf(two.URL) + `"}`, http.StatusConflict, "payee_mismatch"},
	} {
		if code, out := post("/redeem", c.body); code != c.code || out["reason"] != c.reason {
			t.Errorf("%s: %d %v, want %d %s", c.body, code, out, c.code, c.reason)
		}
	}
	if n := chainEvents(t, d, "anet.payment.authorized"); n != 0 {
		t.Fatalf("%d authorizations signed for refused redemptions", n)
	}
	if code, out := post("/redeem", `{"amount":3,"reference":"r-one","pay_to":"`+hubAIDOf(one.URL)+`"}`); code != http.StatusOK || out["verified"] != true {
		t.Fatalf("a redemption to the confirmed hub: %d %v", code, out)
	}

	// The node moves to the second hub. A redemption confirmed for the
	// first — the question asked before the move, answered after — is
	// refused; confirmed for the second, it is signed to the second.
	if err := d.HubRegister(context.Background(), two.URL, "redeemer", nil, ""); err != nil {
		t.Fatal(err)
	}
	d.stopRelayLoop()
	grantOn(two.URL, d.AID(), 40)
	if code, out := post("/redeem", `{"amount":2,"reference":"r-stale","pay_to":"`+hubAIDOf(one.URL)+`"}`); code != http.StatusConflict {
		t.Fatalf("a redemption confirmed for the hub this node left: %d %v", code, out)
	}
	if code, out := post("/redeem", `{"amount":2,"reference":"r-two","pay_to":"`+hubAIDOf(two.URL)+`"}`); code != http.StatusOK || out["verified"] != true {
		t.Fatalf("a redemption to the new hub: %d %v", code, out)
	}
	if b1, b2 := balanceOf(one.URL, d.AID()), balanceOf(two.URL, d.AID()); b1 != 37 || b2 != 38 {
		t.Fatalf("balances %d at the first hub, %d at the second; want 37 and 38", b1, b2)
	}
}
