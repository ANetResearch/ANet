package x402a2a

import "testing"

// The binding is computed by both daemons and stored by the hub, so its
// bytes are a contract: pinned with vectors computed independently of
// this package (Python hashlib).
func TestPayBindIsPinned(t *testing.T) {
	for _, v := range []struct{ ix, nonce, want string }{
		{"ix_000102030405060708090a0b0c0d0e0f", "AAECAwQFBgcICQoLDA0ODw",
			"23040ee9591075d6b67b639cffc495070eeb39da0eba7225bb5097886560c794"},
		{"ix_000102030405060708090a0b0c0d0e0f", "",
			"87b1defd0dedf56c9fe218b414641135dd798027f0e337b4ec93c6d0fbd464ef"},
		{"ix_0123456789abcdef0123456789abcdef", "AAECAwQFBgcICQoLDA0ODw",
			"80eebf319c70ca5038d1954e922139d4e926af5bbfca5f86816a7dc996c7778c"},
	} {
		if got := PayBind(v.ix, v.nonce); got != v.want {
			t.Errorf("PayBind(%q, %q) = %s, want %s", v.ix, v.nonce, got, v.want)
		}
	}
	if PayBind("a", "bc") == PayBind("ab", "c") {
		t.Error("the separator does not separate")
	}
}

// The a2a-x402 strings, pinned: they are read by clients this repository
// does not build.
func TestTheWireStringsArePinned(t *testing.T) {
	for got, want := range map[string]string{
		ExtensionURI: "https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2",
		KeyStatus:    "x402.payment.status", KeyRequired: "x402.payment.required",
		KeyPayload: "x402.payment.payload", KeyReceipts: "x402.payment.receipts", KeyError: "x402.payment.error",
		StatusRequired: "payment-required", StatusSubmitted: "payment-submitted", StatusRejected: "payment-rejected",
		StatusVerified: "payment-verified", StatusCompleted: "payment-completed", StatusFailed: "payment-failed",
		CodeInsufficientFunds: "INSUFFICIENT_FUNDS", CodeInvalidSignature: "INVALID_SIGNATURE",
		CodeExpiredPayment: "EXPIRED_PAYMENT", CodeDuplicateNonce: "DUPLICATE_NONCE",
		CodeNetworkMismatch: "NETWORK_MISMATCH", CodeInvalidAmount: "INVALID_AMOUNT",
		CodeSettlementFailed:        "SETTLEMENT_FAILED",
		KeyAccept:                   "anet.payment.accept",
		ReasonNeedsOperatorApproval: "needs_operator_approval",
		ReasonExtensionNotActivated: "payment_extension_not_activated",
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}

// Activation is by the exact URI; an unknown or near miss is not.
func TestActivated(t *testing.T) {
	if !Activated([]string{"https://example/other", ExtensionURI}) {
		t.Error("the a2a-x402 URI among others does not activate")
	}
	for _, uris := range [][]string{nil, {ExtensionURI + "/"}, {"https://example/other"}} {
		if Activated(uris) {
			t.Errorf("%q activates", uris)
		}
	}
}
