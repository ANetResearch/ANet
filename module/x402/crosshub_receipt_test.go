//go:build !no_x402

package x402

import (
	"encoding/base64"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/payment"
)

// signedReceipt is a receipt the hub ledger signed, as the payee's node gets it in a settlement answer.
func signedReceipt(t *testing.T, ledger *identity.Controller, payer, payTo, network string) string {
	t.Helper()
	rc := &payment.Receipt{AuthID: "auth-1", Payer: payer, PayTo: payTo, Amount: 20, Network: network, SettleAt: 1000}
	if err := rc.Sign(ledger); err != nil {
		t.Fatal(err)
	}
	raw, err := rc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// A payment on another hub's ledger comes back with the receipt of that hub, not of the payee's own. The
// payee checks it under the history it pinned for the hub the receipt's network names, and for no other.
func TestAReceiptOfAnotherHubsLedgerIsVerifiedUnderThatHub(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	other, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	network := payment.CreditNetwork(other.AID())
	const payer, payTo = "did:anet:payer", "did:anet:payee"

	// Not held: the receipt is not believed, however well it is signed.
	rc := signedReceipt(t, other, payer, payTo, network)
	if _, ok := m.VerifyReceipt(rc, payer); ok {
		t.Fatal("a receipt of a hub whose key history this node does not hold was verified")
	}
	// Held: it verifies, and says what it says.
	h.peerHubs = map[string][]identity.SignedEvent{other.AID(): other.KEL()}
	facts, ok := m.VerifyReceipt(rc, payer)
	if !ok || facts.Payee != payTo || facts.Amount != 20 || facts.Network != network || facts.AuthID != "auth-1" {
		t.Fatalf("a receipt of the ledger hub, held: ok=%v facts=%+v", ok, facts)
	}
	// Somebody else's payment is not this one.
	if _, ok := m.VerifyReceipt(rc, "did:anet:someone-else"); ok {
		t.Fatal("a receipt for another payer was taken")
	}
	// A hub other than the one the network names cannot sign for it, whatever history is held for it.
	forged := signedReceipt(t, stranger, payer, payTo, network)
	h.peerHubs[stranger.AID()] = stranger.KEL()
	if _, ok := m.VerifyReceipt(forged, payer); ok {
		t.Fatal("a receipt signed by a hub the network does not name was verified")
	}
	// The history held for the named hub is the only one tried: another hub's history does not verify it.
	h.peerHubs[other.AID()] = stranger.KEL()
	if _, ok := m.VerifyReceipt(rc, payer); ok {
		t.Fatal("a receipt was verified under another hub's key history")
	}
	// A receipt on the node's own hub's ledger is still checked under the node's own hub.
	own := signedReceipt(t, h.hub, payer, payTo, payment.CreditNetwork(h.hub.AID()))
	if _, ok := m.VerifyReceipt(own, payer); !ok {
		t.Fatal("a receipt of the node's own hub was not verified")
	}
	// ... and not when a peer hub signed it for the own hub's network.
	spoof := signedReceipt(t, other, payer, payTo, payment.CreditNetwork(h.hub.AID()))
	if _, ok := m.VerifyReceipt(spoof, payer); ok {
		t.Fatal("a peer hub's signature stood for the node's own hub's ledger")
	}
}
