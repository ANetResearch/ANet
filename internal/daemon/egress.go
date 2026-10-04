package daemon

// egress.go is the egress endpoint of one sandboxed run (LEC §2.6, §4.3.3, EG-1 to EG-7).
//
// A sandboxed agent has no network of its own. Its only way out is this endpoint, which the daemon opens
// before the agent starts and closes the moment the run ends (EG-1). On Linux it listens on a Unix socket
// bound into the sandbox at /run/anet/egress.sock, which anet __sandbox-init bridges to the sandbox's
// 127.0.0.1:3128; only a process of this daemon's user may connect (sandbox_linux.go).
//
// The endpoint answers two things and refuses the rest with egress_plain_http (EG-2):
//
//   - the injection route, /anet-upstream/<name>/<path> (LEC §2.6.2): a call to the run's model service,
//     which the endpoint forwards after the request policy (egress_policy.go) with the operator's key;
//   - CONNECT (LEC §2.6.3) to a host the operator allowed, on port 443, over TLS that names it
//     (egress_connect.go). With no host allowed, the default, every CONNECT is refused with
//     egress_host_not_allowed.
//
// Every refusal is a sentence of ux §1.4 I in the model service's own error format (LEC §2.6.4), read by
// the agent's model, so that the agent can say in its reply what it could not do. The run's limits
// (EG-4) are constants; what the run used and what was refused is counted (egressStats) for the run's
// evidence (EG-7).
//
// Requests and answers are passed through, never logged or written anywhere (EG-6); a streamed answer
// is passed on chunk by chunk as it arrives.

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// The per-run limits of EG-4.
const (
	egressMaxRequests       = 60        // model requests forwarded
	egressMaxBody           = 8 << 20   // one injected request body
	egressMaxInjectUpload   = 16 << 20  // all injected request bodies together
	egressMaxConns          = 32        // connections to the endpoint at once
	egressMaxTunnels        = 16        // CONNECT tunnels at once
	egressMaxTunnelsTotal   = 256       // CONNECT tunnels in all
	egressMaxTunnelUpload   = 32 << 20  // bytes sent through the tunnels
	egressMaxTunnelDownload = 512 << 20 // bytes received through the tunnels
)

// egressStats counts what one run did through its endpoint (LEC §2.5 egress, EG-7).
type egressStats struct {
	mu            sync.Mutex
	requests      int
	bytesUp       int64
	bytesDown     int64
	policyRefused map[string]int
	connAllowed   map[string]int
	connRefused   map[string]int
	connUp        int64
	connDown      int64
	// upSpent, downSpent: the run's tunnels reached their allowance of bytes sent or received (EG-4).
	upSpent, downSpent bool
}

// egressSummary is the egress member of anet.autoreply.invoked (LEC §2.5). It names hosts and codes and
// counts; never a path, a body or a key.
type egressSummary struct {
	ModelRequests int                  `json:"model_requests"`
	BytesUp       int64                `json:"bytes_up"`
	BytesDown     int64                `json:"bytes_down"`
	PolicyRefused map[string]int       `json:"policy_refused"`
	Connect       egressConnectSummary `json:"connect"`
}

// egressConnectSummary is the CONNECT part of egressSummary.
type egressConnectSummary struct {
	Allowed   map[string]int `json:"allowed"`
	Refused   map[string]int `json:"refused"`
	BytesUp   int64          `json:"bytes_up"`
	BytesDown int64          `json:"bytes_down"`
}

// reserve takes one model request and n bytes of upload from the run's allowance, or names the limit
// that is used up: run_budget_requests or run_budget_upload. Nothing is taken from a refused one.
func (s *egressStats) reserve(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.requests >= egressMaxRequests:
		return "run_budget_requests"
	case s.bytesUp+int64(n) > egressMaxInjectUpload:
		return "run_budget_upload"
	}
	s.requests++
	s.bytesUp += int64(n)
	return ""
}

// requestsLeft reports whether a model request can still be forwarded.
func (s *egressStats) requestsLeft() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests < egressMaxRequests
}

func (s *egressStats) down(n int) {
	s.mu.Lock()
	s.bytesDown += int64(n)
	s.mu.Unlock()
}

func (s *egressStats) refused(code string) {
	s.mu.Lock()
	if s.policyRefused == nil {
		s.policyRefused = map[string]int{}
	}
	s.policyRefused[code]++
	s.mu.Unlock()
}

func (s *egressStats) connectRefused(code string) {
	s.mu.Lock()
	if s.connRefused == nil {
		s.connRefused = map[string]int{}
	}
	s.connRefused[code]++
	s.mu.Unlock()
}

