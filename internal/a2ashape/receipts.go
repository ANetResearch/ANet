package a2ashape

import (
	"bytes"
	"encoding/json"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
)

// SplitReceipts sorts a task's stored settlement responses (pay_receipts)
// into those this node states as its x402.payment.receipts and those it
// does not stand behind (A2A-DESIGN §8.2, §8.3; SI-6).
//
// On a task this node provides, every stored response is its own hub's
// answer to its own settle call, and all are its own. On a task it started
// the list came from the provider, which may write anything into it: a
// failure (success exactly false) is kept, since it claims nothing was
// paid; an entry whose success is not a boolean is not a settlement
// response a client should read either way and goes to unverified; a
// success is kept only when
// this node verified it — the daemon writes its verdict into the
// response's extensions (x402a2a.ExtSettlementVerified) when it stores the
// list — and one it could not verify is returned in unverified instead. A
// success stored without a verdict (by a node from before verdicts were
// kept) is kept only when pay_state says this node verified a settlement
// for the task. A provider's claim of settlement used to be stated as this
// node's own, "Payment completed." on a task it had never paid
// [redteam:F11].
func SplitReceipts(list []json.RawMessage, outbound bool, payState string) (own, unverified []json.RawMessage) {
	if !outbound {
		return list, nil
	}
	for _, r := range list {
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(r))
		dec.UseNumber()
		if dec.Decode(&m) != nil || m == nil {
			continue // not a settlement response at all
		}
		switch m["success"] {
		case false:
			own = append(own, r)
			continue
		case true:
		default:
			// "true" as a string, 1, absent: a lenient client could read
			// it as paid, and this node verified nothing of it.
			unverified = append(unverified, r)
			continue
		}
		ext, _ := m["extensions"].(map[string]any)
		switch ext[x402a2a.ExtSettlementVerified] {
		case x402a2a.VerdictVerified:
			own = append(own, r)
		case nil:
			if payState == interactions.PayCompleted {
				own = append(own, r)
			} else {
				unverified = append(unverified, r)
			}
		default:
			unverified = append(unverified, r)
		}
	}
	return own, unverified
}
