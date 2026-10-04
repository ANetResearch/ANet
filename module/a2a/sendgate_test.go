//go:build !no_a2a

package a2a

// LEC LA-2, AD-6 (ux UX-R7, UX-R10): while the node is not registered at its
// hub, SendMessage and SendStreamingMessage answer with the join state's
// refusal before the binding, over both bindings: the sentence as message,
// the two ErrorInfo, Retry-After for a passing state; no task is created.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

func TestSendsWaitForTheHub(t *testing.T) {
	args := a2ashape.Args{"hub": "https://hub.example", "aid": "bafyreiujzdegxdncf32epf3dhodzdocis2jhtlgmxgedn73u557hq2abcd"}
	for _, c := range []struct {
		key      string
		want     error
		rest     int
		status   string
		transien bool
	}{
		{"hub_pending_admission", a2a.ErrInternalError, 503, "UNAVAILABLE", true},
		{"hub_retrying", a2a.ErrInternalError, 503, "UNAVAILABLE", true},
		{"no_hub_yet", a2a.ErrInvalidParams, 400, "FAILED_PRECONDITION", false},
		{"hub_refused", a2a.ErrInvalidParams, 400, "FAILED_PRECONDITION", false},
	} {
		t.Run(c.key, func(t *testing.T) {
			e := newEnv(t)
			ref := a2ashape.Refuse(c.key, args)
			if c.transien {
				ref.RetryAfter = 42 * time.Second
			}
			e.seam.gate = ref
			sentence := a2ashape.Sentence(c.key, a2ashape.Lang(), args)
			for _, binding := range bindings {
				cl, ctx := e.client(agentA, binding)
				_, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("hi")})
				if !errors.Is(err, c.want) || !strings.Contains(err.Error(), sentence) {
					t.Errorf("SendMessage over %s: %v, want %v with the sentence", binding, err, c.want)
				}
				var serr error
				for _, err := range cl.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("hi")}) {
					if err != nil {
						serr = err
						break
					}
				}
				switch {
				case binding == a2a.TransportProtocolJSONRPC:
					// a2a-go's JSON-RPC stream client reports a non-200 answer by
					// its status alone (precheck.go); the body, checked below,
					// carries the sentence for clients that read it.
					if serr == nil {
						t.Errorf("SendStreamingMessage over %s: no error", binding)
					}
				case !errors.Is(serr, c.want) || !strings.Contains(serr.Error(), sentence):
					t.Errorf("SendStreamingMessage over %s: %v, want %v with the sentence", binding, serr, c.want)
				}
			}
			// The raw answers: two ErrorInfo, the code and the command,
			// Retry-After only for a passing state.
			hdr := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json", "A2A-Version": "1.0"}
			for _, m := range []string{"SendMessage", "SendStreamingMessage"} {
				resp, body := e.raw(http.MethodPost, agentsPath+"/"+agentA+"/jsonrpc", hdr,
					`{"jsonrpc":"2.0","id":7,"method":"`+m+`","params":{"message":{"role":"ROLE_USER","messageId":"m1","parts":[{"text":"hi"}]}}}`)
				var rpc struct {
					ID    any `json:"id"`
					Error struct {
						Message string               `json:"message"`
						Data    []a2ashape.ErrorInfo `json:"data"`
					} `json:"error"`
				}
				if err := json.Unmarshal(body, &rpc); err != nil || rpc.Error.Message != sentence || len(rpc.Error.Data) != 2 ||
					rpc.Error.Data[1].Metadata["reason"] != a2ashape.ReasonCode(c.key) || rpc.ID != float64(7) {
					t.Errorf("%s JSON-RPC: %d %s", m, resp.StatusCode, body)
				}
				if (resp.Header.Get("Retry-After") != "") != c.transien {
					t.Errorf("%s JSON-RPC Retry-After %q", m, resp.Header.Get("Retry-After"))
				}
			}
			for _, path := range []string{"/message:send", "/message:stream"} {
				resp, body := e.raw(http.MethodPost, agentsPath+"/"+agentA+"/rest"+path, hdr,
					`{"message":{"role":"ROLE_USER","messageId":"m1","parts":[{"text":"hi"}]}}`)
				var rest struct {
					Error struct {
						Code    int                  `json:"code"`
						Status  string               `json:"status"`
						Message string               `json:"message"`
						Details []a2ashape.ErrorInfo `json:"details"`
					} `json:"error"`
				}
				if err := json.Unmarshal(body, &rest); err != nil || resp.StatusCode != c.rest || rest.Error.Status != c.status ||
					rest.Error.Message != sentence || len(rest.Error.Details) != 2 {
					t.Errorf("REST %s: %d %s", path, resp.StatusCode, body)
				}
				if c.transien && resp.Header.Get("Retry-After") != "42" {
					t.Errorf("REST %s Retry-After %q", path, resp.Header.Get("Retry-After"))
				}
			}
			e.seam.mu.Lock()
			sends := len(e.seam.sends)
			e.seam.mu.Unlock()
			if sends != 0 {
				t.Errorf("%d sends reached the kernel", sends)
			}
		})
	}
	// Open gate: the send goes through.
	e := newEnv(t)
	cl, ctx := e.client(agentA, a2a.TransportProtocolJSONRPC)
	if _, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("hi"),
		Config: &a2a.SendMessageConfig{ReturnImmediately: true}}); err != nil {
		t.Fatalf("an open gate refused: %v", err)
	}
}