func (s *egressStats) connectAllowed(host string) {
	s.mu.Lock()
	if s.connAllowed == nil {
		s.connAllowed = map[string]int{}
	}
	s.connAllowed[host]++
	s.mu.Unlock()
}

// tunnelBytes counts n bytes a tunnel carries, sent (up) or received, and reports whether they are within
// the run's allowance (EG-4). Bytes over it are not counted, and from then on the direction is spent.
func (s *egressStats) tunnelBytes(up bool, n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if up {
		if s.upSpent || s.connUp+int64(n) > egressMaxTunnelUpload {
			s.upSpent = true
			return false
		}
		s.connUp += int64(n)
		return true
	}
	if s.downSpent || s.connDown+int64(n) > egressMaxTunnelDownload {
		s.downSpent = true
		return false
	}
	s.connDown += int64(n)
	return true
}

// tunnelSpent names the direction whose allowance the run's tunnels used up: "up", "down" or "".
func (s *egressStats) tunnelSpent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.upSpent:
		return "up"
	case s.downSpent:
		return "down"
	}
	return ""
}

// summary is a copy of the counts.
func (s *egressStats) summary() egressSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := func(m map[string]int) map[string]int {
		out := make(map[string]int, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	return egressSummary{
		ModelRequests: s.requests, BytesUp: s.bytesUp, BytesDown: s.bytesDown, PolicyRefused: cp(s.policyRefused),
		Connect: egressConnectSummary{Allowed: cp(s.connAllowed), Refused: cp(s.connRefused), BytesUp: s.connUp, BytesDown: s.connDown},
	}
}

// add adds another run's counts (one turn can start the agent more than once).
func (a *egressSummary) add(b egressSummary) {
	merge := func(dst *map[string]int, src map[string]int) {
		if len(src) == 0 {
			return
		}
		if *dst == nil {
			*dst = map[string]int{}
		}
		for k, v := range src {
			(*dst)[k] += v
		}
	}
	a.ModelRequests += b.ModelRequests
	a.BytesUp += b.BytesUp
	a.BytesDown += b.BytesDown
	merge(&a.PolicyRefused, b.PolicyRefused)
	merge(&a.Connect.Allowed, b.Connect.Allowed)
	merge(&a.Connect.Refused, b.Connect.Refused)
	a.Connect.BytesUp += b.Connect.BytesUp
	a.Connect.BytesDown += b.Connect.BytesDown
}

// egressSelf are the request-target authorities that name the endpoint itself: a client that sends its
// model requests through the proxy variables writes the route in absolute form to them (LEC §2.6.2).
var egressSelf = map[string]bool{sandboxBridgeListen: true, "localhost:3128": true}

// egressEndpoint serves one sandboxed run.
type egressEndpoint struct {
	up        *egressUpstream
	lang      a2ashape.Language
	stats     egressStats
	transport http.RoundTripper
	srv       *http.Server
	ctx       context.Context
	cancel    context.CancelFunc
	// peerCheck, when set, admits a connection only from the expected process owner (Linux: the
	// daemon's own uid, read with SO_PEERCRED).
	peerCheck func(net.Conn) error
	conns     atomic.Int32
	// net is how CONNECT resolves and dials (egress_connect.go); tunnels are the run's CONNECT tunnels.
	net     egressConnectNet
	tunnels egressTunnels
	// bodySlot lets one request body at a time be read and rewritten. Decoding a JSON body takes many
	// times its size in memory (an 8 MiB body of empty arrays is several hundred MiB), and the run's
	// egressMaxConns connections must not multiply that in the daemon.
	bodySlot chan struct{}
	// gate orders the handlers' start against Close: once closed is set no handler starts, so Close
	// can wait for the ones that did.
	gate      sync.RWMutex
	closed    bool
	handlers  sync.WaitGroup
	closeOnce sync.Once
}

// newEgressEndpoint returns the endpoint of a run whose model service is up. It serves nothing until
// Serve.
func newEgressEndpoint(up *egressUpstream) *egressEndpoint {
	ctx, cancel := context.WithCancel(context.Background())
	e := &egressEndpoint{up: up, lang: a2ashape.Lang(), transport: newEgressTransport(up), ctx: ctx, cancel: cancel,
		bodySlot: make(chan struct{}, 1), net: newEgressConnectNet()}
	e.srv = &http.Server{
		Handler:           e,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	return e
}

// newEgressTransport is the HTTP client side of the injection route (EG-5): TLS verified against the
// system roots, the daemon's own proxy variables honoured (the upstream host is fixed, so they cannot
// be turned against anything), no compression, and no redirect ever followed (the forwarded request
// carries the operator's key). A plain-http custom service is dialed only on the loopback.
func newEgressTransport(up *egressUpstream) *http.Transport {
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	proxy := http.ProxyFromEnvironment
	if up.LoopbackOnly {
		proxy = nil
		d.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
				return fmt.Errorf("the model service %s is not on this host's loopback", address)
			}
			return nil
		}
	}
	return &http.Transport{
		Proxy:                 proxy,
		DialContext:           d.DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   15 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
	}
}

