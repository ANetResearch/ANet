//go:build !no_a2a

package a2a

// A messageId already used for other content (correctness-ux CUX-A4; core
// G1, A2AERR-1): the kernel refuses the send, and the local A2A interface
// answers InvalidParamsError over both bindings, for SendMessage and
// SendStreamingMessage alike — the sentence as message, two ErrorInfo, the
// second with the task the client can go on with. And the message reaches
// the kernel with its numbers as the client wrote them (P-CUX6, the
// interface's half).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

func TestMessageIDReusedOverBothBindings(t *testing.T) {
	for _, variant := range []string{"open", "followup"} {
		t.Run(variant, func(t *testing.T) {
			key := "message_id_reused." + variant
			e := newEnv(t)
			e.seam.refuse = func(module.TaskSend) error {
				ref := a2ashape.Refuse(key, a2ashape.Args{"task": "task9"})
				ref.Task = &a2ashape.RefusalTask{ID: "task9", ContextID: "ctx9", State: a2ashape.TaskStateWorking}
				return ref
			}
			sentence := a2ashape.Sentence(key, a2ashape.Lang(), a2ashape.Args{"task": "task9"})
			for _, binding := range bindings {
				cl, ctx := e.client(agentA, binding)
				_, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("hi")})
				if !errors.Is(err, a2a.ErrInvalidParams) || !strings.Contains(err.Error(), sentence) {
					t.Errorf("SendMessage over %s: %v, want InvalidParams with the sentence", binding, err)
				}
				var serr error
				for _, err := range cl.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: textMessage("hi")}) {
					if err != nil {
						serr = err
						break
					}
				}
				if serr == nil || (binding == a2a.TransportProtocolHTTPJSON && !errors.Is(serr, a2a.ErrInvalidParams)) {
					t.Errorf("SendStreamingMessage over %s: %v, want InvalidParams", binding, serr)
				}
			}
			want := map[string]string{"reason": "message_id_reused", "variant": variant, "taskId": "task9",
				"contextId": "ctx9", "state": "TASK_STATE_WORKING"}
			checkInfo := func(where string, info []a2ashape.ErrorInfo) {
				t.Helper()
				if len(info) != 2 || info[0].Reason != "INVALID_PARAMS" || info[0].Domain != a2ashape.DomainA2A ||
					info[1].Reason != "MESSAGE_ID_REUSED" || info[1].Domain != a2ashape.DomainANet ||
					!reflect.DeepEqual(info[1].Metadata, want) {
					t.Errorf("%s: ErrorInfo %+v, want INVALID_PARAMS then MESSAGE_ID_REUSED with %v", where, info, want)
				}
			}
			hdr := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json", "A2A-Version": "1.0"}
			for m, status := range map[string]int{"SendMessage": http.StatusOK, "SendStreamingMessage": http.StatusBadRequest} {
				resp, body := e.raw(http.MethodPost, agentsPath+"/"+agentA+"/jsonrpc", hdr,
					`{"jsonrpc":"2.0","id":"r1","method":"`+m+`","params":{"message":{"role":"ROLE_USER","messageId":"m1","parts":[{"text":"hi"}]}}}`)
				var rpc struct {
					ID    any `json:"id"`
					Error struct {
						Code    int                  `json:"code"`
						Message string               `json:"message"`
						Data    []a2ashape.ErrorInfo `json:"data"`
					} `json:"error"`
				}
				if err := json.Unmarshal(body, &rpc); err != nil || resp.StatusCode != status || rpc.ID != "r1" ||
					rpc.Error.Code != -32602 || rpc.Error.Message != sentence {
					t.Errorf("%s JSON-RPC: %d %s", m, resp.StatusCode, body)
				}
				checkInfo(m+" JSON-RPC", rpc.Error.Data)
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
				if err := json.Unmarshal(body, &rest); err != nil || resp.StatusCode != http.StatusBadRequest ||
					rest.Error.Code != 400 || rest.Error.Status != "INVALID_ARGUMENT" || rest.Error.Message != sentence {
					t.Errorf("REST %s: %d %s", path, resp.StatusCode, body)
				}
				checkInfo("REST "+path, rest.Error.Details)
			}
		})
	}
}

