//go:build !no_a2a

package a2a

// refusal.go writes a refusal of the reason table as the local A2A interface
// reports it (core G1, ux UX-R10): the A2A error the code's row names, the
// sentence as message, and in the JSON-RPC error's data or the REST error's
// details two google.rpc.ErrorInfo — A2A's first, so that a2a-go and other
// standard clients know the error, anet's second, with the code, the command
// and the variant. A passing state carries Retry-After. The handlers that
// refuse with a reason code (UX-R7's send gate, the extended card, a reused
// messageId) answer through these two functions.

import (
	"net/http"
	"strconv"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// writeRefusalRPC answers a JSON-RPC request with r. A JSON-RPC error is an
// HTTP 200 (a JSON-RPC client reads no error body from any other status);
// the error itself carries the code.
func writeRefusalRPC(w http.ResponseWriter, id any, r *a2ashape.Refusal, lang a2ashape.Language) {
	e := r.A2A(lang)
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	type rpcError struct {
		Code    int                  `json:"code"`
		Message string               `json:"message"`
		Data    []a2ashape.ErrorInfo `json:"data"`
	}
	writeJSON(w, http.StatusOK, struct {
		JSONRPC string   `json:"jsonrpc"`
		ID      any      `json:"id"`
		Error   rpcError `json:"error"`
	}{"2.0", id, rpcError{Code: e.RPCCode, Message: e.Message, Data: e.Details}})
}

// writeRefusalREST answers an HTTP+JSON request with r, in the
// google.rpc.Status form of A2A §11.6.
func writeRefusalREST(w http.ResponseWriter, r *a2ashape.Refusal, lang a2ashape.Language) {
	e := r.A2A(lang)
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	type status struct {
		Code    int                  `json:"code"`
		Status  string               `json:"status"`
		Message string               `json:"message"`
		Details []a2ashape.ErrorInfo `json:"details"`
	}
	writeJSON(w, e.HTTPStatus, map[string]status{"error": {
		Code: e.HTTPStatus, Status: e.Status, Message: e.Message, Details: e.Details}})
}
