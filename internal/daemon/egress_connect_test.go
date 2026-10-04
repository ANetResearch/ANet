package daemon

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// fakeConnectNet stands in for DNS and the network: lookup answers from answers (the last one again once
// they run out), dial sends every connection to target and records the address it was asked for, and
// local is this machine's addresses.
type fakeConnectNet struct {
	mu      sync.Mutex
	answers [][]netip.Addr
	lookups []string
	dialed  []netip.AddrPort
	target  string
	local   []netip.Addr
}

func (f *fakeConnectNet) connectNet() egressConnectNet {
	return egressConnectNet{
		lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.lookups = append(f.lookups, host)
			if len(f.answers) == 0 {
				return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
			}
			a := f.answers[0]
			if len(f.answers) > 1 {
				f.answers = f.answers[1:]
			}
			return a, nil
		},
		dial: func(ctx context.Context, a netip.AddrPort) (net.Conn, error) {
			f.mu.Lock()
			f.dialed = append(f.dialed, a)
			target := f.target
			f.mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, "tcp", target)
		},
		local: func() []netip.Addr { return f.local },
	}
}

func (f *fakeConnectNet) seen() (lookups []string, dialed []netip.AddrPort) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lookups...), append([]netip.AddrPort(nil), f.dialed...)
}

// A public address for pypi.org in these tests (Fastly's; the fake network never dials it).
var testPublicAddr = netip.MustParseAddr("151.101.0.223")

// connectTestEgress is an endpoint whose run allows hosts, on the fake network f.
func connectTestEgress(t *testing.T, f *fakeConnectNet, hosts ...string) (*egressEndpoint, string) {
	t.Helper()
	up := testUpstreamAt(t, egressUpstreamAnthropic, "https://model.invalid")
	up.ConnectAllow = map[string]bool{}
	for _, h := range hosts {
		up.ConnectAllow[h] = true
	}
	ep, sock := startTestEgressWith(t, up, func(e *egressEndpoint) { e.net = f.connectNet() })
	return ep, sock
}

// startTestEgressWith is startTestEgress with the endpoint adjusted before it serves.
func startTestEgressWith(t *testing.T, up *egressUpstream, adjust func(*egressEndpoint)) (*egressEndpoint, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "anet-"+"egc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "e.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ep := newEgressEndpoint(up)
	adjust(ep)
	ep.Serve(ln)
	t.Cleanup(ep.Close)
	return ep, sock
}

// sandboxProxy is the endpoint as the sandbox's proxy variables name it.
var sandboxProxy = url.URL{Scheme: "http", Host: sandboxBridgeListen}

// agentClient is an HTTPS client in the sandbox: every request goes through the endpoint by CONNECT, as
// with HTTPS_PROXY=http://127.0.0.1:3128. serverName overrides the TLS name it sends ("" for the URL's).
func agentClient(sock, serverName string) *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(&sandboxProxy),
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: serverName},
		},
	}
}

// connectRaw sends CONNECT target on a new connection and reads the answer's head.
func connectRaw(t *testing.T, sock, target string) (net.Conn, *bufio.Reader, int, string) {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	hostHeader := target
	if strings.ContainsAny(target, "/@") {
		hostHeader = "pypi.org:443" // the server refuses such a Host header itself
	}
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, hostHeader); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("CONNECT %s: %v", target, err)
	}
	body := ""
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		body = string(b)
	}
	return c, br, resp.StatusCode, body
}

// tlsUpstream is an HTTPS server that answers "upstream" and counts its connections.
func tlsUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "upstream "+r.Host)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, &conns
}

// CN-2, CN-4, CN-5, CN-7: an HTTPS request to an allowed host goes through a tunnel: the daemon resolves
// the host once, dials the address it checked and nothing else, passes the TLS session through, and
// counts the host and the bytes.
func TestEgressConnectTunnel(t *testing.T) {
	srv, conns := tlsUpstream(t)
	f := &fakeConnectNet{answers: [][]netip.Addr{{testPublicAddr}}, target: srv.Listener.Addr().String()}
	ep, sock := connectTestEgress(t, f, "pypi.org")
	for i, target := range []string{"https://pypi.org/simple/", "https://PyPI.Org/simple/"} {
		resp, err := agentClient(sock, "").Get(target)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(string(b), "upstream ") {
			t.Fatalf("%s: %d %q", target, resp.StatusCode, b)
		}
		if conns.Load() != int32(i+1) {
			t.Errorf("the upstream saw %d connections", conns.Load())
		}
	}
	lookups, dialed := f.seen()
	if len(lookups) != 2 || lookups[0] != "pypi.org" || lookups[1] != "pypi.org" {
		t.Errorf("lookups %v", lookups)
	}
	for _, a := range dialed {
		if a != netip.AddrPortFrom(testPublicAddr, 443) {
			t.Errorf("dialed %s", a)
		}
	}
	s := ep.Summary()
	if s.Connect.Allowed["pypi.org"] != 2 || len(s.Connect.Refused) != 0 || s.Connect.BytesUp < 200 || s.Connect.BytesDown < 200 ||
		s.ModelRequests != 0 {
		t.Errorf("summary %+v", s)
	}
}

