package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// UP-1, UP-4 (M-BP-1): whatever credentials the agent sends — its own key in every header a client
// library uses, cookies, proxy and forwarding headers — the model service receives only the operator's
// key, once, in the header of its dialect.
func TestEgressForwardsWithTheOperatorKeyOnly(t *testing.T) {
	f := newFakeModelService(t)
	attacker := map[string]string{
		"x-api-key": testAgentKey, "Authorization": "Bearer " + testAgentKey, "api-key": testAgentKey,
		"x-goog-api-key": testAgentKey, "Proxy-Authorization": "Basic " + testAgentKey, "Cookie": "s=" + testAgentKey,
		"OpenAI-Organization": "org-agent", "OpenAI-Project": "proj-agent", "Forwarded": "for=203.0.113.9",
		"X-Forwarded-For": "203.0.113.9", "X-Forwarded-Host": "evil.example", "Connection": "X-Hop", "X-Hop": testAgentKey,
		"anthropic-version": "2023-06-01",
	}
	for _, tc := range []struct {
		name, path, body string
		key              string
	}{
		{egressUpstreamAnthropic, "/v1/messages", `{"model":"m","max_tokens":10,"messages":[]}`, testOperatorKey},
		{egressUpstreamOpenAI, "/v1/responses", `{"model":"m","input":"hi"}`, testOperatorKey},
		{egressUpstreamCustom, "/chat/completions", `{"model":"m","messages":[]}`, testOperatorKey},
		{egressUpstreamCustom, "/chat/completions", `{"model":"m","messages":[]}`, ""},
	} {
		t.Run(tc.name+"-key-"+fmt.Sprint(tc.key != ""), func(t *testing.T) {
			f.reset()
			up := testUpstream(t, tc.name, f)
			up.Key = tc.key
			_, c, _ := startTestEgress(t, up)
			st, body := doJSON(t, c, "POST", routeURL(tc.name, tc.path), attacker, tc.body)
			if st != 200 {
				t.Fatalf("status %d: %s", st, body)
			}
			got := f.requests()
			if len(got) != 1 {
				t.Fatalf("the service received %d requests", len(got))
			}
			if err := si57Check(up, got[0]); err != nil {
				t.Error(err)
			}
			for k, vs := range got[0].Header {
				for _, v := range vs {
					if strings.Contains(v, testAgentKey) || strings.Contains(v, "agent") && k != "User-Agent" {
						t.Errorf("the agent's %s: %s reached the service", k, v)
					}
				}
			}
			if got[0].Header.Get("Anthropic-Version") != "2023-06-01" {
				t.Errorf("an ordinary header was dropped: %v", got[0].Header)
			}
			if got[0].Header.Get("Accept-Encoding") != "identity" {
				t.Errorf("accept-encoding %q", got[0].Header.Get("Accept-Encoding"))
			}
		})
	}
}

