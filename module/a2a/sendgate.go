//go:build !no_a2a

package a2a

// sendgate.go answers SendMessage and SendStreamingMessage while this node
// is not registered at its hub (LEC AD-6, LA-2; ux UX-R7): before the
// binding runs, with the kernel's refusal in the two-ErrorInfo form of core
// G1 (refusal.go). a2a-go's own encoding keeps one ErrorInfo, so the
// refusal cannot travel through the handler's error. A passing state
// (hub_pending_admission, hub_retrying) is −32603 / REST 503 with
// Retry-After; a lasting one (no_hub_yet, hub_refused, wire_too_old, …) is
// −32602 / REST 400 FAILED_PRECONDITION. No task is created.

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// methodSend is the JSON-RPC name of SendMessage (A2A §9).
const methodSend = "SendMessage"

// precheckSendGate answers a send while the kernel's send gate is closed,
// and reports whether it did. Any other request, a send that does not even
// parse (the binding answers that), and a request whose A2A-Version the
// interface does not speak (the binding answers that too) go on.
func (s *server) precheckSendGate(w http.ResponseWriter, r *http.Request, aid string, jsonrpc bool) bool {
	if info := infoFrom(r.Context()); info != nil && info.versionErr != nil {
		return false
	}
	var id any
	streaming := false
	if jsonrpc {
		body, err := io.ReadAll(r.Body)
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), errReader{err}))
		if err != nil {
			return false
		}
		var env struct {
			JSONRPC string `json:"jsonrpc"`
			ID      any    `json:"id"`
			Method  string `json:"method"`
		}
		if decodeFirst(body, &env) != nil || env.JSONRPC != "2.0" || !validRPCID(env.ID) ||
			(env.Method != methodSend && env.Method != methodSendStreaming) {
			return false
		}
		id, streaming = env.ID, env.Method == methodSendStreaming
	} else {
		rest := strings.TrimPrefix(r.URL.Path, agentsPath+"/"+aid+"/rest")
		if r.Method != http.MethodPost || (rest != "/message:send" && rest != "/message:stream") {
			return false
		}
	}
	var ref *a2ashape.Refusal
	if !errors.As(s.h.seam.SendGate(r.Context()), &ref) {
		return false
	}
	lang := a2ashape.Lang()
	switch {
	case !jsonrpc:
		writeRefusalREST(w, ref, lang)
	case streaming:
		// A streaming call's 200 is read as an event stream by a2a-go's
		// JSON-RPC client (precheck.go): the refusal goes with the status
		// the HTTP+JSON binding gives its error, so the client sees it.
		writeRefusalRPCStatus(w, id, ref, lang)
	default:
		writeRefusalRPC(w, id, ref, lang)
	}
	return true
}

// writeRefusalRPCStatus is writeRefusalRPC with the HTTP status of the
// error rather than 200.
func writeRefusalRPCStatus(w http.ResponseWriter, id any, r *a2ashape.Refusal, lang a2ashape.Language) {
	e := r.A2A(lang)
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	type rpcError struct {
		Code    int                  `json:"code"`
		Message string               `json:"message"`
		Data    []a2ashape.ErrorInfo `json:"data"`
	}
	writeJSON(w, e.HTTPStatus, struct {
		JSONRPC string   `json:"jsonrpc"`
		ID      any      `json:"id"`
		Error   rpcError `json:"error"`
	}{"2.0", id, rpcError{Code: e.RPCCode, Message: e.Message, Data: e.Details}})
}
