//go:build !no_x402

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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

	// The real settlement again, with figures of the provider's own beside
	// the hub's receipt: what is stated is what the receipt says.
	again := stored[0]
	again.Amount, again.Payer = "999999", "did:anet:someone"
	again.Extensions = map[string]any{payment.ExtReceipt: stored[0].Extensions[payment.ExtReceipt]}
	mb, _ = json.Marshal(map[string]any{x402a2a.KeyReceipts: append([]any{again}, forged...)})
	body = mustMarshal(t, &delegation.StatusMsg{State: delegation.StateWorking, Metadata: mb,
		At: uint64(time.Now().UnixMilli())})
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeStatus, id, body)); r.class != rxAccepted {
		t.Fatalf("status not accepted: %+v", r)
	}
	var now []payment.SettlementResponse
	_ = json.Unmarshal(getIX(t, req, id).PayReceipts, &now)
	for _, r := range now {
		if r.Transaction == real && (r.Amount != "30" || r.Payer != req.AID()) {
			t.Errorf("the verified settlement states amount %s payer %s, want the receipt's 30 and this node",
				r.Amount, r.Payer)
		}
	}
}

// pvPaid is a capability call this node paid for and saw completed, and
// the one settlement it verified, as stored.
func pvPaid(t *testing.T) (req, prov *Daemon, id string, real payment.SettlementResponse) {
	t.Helper()
	work := &meteredWork{price: 30}
	_, req, prov = paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	id, err := req.DelegateCapability(context.Background(), prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req, prov, req, req)
	rix := getIX(t, req, id)
	var stored []payment.SettlementResponse
	if rix.PayState != interactions.PayCompleted || json.Unmarshal(rix.PayReceipts, &stored) != nil ||
		len(stored) != 1 || !stored[0].Success {
		t.Fatalf("precondition: a paid task, got %q %s", rix.PayState, rix.PayReceipts)
	}
	return req, prov, id, stored[0]
}

// pvSendReceipts has the provider send a working status carrying list as
// x402.payment.receipts.
func pvSendReceipts(t *testing.T, req, prov *Daemon, id string, list []any) {
	t.Helper()
	mb, _ := json.Marshal(map[string]any{x402a2a.KeyReceipts: list})
	body := mustMarshal(t, &delegation.StatusMsg{State: delegation.StateWorking, Metadata: mb,
		At: uint64(time.Now().UnixMilli())})
	if r := receive(t, req, sealFrom(t, prov, req, seal.TypeStatus, id, body)); r.class != rxAccepted {
		t.Fatalf("status not accepted: %+v", r)
	}
}

// pvSettledEvidence is this node's anet.payment.settled records for id.
func pvSettledEvidence(req *Daemon, id string) []map[string]any {
	var out []map[string]any
	req.ledger.scan(EvPaymentSettled, 0, func(_ int64, p map[string]any) {
		if p["interaction_id"] == id {
			out = append(out, p)
		}
	})
	return out
}