// CN-2 (M-CN-6, M-CN-7): only CONNECT host:443, and only a host that is an entry exactly: no suffix, no
// address, no other port. Each refusal is the sentence in plain text, counted by code, and nothing is
// resolved or dialed.
func TestEgressConnectRefusesWhatIsNotAllowed(t *testing.T) {
	f := &fakeConnectNet{answers: [][]netip.Addr{{testPublicAddr}}, target: "127.0.0.1:9"}
	ep, sock := connectTestEgress(t, f, "pypi.org", "files.pythonhosted.org")
	for _, tc := range []struct {
		target, code string
		host         string
	}{
		{"evil.pypi.org:443", "egress_host_not_allowed", "evil.pypi.org"},
		{"pypi.org.evil.example:443", "egress_host_not_allowed", "pypi.org.evil.example"},
		{"pythonhosted.org:443", "egress_host_not_allowed", "pythonhosted.org"},
		{"pypi.org.:443", "egress_host_not_allowed", "pypi.org."},
		{"151.101.0.223:443", "egress_host_not_allowed", "151.101.0.223"},
		{"[::1]:443", "egress_host_not_allowed", "::1"},
		{"localhost:443", "egress_host_not_allowed", "localhost"},
		{"pypi.org:80", "egress_port", ""},
		{"pypi.org:22", "egress_port", ""},
		{"pypi.org:0443", "egress_port", ""},
		{"pypi.org", "egress_port", ""},
		{"user@pypi.org:443", "egress_plain_http", ""},
		{"/pypi.org:443", "egress_plain_http", ""},
	} {
		_, _, st, body := connectRaw(t, sock, tc.target)
		want := a2ashape.Sentence(tc.code, a2ashape.Lang(), a2ashape.Args{"host": tc.host})
		if st != 403 || strings.TrimSpace(body) != want {
			t.Errorf("%s: %d %q, want 403 %q", tc.target, st, body, want)
		}
	}
	if lookups, dialed := f.seen(); len(lookups) != 0 || len(dialed) != 0 {
		t.Errorf("resolved %v, dialed %v", lookups, dialed)
	}
	s := ep.Summary().Connect.Refused
	if s["egress_host_not_allowed"] != 7 || s["egress_port"] != 4 || s["egress_plain_http"] != 2 {
		t.Errorf("refused %v", s)
	}
	// With nothing allowed, the default, every CONNECT is refused.
	_, sock2 := connectTestEgress(t, f)
	if _, _, st, _ := connectRaw(t, sock2, "pypi.org:443"); st != 403 {
		t.Errorf("an empty allowlist: %d", st)
	}
}

// CN-4 (M-CN-1): an allowed host that resolves to a special address — loopback, the metadata service,
// a private network, the same in IPv4-mapped or NAT64 form, this machine's own — is refused as a whole,
// even with a public address beside it, and nothing is dialed.
func TestEgressConnectRefusesSpecialAddresses(t *testing.T) {
	self := netip.MustParseAddr("151.101.64.1")
	for _, answer := range [][]netip.Addr{
		{netip.MustParseAddr("127.0.0.1")},
		{netip.MustParseAddr("169.254.169.254")},
		{netip.MustParseAddr("10.1.2.3")},
		{netip.MustParseAddr("::ffff:127.0.0.1")},
		{netip.MustParseAddr("64:ff9b::a9fe:a9fe")},
		{netip.MustParseAddr("::1")},
		{testPublicAddr, netip.MustParseAddr("192.168.0.10")},
		{self},
		{netip.AddrFrom16(self.As16())}, // this machine's address, as a resolver may give an IPv4 one
	} {
		f := &fakeConnectNet{answers: [][]netip.Addr{answer}, target: "127.0.0.1:9", local: []netip.Addr{self}}
		ep, sock := connectTestEgress(t, f, "pypi.org")
		_, _, st, body := connectRaw(t, sock, "pypi.org:443")
		want := a2ashape.Sentence("egress_private_address", a2ashape.Lang(), a2ashape.Args{"host": "pypi.org"})
		if st != 403 || strings.TrimSpace(body) != want {
			t.Errorf("%v: %d %q", answer, st, body)
		}
		if _, dialed := f.seen(); len(dialed) != 0 {
			t.Errorf("%v: dialed %v", answer, dialed)
		}
		if ep.Summary().Connect.Refused["egress_private_address"] != 1 {
			t.Errorf("%v: %+v", answer, ep.Summary())
		}
	}
	// A host that does not resolve is 502, without a sentence of its own.
	f := &fakeConnectNet{target: "127.0.0.1:9"}
	quietLog(t)
	ep, sock := connectTestEgress(t, f, "pypi.org")
	if _, _, st, _ := connectRaw(t, sock, "pypi.org:443"); st != 502 {
		t.Errorf("no such host: %d", st)
	}
	if s := ep.Summary(); len(s.Connect.Refused) != 0 || len(s.Connect.Allowed) != 0 {
		t.Errorf("no such host: %+v", s)
	}
}

