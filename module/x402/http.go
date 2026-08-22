//go:build !no_x402

package x402

import (
	"encoding/json"
	"io"
	"net/http"
)

// The voucher door's own HTTP plumbing.
//
// Copied rather than shared with the kernel's control API on purpose:
// these two faces have opposite postures. The control API is loopback and
// bearer-gated and may assume a friendly caller; this one is PUBLIC and
// may assume nothing. Sharing helpers would eventually mean sharing a
// default, and the default that suits a private door is the wrong one for
// an open port.

// maxRedeemBody bounds a redemption request. Generous enough for
// arguments carrying a small payload, small enough that an open port is
// not a memory faucet.
const maxRedeemBody = 8 << 20

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, into any) error {
	return json.NewDecoder(io.LimitReader(r.Body, maxRedeemBody)).Decode(into)
}