// EG-2: the endpoint is the injection route and CONNECT, nothing else. A request for any other target —
// another path, another host in absolute form, https in absolute form — is 403 egress_plain_http, and
// with no host allowed every CONNECT is 403 egress_host_not_allowed in plain text naming the host.
func TestEgressRefusesWhatIsNotTheRoute(t *testing.T) {
	f := newFakeModelService(t)
	up := testUpstream(t, egressUpstreamAnthropic, f)
	ep, _, sock := startTestEgress(t, up)
	for _, raw := range []string{
		"GET / HTTP/1.1\r\nHost: 127.0.0.1:3128\r\n\r\n",
		"POST /v1/messages HTTP/1.1\r\nHost: 127.0.0.1:3128\r\nContent-Length: 2\r\n\r\n{}",
		"GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"POST http://api.anthropic.com/v1/messages HTTP/1.1\r\nHost: api.anthropic.com\r\nContent-Length: 2\r\n\r\n{}",
		"POST http://169.254.169.254/anet-upstream/anthropic/v1/messages HTTP/1.1\r\nHost: 169.254.169.254\r\nContent-Length: 2\r\n\r\n{}",
		"POST https://127.0.0.1:3128/anet-upstream/anthropic/v1/messages HTTP/1.1\r\nHost: 127.0.0.1:3128\r\nContent-Length: 2\r\n\r\n{}",
		"GET /anet-upstreamx/anthropic/v1/models HTTP/1.1\r\nHost: 127.0.0.1:3128\r\n\r\n",
	} {
		resp := rawRequest(t, sock, raw)
		msg, code, ok := refusalMessage(resp.body)
		if resp.status != 403 || !ok || !strings.HasPrefix(msg, "anet: ") || code != "" {
			t.Errorf("%q: %d %s", strings.SplitN(raw, "\r\n", 2)[0], resp.status, resp.body)
		}
		if msg != a2ashape.Sentence("egress_plain_http", a2ashape.Lang(), nil) {
			t.Errorf("%q: message %q", strings.SplitN(raw, "\r\n", 2)[0], msg)
		}
	}
	resp := rawRequest(t, sock, "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n")
	if resp.status != 403 || !strings.HasPrefix(resp.header.Get("Content-Type"), "text/plain") ||
		strings.TrimSpace(string(resp.body)) != a2ashape.Sentence("egress_host_not_allowed", a2ashape.Lang(), a2ashape.Args{"host": "github.com"}) {
		t.Errorf("CONNECT: %d %v %q", resp.status, resp.header, resp.body)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("the service received %d requests", n)
	}
	s := ep.Summary()
	if s.PolicyRefused["egress_plain_http"] != 7 || s.Connect.Refused["egress_host_not_allowed"] != 1 || s.ModelRequests != 0 {
		t.Errorf("summary %+v", s)
	}
}

