package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// The payee list routes (A2A-DESIGN §8.6): add and remove edit the file
// AdmitSpend reads, keep what else it holds, record every change as
// anet.policy.changed, refuse what is not an AID, and refuse to edit a list
// that is turned off rather than write a file nothing reads.
func TestPayeeListRoutes(t *testing.T) {
	p := newPlane(t)
	d := p.d
	const aid = "did:anet:bpayee000001"
	call := func(path, body string) (int, map[string]any) {
		t.Helper()
		resp, b := p.req(t, "POST", path, body, p.bearer)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return resp.StatusCode, out
	}
	list := func() PayeesStatus {
		t.Helper()
		resp, b := p.req(t, "POST", "/payees/list", `{}`, p.bearer)
		var st PayeesStatus
		if resp.StatusCode != 200 || json.Unmarshal(b, &st) != nil {
			t.Fatalf("list: %d %s", resp.StatusCode, b)
		}
		return st
	}
	if st := list(); !st.Enabled || st.File != defaultPayeesFile || len(st.Payees) != 0 {
		t.Fatalf("a fresh node's payee list: %+v", st)
	}
	path := d.peerFile(defaultPayeesFile)
	if err := os.WriteFile(path, []byte("# who we pay\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.AdmitSpend(aid, 1, module.PurposeTaskManual); err == nil {
		t.Fatal("a payee not on the list was admitted")
	}
	_, prior := d.ledger.Evidence(EvidenceQuery{EventType: EvPolicyChanged})
	if code, out := call("/payees/add", `{"aid":"`+aid+`"}`); code != 200 || out["changed"] != true {
		t.Fatalf("add: %d %v", code, out)
	}
	if code, out := call("/payees/add", `{"aid":"`+aid+`"}`); code != 200 || out["changed"] != false {
		t.Fatalf("add twice: %d %v", code, out)
	}
	if st := list(); strings.Join(st.Payees, ",") != aid {
		t.Fatalf("after add: %+v", st)
	}
	if err := d.AdmitSpend(aid, 1, module.PurposeTaskManual); err != nil {
		t.Fatalf("a payee on the list: %v", err)
	}
	_, recs := d.ledger.Evidence(EvidenceQuery{EventType: EvPolicyChanged})
	if len(recs) != len(prior)+1 || recs[len(recs)-1].Payload["field"] != "payments.payees" ||
		recs[len(recs)-1].Payload["to"] != aid {
		t.Fatalf("policy change evidence after add: %+v", recs[len(prior):])
	}
	if code, out := call("/payees/remove", `{"aid":"`+aid+`"}`); code != 200 || out["changed"] != true {
		t.Fatalf("remove: %d %v", code, out)
	}
	if b, _ := os.ReadFile(path); string(b) != "# who we pay\n" {
		t.Fatalf("payee file after remove: %q", b)
	}
	_, recs = d.ledger.Evidence(EvidenceQuery{EventType: EvPolicyChanged})
	if len(recs) != len(prior)+2 || recs[len(recs)-1].Payload["from"] != aid {
		t.Fatalf("policy change evidence after remove: %+v", recs[len(prior):])
	}
	for _, body := range []string{`{}`, `{"aid":"not an aid"}`, `{"aid":"did:anet:b#x"}`} {
		if code, _ := call("/payees/add", body); code != 400 {
			t.Errorf("add %s: %d, want 400", body, code)
		}
	}
	// The list turned off: nothing to edit, and nothing is written.
	off := ""
	if err := d.SetSpendLimits(nil, &off); err != nil {
		t.Fatal(err)
	}
	if code, out := call("/payees/add", `{"aid":"`+aid+`"}`); code != 409 || !strings.Contains(out["error"].(string), "--payees-file") {
		t.Fatalf("add to a list that is off: %d %v", code, out)
	}
	if st := list(); st.Enabled || st.File != "" {
		t.Fatalf("list off: %+v", st)
	}
}

// /payments/status names the hub and its AID when asked — the payee of a
// redemption, which `anet redeem` shows before it asks — and not
// otherwise, so `anet payments show` does not wait on a hub.
func TestPaymentsStatusNamesTheHubWhenAsked(t *testing.T) {
	srv, req, _ := registeredPair(t)
	hub := srv.URL
	token, err := loadOrGenControlToken(req.layout)
	if err != nil {
		t.Fatal(err)
	}
	p := newPlaneFor(t, req, token)
	resp, b := p.req(t, "POST", "/payments/status", `{"hub":true}`, p.bearer)
	var out map[string]any
	if resp.StatusCode != 200 || json.Unmarshal(b, &out) != nil {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	want, _, err := req.hubIdentity(t.Context(), hub)
	if err != nil {
		t.Fatal(err)
	}
	if out["hub"] != hub || out["hub_aid"] != want || want == "" || out["explicit_max"] == nil {
		t.Fatalf("with hub: %v (hub AID %q)", out, want)
	}
	resp, b = p.req(t, "POST", "/payments/status", `{}`, p.bearer)
	out = nil
	if resp.StatusCode != 200 || json.Unmarshal(b, &out) != nil {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if _, ok := out["hub_aid"]; ok {
		t.Fatalf("without hub: %v", out)
	}
}
