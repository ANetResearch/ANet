//go:build !no_x402

package daemon

// The account reads (A2A-DESIGN §3.7): the balance, the ledger and the
// redemption list are served to the account holder only, signed with
// relayauth v2, and the fake hub refuses them otherwise — as the real hub
// does, so a daemon that went back to a bare GET would fail here rather
// than on the first real hub it met.

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/relayauth"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/module/x402"
)

func TestLedgerReadsAreSigned(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	d := registered(t, srv.URL, "reader")
	other := registered(t, srv.URL, "other")
	grantOn(srv.URL, d.AID(), 40)
	h := fakeHubAt(t, srv.URL)

	rd, err := d.RedeemCredit(ctx, 3, "inv-signed-1")
	if err != nil || rd["verified"] != true {
		t.Fatalf("redeem: %v %v", rd, err)
	}
	authID, _ := rd["auth_id"].(string)

	h.mu.Lock()
	refusedBefore := h.authFailures
	h.mu.Unlock()
	out, err := d.Balance(ctx)
	if err != nil || out["balance"] != int64(37) {
		t.Fatalf("balance = %v, %v", out, err)
	}
	// The ledger entry of the redemption is under its authorization id, and
	// the redemption list names it by reference: both reached through this
	// node's own control plane, which is how prodtest 9f finds them.
	found := false
	for _, e := range out["entries"].([]map[string]any) {
		if e["reason"] == authID && e["delta"] == float64(-3) {
			found = true
		}
	}
	if !found {
		t.Errorf("the redemption is not in the ledger entries: %v", out["entries"])
	}
	listed := false
	for _, r := range out["redemptions"].([]x402.HubRedemption) {
		listed = listed || (r.Reference == "inv-signed-1" && r.AuthID == authID && r.Amount == 3)
	}
	if !listed {
		t.Errorf("the redemption is not in the list: %v", out["redemptions"])
	}
	// Reconciliation reads all three, signed, and agrees with this node's
	// own record of the redemption.
	rep, err := d.payer().Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r := rep.(x402.ReconcileReport); !r.Agrees || r.RedemptionsMatched != 1 {
		t.Errorf("reconcile: %+v", r)
	}
	h.mu.Lock()
	if h.authFailures != refusedBefore {
		t.Errorf("%d signed account reads were refused", h.authFailures-refusedBefore)
	}
	h.mu.Unlock()

	// Unsigned, or signed by somebody else: 401, and no data.
	for _, action := range []string{relayauth.ActionBalance, relayauth.ActionLedger, relayauth.ActionRedemptions} {
		path := "/agents/" + d.AID() + "/" + action
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("unsigned %s: %d", path, resp.StatusCode)
		}
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		ts := uint64(time.Now().UnixMilli())
		sig, seq := other.self.Sign(relayauth.PreimageV2(action, other.AID(), hubAIDOf(srv.URL), ts,
			http.MethodGet, req.URL.RequestURI(), nil))
		req.Header.Set(hubapi.HeaderAID, other.AID())
		req.Header.Set(hubapi.HeaderTS, strconv.FormatUint(ts, 10))
		req.Header.Set(hubapi.HeaderSeq, strconv.FormatUint(seq, 10))
		req.Header.Set(hubapi.HeaderSig, relayauth.EncodeSig(sig))
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s signed by another agent: %d", path, resp.StatusCode)
		}
	}
}