// UP-1: only the listed methods and paths of the run's own upstream; everything else on the route, an
// unlisted path, another method, another upstream's name, a path that is not plain, is 403 policy_path,
// whose sentence lists what may be called.
func TestEgressOnlyListedPaths(t *testing.T) {
	f := newFakeModelService(t)
	for _, tc := range []struct {
		name string
		bad  []string // method and request target
		good []string
	}{
		{egressUpstreamAnthropic,
			[]string{"DELETE /anet-upstream/anthropic/v1/messages", "POST /anet-upstream/anthropic/v1/messages/batches",
				"GET /anet-upstream/anthropic/v1/files", "POST /anet-upstream/anthropic/v1/complete",
				"POST /anet-upstream/anthropic/v1/skills", "GET /anet-upstream/anthropic/v1/models/../messages",
				"POST /anet-upstream/anthropic/v1//messages", "POST /anet-upstream/anthropic/v1/messages/",
				"POST /anet-upstream/anthropic/v1/%6Dessages", "GET /anet-upstream/anthropic/v1/models/a%2Fb",
				"POST /anet-upstream/anthropic/./v1/messages", "HEAD /anet-upstream/anthropic/v1/models",
				"GET /anet-upstream/anthropic/v1/models/a/b", "GET /anet-upstream/anthropic/v1/models/",
				"POST /anet-upstream/openai/v1/responses", "POST /anet-upstream/custom/chat/completions",
				"POST /anet-upstream/ANTHROPIC/v1/messages", "POST /anet-upstream/", "GET /anet-upstream/anthropic"},
			[]string{"POST /anet-upstream/anthropic/v1/messages", "POST /anet-upstream/anthropic/v1/messages?beta=true",
				"POST /anet-upstream/anthropic/v1/messages/count_tokens", "GET /anet-upstream/anthropic/v1/models",
				"GET /anet-upstream/anthropic/v1/models/claude-haiku-4-5"}},
		{egressUpstreamOpenAI,
			[]string{"POST /anet-upstream/openai/v1/embeddings", "POST /anet-upstream/openai/v1/files",
				"GET /anet-upstream/openai/v1/responses/resp_1", "POST /anet-upstream/openai/v1/conversations",
				"POST /anet-upstream/openai/v1/realtime/sessions", "POST /anet-upstream/anthropic/v1/messages"},
			[]string{"POST /anet-upstream/openai/v1/responses", "POST /anet-upstream/openai/v1/chat/completions",
				"GET /anet-upstream/openai/v1/models", "GET /anet-upstream/openai/v1/models/ft:gpt-x:org::1"}},
		{egressUpstreamCustom,
			[]string{"POST /anet-upstream/custom/v1/chat/completions", "POST /anet-upstream/custom/completions",
				"POST /anet-upstream/custom/api/generate", "GET /anet-upstream/custom/models/x", "POST /anet-upstream/custom/responses"},
			[]string{"POST /anet-upstream/custom/chat/completions", "GET /anet-upstream/custom/models"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.reset()
			up := testUpstream(t, tc.name, f)
			_, _, sock := startTestEgress(t, up)
			for _, mt := range tc.bad {
				resp := rawRequest(t, sock, mt+" HTTP/1.1\r\nHost: 127.0.0.1:3128\r\nContent-Length: 2\r\n\r\n{}")
				msg, _, ok := refusalMessage(resp.body)
				if strings.HasPrefix(mt, "HEAD ") { // an answer to HEAD has no body
					msg, ok = a2ashape.Sentence("policy_path", a2ashape.Lang(), a2ashape.Args{"paths": up.pathsText()}), true
				}
				if resp.status != 403 || !ok || msg != a2ashape.Sentence("policy_path", a2ashape.Lang(), a2ashape.Args{"paths": up.pathsText()}) {
					t.Errorf("%s: %d %s", mt, resp.status, resp.body)
				}
			}
			if n := len(f.requests()); n != 0 {
				t.Fatalf("the service received %d requests for unlisted paths: %+v", n, f.requests())
			}
			for _, mt := range tc.good {
				body := "{}"
				if strings.HasPrefix(mt, "GET") {
					body = ""
				}
				resp := rawRequest(t, sock, mt+" HTTP/1.1\r\nHost: 127.0.0.1:3128\r\nContent-Length: "+fmt.Sprint(len(body))+"\r\n\r\n"+body)
				if resp.status != 200 {
					t.Errorf("%s: %d %s", mt, resp.status, resp.body)
				}
			}
			for _, r := range f.requests() {
				if err := si57Check(up, r); err != nil {
					t.Error(err)
				}
			}
		})
	}
	up := testUpstream(t, egressUpstreamAnthropic, f)
	if p := up.pathsText(); p != "POST /anet-upstream/anthropic/v1/messages, POST /anet-upstream/anthropic/v1/messages/count_tokens, "+
		"GET /anet-upstream/anthropic/v1/models, GET /anet-upstream/anthropic/v1/models/<id>" {
		t.Errorf("paths %q", p)
	}
}

// LEC §2.6.2: the route is also taken in absolute form when it names the endpoint itself, as a client
// that sends everything through the proxy variables writes it; the query is passed on.
func TestEgressAcceptsAbsoluteFormToItself(t *testing.T) {
	f := newFakeModelService(t)
	up := testUpstream(t, egressUpstreamAnthropic, f)
	_, _, sock := startTestEgress(t, up)
	for _, host := range []string{"127.0.0.1:3128", "localhost:3128", "LOCALHOST:3128"} {
		body := `{"messages":[]}`
		resp := rawRequest(t, sock, "POST http://"+host+"/anet-upstream/anthropic/v1/messages?beta=true HTTP/1.1\r\nHost: "+host+
			"\r\nContent-Length: "+fmt.Sprint(len(body))+"\r\n\r\n"+body)
		if resp.status != 200 {
			t.Errorf("%s: %d %s", host, resp.status, resp.body)
		}
	}
	got := f.requests()
	if len(got) != 3 || got[0].RawQuery != "beta=true" || got[0].Path != "/v1/messages" {
		t.Fatalf("received %+v", got)
	}
}