// The numbers a client writes in its message — the metadata, a capability
// call's args — reach the kernel as it wrote them, over both bindings and
// both kinds of send: 9007199254740993 is not 9007199254740992, and 1.0 is
// not 1 (correctness-ux CUX-D1). a2a-go reads them as doubles; the
// interface reads the message from the request's own bytes. Random
// messages: what the kernel gets is the message decoded with its literals.
func TestTheClientsNumbersReachTheKernel(t *testing.T) {
	e := newEnv(t)
	hdr := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json", "A2A-Version": "1.0"}
	r := rand.New(rand.NewSource(1))
	literals := []string{"9007199254740993", "9007199254740992", "1", "1.0", "1e2", "-0", "0.1000", "123456789012345678", "-2.5E-3"}
	value := func() string {
		switch r.Intn(4) {
		case 0:
			return literals[r.Intn(len(literals))]
		case 1:
			return fmt.Sprintf("[%s,%q]", literals[r.Intn(len(literals))], "s")
		case 2:
			return fmt.Sprintf(`{"ü":%s,"a":null}`, literals[r.Intn(len(literals))])
		}
		return `"x"`
	}
	sends := []struct {
		rpc  string // the JSON-RPC method, or "" for REST
		path string
	}{{"SendMessage", ""}, {"SendStreamingMessage", ""}, {"", "/message:send"}, {"", "/message:stream"}}
	for i := 0; i < 40; i++ {
		var parts string
		if r.Intn(2) == 0 {
			parts = fmt.Sprintf(`[{"data":{"skill":"calc.add","args":{"a":%s,"b":%s}}}]`, value(), value())
		} else {
			parts = `[{"text":"hello"}]`
		}
		msg := fmt.Sprintf(`{"role":"ROLE_USER","messageId":"m-%d","parts":%s,"metadata":{"n":%s,"o":%s}}`, i, parts, value(), value())
		s := sends[i%len(sends)]
		var resp *http.Response
		var body []byte
		if s.rpc != "" {
			resp, body = e.raw(http.MethodPost, agentsPath+"/"+agentA+"/jsonrpc", hdr,
				`{"jsonrpc":"2.0","id":1,"method":"`+s.rpc+`","params":{"message":`+msg+`,"configuration":{"returnImmediately":true}}}`)
		} else {
			resp, body = e.raw(http.MethodPost, agentsPath+"/"+agentA+"/rest"+s.path, hdr,
				`{"message":`+msg+`,"configuration":{"returnImmediately":true}}`)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s%s: %d %s", s.rpc, s.path, resp.StatusCode, body)
		}
		e.seam.mu.Lock()
		got := e.seam.sends[len(e.seam.sends)-1].Message
		e.seam.mu.Unlock()
		dec := json.NewDecoder(strings.NewReader(msg))
		dec.UseNumber()
		var want a2ashape.Message
		if err := dec.Decode(&want); err != nil {
			t.Fatal(err)
		}
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		if !bytes.Equal(gb, wb) {
			t.Fatalf("%s%s: the kernel got\n %s\nfor\n %s", s.rpc, s.path, gb, wb)
		}
	}
	e.seam.mu.Lock()
	n := len(e.seam.sends)
	e.seam.mu.Unlock()
	if n != 40 {
		t.Fatalf("%d sends reached the kernel, want 40 (a send carried out twice?)", n)
	}
}

// literalMessage reads any request body without panicking, and what it
// reads, written again and read again, is the same message: its numbers
// keep their literals through the round trip.
func FuzzLiteralMessage(f *testing.F) {
	f.Add([]byte(`{"message":{"role":"ROLE_USER","messageId":"m","parts":[{"data":{"skill":"s","args":{"a":9007199254740993,"b":1.0}}}],"metadata":{"n":-0}}}`))
	f.Add([]byte(`{"message":{"messageId":"m","parts":[{"text":"x"},{"raw":"aGk=","filename":"a"}],"metadata":{"x":[1e400]}}}`))
	f.Add([]byte(`{"message":null}`))
	f.Add([]byte(`{"Message":{"parts":[{"url":"file:///x"}]}} trailing`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, body []byte) {
		m := literalMessage(body)
		if m == nil {
			return
		}
		b, err := json.Marshal(m)
		if err != nil {
			return // a part that does not encode (no content); the binding refuses it
		}
		again := literalMessage([]byte(`{"message":` + string(b) + `}`))
		if again == nil {
			t.Fatalf("%s does not read again", b)
		}
		b2, err := json.Marshal(again)
		if err != nil || !bytes.Equal(b, b2) {
			t.Fatalf("round trip:\n %s\n %s (%v)", b, b2, err)
		}
	})
}

// A SendMessage whose parameters do not read is still the binding's to
// answer, in the binding's own form: carrying out sends before the binding
// changed nothing for it (a JSON-RPC error with HTTP 200, which a JSON-RPC
// client reads).
func TestABadSendMessageIsTheBindingsToAnswer(t *testing.T) {
	e := newEnv(t)
	hdr := map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json", "A2A-Version": "1.0"}
	resp, body := e.raw(http.MethodPost, agentsPath+"/"+agentA+"/jsonrpc", hdr,
		`{"jsonrpc":"2.0","id":3,"method":"SendMessage","params":{"message":{"role":"ROLE_USER","messageId":"m1","parts":"not parts"}}}`)
	var rpc struct {
		ID    any `json:"id"`
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil || resp.StatusCode != http.StatusOK || rpc.Error == nil || rpc.ID != float64(3) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	e.seam.mu.Lock()
	defer e.seam.mu.Unlock()
	if len(e.seam.sends) != 0 {
		t.Fatalf("%d sends reached the kernel", len(e.seam.sends))
	}
}
