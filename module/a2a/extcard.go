//go:build !no_a2a

package a2a

// extcard.go answers GetExtendedAgentCard (HPP §2.7, §4.7; core CORE-K23):
// the JSON-RPC method of that name and the HTTP+JSON GET …/extendedAgentCard,
// both under the proxy card's interfaces and behind the same bearer.
//
// The kernel fetches the remote agent's extended card (TaskSeam.
// ExtendedCard: the same fetch, the same codes as the CLI and MCP) and this
// file makes the proxy extended card of it, signed by this node like every
// proxy card, and written exactly as signed. Every other outcome is the
// refusal of its code in the two-ErrorInfo form of core G1, which a2a-go's
// own error encoding cannot carry, so the call is answered here, before
// the binding:
//
//	outcome                               A2A error (JSON-RPC / REST)            anet code                               Retry-After
//	CORE-K23 rule 2 does not hold         UnsupportedOperation (-32004 / 400)    extended_card_not_offered               —
//	the card                              the proxy extended card                —                                       —
//	the agent answers not_configured      ExtendedAgentCardNotConfigured (-32007 / 400) extended_card_not_configured     —
//	the agent answers busy                InternalError (-32603 / 503)           extended_card_unavailable               60
//	no answer in time, or not sent        InternalError (-32603 / 503)           extended_card_unavailable               15
//	the card does not verify              InvalidAgentResponse (-32006 / 500)    extended_card_unavailable.bad_signature —
//	this node may not send                its state's row                        the hub or identity code                when passing
//
// A JSON-RPC error is an HTTP 200, as every refusal of this interface is
// (refusal.go). The card goes with the proxy card's caching headers.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// methodGetExtendedCard is the JSON-RPC method (A2A §9).
const methodGetExtendedCard = "GetExtendedAgentCard"

// restExtendedCard is the HTTP+JSON path under an agent's REST interface.
const restExtendedCard = "/extendedAgentCard"

// extendedCardCall reads r as a GetExtendedAgentCard call: ok, and the
// JSON-RPC id. The body is put back for the binding.
func extendedCardCall(r *http.Request, aid string, jsonrpc bool) (id any, ok bool) {
	if !jsonrpc {
		rest := strings.TrimPrefix(r.URL.Path, agentsPath+"/"+aid+"/rest")
		return nil, r.Method == http.MethodGet && rest == restExtendedCard
	}
	body, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), errReader{err}))
	if err != nil {
		return nil, false
	}
	var env struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Method  string `json:"method"`
	}
	if decodeFirst(body, &env) != nil || env.JSONRPC != "2.0" || !validRPCID(env.ID) || env.Method != methodGetExtendedCard {
		return nil, false
	}
	return env.ID, true
}

// precheckExtendedCard answers a GetExtendedAgentCard call, and reports
// whether r was one.
func (s *server) precheckExtendedCard(w http.ResponseWriter, r *http.Request, aid string, jsonrpc bool) bool {
	id, ok := extendedCardCall(r, aid, jsonrpc)
	if !ok {
		return false
	}
	body, err := s.extendedCard(r.Context(), aid)
	if err != nil {
		var ref *a2ashape.Refusal
		switch {
		case errors.As(err, &ref) && jsonrpc:
			writeRefusalRPC(w, id, ref, a2ashape.Lang())
		case errors.As(err, &ref):
			writeRefusalREST(w, ref, a2ashape.Lang())
		default:
			err = toSDKError(err)
			if jsonrpc {
				writeRPCErrorFor(w, id, err)
			} else {
				writeError(w, restStatus(err), err, err.Error())
			}
		}
		return true
	}
	if !jsonrpc {
		writeCacheable(w, r, body)
		return true
	}
	// The card in the result exactly as signed.
	idJSON, err := json.Marshal(id)
	if err != nil {
		idJSON = []byte("null")
	}
	setCacheHeaders(w, body)
	w.Header().Set("Content-Type", mediaJSON)
	w.WriteHeader(http.StatusOK)
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.Write(idJSON)
	b.WriteString(`,"result":`)
	b.Write(body)
	b.WriteString("}\n")
	_, _ = w.Write(b.Bytes())
	return true
}

// extendedCard is the signed proxy extended card of aid, or why there is
// none.
func (s *server) extendedCard(ctx context.Context, aid string) ([]byte, error) {
	ea, err := s.seam.ExtendedCard(ctx, aid)
	if err != nil {
		return nil, err
	}
	if ea.Agent.AID != aid {
		return nil, fmt.Errorf("the extended card answered for another agent")
	}
	card, _, err := s.cards.builder.buildExtended(ea)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(card)
	if err != nil {
		return nil, err
	}
	if s.cards.signer != nil {
		if body, err = s.cards.signer.SignProxyCard(body); err != nil {
			return nil, fmt.Errorf("sign: %w", err)
		}
	}
	return body, nil
}

// extendedAgentCard is the handler's GetExtendedAgentCard, for a call that
// reaches the binding (precheckExtendedCard answers the ones that come over
// HTTP): the same card, or the A2A error of the same refusal.
func (s *server) extendedAgentCard(ctx context.Context, aid string) (*a2a.AgentCard, error) {
	body, err := s.extendedCard(ctx, aid)
	if err != nil {
		return nil, toSDKError(err)
	}
	var c a2a.AgentCard
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, a2a.NewError(a2a.ErrInternalError, "the proxy extended card could not be read")
	}
	return &c, nil
}

// setCacheHeaders gives a card's answer the proxy card's caching headers
// (writeCacheable): Cache-Control private with a max-age, and the ETag of
// the bytes.
func setCacheHeaders(w http.ResponseWriter, body []byte) string {
	etag := cardETag(body)
	h := w.Header()
	h.Set("Cache-Control", "private, max-age="+strconv.Itoa(cardMaxAge))
	h.Set("ETag", etag)
	return etag
}