// EG-4: per run at most 60 model requests (then 429 run_budget_requests), one body at most 8 MiB (413
// egress_too_large), 16 MiB of bodies in all (429 run_budget_upload). Refused requests use none of the
// allowance and are counted by code.
func TestEgressRunLimits(t *testing.T) {
	f := newFakeModelService(t)
	up := testUpstream(t, egressUpstreamAnthropic, f)
	ep, c, sock := startTestEgress(t, up)
	big := func(n int) string { return `{"messages":[{"role":"user","content":"` + strings.Repeat("a", n) + `"}]}` }
	resp := rawRequestEarlyAnswer(t, sock, "POST /anet-upstream/anthropic/v1/messages HTTP/1.1\r\nHost: x\r\n", big(egressMaxBody))
	if msg, _, _ := refusalMessage(resp.body); resp.status != 413 || msg != a2ashape.Sentence("egress_too_large", a2ashape.Lang(), a2ashape.Args{"n": 8}) {
		t.Errorf("over 8 MiB: %d %s", resp.status, resp.body)
	}
	for i := 0; i < 2; i++ {
		if st, body := doJSON(t, c, "POST", routeURL("anthropic", "/v1/messages"), nil, big(7<<20+512<<10)); st != 200 {
			t.Fatalf("a 7.5 MiB body: %d %s", st, body)
		}
	}
	st, body := doJSON(t, c, "POST", routeURL("anthropic", "/v1/messages"), nil, big(1<<20+64<<10))
	if msg, _, _ := refusalMessage(body); st != 429 || msg != a2ashape.Sentence("run_budget_upload", a2ashape.Lang(), a2ashape.Args{"n": 16}) {
		t.Errorf("over 16 MiB in all: %d %s", st, body)
	}
	for i := 2; i < egressMaxRequests; i++ {
		if st, body := doJSON(t, c, "GET", routeURL("anthropic", "/v1/models"), nil, ""); st != 200 {
			t.Fatalf("request %d: %d %s", i+1, st, body)
		}
	}
	st, body = doJSON(t, c, "GET", routeURL("anthropic", "/v1/models"), nil, "")
	if msg, _, _ := refusalMessage(body); st != 429 || msg != a2ashape.Sentence("run_budget_requests", a2ashape.Lang(), a2ashape.Args{"n": 60}) {
		t.Errorf("request 61: %d %s", st, body)
	}
	if n := len(f.requests()); n != egressMaxRequests {
		t.Errorf("the service received %d requests, want %d", n, egressMaxRequests)
	}
	s := ep.Summary()
	if s.ModelRequests != 60 || s.BytesUp < 15<<20 || s.BytesUp > 16<<20 || s.BytesDown != 60*int64(len(`{"ok":true}`)) ||
		s.PolicyRefused["egress_too_large"] != 1 || s.PolicyRefused["run_budget_upload"] != 1 || s.PolicyRefused["run_budget_requests"] != 1 {
		t.Errorf("summary %+v", s)
	}
}

// EG-4: at most 32 connections at once; the next is answered 429 egress_tunnels and closed, and a place
// freed by a closed connection is taken again.
func TestEgressConnectionLimit(t *testing.T) {
	f := newFakeModelService(t)
	up := testUpstream(t, egressUpstreamAnthropic, f)
	ep, _, sock := startTestEgress(t, up)
	var open []net.Conn
	defer func() {
		for _, c := range open {
			_ = c.Close()
		}
	}()
	for i := 0; i < egressMaxConns; i++ {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, c)
	}
	egressWaitFor(t, func() bool { return ep.conns.Load() == egressMaxConns })
	over, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = over.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(over), nil)
	if err != nil {
		t.Fatalf("the connection over the limit: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	if msg, _, _ := refusalMessage(b); resp.StatusCode != 429 || msg != a2ashape.Sentence("egress_tunnels", a2ashape.Lang(), nil) {
		t.Errorf("over the limit: %d %s", resp.StatusCode, b)
	}
	if _, err := over.Read(make([]byte, 1)); err == nil {
		t.Error("the connection over the limit stayed open")
	}
	over.Close()
	_ = open[0].Close()
	open = open[1:]
	egressWaitFor(t, func() bool { return ep.conns.Load() == egressMaxConns-1 })
	resp2 := rawRequest(t, sock, "GET /anet-upstream/anthropic/v1/models HTTP/1.1\r\nHost: x\r\n\r\n")
	if resp2.status != 200 {
		t.Errorf("after a place was freed: %d %s", resp2.status, resp2.body)
	}
	if ep.Summary().PolicyRefused["egress_tunnels"] != 1 {
		t.Errorf("summary %+v", ep.Summary())
	}
}