// Serve accepts the run's connections on ln until Close.
func (e *egressEndpoint) Serve(ln net.Listener) {
	go func() { _ = e.srv.Serve(&egressListener{Listener: ln, e: e}) }()
}

// Close ends the run's egress (EG-1): the listener and every connection are closed, CONNECT tunnels
// among them, every forwarded request is cancelled, and Close returns once the handlers have.
func (e *egressEndpoint) Close() {
	e.closeOnce.Do(func() {
		e.gate.Lock()
		e.closed = true
		e.gate.Unlock()
		e.cancel()
		// A tunnel's connection was hijacked from the server, which no longer tracks it.
		e.tunnels.closeAll(true)
		_ = e.srv.Close()
		e.handlers.Wait()
		if t, ok := e.transport.(*http.Transport); ok {
			t.CloseIdleConnections()
		}
	})
}

// Summary is what the run did through the endpoint so far.
func (e *egressEndpoint) Summary() egressSummary { return e.stats.summary() }

// egressListener admits connections: only from the expected peer, and at most egressMaxConns at once;
// one over the limit is answered 429 egress_tunnels and closed (EG-4).
type egressListener struct {
	net.Listener
	e *egressEndpoint
}

func (l *egressListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.e.peerCheck != nil {
			if err := l.e.peerCheck(c); err != nil {
				log.Printf("anet: sandbox egress: refused a connection: %v", err)
				_ = c.Close()
				continue
			}
		}
		if l.e.conns.Add(1) > egressMaxConns {
			l.e.conns.Add(-1)
			l.e.stats.refused("egress_tunnels")
			go l.e.refuseConn(c)
			continue
		}
		return &egressConn{Conn: c, e: l.e}, nil
	}
}

