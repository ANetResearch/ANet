package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newBareDaemon is a daemon with an identity and no hub, for control-plane tests.
func newBareDaemon(t *testing.T) *Daemon {
	t.Helper()
	root := t.TempDir()
	b, _ := json.Marshal(map[string]any{"control_addr": "127.0.0.1:0"})
	if err := os.WriteFile(filepath.Join(root, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := New(NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// writeStub writes an executable script and returns its path.
func writeStub(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stub.sh")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// plane is one daemon's control plane served on a real loopback listener.
type plane struct {
	d     *Daemon
	cp    *controlPlane
	srv   *httptest.Server
	token string
	port  string
}

// newPlane serves a fresh daemon's control plane with its control token created on disk, as
// ServeControl does.
func newPlane(t *testing.T) *plane {
	t.Helper()
	d := newBareDaemon(t)
	token, err := loadOrGenControlToken(d.layout)
	if err != nil {
		t.Fatal(err)
	}
	return newPlaneFor(t, d, token)
}

func newPlaneFor(t *testing.T, d *Daemon, token string) *plane {
	t.Helper()
	cp := d.ControlHandler(token).(*controlPlane)
	srv := httptest.NewServer(cp)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return &plane{d: d, cp: cp, srv: srv, token: token, port: u.Port()}
}

func (p *plane) origin() string { return "http://127.0.0.1:" + p.port }

// req sends one request; mod may adjust it before sending.
func (p *plane) req(t *testing.T, method, path, body string, mod func(*http.Request)) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, p.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if mod != nil {
		mod(r)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (p *plane) bearer(r *http.Request) { r.Header.Set("Authorization", "Bearer "+p.token) }

// ticket obtains a console ticket with the bearer token.
func (p *plane) ticket(t *testing.T) string {
	t.Helper()
	resp, b := p.req(t, "POST", "/console/ticket", "{}", p.bearer)
	if resp.StatusCode != 200 {
		t.Fatalf("ticket = %d %s", resp.StatusCode, b)
	}
	var out struct {
		Ticket string `json:"ticket"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Ticket == "" {
		t.Fatalf("ticket answer %s", b)
	}
	if !strings.HasSuffix(out.URL, "/console#t="+out.Ticket) || !strings.HasPrefix(out.URL, "http://127.0.0.1:"+p.port+"/") {
		t.Fatalf("ticket URL %q", out.URL)
	}
	return out.Ticket
}

// session is a browser console session: its cookie and CSRF value.
type session struct {
	cookie *http.Cookie
	csrf   string
}

func (p *plane) startSession(t *testing.T, ticket string) (*http.Response, []byte) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"ticket": ticket})
	return p.req(t, "POST", "/console/session", string(b), func(r *http.Request) { r.Header.Set("Origin", p.origin()) })
}

func (p *plane) session(t *testing.T) session {
	t.Helper()
	resp, b := p.startSession(t, p.ticket(t))
	if resp.StatusCode != 200 {
		t.Fatalf("session = %d %s", resp.StatusCode, b)
	}
	var out struct {
		CSRF string `json:"csrf"`
	}
	_ = json.Unmarshal(b, &out)
	var ck *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "anet_s_"+p.port {
			ck = c
		}
	}
	if ck == nil || out.CSRF == "" {
		t.Fatalf("no session cookie or csrf: %v %s", resp.Header["Set-Cookie"], b)
	}
	return session{cookie: ck, csrf: out.CSRF}
}

// as returns a request modifier that sends the session like the console page does.
func (s session) as(p *plane) func(*http.Request) {
	return func(r *http.Request) {
		r.AddCookie(s.cookie)
		r.Header.Set("X-Anet-CSRF", s.csrf)
		r.Header.Set("Origin", p.origin())
	}
}

func refusedForSession(resp *http.Response, body []byte) bool {
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	var e struct {
		Refused string `json:"refused"`
	}
	return json.Unmarshal(body, &e) == nil && e.Refused == "session"
}

// splitPattern turns a ServeMux pattern into a concrete method and path.
func splitPattern(pat string) (method, path string) {
	method = "POST"
	path = pat
	if m, rest, ok := strings.Cut(pat, " "); ok {
		method, path = m, rest
	}
	path = regexp.MustCompile(`\{[^}]*\}`).ReplaceAllString(path, "x")
	return method, path
}

// SI-7 / A2A-DESIGN §7.3: every registered route is either on the session allowlist or refused for a
// session. Routes are read off the recording mux, so a route added later by any file is covered and,
// unless someone adds it to sessionRoutes, refused.
func TestEveryRouteIsAllowlistedOrRefusedForASession(t *testing.T) {
	p := newPlane(t)
	s := p.session(t)
	registered := map[string]bool{}
	for _, pat := range p.cp.api.patterns {
		registered[pat] = true
		method, path := splitPattern(pat)
		resp, body := p.req(t, method, path, "{}", s.as(p))
		_, allowed := sessionRoutes[pat]
		refused := refusedForSession(resp, body)
		switch {
		case allowed && refused:
			t.Errorf("%s is on the session allowlist but was refused: %s", pat, body)
		case !allowed && !refused:
			t.Errorf("%s is not on the session allowlist but a session reached it (%d %s)", pat, resp.StatusCode, body)
		}
	}
	for pat := range sessionRoutes {
		if !registered[pat] {
			t.Errorf("session allowlist names %s, which is not a registered route", pat)
		}
	}
	// The only unauthenticated top-level routes are the listed public ones; everything else goes
	// through the authentication gate ("/").
	for _, pat := range p.cp.top.patterns {
		if pat != "/" && !publicRoutes[pat] {
			t.Errorf("top-level route %s bypasses the authentication gate", pat)
		}
	}
	// The routes the design names as bearer-only stay refused.
	// The task routes (§12) are bearer-only too: a console that later moves
	// to them needs a sessionRule with jsonFields, never a blanket entry.
	for _, pat := range []string{"POST /pull", "POST /autoreply", "POST /shutdown", "POST /x402-authorize",
		"POST /redeem", "POST /reconcile", "POST /hub-leave", "POST /visibility", "POST /p2p-advertise",
		"POST /console/ticket", "POST /tasks/send", "POST /tasks/get", "POST /tasks/list", "POST /tasks/cancel",
		"POST /tasks/wait", "POST /tasks/reply", "POST /agents/list", "POST /agents/card"} {
		if !registered[pat] {
			t.Errorf("%s is not a registered route", pat)
		}
		if _, ok := sessionRoutes[pat]; ok {
			t.Errorf("%s must be bearer-only", pat)
		}
	}
}

// A route not on the allowlist is refused without reaching its handler: /shutdown under a session does
// not stop the daemon.
func TestASessionCannotReachABearerOnlyHandler(t *testing.T) {
	p := newPlane(t)
	s := p.session(t)
	resp, body := p.req(t, "POST", "/shutdown", "{}", s.as(p))
	if !refusedForSession(resp, body) {
		t.Fatalf("/shutdown under a session = %d %s", resp.StatusCode, body)
	}
	select {
	case <-p.d.stop:
		t.Fatal("the shutdown handler ran for a session")
	default:
	}
}

// A session request needs the CSRF header, an Origin, and must not come from another site or port.
func TestSessionRequestsNeedCSRFAndOrigin(t *testing.T) {
	p := newPlane(t)
	s := p.session(t)
	cases := []struct {
		name string
		mod  func(*http.Request)
		ok   bool
	}{
		{"complete", s.as(p), true},
		{"no csrf", func(r *http.Request) { s.as(p)(r); r.Header.Del("X-Anet-CSRF") }, false},
		{"wrong csrf", func(r *http.Request) { s.as(p)(r); r.Header.Set("X-Anet-CSRF", s.csrf+"x") }, false},
		{"no origin", func(r *http.Request) { s.as(p)(r); r.Header.Del("Origin") }, false},
		{"same-site page", func(r *http.Request) { s.as(p)(r); r.Header.Set("Sec-Fetch-Site", "same-site") }, false},
		{"same-origin page", func(r *http.Request) { s.as(p)(r); r.Header.Set("Sec-Fetch-Site", "same-origin") }, true},
		{"no cookie", func(r *http.Request) { r.Header.Set("X-Anet-CSRF", s.csrf); r.Header.Set("Origin", p.origin()) }, false},
	}
	for _, c := range cases {
		resp, body := p.req(t, "POST", "/threads", "{}", c.mod)
		if c.ok && resp.StatusCode != 200 {
			t.Errorf("%s: %d %s", c.name, resp.StatusCode, body)
		}
		if !c.ok && resp.StatusCode == 200 {
			t.Errorf("%s: accepted", c.name)
		}
	}
}

// JSON bodies under a session may carry only the fields the console sends: local file paths and
// payment are refused before the handler runs.
func TestSessionJSONBodiesCarryOnlyConsoleFields(t *testing.T) {
	p := newPlane(t)
	s := p.session(t)
	refused := []struct{ path, body string }{
		{"/delegate", `{"provider":"a","goal":"g","attachments":["/etc/passwd"]}`},
		{"/delegate", `{"provider":"a","goal":"g","pay":true}`},
		{"/delegate", `{"provider":"a","capability":"x.y"}`},
		{"/message", `{"interaction_id":"i","body":"b","attachments":["/home/u/.ssh/id_ed25519"]}`},
		{"/end", `{"interaction_id":"i","extra":1}`},
		{"/console/switch", `{"aid":"a","url":"http://x"}`},
	}
	for _, c := range refused {
		resp, body := p.req(t, "POST", c.path, c.body, s.as(p))
		if !refusedForSession(resp, body) {
			t.Errorf("%s %s: want session refusal, got %d %s", c.path, c.body, resp.StatusCode, body)
		}
	}
	// The same routes with only the console's fields reach the handler (which then fails for its own
	// reasons: no hub, no such interaction).
	for _, c := range []struct{ path, body string }{
		{"/delegate", `{"provider":"a","goal":"g"}`},
		{"/message", `{"interaction_id":"i","body":"b"}`},
	} {
		resp, body := p.req(t, "POST", c.path, c.body, s.as(p))
		if refusedForSession(resp, body) {
			t.Errorf("%s with console fields was refused: %s", c.path, body)
		}
	}
	// The bearer token is not restricted this way.
	resp, body := p.req(t, "POST", "/delegate", `{"provider":"a","goal":"g","attachments":["/nonexistent"]}`, p.bearer)
	if refusedForSession(resp, body) {
		t.Errorf("bearer request refused by the session gate: %s", body)
	}
}

// A ticket is single use and expires after 60 seconds.
func TestConsoleTicketIsSingleUseAndExpires(t *testing.T) {
	p := newPlane(t)
	tk := p.ticket(t)
	if resp, b := p.startSession(t, tk); resp.StatusCode != 200 {
		t.Fatalf("first use = %d %s", resp.StatusCode, b)
	}
	if resp, _ := p.startSession(t, tk); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("second use = %d, want 401", resp.StatusCode)
	}
	tk2 := p.ticket(t)
	base := time.Now()
	p.cp.sessions.mu.Lock()
	p.cp.sessions.now = func() time.Time { return base.Add(consoleTicketTTL + time.Second) }
	p.cp.sessions.mu.Unlock()
	if resp, _ := p.startSession(t, tk2); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired ticket = %d, want 401", resp.StatusCode)
	}
	if resp, _ := p.startSession(t, "not-a-ticket"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown ticket = %d, want 401", resp.StatusCode)
	}
}

// Starting a session needs the page's Origin and a JSON body; a cross-site form cannot do it.
func TestConsoleSessionStartNeedsOriginAndJSON(t *testing.T) {
	p := newPlane(t)
	body, _ := json.Marshal(map[string]string{"ticket": p.ticket(t)})
	resp, _ := p.req(t, "POST", "/console/session", string(body), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("no Origin = %d, want 403", resp.StatusCode)
	}
	resp, _ = p.req(t, "POST", "/console/session", string(body), func(r *http.Request) {
		r.Header.Set("Origin", p.origin())
		r.Header.Set("Content-Type", "text/plain")
	})
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain = %d, want 415", resp.StatusCode)
	}
	resp, _ = p.req(t, "POST", "/console/session", string(body), func(r *http.Request) {
		r.Header.Set("Origin", "http://evil.example")
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Origin = %d, want 403", resp.StatusCode)
	}
}

// The session cookie is HttpOnly, SameSite=Strict, Path=/ and named after the port.
func TestSessionCookieAttributes(t *testing.T) {
	p := newPlane(t)
	resp, _ := p.startSession(t, p.ticket(t))
	raw := strings.Join(resp.Header["Set-Cookie"], "\n")
	for _, want := range []string{"anet_s_" + p.port + "=", "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(raw, want) {
			t.Errorf("Set-Cookie %q lacks %q", raw, want)
		}
	}
	if strings.Contains(raw, "Domain=") {
		t.Errorf("Set-Cookie must not widen the domain: %q", raw)
	}
}

// The CSRF value is returned once and no route returns it from the cookie: /status under the session
// does not contain it.
func TestCSRFIsNotRetrievableWithTheCookie(t *testing.T) {
	p := newPlane(t)
	s := p.session(t)
	for _, path := range []string{"/status", "/threads", "/identities"} {
		_, body := p.req(t, "POST", path, "{}", s.as(p))
		if bytes.Contains(body, []byte(s.csrf)) {
			t.Errorf("%s returned the CSRF value", path)
		}
	}
	_, page := p.req(t, "GET", "/console", "", func(r *http.Request) { r.AddCookie(s.cookie) })
	if bytes.Contains(page, []byte(s.csrf)) {
		t.Error("/console returned the CSRF value to a request carrying the cookie")
	}
}

// SI-7 / §7.1: the Host header must name this listener on a loopback name, else 421.
func TestHostAllowlistRefusesOtherNames(t *testing.T) {
	p := newPlane(t)
	other := "1"
	if p.port == "1" {
		other = "2"
	}
	for _, c := range []struct {
		host string
		ok   bool
	}{
		{"127.0.0.1:" + p.port, true},
		{"localhost:" + p.port, true},
		{"LocalHost:" + p.port, true},
		{"[::1]:" + p.port, true},
		{"evil.example:" + p.port, false},
		{"127.0.0.1.nip.io:" + p.port, false},
		{"127.0.0.1:" + other, false},
		{"127.0.0.1", false},
		{"0.0.0.0:" + p.port, false},
	} {
		for _, path := range []string{"/ping", "/console", "/attachment?interaction_id=a&cid=b", "/status"} {
			resp, _ := p.req(t, "GET", path, "", func(r *http.Request) { r.Host = c.host; p.bearer(r) })
			if c.ok && resp.StatusCode == http.StatusMisdirectedRequest {
				t.Errorf("Host %q %s refused", c.host, path)
			}
			if !c.ok && resp.StatusCode != http.StatusMisdirectedRequest {
				t.Errorf("Host %q %s = %d, want 421", c.host, path, resp.StatusCode)
			}
		}
	}
}

// A request carrying a foreign Origin is refused on every route, public ones included.
func TestForeignOriginIsRefused(t *testing.T) {
	p := newPlane(t)
	for _, o := range []string{"http://evil.example", "null", "https://127.0.0.1:" + p.port, "http://127.0.0.1:1"} {
		resp, _ := p.req(t, "GET", "/ping", "", func(r *http.Request) { r.Header.Set("Origin", o) })
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Origin %q = %d, want 403", o, resp.StatusCode)
		}
	}
	resp, _ := p.req(t, "GET", "/ping", "", func(r *http.Request) { r.Header.Set("Origin", p.origin()) })
	if resp.StatusCode != 200 {
		t.Errorf("own Origin = %d", resp.StatusCode)
	}
}

// /ping carries no Access-Control-Allow-Origin header.
func TestPingHasNoCORSHeader(t *testing.T) {
	p := newPlane(t)
	resp, body := p.req(t, "GET", "/ping", "", nil)
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte(p.d.AID())) {
		t.Fatalf("/ping = %d %s", resp.StatusCode, body)
	}
	if v := resp.Header.Get("Access-Control-Allow-Origin"); v != "" {
		t.Fatalf("/ping sends Access-Control-Allow-Origin %q", v)
	}
}

// Unauthenticated requests to authenticated routes are 401; a wrong bearer token is 401.
func TestAuthenticatedRoutesNeedACredential(t *testing.T) {
	p := newPlane(t)
	for _, path := range []string{"/status", "/threads", "/console/ticket", "/console/switch"} {
		if resp, _ := p.req(t, "POST", path, "{}", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without credential = %d", path, resp.StatusCode)
		}
		if resp, _ := p.req(t, "POST", path, "{}", func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") }); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with a wrong token = %d", path, resp.StatusCode)
		}
	}
	if resp, _ := p.req(t, "GET", "/attachment?interaction_id=a&cid=b", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/attachment without credential = %d", resp.StatusCode)
	}
}

// §7.1: a non-loopback control address is refused at start.
func TestNonLoopbackControlAddrIsRefused(t *testing.T) {
	for _, c := range []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:39811", true}, {"localhost:39811", true}, {"[::1]:39811", true},
		{"0.0.0.0:39811", false}, {":39811", false}, {"192.168.1.5:39811", false},
		{"[::]:39811", false}, {"example.org:39811", false}, {"127.0.0.2:39811", false},
	} {
		err := checkLoopbackControlAddr(c.addr)
		if c.ok != (err == nil) {
			t.Errorf("%s: err=%v", c.addr, err)
		}
	}
	d := newBareDaemon(t)
	d.mu.Lock()
	d.cfg.ControlAddr = "0.0.0.0:0"
	d.mu.Unlock()
	ln, err := d.listenControl()
	if err == nil {
		ln.Close()
		t.Fatal("listenControl bound a non-loopback address")
	}
}

// §7.4: the switcher obtains a ticket from another local daemon and returns that daemon's console URL.
func TestConsoleSwitchReturnsTheTargetsTicketURL(t *testing.T) {
	rt := t.TempDir()
	if err := os.Chmod(rt, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", rt)

	a := newPlane(t)
	bd := newBareDaemon(t)
	btok, err := loadOrGenControlToken(bd.layout)
	if err != nil {
		t.Fatal(err)
	}
	b := newPlaneFor(t, bd, btok)
	bd.mu.Lock()
	bd.cfg.ControlAddr = net.JoinHostPort("127.0.0.1", b.port)
	bd.mu.Unlock()
	bd.writeRegistry()

	s := a.session(t)
	resp, body := a.req(t, "POST", "/console/switch", `{"aid":"`+bd.AID()+`"}`, s.as(a))
	if resp.StatusCode != 200 {
		t.Fatalf("switch = %d %s", resp.StatusCode, body)
	}
	var out struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(body, &out)
	u, err := url.Parse(out.URL)
	if err != nil || u.Port() != b.port || !strings.HasPrefix(u.Fragment, "t=") {
		t.Fatalf("switch URL %q", out.URL)
	}
	if r, bb := b.startSession(t, strings.TrimPrefix(u.Fragment, "t=")); r.StatusCode != 200 {
		t.Fatalf("the returned ticket does not open the target console: %d %s", r.StatusCode, bb)
	}

	// A registry entry for B's AID at a non-loopback address is skipped: B's control token is never
	// sent there, even though something answers /ping at that address.
	if lan := nonLoopbackIPv4(); lan != "" {
		var sawToken atomic.Bool
		ln, err := net.Listen("tcp", net.JoinHostPort(lan, "0"))
		if err == nil {
			fakeSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					sawToken.Store(true)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"url":"http://127.0.0.1:1/console#t=x"}`))
			})}
			go func() { _ = fakeSrv.Serve(ln) }()
			defer fakeSrv.Close()
			remote := IdentityEntry{AID: bd.AID(), Name: "remote", ControlAddr: ln.Addr().String(), DataDir: bd.layout.Root}
			rb, _ := json.Marshal(remote)
			if err := os.WriteFile(filepath.Join(DaemonsDir(), "0remote.json"), rb, 0o600); err != nil {
				t.Fatal(err)
			}
			resp, body = a.req(t, "POST", "/console/switch", `{"aid":"`+bd.AID()+`"}`, s.as(a))
			if sawToken.Load() {
				t.Fatal("the switcher sent a control token to a non-loopback address")
			}
			if resp.StatusCode != 200 {
				t.Fatalf("switch with a non-loopback decoy entry = %d %s", resp.StatusCode, body)
			}
			_ = os.Remove(filepath.Join(DaemonsDir(), "0remote.json"))
		}
	}

	// A registry entry that names B's AID but A's data dir is not followed: the token in that dir
	// belongs to A.
	fake := IdentityEntry{AID: "E-not-a-real-aid", Name: "x", ControlAddr: net.JoinHostPort("127.0.0.1", a.port), DataDir: a.d.layout.Root}
	fb, _ := json.Marshal(fake)
	if err := os.WriteFile(filepath.Join(DaemonsDir(), "fake.json"), fb, 0o600); err != nil {
		t.Fatal(err)
	}
	resp, body = a.req(t, "POST", "/console/switch", `{"aid":"E-not-a-real-aid"}`, s.as(a))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("switch to a mismatched registry entry = %d %s", resp.StatusCode, body)
	}
}

// nonLoopbackIPv4 returns an IPv4 address of this host that is not loopback, or "".
func nonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.String()
		}
	}
	return ""
}