// Request bodies are read and rewritten one at a time per run: decoding a JSON body takes many times
// its size in memory, so 32 connections each sending 8 MiB at once would otherwise cost the daemon
// gigabytes. A second body waits while the first is still arriving, and goes through once it is done.
func TestEgressReadsOneBodyAtATime(t *testing.T) {
	f := newFakeModelService(t)
	up := testUpstream(t, egressUpstreamAnthropic, f)
	ep, c, sock := startTestEgress(t, up)
	slow, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	_ = slow.SetDeadline(time.Now().Add(15 * time.Second))
	const body = `{"messages":[]}`
	if _, err := io.WriteString(slow, "POST /anet-upstream/anthropic/v1/messages HTTP/1.1\r\nHost: x\r\nContent-Length: "+
		fmt.Sprint(len(body))+"\r\n\r\n"+body[:7]); err != nil {
		t.Fatal(err)
	}
	egressWaitFor(t, func() bool { return len(ep.bodySlot) == 1 })
	second := make(chan int, 1)
	go func() {
		resp, err := c.Post(routeURL("anthropic", "/v1/messages"), "application/json", strings.NewReader(body))
		if err != nil {
			second <- -1
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		second <- resp.StatusCode
	}()
	select {
	case st := <-second:
		t.Fatalf("a second body was taken while the first was still arriving: %d", st)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := io.WriteString(slow, body[7:]); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(slow), nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("the first request: %v %v", resp, err)
	}
	resp.Body.Close()
	select {
	case st := <-second:
		if st != 200 {
			t.Errorf("the second request: %d", st)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second body was never taken")
	}
	if n := len(f.requests()); n != 2 {
		t.Errorf("the service received %d requests", n)
	}
}

// EG-6: a streamed answer reaches the agent event by event (the second event is only sent after the
// agent has read the first), and the answer's cookies, hop-by-hop headers and the operator's account
// identifiers are dropped.
func TestEgressStreamsChunkByChunk(t *testing.T) {
	f := newFakeModelService(t)
	gotFirst := make(chan struct{})
	f.handle = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Set-Cookie", "session=operator")
		h.Set("Anthropic-Organization-Id", "org-operator")
		h.Set("Connection", "X-Upstream-Hop")
		h.Set("X-Upstream-Hop", "1")
		h.Set("Request-Id", "req_1")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"n\":1}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-gotFirst:
		case <-time.After(10 * time.Second):
			return
		}
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"n\":2}\n\n")
	}
	up := testUpstream(t, egressUpstreamAnthropic, f)
	_, c, _ := startTestEgress(t, up)
	req, _ := http.NewRequest("POST", routeURL("anthropic", "/v1/messages"), strings.NewReader(`{"stream":true,"messages":[]}`))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for _, h := range []string{"Set-Cookie", "Anthropic-Organization-Id", "X-Upstream-Hop"} {
		if v := resp.Header.Get(h); v != "" {
			t.Errorf("%s: %s reached the agent", h, v)
		}
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Request-Id") != "req_1" {
		t.Errorf("headers %v", resp.Header)
	}
	br := bufio.NewReader(resp.Body)
	first := make(chan string, 1)
	go func() {
		var b strings.Builder
		for {
			line, err := br.ReadString('\n')
			b.WriteString(line)
			if err != nil || line == "\n" {
				first <- b.String()
				return
			}
		}
	}()
	select {
	case ev := <-first:
		if !strings.Contains(ev, "message_start") {
			t.Fatalf("first event %q", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first event did not arrive before the stream ended: the endpoint buffers")
	}
	close(gotFirst)
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "message_stop") {
		t.Errorf("rest %q", rest)
	}
}

