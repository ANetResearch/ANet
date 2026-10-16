package autoreply

// The keyed egress endpoint (LEC §2.6.1 to §2.6.4, EG-3): on macOS the run's endpoint listens on a port of
// the host's loopback, where any local process can connect, and serves only requests that carry the run's
// random key. These tests run the keyed endpoint on a loopback port on every platform.

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

// testRunKey is a run key of the real shape: 64 hex digits (256 bits).
const testRunKey = "5e1f0c3a9b7d2e4f6a8c0b1d3e5f7a9c2b4d6e8f0a1c3e5b7d9f1a3c5e7b9d0f"

// startKeyedEgress serves a keyed endpoint for up on a port of 127.0.0.1, as on macOS, and returns it with
// its address.
func startKeyedEgress(t *testing.T, up *egressUpstream, adjust func(*egressEndpoint)) (*egressEndpoint, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ep := newEgressEndpoint(up)
	ep.keyed(ln.Addr().String(), testRunKey)
	if adjust != nil {
		adjust(ep)
	}
	ep.Serve(ln)
	t.Cleanup(ep.Close)
	return ep, ln.Addr().String()
}

// keyedRequest sends one raw request line to addr and returns the answer.
func keyedRequest(t *testing.T, addr, method, target string, h map[string]string, body string) (*http.Response, string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: %s\r\n", method, target, addr)
	for k, v := range h {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	if body != "" {
		fmt.Fprintf(&b, "Content-Type: application/json\r\nContent-Length: %d\r\n", len(body))
	}
	b.WriteString("\r\n" + body)
	if _, err := io.WriteString(c, b.String()); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return resp, string(got)
}

// EG-3, §2.6.2, §2.6.4: on the keyed endpoint the injection route is /anet-upstream/<S>/<name>/…; with
// the run's key a call reaches the model service with the operator's key, and without it — no key, a
// wrong one, the key elsewhere in the path, or escaped — the answer is 404 egress_unauthorized in the
// service's own error format, counted, and nothing reaches the service.
func TestEgressKeyedRouteNeedsTheRunKey(t *testing.T) {
	f := newFakeModelService(t)
	up := testUpstream(t, egressUpstreamAnthropic, f)
	ep, addr := startKeyedEgress(t, up, nil)
	body := `{"model":"x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	h := map[string]string{"x-api-key": testAgentKey, "anthropic-version": "2023-06-01"}

	for _, target := range []string{
		"/anet-upstream/" + testRunKey + "/anthropic/v1/messages",
		"http://" + addr + "/anet-upstream/" + testRunKey + "/anthropic/v1/messages",
		"http://localhost:" + addr[strings.LastIndexByte(addr, ':')+1:] + "/anet-upstream/" + testRunKey + "/anthropic/v1/messages",
	} {
		resp, got := keyedRequest(t, addr, "POST", target, h, body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s with the run's key: %d %s", target, resp.StatusCode, got)
		}
	}
	reqs := f.requests()
	if len(reqs) != 3 {
		t.Fatalf("the service received %d requests, want 3", len(reqs))
	}
	for _, r := range reqs {
		if r.Header.Get("x-api-key") != testOperatorKey || r.Path != "/v1/messages" || strings.Contains(r.Path, testRunKey) {
			t.Errorf("forwarded %s with key %q", r.Path, r.Header.Get("x-api-key"))
		}
	}
	f.reset()

	wrong := strings.Repeat("0", len(testRunKey))
	for _, target := range []string{
		"/anet-upstream/anthropic/v1/messages",
		"/anet-upstream/" + wrong + "/anthropic/v1/messages",
		"/anet-upstream/" + strings.ToUpper(testRunKey) + "/anthropic/v1/messages",
		"/anet-upstream/" + testRunKey + "0/anthropic/v1/messages",
		"/anet-upstream/" + testRunKey[:len(testRunKey)-1] + "/anthropic/v1/messages",
		"/anet-upstream/anthropic/" + testRunKey + "/v1/messages",
		"/anet-upstream/%35" + testRunKey[1:] + "/anthropic/v1/messages",
		"/anet-upstream//anthropic/v1/messages",
		"/anet-upstream/",
	} {
		resp, got := keyedRequest(t, addr, "POST", target, h, body)
		msg, _, ok := refusalMessage([]byte(got))
		if resp.StatusCode != http.StatusNotFound || !ok || msg != egressSentence(ep.lang, "egress_unauthorized", nil) {
			t.Errorf("%s: %d %s, want 404 egress_unauthorized", target, resp.StatusCode, got)
		}
		if !resp.Close {
			t.Errorf("%s: the refused connection is kept open", target)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests without the run's key reached the service", n)
	}
	if s := ep.Summary(); s.PolicyRefused["egress_unauthorized"] != 9 || s.ModelRequests != 3 {
		t.Errorf("counts %+v", s)
	}

	// What is not the route stays egress_plain_http, key or not; the Linux endpoint's own name is not
	// this endpoint's.
	for _, target := range []string{
		"http://example.com/",
		"http://" + sandboxBridgeListen + "/anet-upstream/" + testRunKey + "/anthropic/v1/messages",
		"/v1/messages",
	} {
		resp, got := keyedRequest(t, addr, "POST", target, h, body)
		if _, _, ok := refusalMessage([]byte(got)); resp.StatusCode != http.StatusForbidden || !ok ||
			!strings.Contains(got, egressSentence(ep.lang, "egress_plain_http", nil)) {
			t.Errorf("%s: %d %s, want 403 egress_plain_http", target, resp.StatusCode, got)
		}
	}
}

// basicAuth is a Proxy-Authorization value.
func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// EG-3, §2.6.3: on the keyed endpoint a CONNECT needs Proxy-Authorization Basic "anet:" ‖ S. Without it —
// none, a wrong password or user, another scheme, bad base64, two of them — the answer is 407
// egress_unauthorized with a Proxy-Authenticate challenge, counted, before the host is looked at; with
// it, an HTTPS client whose proxy variable carries the key reaches an allowed host.
func TestEgressKeyedConnectNeedsTheRunKey(t *testing.T) {
	srv, conns := tlsUpstream(t)
	f := &fakeConnectNet{answers: [][]netip.Addr{{testPublicAddr}}, target: srv.Listener.Addr().String()}
	up := testUpstreamAt(t, egressUpstreamAnthropic, "https://model.invalid")
	up.ConnectAllow = map[string]bool{"pypi.org": true}
	ep, addr := startKeyedEgress(t, up, func(e *egressEndpoint) { e.net = f.connectNet() })

	refused := 0
	for name, h := range map[string]map[string]string{
		"none":           nil,
		"wrong password": {"Proxy-Authorization": basicAuth("anet", strings.Repeat("0", len(testRunKey)))},
		"wrong user":     {"Proxy-Authorization": basicAuth("root", testRunKey)},
		"key as user":    {"Proxy-Authorization": basicAuth(testRunKey, "anet")},
		"bearer":         {"Proxy-Authorization": "Bearer " + testRunKey},
		"not Basic":      {"Proxy-Authorization": strings.Replace(basicAuth("anet", testRunKey), "Basic", "Digest", 1)},
		"bad base64":     {"Proxy-Authorization": "Basic !!" + testRunKey},
		"no password":    {"Proxy-Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("anet"))},
	} {
		resp, got := keyedRequest(t, addr, http.MethodConnect, "pypi.org:443", h, "")
		refused++
		if resp.StatusCode != http.StatusProxyAuthRequired || resp.Header.Get("Proxy-Authenticate") == "" ||
			strings.TrimSpace(got) != egressSentence(ep.lang, "egress_unauthorized", nil) {
			t.Errorf("%s: %d %q %v, want 407 egress_unauthorized", name, resp.StatusCode, got, resp.Header)
		}
	}
	// Two values, one of them right, are refused too.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	fmt.Fprintf(c, "CONNECT pypi.org:443 HTTP/1.1\r\nHost: pypi.org:443\r\nProxy-Authorization: %s\r\nProxy-Authorization: %s\r\n\r\n",
		basicAuth("anet", testRunKey), basicAuth("anet", "x"))
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	c.Close()
	refused++
	if err != nil || resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("two Proxy-Authorization values: %v %v", resp, err)
	}
	if lookups, dialed := f.seen(); len(lookups) != 0 || len(dialed) != 0 {
		t.Errorf("an unauthorized CONNECT was looked at: lookups %v, dials %v", lookups, dialed)
	}
	if n := ep.Summary().Connect.Refused["egress_unauthorized"]; n != refused {
		t.Errorf("connect.refused[egress_unauthorized] = %d, want %d", n, refused)
	}

	// The agent's HTTPS client, with the proxy variable the sandbox gives it.
	proxy, _ := url.Parse("http://" + egressKeyUser + ":" + testRunKey + "@" + addr)
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxy),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	r, err := client.Get("https://pypi.org/simple/")
	if err != nil {
		t.Fatalf("through the keyed endpoint: %v", err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.HasPrefix(string(b), "upstream") || conns.Load() != 1 {
		t.Errorf("tunnel: %d %q, %d upstream connections", r.StatusCode, b, conns.Load())
	}
	if ep.Summary().Connect.Allowed["pypi.org"] != 1 {
		t.Errorf("counts %+v", ep.Summary().Connect)
	}
	// And the scheme is case-insensitive, as HTTP's are.
	resp2, got := keyedRequest(t, addr, http.MethodConnect, "example.com:443",
		map[string]string{"Proxy-Authorization": strings.Replace(basicAuth("anet", testRunKey), "Basic", "basic", 1)}, "")
	if resp2.StatusCode != http.StatusForbidden || !strings.Contains(got, "example.com") {
		t.Errorf("an authorized CONNECT to a host not allowed: %d %q", resp2.StatusCode, got)
	}
}

// EG-3: an endpoint without a key (Linux, on its Unix socket) is not changed by any of this: the route has
// no key segment and a CONNECT needs no Proxy-Authorization.
func TestEgressUnkeyedEndpointTakesNoKey(t *testing.T) {
	f := newFakeModelService(t)
	up := testUpstream(t, egressUpstreamAnthropic, f)
	ep, c, _ := startTestEgress(t, up)
	if len(ep.key) != 0 || !ep.proxyAuthorized(http.Header{}) {
		t.Fatal("an unkeyed endpoint asks for a key")
	}
	code, got := doJSON(t, c, "POST", routeURL("anthropic", "/v1/messages"), map[string]string{"anthropic-version": "2023-06-01"},
		`{"model":"x","max_tokens":10,"messages":[]}`)
	if code != 200 {
		t.Fatalf("route: %d %s", code, got)
	}
	code, got = doJSON(t, c, "POST", routeURL(testRunKey+"/anthropic", "/v1/messages"), nil, `{}`)
	if code != http.StatusForbidden {
		t.Errorf("a key segment on the unkeyed route: %d %s", code, got)
	}
}

// MS-3, EG-3: the keyed environment names the endpoint with the run's key in every proxy variable (as the
// password of user anet) and at the start of the injection route; the Linux one is unchanged.
func TestSandboxEgressEnvCarriesTheRunKey(t *testing.T) {
	f := newFakeModelService(t)
	up := testUpstream(t, egressUpstreamAnthropic, f)
	env := map[string]string{}
	for _, kv := range sandboxEgressEnvAt(up, "127.0.0.1:49152", testRunKey) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	for _, k := range egressProxyVars {
		if env[k] != "http://anet:"+testRunKey+"@127.0.0.1:49152" {
			t.Errorf("%s=%q", k, env[k])
		}
	}
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:49152/anet-upstream/"+testRunKey+"/anthropic" ||
		env["ANTHROPIC_API_KEY"] != egressPlaceholderKey || env["NO_PROXY"] != "127.0.0.1,localhost" {
		t.Errorf("keyed environment %v", env)
	}
	linux := strings.Join(sandboxEgressEnv(up), "\n")
	if strings.Contains(linux, "anet:") || strings.Contains(linux, testRunKey) ||
		!strings.Contains(linux, "ANTHROPIC_BASE_URL=http://127.0.0.1:3128/anet-upstream/anthropic\n") {
		t.Errorf("the Linux environment changed:\n%s", linux)
	}
	for _, name := range []string{egressUpstreamOpenAI, egressUpstreamCustom} {
		u := testUpstream(t, name, f)
		got := strings.Join(sandboxEgressEnvAt(u, "127.0.0.1:1", testRunKey), "\n")
		if !strings.Contains(got, "OPENAI_BASE_URL=http://127.0.0.1:1/anet-upstream/"+testRunKey+"/"+u.Name) {
			t.Errorf("%s: %s", name, got)
		}
	}
}

// FuzzEgressRunKey: the keyed endpoint's two checks never panic on any header or request target, take
// only the run's key (Basic, user anet; the route's first segment exactly), and take the key off the path
// of an accepted route and nothing else.
func FuzzEgressRunKey(f *testing.F) {
	f.Add(basicAuth("anet", testRunKey), "/anet-upstream/"+testRunKey+"/anthropic/v1/messages")
	f.Add("basic "+base64.StdEncoding.EncodeToString([]byte("anet:"+testRunKey)), "http://127.0.0.1:1/anet-upstream/x/y")
	f.Add("Basic YW5ldDo=", "/anet-upstream//anthropic")
	f.Add("Basic", "/anet-upstream/"+testRunKey)
	f.Add("", "http://localhost:1/anet-upstream/"+testRunKey+"/openai/v1/models")
	f.Add("Basic "+testRunKey, "*")
	ep := newEgressEndpoint(&egressUpstream{Name: egressUpstreamAnthropic})
	ep.keyed("127.0.0.1:1", testRunKey)
	ep.cancel()
	f.Fuzz(func(t *testing.T, auth, target string) {
		h := http.Header{}
		if auth != "" {
			h.Set("Proxy-Authorization", auth)
		}
		if ep.proxyAuthorized(h) {
			scheme, cred, _ := strings.Cut(strings.TrimSpace(auth), " ")
			raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cred))
			if !strings.EqualFold(scheme, "basic") || err != nil || string(raw) != "anet:"+testRunKey {
				t.Fatalf("accepted Proxy-Authorization %q", auth)
			}
		}
		u, err := url.ParseRequestURI(target)
		if err != nil {
			return
		}
		before := *u
		r := &http.Request{Method: "POST", URL: u, Header: http.Header{}}
		ok := ep.routeKeyOK(r)
		route := ep.targetsSelf(&before) && strings.HasPrefix(before.Path, egressRoutePrefix)
		switch {
		case !route && (!ok || r.URL.Path != before.Path):
			t.Fatalf("%q is not the route, yet ok=%v path %q", target, ok, r.URL.Path)
		case route && ok:
			rest := strings.TrimPrefix(before.Path, egressRoutePrefix)
			key, after, _ := strings.Cut(rest, "/")
			if key != testRunKey || before.RawPath != "" || r.URL.Path != egressRoutePrefix+after {
				t.Fatalf("accepted the route %q (key %q, path now %q)", target, key, r.URL.Path)
			}
		}
	})
}