// The review of the F11 fix: the hub's genuine receipt for this task,
// restated by the provider under a transaction id of its own. It was
// verified a second time — the node's evidence recorded a second
// settlement (second_receipt) for a task it paid once, the task stated
// two verified settlements, and reconcile then reported the invented
// transaction as missing from the hub "though this node holds the hub's
// signed receipt for it". And the real transaction re-sent with another
// network and other receipt bytes was stored as verified in the form the
// provider rewrote. A settlement this node states is the hub's receipt:
// its transaction is the receipt's authorization id (anet-credit:
// transaction = auth_id), and every field is restated from what this node
// recorded when it verified it [redteam:F11].
func TestAHubReceiptRestatedByTheProviderIsNotASecondSettlement(t *testing.T) {
	req, prov, id, real := pvPaid(t)
	enc, _ := real.Extensions[payment.ExtReceipt].(string)
	alias := real
	alias.Transaction = "tx-alias-of-the-same-settlement"
	alias.Extensions = map[string]any{payment.ExtReceipt: enc}
	relabeled := real
	relabeled.Network = "hub:elsewhere"
	relabeled.Extensions = map[string]any{payment.ExtReceipt: "bm90IHRoZSBodWIncyByZWNlaXB0",
		"anet.replayed": true}
	pvSendReceipts(t, req, prov, id, []any{relabeled, alias})

	verified := 0
	for _, ev := range pvSettledEvidence(req, id) {
		if ev["verified"] == true {
			verified++
			if ev["second_receipt"] == true || ev["transaction"] != real.Transaction {
				t.Errorf("a second verified settlement was recorded: %v", ev)
			}
		}
		if ev["transaction"] == alias.Transaction {
			if r, _ := ev["refused"].(string); ev["verified"] != false || !strings.Contains(r, "transaction") {
				t.Errorf("the alias's evidence: %v", ev)
			}
		}
	}
	if verified != 1 {
		t.Errorf("%d verified settlements recorded for a task paid once", verified)
	}

	v := pvView(t, req, id)
	for _, where := range [][]string{{"metadata"}, {"status", "message", "metadata"}} {
		md, _ := pvPath(v, where...).(map[string]any)
		rc, _ := md[a2ashape.KeyX402Receipts].([]any)
		if len(rc) != 1 {
			t.Errorf("%v states %d settlements, want the one: %v", where, len(rc), rc)
			continue
		}
		got, _ := rc[0].(map[string]any)
		ext, _ := got["extensions"].(map[string]any)
		if got["transaction"] != real.Transaction || got["network"] != real.Network ||
			ext[payment.ExtReceipt] != enc || ext["anet.replayed"] != nil {
			t.Errorf("%v states %v, want the hub's receipt as verified", where, got)
		}
	}
	un, _ := json.Marshal(pvPath(v, "metadata", a2ashape.KeyUnverifiedReceipts))
	if !strings.Contains(string(un), alias.Transaction) {
		t.Errorf("anet.unverified_receipts = %s, want the alias in it", un)
	}
}

// The stored list is bounded, and the verified settlement is in it
// whatever the provider sends. A list shorter than the stored one only
// adds, and it used to add without limit: a provider could grow the row
// by a few made-up transactions per status [redteam:F11].
func TestTheStoredReceiptsStayBoundedAndKeepTheVerifiedSettlement(t *testing.T) {
	req, prov, id, real := pvPaid(t)
	made := func(prefix string, n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = map[string]any{"success": true, "transaction": fmt.Sprintf("%s-%d", prefix, i),
				"network": "hub:x", "amount": "30"}
		}
		return out
	}
	pvSendReceipts(t, req, prov, id, made("tx-long", maxPeerReceipts+10))
	for round := 0; round < 8; round++ {
		pvSendReceipts(t, req, prov, id, made(fmt.Sprintf("tx-short%d", round), 5))
	}
	var stored []json.RawMessage
	if err := json.Unmarshal(getIX(t, req, id).PayReceipts, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) > maxPeerReceipts {
		t.Errorf("%d receipts stored, want at most %d", len(stored), maxPeerReceipts)
	}
	found := false
	for _, r := range stored {
		found = found || verifiedTx(r) == real.Transaction
	}
	if !found {
		t.Errorf("the verified settlement %s is no longer stored", real.Transaction)
	}
	rc, _ := json.Marshal(pvPath(pvView(t, req, id), "metadata", a2ashape.KeyX402Receipts))
	if !strings.Contains(string(rc), real.Transaction) {
		t.Errorf("x402.payment.receipts = %s, want the verified settlement", rc)
	}
}

// notePaymentReceipts reads the ledger before it checks a list, and a
// delivery of the same task arriving at the same time (p2p and hub) may
// verify and store a settlement after that read. The stored list's own
// verified mark keeps it: storing a longer list against the older
// snapshot used to drop it [redteam:F11].
func TestAVerifiedSettlementSurvivesAStaleSnapshot(t *testing.T) {
	req, _, id, real := pvPaid(t)
	longer := []any{
		map[string]any{"success": true, "transaction": "tx-racing-1", "network": "hub:x", "amount": "30"},
		map[string]any{"success": true, "transaction": "tx-racing-2", "network": "hub:x", "amount": "30"},
	}
	req.storePeerReceipts(id, longer, map[string]settledFacts{})
	var stored []json.RawMessage
	_ = json.Unmarshal(getIX(t, req, id).PayReceipts, &stored)
	found := false
	for _, r := range stored {
		found = found || verifiedTx(r) == real.Transaction
	}
	if !found {
		t.Errorf("the verified settlement was dropped: %s", getIX(t, req, id).PayReceipts)
	}
}