// EG-1: closing the endpoint ends the run's egress at once: an answer still streaming is cut, the
// request to the model service is cancelled, and nothing more is accepted.
func TestEgressCloseEndsTheRun(t *testing.T) {
	f := newFakeModelService(t)
	cancelled := make(chan struct{})
	f.handle = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: 1\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(20 * time.Second):
		}
	}
	up := testUpstream(t, egressUpstreamAnthropic, f)
	ep, c, sock := startTestEgress(t, up)
	resp, err := c.Post(routeURL("anthropic", "/v1/messages"), "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { ep.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	readErr := make(chan error, 1)
	go func() { _, err := io.ReadAll(resp.Body); readErr <- err }()
	select {
	case <-readErr:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent's stream was not cut")
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the request to the model service was not cancelled")
	}
	if conn, err := net.Dial("unix", sock); err == nil {
		conn.Close()
		t.Error("the endpoint still accepts connections")
	}
}

// egressFuncTransport is a model service side given as a function.
type egressFuncTransport func(*http.Request) (*http.Response, error)

func (f egressFuncTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// EG-1, EG-7: Close returns only once every handler has, so the run's counts read right after it (the
// plan's egress summary) are complete and nothing of the run is still working.
func TestEgressCloseWaitsForItsHandlers(t *testing.T) {
	up := testUpstreamAt(t, egressUpstreamAnthropic, "https://model.invalid")
	inFlight := make(chan struct{})
	var finished atomic.Bool
	ep := newEgressEndpoint(up)
	ep.transport = egressFuncTransport(func(r *http.Request) (*http.Response, error) {
		close(inFlight)
		<-r.Context().Done()
		time.Sleep(200 * time.Millisecond) // a handler still unwinding when the run ends
		finished.Store(true)
		return nil, r.Context().Err()
	})
	sock := filepath.Join(t.TempDir(), "e.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ep.Serve(ln)
	t.Cleanup(ep.Close)
	go func() {
		_ = rawRequestQuiet(sock, "POST /anet-upstream/anthropic/v1/messages HTTP/1.1\r\nHost: x\r\nContent-Length: 15\r\n\r\n{\"messages\":[]}")
	}()
	select {
	case <-inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("the request was not forwarded")
	}
	ep.Close()
	if !finished.Load() {
		t.Fatal("Close returned while a handler was still running")
	}
}

// rawRequestQuiet writes raw to a new connection to sock and reads until it closes, reporting nothing.
func rawRequestQuiet(sock, raw string) error {
	c, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, raw); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, c)
	return err
}

// The forwarded request carries the operator's key, so a redirect from the model service is passed to
// the agent as it is and never followed.
func TestEgressDoesNotFollowRedirects(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer other.Close()
	f := newFakeModelService(t)
	f.handle = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		http.Redirect(w, r, other.URL+"/collect", http.StatusTemporaryRedirect)
	}
	up := testUpstream(t, egressUpstreamAnthropic, f)
	_, c, _ := startTestEgress(t, up)
	st, _ := doJSON(t, c, "POST", routeURL("anthropic", "/v1/messages"), nil, `{"messages":[]}`)
	if st != http.StatusTemporaryRedirect || reached.Load() != 0 {
		t.Errorf("status %d, the redirect target was reached %d times", st, reached.Load())
	}
}

