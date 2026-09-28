package daemon

// Fuzz target for the control plane's JSON routes (docs/notes/0033): the
// request bodies the CLI, the console and `anet mcp` (a model's tool
// arguments) send. Under plain `go test` it runs its seeds only.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fuzzRoutes are the control-plane routes the target drives: every
// authenticated JSON route except those that stop the daemon, move it to
// another hub, configure a program to run, or write where the body says
// (shutdown, hub-register, hub-leave, autoreply, autoreply-test, pull).
var fuzzRoutes = []string{
	"/status", "/p2p-advertise", "/accept", "/profile", "/find", "/delegate", "/inbox", "/message", "/end",
	"/end-accept", "/results", "/review", "/threads", "/thread", "/identities", "/evidence", "/balance",
	"/redeem", "/x402-authorize", "/reconcile", "/audit-hub", "/visibility",
	"/peers/list", "/peers/allow", "/peers/trust", "/peers/deny", "/peers/remove",
	"/inbound/policy", "/inbound/pending", "/inbound/approve", "/inbound/reject",
	"/tasks/send", "/tasks/get", "/tasks/list", "/tasks/cancel", "/tasks/wait", "/tasks/reply",
	"/agents/list", "/agents/card", "/tasks/pay", "/tasks/pay-manual",
	"/payments/status", "/payments/limits", "/payees/list", "/payees/add", "/payees/remove", "/card",
}

// unsafeKey reports a body member that names something on this machine
// (a file to attach, a directory, a URL to fetch, a command): not what
// this target is about, and not something to let a fuzzer choose.
func unsafeKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range []string{"path", "dir", "file", "attach", "url", "exec", "command", "cmd", "hub"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

func hasUnsafeKey(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if unsafeKey(k) || hasUnsafeKey(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if hasUnsafeKey(e) {
				return true
			}
		}
	}
	return false
}

// FuzzControlRoutes: any body on any JSON route is answered without a
// panic, with JSON when the answer says it is JSON, and never with a 5xx
// that is not an error object.
func FuzzControlRoutes(f *testing.F) {
	quietLog(f)
	srv, req, prov := registeredPair(f)
	_ = srv
	id, err := req.Delegate(context.Background(), prov.AID(), "a task", nil)
	if err != nil {
		f.Fatal(err)
	}
	route := func(p string) uint8 {
		for i, r := range fuzzRoutes {
			if r == p {
				return uint8(i)
			}
		}
		f.Fatalf("no route %s", p)
		return 0
	}
	seeds := []struct{ route, body string }{
		{"/tasks/send", `{"to":"` + prov.AID() + `","text":"hi","return_immediately":true}`},
		{"/tasks/send", `{"to":"` + prov.AID() + `","message":{"messageId":"m1","role":"ROLE_USER","parts":[{"data":{"skill":"x","args":{}}}]}}`},
		{"/tasks/get", `{"task_id":"` + id + `","history_length":-1}`},
		{"/tasks/list", `{"role":"outbound","page_size":1000,"page_token":"zz"}`},
		{"/tasks/wait", `{"task_id":"` + id + `","after_seq":-5,"timeout_ms":50}`},
		{"/tasks/cancel", `{"task_id":"` + id + `"}`},
		{"/tasks/reply", `{"task_id":"` + id + `","text":"x","state":"completed"}`},
		{"/tasks/pay", `{"task_id":"` + id + `","decision":"submit","accept":{"scheme":"credit"}}`},
		{"/tasks/pay-manual", `{"task_id":"` + id + `","decision":"reject"}`},
		{"/agents/list", `{"skill":"text.stats","limit":-1,"cursor":"uncarded:-9"}`},
		{"/agents/card", `{"aid":"` + prov.AID() + `"}`},
		{"/message", `{"interaction_id":"` + id + `","body":"b"}`},
		{"/delegate", `{"provider":"` + prov.AID() + `","goal":"g","pay":true}`},
		{"/delegate", `{"provider":"` + prov.AID() + `","capability":"x.y","args":{"a":1}}`},
		{"/end", `{"interaction_id":"` + id + `","extra":1}`},
		{"/review", `{"interaction_id":"` + id + `","rating":99}`},
		{"/thread", `{"interaction_id":"` + id + `"}`},
		{"/redeem", `{"amount":18446744073709551615,"reference":"r","pay_to":"x"}`},
		{"/x402-authorize", `{"pay_to":"x","amount":-1}`},
		{"/payments/limits", `{"agent_per_task":1e30}`},
		{"/payees/add", `{"aid":"` + prov.AID() + `","max":-1}`},
		{"/peers/allow", `{"aids":["` + prov.AID() + `","not an aid"]}`},
		{"/inbound/policy", `{"policy":"open"}`},
		{"/inbound/approve", `{"interaction_id":"x"}`},
		{"/evidence", `{"since":-1,"limit":0,"event_type":"x"}`},
		{"/find", `{"capability":"text.stats"}`},
		{"/status", ``},
		{"/threads", `[]`},
		{"/inbox", `{"state":"nope"}`},
		{"/card", `{"publish":true}`},
	}
	for _, s := range seeds {
		f.Add(route(s.route), []byte(s.body))
	}
	cp := req.ControlHandler("fuzz-token").(*controlPlane)
	n := 0
	f.Fuzz(func(t *testing.T, r uint8, body []byte) {
		n++
		if n%100 == 0 {
			clearMailbox(t, srv, req.AID())
			clearMailbox(t, srv, prov.AID())
		}
		var v any
		dec := json.NewDecoder(bytes.NewReader(body))
		if dec.Decode(&v) == nil && hasUnsafeKey(v) {
			return
		}
		path := fuzzRoutes[int(r)%len(fuzzRoutes)]
		// A blocking send or wait ends with the request, as when a client
		// gives up; a second is enough to reach every parse.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		hr := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)).WithContext(
			context.WithValue(ctx, ctlPortKey{}, "4242"))
		hr.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		cp.api.ServeHTTP(w, hr)
		out := w.Body.Bytes()
		if strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") && len(out) > 0 && !json.Valid(out) {
			t.Fatalf("%s answered JSON that is not: %s", path, out)
		}
		if w.Code >= 500 {
			var e struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(out, &e) != nil || e.Error == "" {
				t.Fatalf("%s %s: %d without an error: %s", path, body, w.Code, out)
			}
		}
	})
}