// CN-4 (M-CN-2): the address dialed is the one checked. A resolver that answers a public address first
// and a private one after (DNS rebinding) is asked once per CONNECT, and the private address is never
// dialed.
func TestEgressConnectDialsTheAddressChecked(t *testing.T) {
	srv, _ := tlsUpstream(t)
	f := &fakeConnectNet{answers: [][]netip.Addr{{testPublicAddr}, {netip.MustParseAddr("10.0.0.5")}},
		target: srv.Listener.Addr().String()}
	_, sock := connectTestEgress(t, f, "pypi.org")
	resp, err := agentClient(sock, "").Get("https://pypi.org/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	lookups, dialed := f.seen()
	if len(lookups) != 1 || len(dialed) != 1 || dialed[0] != netip.AddrPortFrom(testPublicAddr, 443) {
		t.Errorf("lookups %v, dialed %v", lookups, dialed)
	}
	// The dialer's own check: it opens a connection only to the address it was given.
	want := netip.MustParseAddrPort("[2a04:4e42::223]:443")
	ctl := egressDialControl(want)
	for addr, ok := range map[string]bool{"[2a04:4e42::223]:443": true, "[2a04:4e42::224]:443": false,
		"[2a04:4e42::223]:80": false, "10.0.0.5:443": false, "pypi.org:443": false} {
		if err := ctl("tcp6", addr, nil); (err == nil) != ok {
			t.Errorf("%s: %v", addr, err)
		}
	}
	if err := egressDialControl(netip.MustParseAddrPort("151.101.0.223:443"))("tcp4", "151.101.0.223:443", nil); err != nil {
		t.Error(err)
	}
}

// CN-5 (M-CN-3, M-CN-4): after the 200 the agent's TLS must name the CONNECT's host, without Encrypted
// Client Hello, and be TLS at all; otherwise the connection is dropped before anything is dialed, and the
// reason is counted.
func TestEgressConnectChecksTheTLSName(t *testing.T) {
	srv, conns := tlsUpstream(t)
	f := &fakeConnectNet{answers: [][]netip.Addr{{testPublicAddr}}, target: srv.Listener.Addr().String()}
	ep, sock := connectTestEgress(t, f, "pypi.org")
	quietLog(t)
	if _, err := agentClient(sock, "evil.example").Get("https://pypi.org/"); err == nil {
		t.Error("a TLS name other than the CONNECT's went through")
	}
	c, _, st, _ := connectRaw(t, sock, "pypi.org:443")
	if st != 200 {
		t.Fatal(st)
	}
	ech := tls.Client(c, &tls.Config{ServerName: "pypi.org", MinVersion: tls.VersionTLS13,
		EncryptedClientHelloConfigList: echConfigList(t, "pypi.org")})
	if err := ech.Handshake(); err == nil {
		t.Error("Encrypted Client Hello went through")
	}
	c2, br, st, _ := connectRaw(t, sock, "pypi.org:443")
	if st != 200 {
		t.Fatal(st)
	}
	_, _ = io.WriteString(c2, "GET / HTTP/1.1\r\nHost: pypi.org\r\n\r\n")
	if n, err := br.Read(make([]byte, 1)); err == nil {
		t.Errorf("plain HTTP in the tunnel was answered (%d bytes)", n)
	}
	if _, dialed := f.seen(); len(dialed) != 0 || conns.Load() != 0 {
		t.Errorf("dialed %v; the upstream saw %d connections", dialed, conns.Load())
	}
	s := ep.Summary().Connect
	if s.Refused[egressSNIMismatch] != 1 || s.Refused[egressECH] != 1 || s.Refused[egressNotTLS] != 1 || len(s.Allowed) != 0 {
		t.Errorf("connect %+v", s)
	}
}

// CN-3 (M-CN-5): CONNECT dials directly, whatever proxy the daemon's environment names: a proxy there
// never sees a connection, and the tunnel still reaches its host.
func TestEgressConnectUsesNoProxy(t *testing.T) {
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	var proxied atomic.Int32
	go func() {
		for {
			c, err := proxy.Accept()
			if err != nil {
				return
			}
			proxied.Add(1)
			c.Close()
		}
	}()
	u := "http://u:secret@" + proxy.Addr().String()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(k, u)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	srv, _ := tlsUpstream(t)
	target := netip.MustParseAddrPort(srv.Listener.Addr().String())
	f := &fakeConnectNet{answers: [][]netip.Addr{{testPublicAddr}}}
	_, sock := startTestEgressWith(t, func() *egressUpstream {
		up := testUpstreamAt(t, egressUpstreamAnthropic, "https://model.invalid")
		up.ConnectAllow = map[string]bool{"pypi.org": true}
		return up
	}(), func(e *egressEndpoint) {
		e.net = f.connectNet()
		// The real dialer, sent to the local upstream in place of the address checked.
		e.net.dial = func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
			return defaultEgressConnectNet().dial(ctx, target)
		}
	})
	resp, err := agentClient(sock, "").Get("https://pypi.org/")
	if err != nil {
		t.Fatalf("through the tunnel: %v", err)
	}
	resp.Body.Close()
	if proxied.Load() != 0 {
		t.Errorf("the proxy of the daemon's environment saw %d connections", proxied.Load())
	}
}

