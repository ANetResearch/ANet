//go:build !no_x402

package x402

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/ael"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
)

// fakeHub serves the issuance and ledger endpoints a node audits.
//
// It signs its chain with a real key, because the thing under test is
// whether the node verifies signatures — a fake that served unsigned
// records would let the verifier pass by doing nothing.
type fakeIssuer struct {
	ctrl    *identity.Controller
	records []*ael.EventRecord
	entries []map[string]any
	page    int
	balance int64
}

func (f *fakeIssuer) issue(t *testing.T, kind, aid string, amount int64) {
	t.Helper()
	prev := ael.GenesisPrev()
	seq := uint64(0)
	if n := len(f.records); n > 0 {
		prev = f.records[n-1].ID
		seq = f.records[n-1].Seq + 1
	}
	rec := &ael.EventRecord{
		ChainDID: f.ctrl.AID(), Seq: seq, PrevID: prev, EventType: kind,
		VersionMajor: ael.VersionMajor2,
		Payload:      map[string]any{"aid": aid, "amount": amount, "reason": "test"},
		Timestamp:    time.Now().UnixMilli(), CriticalExtensions: []string{},
	}
	if err := rec.Sign(f.ctrl); err != nil {
		t.Fatal(err)
	}
	f.records = append(f.records, rec)
}