// LEC §2.6.4: refusals are in the model service's own error format, with the sentence of this process's
// language as the message (anthropic: permission_error; openai and custom: invalid_request_error with
// the code).
func TestEgressRefusalsSpeakTheUpstreamDialect(t *testing.T) {
	f := newFakeModelService(t)
	for _, lang := range []string{"C", "zh_CN.UTF-8"} {
		t.Setenv("LC_ALL", lang)
		for _, name := range []string{egressUpstreamAnthropic, egressUpstreamOpenAI, egressUpstreamCustom} {
			up := testUpstream(t, name, f)
			_, c, _ := startTestEgress(t, up)
			p := map[string]string{egressUpstreamAnthropic: "/v1/messages", egressUpstreamOpenAI: "/v1/responses", egressUpstreamCustom: "/chat/completions"}[name]
			st, body := doJSON(t, c, "POST", routeURL(name, p), nil, `[1]`)
			want := a2ashape.Sentence("policy_encoding", a2ashape.Lang(), nil)
			if !strings.HasPrefix(want, "anet: ") {
				t.Fatalf("sentence %q", want)
			}
			var got map[string]any
			_ = json.Unmarshal(body, &got)
			var exp map[string]any
			if name == egressUpstreamAnthropic {
				exp = map[string]any{"type": "error", "error": map[string]any{"type": "permission_error", "message": want}}
			} else {
				exp = map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": want, "code": "policy_encoding"}}
			}
			if st != 400 || fmt.Sprint(got) != fmt.Sprint(exp) {
				t.Errorf("%s %s: %d %s", lang, name, st, body)
			}
		}
	}
}

// A model service that cannot be reached is a bare 502 to the agent; the detail stays in the log.
func TestEgressUnreachableServiceIs502(t *testing.T) {
	f := newFakeModelService(t)
	up := testUpstream(t, egressUpstreamAnthropic, f)
	f.srv.Close()
	quietLog(t)
	ep, c, _ := startTestEgress(t, up)
	st, body := doJSON(t, c, "POST", routeURL("anthropic", "/v1/messages"), nil, `{"messages":[]}`)
	if st != 502 || bytes.Contains(body, []byte("anet:")) || bytes.Contains(body, []byte("127.0.0.1")) {
		t.Errorf("%d %s", st, body)
	}
	if s := ep.Summary(); s.ModelRequests != 1 {
		t.Errorf("summary %+v", s)
	}
}