// EG-4: at most 16 tunnels at once and 256 in all (429 egress_tunnels); a closed tunnel frees its place.
func TestEgressConnectTunnelLimits(t *testing.T) {
	echo := echoServer(t)
	f := &fakeConnectNet{answers: [][]netip.Addr{{testPublicAddr}}, target: echo}
	ep, sock := connectTestEgress(t, f, "pypi.org")
	hello := helloRecord([][2][]byte{sniExt(host("pypi.org"))})
	var open []net.Conn
	for i := 0; i < egressMaxTunnels; i++ {
		c, br, st, _ := connectRaw(t, sock, "pypi.org:443")
		if st != 200 {
			t.Fatalf("tunnel %d: %d", i+1, st)
		}
		mustEcho(t, c, br, hello)
		open = append(open, c)
	}
	_, _, st, body := connectRaw(t, sock, "pypi.org:443")
	if st != 429 || strings.TrimSpace(body) != a2ashape.Sentence("egress_tunnels", a2ashape.Lang(), nil) {
		t.Errorf("tunnel 17: %d %q", st, body)
	}
	open[0].Close()
	egressWaitFor(t, func() bool {
		ep.tunnels.mu.Lock()
		defer ep.tunnels.mu.Unlock()
		return ep.tunnels.active == egressMaxTunnels-1
	})
	if c, br, st, _ := connectRaw(t, sock, "pypi.org:443"); st != 200 {
		t.Errorf("after one closed: %d", st)
	} else {
		mustEcho(t, c, br, hello)
	}
	ep.tunnels.mu.Lock()
	total := ep.tunnels.total
	ep.tunnels.total = egressMaxTunnelsTotal
	ep.tunnels.mu.Unlock()
	if total != egressMaxTunnels+1 {
		t.Errorf("%d tunnels counted, want %d", total, egressMaxTunnels+1)
	}
	for _, c := range open[1:] {
		c.Close()
	}
	egressWaitFor(t, func() bool {
		ep.tunnels.mu.Lock()
		defer ep.tunnels.mu.Unlock()
		return ep.tunnels.active <= 1
	})
	if _, _, st, _ := connectRaw(t, sock, "pypi.org:443"); st != 429 {
		t.Errorf("tunnel 257: %d", st)
	}
	if n := ep.Summary().Connect.Refused["egress_tunnels"]; n != 2 {
		t.Errorf("refused egress_tunnels %d times", n)
	}
}

