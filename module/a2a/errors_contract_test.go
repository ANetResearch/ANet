//go:build !no_a2a

package a2a

// A2AERR-1 (core §5.2, G1): the shape of a refusal on the local A2A
// interface, as a2a-go's JSON-RPC and REST clients read it. Each client
// recognizes the standard error behind the code; the raw body's data or
// details is an array whose first ErrorInfo is in the A2A domain and whose
// second carries the anet code. The handlers that produce these refusals
// have their trigger paths beside them (the extended card in
// extcard_test.go, a reused messageId, the send gate); this test fixes
// what they write.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// refusalServer answers every request with the refusal of key: JSON-RPC
// under /rpc, REST elsewhere; it keeps the last body and Retry-After.
type refusalServer struct {
	*httptest.Server
	key   string
	body  []byte
	retry string
}

func newRefusalServer(t *testing.T) *refusalServer {
	t.Helper()
	s := &refusalServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		ref := a2ashape.Refuse(s.key, a2ashape.Args{"task": "t_9f3c", "hub": "https://hub.example",
			"aid": "bafyreiujzdegxdncf32epf3dhodzdocis2jhtlgmxgedn73u557hq2abcd"})
		if r.URL.Path == "/rpc" {
			var req struct {
				ID any `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			writeRefusalRPC(rec, req.ID, ref, a2ashape.English)
		} else {
			writeRefusalREST(rec, ref, a2ashape.English)
		}
		s.body, s.retry = rec.Body.Bytes(), rec.Header().Get("Retry-After")
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = io.Copy(w, bytes.NewReader(s.body))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestA2AERR_1(t *testing.T) {
	s := newRefusalServer(t)
	u, _ := url.Parse(s.URL)
	transports := map[string]a2aclient.Transport{
		"jsonrpc": a2aclient.NewJSONRPCTransport(s.URL+"/rpc", s.Client()),
		"rest":    a2aclient.NewRESTTransport(u, s.Client()),
	}
	for _, c := range []struct {
		key   string
		want  error
		retry bool
	}{
		{"no_local_task", a2a.ErrTaskNotFound, false},
		{"extended_card_not_offered", a2a.ErrUnsupportedOperation, false},
		{"extended_card_not_configured", a2a.ErrExtendedCardNotConfigured, false},
		{"extended_card_unavailable", a2a.ErrInternalError, true},
		{"extended_card_unavailable.bad_signature", a2a.ErrInvalidAgentResponse, false},
		{"message_id_reused.open", a2a.ErrInvalidParams, false},
		{"message_id_reused.followup", a2a.ErrInvalidParams, false},
		{"hub_pending_admission", a2a.ErrInternalError, true},
		{"identity_stopped", a2a.ErrInvalidParams, false},
	} {
		for name, tr := range transports {
			s.key = c.key
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := tr.GetTask(ctx, a2aclient.ServiceParams{}, &a2a.GetTaskRequest{ID: "t_9f3c"})
			cancel()
			sentence := a2ashape.Sentence(c.key, a2ashape.English, a2ashape.Args{"task": "t_9f3c", "hub": "https://hub.example",
				"aid": "bafyreiujzdegxdncf32epf3dhodzdocis2jhtlgmxgedn73u557hq2abcd"})
			if !errors.Is(err, c.want) || !strings.Contains(err.Error(), sentence) {
				t.Errorf("%s over %s: %v; want %v with the sentence", c.key, name, err, c.want)
			}
			var raw struct {
				Error struct {
					Data    []a2ashape.ErrorInfo `json:"data"`
					Details []a2ashape.ErrorInfo `json:"details"`
				} `json:"error"`
			}
			if jerr := json.Unmarshal(s.body, &raw); jerr != nil {
				t.Fatalf("%s over %s: %v", c.key, name, jerr)
			}
			infos := raw.Error.Data
			if name == "rest" {
				infos = raw.Error.Details
			}
			code := a2ashape.ReasonCode(c.key)
			if len(infos) != 2 || infos[0].Domain != a2ashape.DomainA2A || infos[1].Domain != a2ashape.DomainANet ||
				infos[1].Metadata["reason"] != code || infos[0].Type != a2ashape.ErrorInfoType {
				t.Errorf("%s over %s: ErrorInfo %+v", c.key, name, infos)
			}
			if (s.retry != "") != c.retry {
				t.Errorf("%s over %s: Retry-After %q", c.key, name, s.retry)
			}
		}
	}
}
