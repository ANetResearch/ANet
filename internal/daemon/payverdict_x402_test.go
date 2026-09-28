//go:build !no_x402

package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
)

// A task this node paid: the settlement it verified is stored with its
// verdict and stated, and the provider cannot take it back or add to it
// afterwards. The stored list used to be replaced by any list from the
// provider at least as long, so a status could hide the real transaction
// behind made-up ones [redteam:F11].
func TestAVerifiedSettlementStaysAndAForgedOneIsNotStated(t *testing.T) {
	work := &meteredWork{price: 30}
	_, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	id, err := req.DelegateCapability(context.Background(), prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req, prov, req, req)
	rix := getIX(t, req, id)
	if rix.State != interactions.StateCompleted || rix.PayState != interactions.PayCompleted {
		t.Fatalf("requester: %s / %q", rix.State, rix.PayState)
	}
	var stored []payment.SettlementResponse
	if json.Unmarshal(rix.PayReceipts, &stored) != nil || len(stored) != 1 || !stored[0].Success {
		t.Fatalf("stored receipts %s", rix.PayReceipts)
	}
	real := stored[0].Transaction
	if stored[0].Extensions[x402a2a.ExtSettlementVerified] != x402a2a.VerdictVerified {
		t.Errorf("the verified settlement is stored without the verdict: %s", rix.PayReceipts)
	}

	// The provider, afterwards, sends a longer history without the real
	// settlement: two made-up ones, one of them marked verified by itself.
	forged := []any{
		map[string]any{"success": true, "transaction": "tx-made-up-1", "network": "hub:x", "amount": "30"},
		map[string]any{"success": true, "transaction": "tx-made-up-2", "network": "hub:x", "amount": "30",
			"extensions": map[string]any{x402a2a.ExtSettlementVerified: x402a2a.VerdictVerified}},
		map[string]any{"success": "true", "transaction": "tx-made-up-3", "network": "hub:x", "amount": "30"},
	}
	mb, _ := json.Marshal(map[string]any{x402a2a.KeyReceipts: forged})
	body := mustMarshal(t, &delegation.StatusMsg{State: delegation.StateWorking, Metadata: mb,
		At: uint64(time.Now().UnixMilli())})
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeStatus, id, body)); r.class != rxAccepted {
		t.Fatalf("status not accepted: %+v", r)
	}
	rix = getIX(t, req, id)
	if !strings.Contains(string(rix.PayReceipts), real) {
		t.Errorf("the verified settlement %s was dropped: %s", real, rix.PayReceipts)
	}
	v := pvView(t, req, id)
	for _, where := range [][]string{{"metadata"}, {"status", "message", "metadata"}} {
		md, _ := pvPath(v, where...).(map[string]any)
		rc, _ := json.Marshal(md[a2ashape.KeyX402Receipts])
		if !strings.Contains(string(rc), real) || strings.Contains(string(rc), "tx-made-up") {
			t.Errorf("%v x402.payment.receipts = %s, want the verified settlement only", where, rc)
		}
		if md[a2ashape.KeyX402Status] != a2ashape.PaymentCompleted {
			t.Errorf("%v x402.payment.status = %v", where, md[a2ashape.KeyX402Status])
		}
	}
	un, _ := json.Marshal(pvPath(v, "metadata", a2ashape.KeyUnverifiedReceipts))
	for _, tx := range []string{"tx-made-up-1", "tx-made-up-2", "tx-made-up-3"} {
		if !strings.Contains(string(un), tx) {
			t.Errorf("anet.unverified_receipts = %s, want %s in it", un, tx)
		}
	}
	if strings.Contains(string(un), real) {
		t.Errorf("the verified settlement is listed as unverified: %s", un)
	}
}