// refuseConn answers a connection over the limit without reading its request, and closes it.
func (e *egressEndpoint) refuseConn(c net.Conn) {
	defer c.Close()
	body := e.refusalBody(http.StatusTooManyRequests, "egress_tunnels", nil)
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(c, "HTTP/1.1 429 Too Many Requests\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(body))
	_, _ = c.Write(body)
}

// egressConn gives its place back when it closes.
type egressConn struct {
	net.Conn
	e    *egressEndpoint
	once sync.Once
}

func (c *egressConn) Close() error {
	c.once.Do(func() { c.e.conns.Add(-1) })
	return c.Conn.Close()
}

// ServeHTTP answers one request from the sandbox.
func (e *egressEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.gate.RLock()
	if e.closed {
		e.gate.RUnlock()
		e.badGateway(w, context.Canceled)
		return
	}
	e.handlers.Add(1)
	e.gate.RUnlock()
	defer e.handlers.Done()
	if r.Method == http.MethodConnect {
		e.connect(w, r)
		return
	}
	upPath, routed, ok := e.routePath(r)
	switch {
	case !routed:
		e.refuse(w, refuseEgress(http.StatusForbidden, "egress_plain_http", nil))
	case !ok:
		e.refuse(w, refuseEgress(http.StatusForbidden, "policy_path", a2ashape.Args{"paths": e.up.pathsText()}))
	default:
		e.inject(w, r, upPath)
	}
}

// routePath reads the request target: routed when it is the injection route (origin form, or absolute
// form naming the endpoint itself), and then ok with the upstream path when it is this run's upstream
// and a plain path (no dot segments, doubled or trailing slashes, or escapes beyond the default).
func (e *egressEndpoint) routePath(r *http.Request) (upPath string, routed, ok bool) {
	u := r.URL
	if u == nil {
		return "", false, false
	}
	if u.IsAbs() || u.Host != "" {
		if !strings.EqualFold(u.Scheme, "http") || !egressSelf[strings.ToLower(u.Host)] {
			return "", false, false
		}
	}
	rest, isRoute := strings.CutPrefix(u.Path, egressRoutePrefix)
	if !isRoute {
		return "", false, false
	}
	name, p, _ := strings.Cut(rest, "/")
	p = "/" + p
	if name != e.up.Name || u.RawPath != "" || path.Clean(u.Path) != u.Path || strings.Contains(u.Path, "//") {
		return "", true, false
	}
	return p, true, true
}

// inject forwards one request on the injection route (UP-1, UP-4, BP-1 to BP-7, EG-4 to EG-6).
func (e *egressEndpoint) inject(w http.ResponseWriter, r *http.Request, upPath string) {
	kind, ok := e.up.match(r.Method, upPath)
	if !ok {
		e.refuse(w, refuseEgress(http.StatusForbidden, "policy_path", a2ashape.Args{"paths": e.up.pathsText()}))
		return
	}
	if !e.stats.requestsLeft() {
		e.refuse(w, refuseEgress(http.StatusTooManyRequests, "run_budget_requests", a2ashape.Args{"n": egressMaxRequests}))
		return
	}
	var body []byte
	hasBody := r.Method == http.MethodPost
	if hasBody {
		if !contentEncodingOK(r.Header) {
			e.refuse(w, refuseEgress(http.StatusUnsupportedMediaType, "policy_encoding", nil))
			return
		}
		select {
		case e.bodySlot <- struct{}{}:
		case <-r.Context().Done(): // the agent went away, or the run ended
			e.badGateway(w, r.Context().Err())
			return
		}
		var ref *egressRefusal
		body, ref = e.readInjectBody(r, kind)
		<-e.bodySlot
		if ref != nil {
			e.refuse(w, ref)
			return
		}
	}
	switch e.stats.reserve(len(body)) {
	case "run_budget_requests":
		e.refuse(w, refuseEgress(http.StatusTooManyRequests, "run_budget_requests", a2ashape.Args{"n": egressMaxRequests}))
		return
	case "run_budget_upload":
		e.refuse(w, refuseEgress(http.StatusTooManyRequests, "run_budget_upload", a2ashape.Args{"n": egressMaxInjectUpload >> 20}))
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(e.ctx, cancel)
	defer stop()
	var rd io.Reader = http.NoBody
	if hasBody {
		rd = bytes.NewReader(body)
	}
	out, err := http.NewRequestWithContext(ctx, r.Method, e.up.target(upPath, r.URL.RawQuery).String(), rd)
	if err != nil {
		e.badGateway(w, err)
		return
	}
	out.Header = injectHeaders(e.up, r.Header, hasBody)
	if hasBody {
		out.ContentLength = int64(len(body))
	}
	resp, err := e.transport.RoundTrip(out)
	if err != nil {
		e.badGateway(w, err)
		return
	}
	defer resp.Body.Close()
	filterResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			e.stats.down(n)
			if flusher != nil {
				flusher.Flush() // EG-6: a streamed answer reaches the agent as it arrives
			}
		}
		if rerr != nil {
			return
		}
	}
}

// readInjectBody reads a POST body of at most egressMaxBody and applies the body policy (BP-1 to BP-5).
// The caller holds the body slot.
func (e *egressEndpoint) readInjectBody(r *http.Request, kind egressRouteKind) ([]byte, *egressRefusal) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, egressMaxBody+1))
	if err != nil {
		// A body that ended early or was framed wrong; if the agent went away, nobody reads this.
		return nil, refuseEgress(http.StatusBadRequest, "policy_encoding", nil)
	}
	if len(raw) > egressMaxBody {
		return nil, refuseEgress(http.StatusRequestEntityTooLarge, "egress_too_large", a2ashape.Args{"n": egressMaxBody >> 20})
	}
	return rewriteInjectBody(e.up, kind, raw)
}

// refuse answers a request with a refusal and counts it (BP-7, EG-4).
func (e *egressEndpoint) refuse(w http.ResponseWriter, ref *egressRefusal) {
	e.stats.refused(ref.Code)
	body := e.refusalBody(ref.Status, ref.Code, ref.Args)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(ref.Status)
	_, _ = w.Write(body)
}

func (e *egressEndpoint) refusalBody(status int, code string, args a2ashape.Args) []byte {
	return egressRefusalBody(e.up, status, code, egressSentence(e.lang, code, args))
}

// badGateway answers a request the model service could not be asked: it was not reachable, or the run
// ended. The agent reads egress_upstream_unreachable's sentence in the service's error format; the
// detail stays in the local log.
func (e *egressEndpoint) badGateway(w http.ResponseWriter, err error) {
	if !errors.Is(err, context.Canceled) {
		log.Printf("anet: sandbox egress: %s: %v", e.up.Name, err)
	}
	body := e.refusalBody(http.StatusBadGateway, "egress_upstream_unreachable", nil)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write(body)
}