// EG-5: the injection route may use the daemon's proxy settings, except for a plain-http custom service,
// which is dialed on the loopback only.
func TestEgressTransport(t *testing.T) {
	f := newFakeModelService(t)
	if tr := newEgressTransport(testUpstream(t, egressUpstreamAnthropic, f)); tr.Proxy == nil || !tr.DisableCompression {
		t.Error("the built-in upstream's transport ignores the daemon's proxy or asks for compression")
	}
	up := testUpstream(t, egressUpstreamCustom, f)
	tr := newEgressTransport(up)
	if !up.LoopbackOnly || tr.Proxy != nil {
		t.Fatalf("plain-http custom: loopback-only %v, proxy set %v", up.LoopbackOnly, tr.Proxy != nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := tr.DialContext(ctx, "tcp", "192.0.2.1:80"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("dialing off the loopback: %v", err)
	}
	c, err := tr.DialContext(ctx, "tcp", strings.TrimPrefix(f.srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dialing the loopback: %v", err)
	}
	c.Close()
}

// rawResponse is an answer to a request written byte for byte.
type rawResponse struct {
	status int
	header http.Header
	body   []byte
}

// rawRequest writes raw to a new connection to the endpoint's socket and reads one answer.
func rawRequest(t testing.TB, sock, raw string) rawResponse {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	method, _, _ := strings.Cut(raw, " ")
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("%q: %v", raw, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return rawResponse{status: resp.StatusCode, header: resp.Header, body: b}
}

// rawRequestEarlyAnswer sends head and a body of its Content-Length from another goroutine and reads
// the answer, which may come before the body is all written (write errors after it are ignored).
func rawRequestEarlyAnswer(t testing.TB, sock, head, body string) rawResponse {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	go func() {
		_, _ = io.WriteString(c, head+"Content-Length: "+fmt.Sprint(len(body))+"\r\n\r\n"+body)
	}()
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return rawResponse{status: resp.StatusCode, header: resp.Header, body: b}
}

// egressWaitFor polls cond for up to five seconds.
func egressWaitFor(t testing.TB, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- F-EG ---

// recordingTransport answers every forwarded request with 200 {} and keeps it.
type recordingTransport struct {
	mu  sync.Mutex
	got []receivedRequest
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	rt.mu.Lock()
	rt.got = append(rt.got, receivedRequest{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, URL: r.URL.String(),
		Header: r.Header.Clone(), Body: body})
	rt.mu.Unlock()
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
}

// FuzzEgressRequest (F-EG): any request the endpoint's HTTP server can parse — request line, target in
// any form, any headers, any body — is answered without a panic; what it forwards passes SI-57; what it
// refuses is a sentence beginning "anet: " in the upstream's format (plain text for CONNECT). Under plain
// go test it runs its seeds only.
func FuzzEgressRequest(f *testing.F) {
	for _, s := range []string{
		"POST /anet-upstream/anthropic/v1/messages HTTP/1.1\r\nHost: 127.0.0.1:3128\r\nx-api-key: k\r\nanthropic-beta: mcp-client-2025-11-20,claude-code-20250219\r\nContent-Length: 77\r\n\r\n{\"model\":\"opus\",\"max_tokens\":99999,\"messages\":[],\"tools\":[{\"type\":\"bash_x\"}]}",
		"POST http://127.0.0.1:3128/anet-upstream/anthropic/v1/messages?beta=true HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n\r\n",
		"GET /anet-upstream/anthropic/v1/models/claude-haiku-4-5 HTTP/1.1\r\nHost: x\r\nConnection: x-api-key, Cookie\r\nCookie: a=b\r\n\r\n",
		"CONNECT pypi.org:443 HTTP/1.1\r\nHost: pypi.org:443\r\n\r\n",
		"GET http://169.254.169.254/latest/meta-data HTTP/1.1\r\nHost: 169.254.169.254\r\n\r\n",
		"POST /anet-upstream/anthropic/v1/messages HTTP/1.1\r\nHost: x\r\nContent-Encoding: gzip\r\nContent-Length: 2\r\n\r\n{}",
		"POST /anet-upstream/anthropic/v1/messages/count_tokens HTTP/1.1\r\nHost: x\r\nContent-Length: 40\r\n\r\n{\"mcp_servers\":[{\"url\":\"https://x\"}]}  ",
		"POST /anet-upstream/anthropic/v1/%2e%2e/messages HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\n{}",
		"OPTIONS * HTTP/1.1\r\nHost: x\r\n\r\n",
		"POST /anet-upstream/openai/v1/responses HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\n{}",
	} {
		f.Add([]byte(s))
	}
	up := testUpstreamAt(f, egressUpstreamAnthropic, "https://model.invalid")
	f.Fuzz(func(t *testing.T, raw []byte) {
		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
		if err != nil {
			return // the server answers 400 itself
		}
		rt := &recordingTransport{}
		ep := newEgressEndpoint(up)
		ep.transport = rt
		defer ep.Close()
		rec := httptest.NewRecorder()
		ep.ServeHTTP(rec, req)
		for _, r := range rt.got {
			if err := si57Check(up, r); err != nil {
				t.Fatalf("forwarded against SI-57: %v\nrequest %q", err, raw)
			}
		}
		if len(rt.got) > 1 {
			t.Fatalf("one request was forwarded %d times", len(rt.got))
		}
		res := rec.Result()
		body, _ := io.ReadAll(res.Body)
		switch {
		case len(rt.got) == 1:
			if res.StatusCode != 200 {
				t.Fatalf("forwarded but answered %d", res.StatusCode)
			}
		case req.Method == http.MethodConnect:
			if res.StatusCode != 403 || !strings.HasPrefix(string(body), "anet: ") {
				t.Fatalf("CONNECT answered %d %q", res.StatusCode, body)
			}
		default:
			msg, _, ok := refusalMessage(body)
			if !ok || !strings.HasPrefix(msg, "anet: ") {
				t.Fatalf("refusal %d %q for %q", res.StatusCode, body, raw)
			}
			switch res.StatusCode {
			case 400, 403, 413, 415, 429:
			default:
				t.Fatalf("refusal status %d", res.StatusCode)
			}
		}
	})
}
