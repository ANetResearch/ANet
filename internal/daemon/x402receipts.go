package daemon

// The receipts a task's messages carry (A2A-DESIGN §8.2, a2a-x402 v0.2 §7
// and §9.2; 0017 Q18).
//
//   - Every terminal message of a task that took part in the payment flow
//     (pay_state not "") carries x402.payment.receipts, the whole history,
//     possibly empty: the result, the canceled after a declined quote or a
//     cancel, the failed of a lapsed quote, and a terminal status sent
//     through SendStatus.
//   - A failure in that list has transaction "". An anet hub answers a
//     refusal with the refused authorization's id as its transaction; the
//     id is kept as extensions["anet.auth_id"]. The provider normalizes
//     what its hub answered before the list goes anywhere, and the
//     requester normalizes what a provider sent before storing it, so a
//     local client sees one form whoever produced the list.

import (
	"bytes"
	"encoding/json"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
)

// normalizeReceipt is one SettlementResponse as a receipt list carries it:
// a failure's transaction moved to extensions["anet.auth_id"] (an id
// already there is kept) and set to "". A success, a failure already in
// that form, and anything that is not a JSON object come back unchanged.
func normalizeReceipt(raw json.RawMessage) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) != nil || m == nil {
		return raw
	}
	if ok, _ := m["success"].(bool); ok {
		return raw
	}
	tx, _ := m["transaction"].(string)
	if _, present := m["transaction"]; present && tx == "" {
		return raw
	}
	if tx != "" {
		ext, _ := m["extensions"].(map[string]any)
		if ext == nil {
			ext = map[string]any{}
		}
		if _, set := ext[x402a2a.ExtAuthID]; !set {
			ext[x402a2a.ExtAuthID] = tx
		}
		m["extensions"] = ext
	}
	m["transaction"] = ""
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

// normalizeReceipts is normalizeReceipt over a list.
func normalizeReceipts(list []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, len(list))
	for i, r := range list {
		out[i] = normalizeReceipt(r)
	}
	return out
}

// withTerminalReceipts adds x402.payment.receipts to the metadata of a
// terminal status for a task that took part in the payment flow, when the
// caller did not set it. meta may be nil; the result is nil only when
// there is nothing to add to a nil meta.
func withTerminalReceipts(ix *interactions.Interaction, state interactions.State, meta map[string]any) map[string]any {
	if ix == nil || !state.IsTerminal() || ix.PayState == interactions.PayNone {
		return meta
	}
	if _, set := meta[x402a2a.KeyReceipts]; set {
		return meta
	}
	out := make(map[string]any, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}
	out[x402a2a.KeyReceipts] = receiptList(ix)
	return out
}
