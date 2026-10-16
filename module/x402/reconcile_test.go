//go:build !no_x402

package x402

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func recent(h time.Duration) string { return stamp(time.Now().Add(-h).UnixMilli()) }

func reconcileWith(t *testing.T, f *fakeHub) (ReconcileReport, *testHost) {
	t.Helper()
	m, h := f.hubbed()
	_ = m
	rep, err := m.reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rep, h
}

func TestReconcileFindsASettlementTheHubDoesNotAccountFor(t *testing.T) {
	f := newFakeHub(t)
	f.balance = 75
	f.entries = []ledgerEntry{{Seq: 1, Delta: 100, Reason: "issued: grant", At: recent(5 * time.Hour)},
		{Seq: 2, Delta: -25, Reason: "tx-known", At: recent(4 * time.Hour)}}
	m, h := f.hubbed()
	_ = h.RecordEvidence(EvPaymentSettled, map[string]any{"transaction": "tx-known", "verified": true})
	_ = h.RecordEvidence(EvPaymentSettled, map[string]any{"transaction": "tx-missing", "verified": true})
	rep, err := m.reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Matched != 1 || len(rep.Missing) != 1 || !strings.Contains(rep.Missing[0], "signed receipt") || rep.Agrees {
		t.Fatalf("matched %d, missing %v, agrees %v", rep.Matched, rep.Missing, rep.Agrees)
	}
}

// The ledger keeps 14 days: the entries add up to how the balance moved in
// the window, and the balance before the window is the difference. A
// payment older than the window is not held against the hub.
func TestReconcileComparesTheWindowOnly(t *testing.T) {
	f := newFakeHub(t)
	f.balance = 300
	f.entries = []ledgerEntry{{Seq: 40, Delta: 20, Reason: "tx-a", At: recent(48 * time.Hour)},
		{Seq: 41, Delta: -5, Reason: "tx-b", At: recent(24 * time.Hour)}}
	m, h := f.hubbed()
	_ = h.RecordEvidence(EvPaymentSettled, map[string]any{"transaction": "tx-a", "verified": true})
	_ = h.RecordEvidence(EvPaymentSettled, map[string]any{"transaction": "tx-old", "verified": true,
		"_recorded_at": time.Now().Add(-30 * 24 * time.Hour).UnixMilli()})
	rep, err := m.reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.WindowDays != 14 || rep.WindowSum != 15 || rep.OpeningBalance != 285 {
		t.Errorf("window %d days, sum %d, opening %d", rep.WindowDays, rep.WindowSum, rep.OpeningBalance)
	}
	if rep.OutsideWindow != 1 || len(rep.Missing) != 0 || !rep.Agrees {
		t.Errorf("outside %d, missing %v, agrees %v, problems %v", rep.OutsideWindow, rep.Missing, rep.Agrees, rep.Problems)
	}
}

// The newest proof, extended by the ledger since its close, must give the
// balance now.
func TestReconcileChecksTheBalanceAgainstTheLastProof(t *testing.T) {
	f := newFakeHub(t)
	d := today()
	f.entries = []ledgerEntry{{Seq: 12, Delta: 30, Reason: "tx-in", At: recent(2 * time.Hour)}}
	f.addEpoch(d-1, 100, 10)
	f.balance = 140 // 100 + 30 would be 130
	m, _ := f.hubbed()
	if rep := mustAudit(t, m); !rep.Verified {
		t.Fatalf("audit: %+v", rep)
	}
	rep, err := m.reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Agrees || len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "add up to 130") {
		t.Fatalf("problems: %v", rep.Problems)
	}
	f.balance = 130
	if rep, _ := m.reconcile(context.Background()); !rep.Agrees {
		t.Errorf("a balance that continues the proof disagrees: %v", rep.Problems)
	}
}

func TestReconcileDoesNotHoldTheHubToAProvidersUnverifiedClaim(t *testing.T) {
	f := newFakeHub(t)
	f.balance = 75
	f.entries = []ledgerEntry{{Seq: 1, Delta: 100, Reason: "issued: grant", At: recent(5 * time.Hour)},
		{Seq: 2, Delta: -25, Reason: "bafy-auth-1", At: recent(4 * time.Hour)}}
	m, h := f.hubbed()
	_ = h.RecordEvidence(EvPaymentSettled, map[string]any{"transaction": "tx-made-up", "verified": false})
	_ = h.RecordEvidence(EvPaymentSettled, map[string]any{"transaction": "tx-named", "auth_id": "bafy-auth-1", "verified": true})
	rep, err := m.reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Missing) != 0 || !rep.Agrees || rep.Matched != 1 {
		t.Errorf("missing = %v, agrees = %v, matched = %d", rep.Missing, rep.Agrees, rep.Matched)
	}
}

func TestReconcileFindsRedemptionsInTheHubsList(t *testing.T) {
	f := newFakeHub(t)
	f.balance = 95
	f.entries = []ledgerEntry{{Seq: 1, Delta: 100, Reason: "issued: grant", At: recent(5 * time.Hour)},
		{Seq: 2, Delta: -5, Reason: "auth-r1", At: recent(4 * time.Hour)}}
	f.redemptions = []map[string]any{{"auth_id": "auth-r1", "amount": 5, "reference": "inv-1"}}
	m, h := f.hubbed()
	_ = h.RecordEvidence(EvCreditRedeemed, map[string]any{"amount": 5, "reference": "inv-1", "auth_id": "auth-r1", "verified": true})
	rep, err := m.reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Agrees || rep.RedemptionsMatched != 1 || rep.HubRedemptions != 1 {
		t.Fatalf("a listed redemption: %+v", rep)
	}
	_ = h.RecordEvidence(EvCreditRedeemed, map[string]any{"amount": 7, "reference": "inv-2", "auth_id": "auth-r2", "verified": true})
	rep, err = m.reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Agrees || len(rep.Missing) != 1 || !strings.Contains(rep.Missing[0], "inv-2") {
		t.Fatalf("an unlisted redemption: agrees=%v missing=%v", rep.Agrees, rep.Missing)
	}
	if f.unsigned != 0 {
		t.Errorf("%d account reads went unsigned", f.unsigned)
	}
	resp, err := http.Get(h.url + "/agents/" + h.AID() + "/redemptions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unsigned read of the redemption list: %d", resp.StatusCode)
	}
}

// The window's total, not the page: an account with more entries than a
// page reports the total and that it only saw part.
func TestReconcileUsesTheWindowTotalNotThePage(t *testing.T) {
	f := newFakeHub(t)
	f.balance = 300
	f.page = 2
	for i, d := range []int64{100, 100, 50, 25, 15, 10} {
		f.entries = append(f.entries, ledgerEntry{Seq: uint64(i + 1), Delta: d, Reason: "issued: grant", At: recent(time.Duration(10-i) * time.Hour)})
	}
	rep, _ := reconcileWith(t, f)
	if rep.WindowSum != 300 || rep.OpeningBalance != 0 || rep.Entries != 6 || !rep.Truncated {
		t.Errorf("sum %d opening %d entries %d truncated %v", rep.WindowSum, rep.OpeningBalance, rep.Entries, rep.Truncated)
	}
	if !rep.Agrees {
		t.Errorf("a consistent account disagrees: %v", rep.Problems)
	}
}