func (f *fakeIssuer) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	kelBytes, err := identity.MarshalKEL(f.ctrl.KEL())
	if err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("GET /hub/identity", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"aid": f.ctrl.AID()})
	})
	mux.HandleFunc("GET /agents/{aid}/kel", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"kel": base64.StdEncoding.EncodeToString(kelBytes)})
	})
	mux.HandleFunc("GET /x402/issuance", func(w http.ResponseWriter, _ *http.Request) {
		out := []map[string]any{}
		for _, r := range f.records {
			raw, _ := coredet.Marshal(r)
			out = append(out, map[string]any{
				"seq": r.Seq, "id": r.ID, "prev_id": r.PrevID, "kind": r.EventType,
				"record": base64.StdEncoding.EncodeToString(raw),
			})
		}
		head := ""
		var hseq uint64
		if n := len(f.records); n > 0 {
			head, hseq = f.records[n-1].ID, f.records[n-1].Seq
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"chain_did": f.ctrl.AID(), "entries": out, "head_id": head, "head_seq": hseq})
	})
	mux.HandleFunc("GET /x402/issuance/head", func(w http.ResponseWriter, _ *http.Request) {
		head := ""
		var hseq uint64
		if n := len(f.records); n > 0 {
			head, hseq = f.records[n-1].ID, f.records[n-1].Seq
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"chain_did": f.ctrl.AID(), "seq": hseq, "head_id": head})
	})
	mux.HandleFunc("GET /agents/{aid}/balance", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"credits": f.balance})
	})
	mux.HandleFunc("GET /agents/{aid}/ledger", func(w http.ResponseWriter, _ *http.Request) {
		// Mirrors the hub: entries is a page, total and sum cover the
		// account. f.page caps what is returned so a test can reproduce
		// an account with more history than one page.
		page := f.entries
		if f.page > 0 && len(page) > f.page {
			page = page[:f.page]
		}
		var sum int64
		for _, e := range f.entries {
			sum += asInt64(e["delta"])
		}
		out := map[string]any{
			"entries": page, "total": len(f.entries), "sum": sum,
			"returned": len(page),
		}
		if len(page) < len(f.entries) {
			out["truncated"] = true
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /x402/witness", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "stored"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func hubbedModule(t *testing.T, f *fakeIssuer) (*Module, *testHost) {
	t.Helper()
	srv := f.serve(t)
	h := newHost(t)
	h.hub = f.ctrl
	h.url = srv.URL
	return newModule(t, h), h
}

// A node can verify the hub's supply chain from the signatures alone.
func TestANodeVerifiesTheHubsIssuanceChain(t *testing.T) {
	hub, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{ctrl: hub}
	f.issue(t, evCreditIssued, "did:anet:a", 100)
	f.issue(t, evCreditIssued, "did:anet:b", 500)
	f.issue(t, evCreditRetired, "did:anet:a", 40)
	m, _ := hubbedModule(t, f)

	rep, err := m.auditIssuance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Verified {
		t.Fatalf("a genuine chain did not verify: %v", rep.Problems)
	}
	// Totals come from the signed records, not from the hub's summary.
	if rep.Issued != 600 || rep.Retired != 40 {
		t.Errorf("issued=%d retired=%d, want 600/40", rep.Issued, rep.Retired)
	}
	if rep.Entries != 3 {
		t.Errorf("entries = %d", rep.Entries)
	}
}

// A chain whose records do not verify against the hub's key is refused.
func TestAChainSignedBySomebodyElseIsRefused(t *testing.T) {
	hub, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	impostor, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	// The chain is served by hub but signed by impostor.
	f := &fakeIssuer{ctrl: impostor}
	f.issue(t, evCreditIssued, "did:anet:a", 100)
	srv := f.serve(t)
	h := newHost(t)
	h.hub = hub // the node believes its hub is `hub`
	h.url = srv.URL
	m := newModule(t, h)

	rep, err := m.auditIssuance(context.Background())
	if err != nil {
		// Refusing at the fetch stage is also correct: the hub served a
		// chain for a DID that is not the hub this node settles on.
		if !strings.Contains(err.Error(), "not for itself") {
			t.Fatalf("refused for the wrong reason: %v", err)
		}
		return
	}
	if rep.Verified {
		t.Error("a chain signed by somebody other than this node's hub verified")
	}
}

// The payoff: a hub that rewrites its supply history contradicts a record
// the node already holds, and the node can say exactly where.
func TestAuditDetectsAHubThatRewroteItsSupplyHistory(t *testing.T) {
	hub, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{ctrl: hub}
	f.issue(t, evCreditIssued, "did:anet:a", 100)
	f.issue(t, evCreditIssued, "did:anet:b", 200)
	m, h := hubbedModule(t, f)

	// The node witnesses what it sees now.
	seq, id, err := m.WitnessHub(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || seq != 1 {
		t.Fatalf("witnessed seq=%d id=%q", seq, id)
	}
	// The observation is on the node's OWN chain, which is what the hub
	// cannot reach.
	if len(h.eventsOf(EvIssuanceHeadSeen)) != 1 {
		t.Fatal("the observed head was not recorded on this node's chain")
	}
	// A clean audit right now.
	rep, err := m.auditIssuance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Verified {
		t.Fatalf("an unmodified chain reported problems: %v", rep.Problems)
	}

	// The hub rewrites: the second grant becomes larger, which changes
	// the record and therefore its id.
	f.records = f.records[:1]
	f.issue(t, evCreditIssued, "did:anet:b", 9000)

	rep, err = m.auditIssuance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verified {
		t.Fatal("a rewritten supply history verified — the node's own record was not consulted")
	}
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "rewrote its issuance history") {
		t.Errorf("problems = %v", rep.Problems)
	}
	// The rewritten chain is internally consistent and correctly signed.
	// Only the node's earlier observation reveals it, which is the whole
	// argument for recording heads.
	if !strings.Contains(rep.Problems[0], "seq 1") {
		t.Errorf("the report does not say where: %v", rep.Problems)
	}
}

// Witnessing is off unless asked for.
func TestWitnessingIsOptional(t *testing.T) {
	m, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.(*Module).cfg.WitnessHub {
		t.Error("witnessing defaults to on — a trimmed node would be made to do work it did not ask for")
	}
	on, err := New([]byte(`{"witness_hub":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !on.(*Module).cfg.WitnessHub {
		t.Error("witness_hub:true was not honoured")
	}
}

// Reconciliation compares two records that already existed and were
// never compared: this node's signed history of what it authorized and
// was paid, and the hub's published entries for the same account.
func TestReconcileFindsASettlementTheHubDoesNotAccountFor(t *testing.T) {
	hub, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{ctrl: hub, balance: 75}
	f.entries = []map[string]any{
		{"delta": 100, "reason": "registration grant"},
		{"delta": -25, "reason": "tx-known"},
	}
	m, h := hubbedModule(t, f)

	// This node holds two settlements. One matches an entry the hub
	// published; the other does not.
	_ = h.RecordEvidence(EvPaymentSettled, map[string]any{
		"transaction": "tx-known", "verified": true})
	_ = h.RecordEvidence(EvPaymentSettled, map[string]any{
		"transaction": "tx-missing", "verified": true})

	rep, err := m.reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Matched != 1 {
		t.Errorf("matched = %d, want 1", rep.Matched)
	}
	if len(rep.Missing) != 1 || !strings.Contains(rep.Missing[0], "tx-missing"[:8]) {
		t.Fatalf("missing = %v", rep.Missing)
	}
	// The node holds the hub's signed receipt for it, which is the hub
	// contradicting its own signature rather than merely omitting
	// something.
	if !strings.Contains(rep.Missing[0], "signed receipt") {
		t.Errorf("the report does not say the receipt is held: %v", rep.Missing)
	}
	if rep.Agrees {
		t.Error("a missing settlement was reported as agreement")
	}
}

// A hub whose own two statements disagree is a finding, even though both
// come from the hub.
func TestReconcileCatchesAHubContradictingItself(t *testing.T) {
	hub, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	// The balance says 500; the entries it published sum to 100.
	f := &fakeIssuer{ctrl: hub, balance: 500}
	f.entries = []map[string]any{{"delta": 100, "reason": "registration grant"}}
	m, _ := hubbedModule(t, f)

	rep, err := m.reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Agrees {
		t.Fatal("a hub whose balance and entries disagree was reported as agreeing")
	}
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "sum to 100") {
		t.Errorf("problems = %v", rep.Problems)
	}
	if rep.Balance != 500 || rep.Derived != 100 {
		t.Errorf("balance=%d derived=%d", rep.Balance, rep.Derived)
	}
}

// A clean account reconciles, and a grant is not reported as suspicious.
func TestACleanAccountReconciles(t *testing.T) {
	hub, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{ctrl: hub, balance: 75}
	f.entries = []map[string]any{
		{"delta": 100, "reason": "registration grant"},
		{"delta": -25, "reason": "tx-1"},
	}
	m, h := hubbedModule(t, f)
	_ = h.RecordEvidence(EvPaymentSettled, map[string]any{
		"transaction": "tx-1", "verified": true})

	rep, err := m.reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Agrees {
		t.Fatalf("a clean account did not reconcile: problems=%v missing=%v",
			rep.Problems, rep.Missing)
	}
	// The grant is listed as unexplained-but-normal rather than flagged:
	// credit arriving from the operator has no counterpart on this node,
	// and reporting it as a discrepancy would make every honest account
	// look wrong.
	if len(rep.Unexplained) != 1 || !strings.Contains(rep.Unexplained[0], "normal") {
		t.Errorf("unexplained = %v", rep.Unexplained)
	}
}

// Reconciling must use the account total, not the page it was handed.
//
// The ledger endpoint returns the newest hundred by default and said
// nothing about it, so summing what came back compared a page against a
// balance. Every account with more history than one page reported a
// discrepancy that was the cap rather than the ledger — dmax showed a
// balance of 866 against entries summing to -109, and the check that was
// meant to detect a hub contradicting itself was reporting a difference
// it had manufactured.
func TestReconcileUsesTheAccountTotalNotThePage(t *testing.T) {
	hub, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{ctrl: hub, balance: 300, page: 2}
	// Six entries summing to 300; a two-entry page sums to 200.
	for _, d := range []int64{100, 100, 50, 25, 15, 10} {
		f.entries = append(f.entries, map[string]any{"delta": d, "reason": "registration grant"})
	}
	m, _ := hubbedModule(t, f)

	rep, err := m.reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Derived != 300 {
		t.Errorf("derived = %d, want 300 — the page was summed instead of the account",
			rep.Derived)
	}
	if !rep.Agrees {
		t.Errorf("a consistent account was reported as disagreeing: %v", rep.Problems)
	}
	// And the reader is told there was more than the page, so "the ledger
	// agrees" and "I only saw part of it" stay distinguishable.
	if rep.Entries != 6 || !rep.Truncated {
		t.Errorf("entries=%d truncated=%v, want 6/true", rep.Entries, rep.Truncated)
	}
}