// EG-4: once the run's tunnels sent 32 MiB, every tunnel of the run is closed, the bytes over the limit
// are not carried, and every later CONNECT is 429 run_budget_upload; once they received 512 MiB, the
// same with 413 egress_too_large.
func TestEgressConnectByteLimits(t *testing.T) {
	echo := echoServer(t)
	hello := helloRecord([][2][]byte{sniExt(host("pypi.org"))})
	for _, tc := range []struct {
		up     bool
		status int
		code   string
		n      int
	}{
		{true, 429, "run_budget_upload", 32},
		{false, 413, "egress_too_large", 512},
	} {
		f := &fakeConnectNet{answers: [][]netip.Addr{{testPublicAddr}}, target: echo}
		ep, sock := connectTestEgress(t, f, "pypi.org")
		other, otherBr, st, _ := connectRaw(t, sock, "pypi.org:443")
		if st != 200 {
			t.Fatal(st)
		}
		mustEcho(t, other, otherBr, hello)
		c, br, st, _ := connectRaw(t, sock, "pypi.org:443")
		if st != 200 {
			t.Fatal(st)
		}
		mustEcho(t, c, br, hello)
		ep.stats.mu.Lock()
		if tc.up {
			ep.stats.connUp = egressMaxTunnelUpload - 10
		} else {
			ep.stats.connDown = egressMaxTunnelDownload - 10
		}
		ep.stats.mu.Unlock()
		_, _ = c.Write([]byte(strings.Repeat("x", 64)))
		// Both tunnels end, the one that went over and the other one, at once, and the bytes over the
		// limit are not carried: the echo of the 64 bytes never comes back.
		for i, conn := range []net.Conn{c, other} {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			got, err := io.ReadAll([]*bufio.Reader{br, otherBr}[i])
			if err != nil && !isClosed(err) {
				t.Errorf("up=%v: tunnel %d: %v", tc.up, i, err)
			}
			if len(got) != 0 {
				t.Errorf("up=%v: tunnel %d carried %d bytes over the limit", tc.up, i, len(got))
			}
		}
		_, _, st, body := connectRaw(t, sock, "pypi.org:443")
		if st != tc.status || strings.TrimSpace(body) != a2ashape.Sentence(tc.code, a2ashape.Lang(), a2ashape.Args{"n": tc.n}) {
			t.Errorf("up=%v: after the limit %d %q", tc.up, st, body)
		}
		s := ep.Summary().Connect
		if s.BytesUp > egressMaxTunnelUpload || s.BytesDown > egressMaxTunnelDownload {
			t.Errorf("up=%v: carried over the limit: %+v", tc.up, s)
		}
	}
}

// EG-1: closing the endpoint closes its tunnels, both sides, and returns.
func TestEgressCloseClosesTunnels(t *testing.T) {
	var upstreamEnded atomic.Int32
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(c, c) // echo until the tunnel ends
				upstreamEnded.Add(1)
				c.Close()
			}()
		}
	}()
	f := &fakeConnectNet{answers: [][]netip.Addr{{testPublicAddr}}, target: ln.Addr().String()}
	ep, sock := connectTestEgress(t, f, "pypi.org")
	hello := helloRecord([][2][]byte{sniExt(host("pypi.org"))})
	var agents []*bufio.Reader
	var conns []net.Conn
	for i := 0; i < 3; i++ {
		c, br, st, _ := connectRaw(t, sock, "pypi.org:443")
		if st != 200 {
			t.Fatal(st)
		}
		mustEcho(t, c, br, hello)
		agents, conns = append(agents, br), append(conns, c)
	}
	// One more that has its 200 but has not sent its hello yet.
	waitingConn, waiting, st, _ := connectRaw(t, sock, "pypi.org:443")
	if st != 200 {
		t.Fatal(st)
	}
	agents, conns = append(agents, waiting), append(conns, waitingConn)
	done := make(chan struct{})
	go func() { ep.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("Close did not return with its tunnels open")
		for _, c := range conns { // let the tunnels end from the agent's side, so Close can
			c.Close()
		}
		<-done
		return
	}
	for i, br := range agents {
		if _, err := br.ReadByte(); err == nil {
			t.Errorf("tunnel %d still open", i)
		}
	}
	egressWaitFor(t, func() bool { return upstreamEnded.Load() == 3 })
	if _, err := net.Dial("unix", sock); err == nil {
		t.Error("the endpoint still accepts connections")
	}
}

// echoServer is a TCP server that sends back what it receives.
func echoServer(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

// mustEcho sends b into a tunnel to the echo server and reads it back.
func mustEcho(t *testing.T, c net.Conn, br *bufio.Reader, b []byte) {
	t.Helper()
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(b))
	if _, err := io.ReadFull(br, got); err != nil || string(got) != string(b) {
		t.Fatalf("echo: %v", err)
	}
}

// isClosed is an error of a connection the other side closed (not one that timed out).
func isClosed(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	return errors.Is(err, io.EOF) || strings.Contains(err.Error(), "closed") || strings.Contains(err.Error(), "reset")
}
