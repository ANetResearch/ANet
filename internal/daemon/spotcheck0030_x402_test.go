//go:build !no_x402

package daemon

// F11 spot check after the round-5b merge (docs/notes/0030): a genuine hub
// receipt is not a settlement of any task but the one it was paid for.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The provider takes the hub's real receipt for a payment this node made on
// one task and restates it — same transaction, same receipt bytes — on
// another task this node never paid, twice at once (the p2p and the hub
// copy). The signature and the payer check out; the authorization is not
// one this node signed for that task, so nothing is settled there: no
// pay_state, no settlement keys, the claim listed as unverified. The paid
// task keeps its one verified settlement.
func TestSpotARealReceiptFromAnotherTaskSettlesNothingHere(t *testing.T) {
	req, prov, paid, real := pvPaid(t)
	other, err := req.Delegate(context.Background(), prov.AID(), "a question, nothing to pay", nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pvSendReceipts(t, req, prov, other, []any{real})
		}()
	}
	wg.Wait()
	if oix := getIX(t, req, other); oix.PayState != interactions.PayNone {
		t.Fatalf("pay_state of the unpaid task = %q after another task's receipt", oix.PayState)
	}
	ev := pvSettledEvidence(req, other)
	if len(ev) != 1 || ev[0]["verified"] != false {
		t.Fatalf("evidence for the unpaid task: %v, want one refused record", ev)
	}
	if r, _ := ev[0]["refused"].(string); !strings.Contains(r, "did not sign for the task") {
		t.Errorf("refused because %q, want the authorization not being this task's", r)
	}
	v := pvView(t, req, other)
	pvNoCompletedPayment(t, v, real.Transaction)
	md, _ := pvPath(v, "metadata").(map[string]any)
	for _, k := range []string{a2ashape.KeyX402Status, a2ashape.KeyX402Receipts} {
		if _, ok := md[k]; ok {
			t.Errorf("the unpaid task states %s: %v", k, md[k])
		}
	}
	un, _ := json.Marshal(md[a2ashape.KeyUnverifiedReceipts])
	if !strings.Contains(string(un), real.Transaction) {
		t.Errorf("anet.unverified_receipts = %s, want the restated receipt", un)
	}

	pix := getIX(t, req, paid)
	if pix.PayState != interactions.PayCompleted {
		t.Errorf("the paid task's pay_state = %q", pix.PayState)
	}
	verified := 0
	for _, e := range pvSettledEvidence(req, paid) {
		if e["verified"] == true {
			verified++
		}
	}
	if verified != 1 {
		t.Errorf("the paid task has %d verified settlements, want 1", verified)
	}
}