// The provider's status and its result both carry the settlement, and
// arrive at once, one by p2p and one through the hub (§3.6). Each found
// the receipt unrecorded and recorded it verified: two settlements of one
// payment on this node's evidence, a task audit reports as paid twice.
// The check is one at a time per task [redteam:F11].
func TestOneSettlementDeliveredTwiceAtOnceIsRecordedOnce(t *testing.T) {
	for round := 0; round < 5; round++ {
		work := &meteredWork{price: 30}
		_, req, prov := paidPair(t, work)
		payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
		id, err := req.DelegateCapability(context.Background(), prov.AID(), "work.do", nil)
		if err != nil {
			t.Fatal(err)
		}
		poll(t, prov, req, prov) // settled at the provider; its answer not yet read here
		var list []any
		if err := json.Unmarshal(getIX(t, prov, id).PayReceipts, &list); err != nil || len(list) != 1 {
			t.Fatalf("provider receipts %s", getIX(t, prov, id).PayReceipts)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				req.notePaymentReceipts(id, map[string]any{x402a2a.KeyReceipts: list}, false)
			}()
		}
		close(start)
		wg.Wait()
		n := 0
		for _, ev := range pvSettledEvidence(req, id) {
			if ev["verified"] == true {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("round %d: %d verified settlement records for one payment", round, n)
		}
	}
}

// A settlement that arrives while this node cannot learn its hub's
// identity — restarted, its hub not answering, the provider's message
// come over p2p (§3.6) — cannot be checked, and is not thereby refused. It
// was recorded once, by transaction, as not checking out, and every later
// delivery of the same settlement was skipped as already recorded: a task
// paid and settled stayed payment-submitted for good, its hub receipt
// listed as the provider's unverified claim [redteam:F11].
func TestASettlementThatCouldNotBeCheckedYetIsCheckedWhenItComesAgain(t *testing.T) {
	work := &meteredWork{price: 30}
	hub, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	id, err := req.DelegateCapability(context.Background(), prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov, req, prov) // quoted, paid, settled; the answer not yet collected
	if rix := getIX(t, req, id); rix.PayState != interactions.PaySubmitted {
		t.Fatalf("precondition: the payment is out, got %q", rix.PayState)
	}
	var list []any
	if pix := getIX(t, prov, id); json.Unmarshal(pix.PayReceipts, &list) != nil || len(list) == 0 {
		t.Fatalf("the provider's receipts: %s", pix.PayReceipts)
	}
	fh := fakeHubAt(t, hub)
	fh.mu.Lock()
	fh.identityDown = true
	fh.mu.Unlock()
	req = reopen(t, req)
	if p := req.payer(); p == nil || p.HomeNetwork() != "" {
		t.Fatal("precondition: this node cannot learn its hub's identity")
	}
	pvSendReceipts(t, req, prov, id, list)
	if rix := getIX(t, req, id); rix.PayState != interactions.PaySubmitted {
		t.Fatalf("an unchecked receipt moved pay_state to %q", rix.PayState)
	}
	if md, _ := pvPath(pvView(t, req, id), "metadata").(map[string]any); md[a2ashape.KeyX402Status] == a2ashape.PaymentCompleted {
		t.Fatal("an unchecked receipt is stated as payment-completed")
	}

	fh.mu.Lock()
	fh.identityDown = false
	fh.mu.Unlock()
	pvSendReceipts(t, req, prov, id, list)
	if rix := getIX(t, req, id); rix.PayState != interactions.PayCompleted {
		t.Fatalf("pay_state = %q once the hub's receipt could be checked", rix.PayState)
	}
	verified := 0
	for _, ev := range pvSettledEvidence(req, id) {
		if ev["verified"] == true {
			verified++
		}
		if ev["second_receipt"] == true {
			t.Errorf("recorded as a second settlement: %v", ev)
		}
	}
	if verified != 1 {
		t.Errorf("%d verified settlements recorded, want 1", verified)
	}
	v := pvView(t, req, id)
	if md, _ := pvPath(v, "metadata").(map[string]any); md[a2ashape.KeyX402Status] != a2ashape.PaymentCompleted {
		t.Errorf("x402.payment.status = %v", md[a2ashape.KeyX402Status])
	}
	for _, where := range [][]string{{"metadata"}, {"status", "message", "metadata"}} {
		md, _ := pvPath(v, where...).(map[string]any)
		if rc, _ := json.Marshal(md[a2ashape.KeyX402Receipts]); !strings.Contains(string(rc), `"verified"`) {
			t.Errorf("%v x402.payment.receipts = %s, want the verified settlement", where, rc)
		}
	}
}
