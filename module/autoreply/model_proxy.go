package autoreply

// model_proxy.go — auto_reply.model_proxy (design v0.3.9 §D): the model services of direct runs, keyed per
// run, with the node's keys kept by the node.
//
// A direct run (a trusted or an admitted peer's, through the runner or as the node's user) is the agent
// working with the network and the tools of its account. When that account is not the node's (the
// runner), the account's user can read the agent's environment and files, so the model keys of the node
// must not be there. The proxy is one listener on this machine's loopback for as long as the
// configuration names it, and it serves two things:
//
//   - the injection route of the egress code, /anet-upstream/<name>/<path> (egress.go, LEC §2.6.2), for
//     the configured upstreams: every request carries Authorization: Bearer <run key>, the random key of
//     one run, valid only while that run is under way. The request policy of the sandbox's route applies
//     (egress_policy.go: one uncompressed JSON object, the pinned model when the upstream names one,
//     bounded output, no server-side tools, no media by URL, the agent's credentials dropped), and the
//     upstream's key is added. Unknown key: 401 in the model service's error format.
//   - the turn tools, /anet-turn/v1/tools and /anet-turn/v1/call (turn_tools.go), with the run's turn
//     token.
//
// Every local account can reach a loopback port; without a key or token that is under way it gets the
// refusal and nothing else. Keys and tokens are compared through their SHA-256 digests and are never
// logged. Requests and answers are passed through and never written anywhere (EG-6).
//
// The key decides; the connections are bounded besides, so that no local account can take the listener
// from the runs: where the platform tells who holds the far end of a connection (Linux, internal/
// localpeer), a connection of an account other than the node's and the runner's is closed when it is
// accepted, and each account holds at most modelProxyMaxConnsPerUID connections. A request has
// modelProxyHeaderTimeout for its header and proxyBodyTimeout for its body, each run reads one body at a
// time (a run that sends its body slowly holds up its own requests only), and an idle connection is
// closed after modelProxyIdleTimeout. Nothing bounds how long an answer takes to stream back.
//
// What one run may use: requests_per_run model requests for an admitted peer's run (100 by default), and
// for a trusted peer's run up to trustedRequestsPerRun; an admitted peer's runs count against
// admitted_budget.runs_per_day and runs_per_peer_per_day (sandbox_budget.go's counts), and a task that
// finds the day's budget used up is refused before its run starts (direct_run.go). A run that has no
// place in the budget all the same gets model_budget_spent for each of its requests, which the agent
// reads as a model error and reports.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/fsperm"
	"github.com/ANetResearch/ANet/internal/localpeer"
	"github.com/ANetResearch/ANet/internal/turntools"
	"github.com/ANetResearch/ANet/module"
)

// modelProxyMaxConns bounds the connections to the proxy at once, and modelProxyMaxConnsPerUID those of
// one local account where the platform tells the account (proxyListener).
const (
	modelProxyMaxConns       = 128
	modelProxyMaxConnsPerUID = 64
)

// The proxy's timeouts: for a request's header, for its body (proxyBodyTimeout), for the whole request
// as the server's backstop, and for an idle connection. None bounds the answer, which a model may
// stream for as long as the run lasts. Variables so tests can shorten them.
var (
	modelProxyHeaderTimeout = 5 * time.Second
	proxyBodyTimeout        = 30 * time.Second
	modelProxyReadTimeout   = 60 * time.Second
	modelProxyIdleTimeout   = 30 * time.Second
)

// proxyPeerUID is the local account holding the far end of an accepted connection
// (localpeer.OwnerUID); localpeer.ErrNoSocketTable where the platform cannot tell. A proxy takes it
// when it starts; a variable so tests can stand in for another account.
var proxyPeerUID = localpeer.OwnerUID

// modelProxyMaxUpload bounds what one run's model requests send in all. An agent sends the whole
// conversation with every request; the bound is there to end a run that loops, not to shape one that works.
const modelProxyMaxUpload = 1 << 30

// modelProxyRunGrace is how long past its own timeout a run's key and token may still be used: the
// module ends them when the run ends, and the deadline only covers a run whose end was never reached.
const modelProxyRunGrace = time.Minute

// modelProxy is the listener of auto_reply.model_proxy.
type modelProxy struct {
	cfg  AutoReplyModelProxyConfig // as it was started, to tell whether a new configuration changes it
	ln   net.Listener
	addr string // the listener's address, its port resolved
	srv  *http.Server
	lang a2ashape.Language

	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	ups   map[string]*proxyUpstream
	keys  map[[32]byte]*proxyRun // model keys by digest
	turns map[[32]byte]*proxyRun // turn tokens by digest
	svc   module.ServiceCaller   // the turn tools' service; nil: none

	conns  atomic.Int32
	closed sync.Once

	// allow is the accounts whose connections are taken (setAccounts); uidConns counts each one's
	// connections. Both under connMu.
	connMu   sync.Mutex
	allow    func() map[int]bool
	uidConns map[int]int
	peerUID  func(net.Conn) (int, error)
}

// proxyUpstream is one upstream: the egress route's description of it (custom dialect) and its client.
type proxyUpstream struct {
	up        *egressUpstream
	transport http.RoundTripper
}

// proxyRun is one run's place in the proxy: whose it is, what it may use, and its key and token.
type proxyRun struct {
	p        *modelProxy
	peer     string // the caller's AID
	ix       string
	admitted bool
	deadline time.Time
	// refusal, when set, is the answer to every model request of the run (model_budget_spent).
	refusal string
	stats   egressStats
	calls   atomic.Int32 // turn tool calls

	ctx    context.Context // ends with the run: its forwarded requests and tool calls are cancelled
	cancel context.CancelFunc
	// bodySlot lets one request body of the run be read at a time.
	bodySlot chan struct{}

	key, token       string // "" when the run has none
	keyHash, tokHash [32]byte
	once             sync.Once
}

// newModelProxy starts the listener of cfg with its upstreams' keys read from their files. An error is a
// refusal naming the key of the configuration that is wrong.
func newModelProxy(cfg AutoReplyModelProxyConfig) (*modelProxy, error) {
	ups, err := proxyUpstreams(cfg)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", strings.TrimSpace(cfg.Listen))
	if err != nil {
		return nil, fmt.Errorf("auto_reply.model_proxy.listen: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &modelProxy{cfg: cfg, ln: ln, addr: ln.Addr().String(), lang: a2ashape.Lang(), ctx: ctx, cancel: cancel,
		ups: ups, keys: map[[32]byte]*proxyRun{}, turns: map[[32]byte]*proxyRun{}, uidConns: map[int]int{},
		peerUID: proxyPeerUID}
	// ReadTimeout bounds the reading of a request; the server lifts it once the body has been read, so
	// it does not bound the answer. No WriteTimeout: an answer streams for as long as the model writes
	// it, and the run's own end cancels it.
	p.srv = &http.Server{
		Handler:           p,
		ReadHeaderTimeout: modelProxyHeaderTimeout,
		ReadTimeout:       modelProxyReadTimeout,
		IdleTimeout:       modelProxyIdleTimeout,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() { _ = p.srv.Serve(&proxyListener{Listener: ln, p: p}) }()
	return p, nil
}

// proxyUpstreams reads the upstreams of cfg: each one the egress route's custom service, with its key.
func proxyUpstreams(cfg AutoReplyModelProxyConfig) (map[string]*proxyUpstream, error) {
	ups := map[string]*proxyUpstream{}
	for i, u := range cfg.Upstreams {
		at := "auto_reply.model_proxy.upstreams[" + strconv.Itoa(i) + "]"
		base, err := parseEgressAPIBase(strings.TrimSpace(u.APIBase))
		if err != nil {
			return nil, a2ashape.Refuse("api_base_insecure", nil)
		}
		key := ""
		if f := strings.TrimSpace(u.APIKeyFile); f != "" {
			if key, err = readKeyFile(f); err != nil {
				log.Printf("anet: auto-reply: model proxy upstream %s: the key file cannot be read: %v", u.Name, err)
				return nil, arConfigInvalid(at+".api_key_file", arRule("读不到，或其中没有密钥", "it cannot be read, or holds no key"))
			}
		}
		up := &egressUpstream{Name: u.Name, Base: base, Key: key, Model: strings.TrimSpace(u.Model), Routes: egressCustomRoutes,
			MaxOutput:    u.outputBound(),
			OpenRouter:   strings.TrimSuffix(strings.ToLower(base.Hostname()), ".") == "openrouter.ai",
			LoopbackOnly: base.Scheme == "http"}
		ups[u.Name] = &proxyUpstream{up: up, transport: newEgressTransport(up)}
	}
	return ups, nil
}

// readKeyFile is the first line of a key file, a regular file of at most 64 KiB.
func readKeyFile(name string) (string, error) {
	f, err := fsperm.OpenNoFollow(name, os.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := readRegular(f, 64<<10)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	if line = strings.TrimSpace(line); line == "" {
		return "", errors.New("empty")
	}
	return line, nil
}

// sameConfig reports a configuration this proxy already serves as it is.
func (p *modelProxy) sameConfig(cfg AutoReplyModelProxyConfig) bool {
	return reflect.DeepEqual(p.cfg, cfg)
}

// setAccounts sets the local accounts whose connections the proxy takes besides the node's own: the
// runner's, which accounts returns when a connection of another account is accepted (nil: the node's
// account only).
func (p *modelProxy) setAccounts(accounts func() []int) {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	if accounts == nil {
		p.allow = nil
		return
	}
	p.allow = func() map[int]bool {
		m := map[int]bool{}
		for _, u := range accounts() {
			m[u] = true
		}
		return m
	}
}

// setService sets the service the turn tools call.
func (p *modelProxy) setService(svc module.ServiceCaller) {
	p.mu.Lock()
	p.svc = svc
	p.mu.Unlock()
}

// Close ends the proxy: the listener, every connection and every forwarded request, and every run's key.
func (p *modelProxy) Close() {
	p.closed.Do(func() {
		p.cancel()
		_ = p.srv.Close()
		p.mu.Lock()
		runs := make([]*proxyRun, 0, len(p.keys)+len(p.turns))
		for _, r := range p.keys {
			runs = append(runs, r)
		}
		for _, r := range p.turns {
			runs = append(runs, r)
		}
		p.mu.Unlock()
		for _, r := range runs {
			r.end()
		}
		for _, u := range p.ups {
			if t, ok := u.transport.(*http.Transport); ok {
				t.CloseIdleConnections()
			}
		}
	})
}

// modelBase is the injection route's address as a run's environment gives it (turntools.EnvModelBase).
func (p *modelProxy) modelBase() string {
	return "http://" + p.addr + strings.TrimSuffix(egressRoutePrefix, "/")
}

// turnURL is the turn tools' address (turntools.EnvURL).
func (p *modelProxy) turnURL() string { return "http://" + p.addr + turntools.RoutePrefix }

// hasUpstreams reports whether the proxy forwards to any model service.
func (p *modelProxy) hasUpstreams() bool { return len(p.ups) > 0 }

// proxyRunSpec is what a run is given by begin.
type proxyRunSpec struct {
	Peer, Ix string
	Admitted bool
	// Model: the run gets a model key; MaxRequests bounds its requests; Refusal answers each of them.
	Model       bool
	MaxRequests int
	Refusal     string
	// Turn: the run gets a turn token.
	Turn bool
	// Timeout is the run's own; the key and token end at the latest modelProxyRunGrace after it.
	Timeout time.Duration
}

// begin mints the key and the token of a run and returns it; its end makes both unusable.
func (p *modelProxy) begin(s proxyRunSpec) (*proxyRun, error) {
	ctx, cancel := context.WithCancel(p.ctx)
	r := &proxyRun{p: p, peer: s.Peer, ix: s.Ix, admitted: s.Admitted, refusal: s.Refusal,
		deadline: time.Now().Add(s.Timeout + modelProxyRunGrace), ctx: ctx, cancel: cancel, bodySlot: make(chan struct{}, 1)}
	r.stats.maxRequests, r.stats.maxUpload = s.MaxRequests, modelProxyMaxUpload
	var err error
	if s.Model {
		if r.key, err = newSecret("mpk-"); err != nil {
			cancel()
			return nil, err
		}
		r.keyHash = sha256.Sum256([]byte(r.key))
	}
	if s.Turn {
		if r.token, err = newSecret("ttk-"); err != nil {
			cancel()
			return nil, err
		}
		r.tokHash = sha256.Sum256([]byte(r.token))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil {
		cancel()
		return nil, errors.New("the model proxy is closed")
	}
	if r.key != "" {
		p.keys[r.keyHash] = r
	}
	if r.token != "" {
		p.turns[r.tokHash] = r
	}
	return r, nil
}

// newSecret is prefix and 32 random bytes in hex.
func newSecret(prefix string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// env is what the run's environment gets: the model route and key, the turn tools' address and token.
func (r *proxyRun) env() []string {
	if r == nil {
		return nil
	}
	var out []string
	if r.key != "" {
		out = append(out, turntools.EnvModelBase+"="+r.p.modelBase(), turntools.EnvModelKey+"="+r.key)
	}
	if r.token != "" {
		out = append(out, turntools.EnvURL+"="+r.p.turnURL(), turntools.EnvToken+"="+r.token)
	}
	return out
}

// end makes the run's key and token unusable and cancels what is under way for it.
func (r *proxyRun) end() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		p := r.p
		p.mu.Lock()
		if r.key != "" && p.keys[r.keyHash] == r {
			delete(p.keys, r.keyHash)
		}
		if r.token != "" && p.turns[r.tokHash] == r {
			delete(p.turns, r.tokHash)
		}
		p.mu.Unlock()
		r.cancel()
	})
}

// summary is what the run did through the model route.
func (r *proxyRun) summary() egressSummary { return r.stats.summary() }

// lookup finds the run of a bearer credential in table, while the run is under way.
func (p *modelProxy) lookup(table map[[32]byte]*proxyRun, cred string) *proxyRun {
	if cred == "" {
		return nil
	}
	h := sha256.Sum256([]byte(cred))
	p.mu.Lock()
	r := table[h]
	p.mu.Unlock()
	if r == nil || r.ctx.Err() != nil || time.Now().After(r.deadline) {
		return nil
	}
	return r
}

// bearer is the credential of a request: Authorization: Bearer, or X-Api-Key for a client that sends its
// key that way. Exactly one of them, once.
func bearer(h http.Header) string {
	auth, xkey := h.Values("Authorization"), h.Values("X-Api-Key")
	switch {
	case len(auth) == 1 && len(xkey) == 0:
		scheme, cred, ok := strings.Cut(strings.TrimSpace(auth[0]), " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(cred)
		}
	case len(xkey) == 1 && len(auth) == 0:
		return strings.TrimSpace(xkey[0])
	}
	return ""
}

// proxyListener bounds the connections at once, in all and per local account, and takes no connection
// of an account other than the node's and the runner's; a connection it does not take is closed.
type proxyListener struct {
	net.Listener
	p *modelProxy
}

func (l *proxyListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, ok := l.p.admitConn(c)
		if !ok {
			_ = c.Close()
			continue
		}
		return &proxyConn{Conn: c, p: l.p, uid: uid}, nil
	}
}

// admitConn decides whether the proxy takes c, and counts it when it does. uid is the account that holds
// its far end, -1 where the platform cannot tell; such a connection is bounded only in all.
func (p *modelProxy) admitConn(c net.Conn) (uid int, ok bool) {
	uid, err := p.peerUID(c)
	switch {
	case errors.Is(err, localpeer.ErrNoSocketTable):
		uid = -1
	case err != nil:
		return 0, false // the far end is gone, or is not in the table: nobody's to take
	}
	if uid >= 0 && uid != os.Getuid() {
		p.connMu.Lock()
		allow := p.allow
		p.connMu.Unlock()
		if allow == nil || !allow()[uid] {
			return 0, false
		}
	}
	p.connMu.Lock()
	defer p.connMu.Unlock()
	if uid >= 0 && p.uidConns[uid] >= modelProxyMaxConnsPerUID {
		return 0, false
	}
	if p.conns.Add(1) > modelProxyMaxConns {
		p.conns.Add(-1)
		return 0, false
	}
	if uid >= 0 {
		p.uidConns[uid]++
	}
	return uid, true
}

type proxyConn struct {
	net.Conn
	p    *modelProxy
	uid  int
	once sync.Once
}

func (c *proxyConn) Close() error {
	c.once.Do(func() {
		p := c.p
		p.conns.Add(-1)
		if c.uid >= 0 {
			p.connMu.Lock()
			if p.uidConns[c.uid]--; p.uidConns[c.uid] <= 0 {
				delete(p.uidConns, c.uid)
			}
			p.connMu.Unlock()
		}
	})
	return c.Conn.Close()
}

// bodyDeadline bounds the read of a request's body from now (proxyBodyTimeout). The server lifts the
// bound once the body has been read to its end, so the answer, which may take longer, is not bounded.
func bodyDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(proxyBodyTimeout))
}

// ServeHTTP answers one request on the proxy's listener.
func (p *modelProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.IsAbs() || r.URL.Host != "" {
		p.refuse(w, nil, refuseEgress(http.StatusForbidden, "egress_plain_http", nil))
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, egressRoutePrefix):
		p.serveModel(w, r)
	case strings.HasPrefix(r.URL.Path, turntools.RoutePrefix+"/"):
		p.serveTurn(w, r)
	default:
		p.refuse(w, nil, refuseEgress(http.StatusNotFound, "model_proxy_unauthorized", nil))
	}
}

// serveModel is the injection route of the model proxy (egressEndpoint.inject, keyed by run).
func (p *modelProxy) serveModel(w http.ResponseWriter, r *http.Request) {
	run := p.lookup(p.keys, bearer(r.Header))
	rest := strings.TrimPrefix(r.URL.Path, egressRoutePrefix)
	name, upPath, _ := strings.Cut(rest, "/")
	pu := p.ups[name]
	var up *egressUpstream
	if pu != nil {
		up = pu.up
	}
	if run == nil {
		w.Header().Set("Connection", "close")
		p.refuse(w, up, refuseEgress(http.StatusUnauthorized, "model_proxy_unauthorized", nil))
		return
	}
	upPath = "/" + upPath
	if pu == nil || r.URL.RawPath != "" || path.Clean(r.URL.Path) != r.URL.Path || strings.Contains(r.URL.Path, "//") {
		run.refuse(w, up, refuseEgress(http.StatusForbidden, "policy_path.proxy", a2ashape.Args{"paths": p.pathsText()}))
		return
	}
	if run.refusal != "" {
		run.refuse(w, up, refuseEgress(http.StatusTooManyRequests, run.refusal, nil))
		return
	}
	kind, ok := up.match(r.Method, upPath)
	if !ok {
		run.refuse(w, up, refuseEgress(http.StatusForbidden, "policy_path.proxy", a2ashape.Args{"paths": p.pathsText()}))
		return
	}
	maxReq, _ := run.stats.limits()
	if !run.stats.requestsLeft() {
		run.refuse(w, up, refuseEgress(http.StatusTooManyRequests, "run_budget_requests", a2ashape.Args{"n": maxReq}))
		return
	}
	var body []byte
	hasBody := r.Method == http.MethodPost
	if hasBody {
		if !contentEncodingOK(r.Header) {
			run.refuse(w, up, refuseEgress(http.StatusUnsupportedMediaType, "policy_encoding", nil))
			return
		}
		// The run's own slot: a run that sends its body slowly holds up its own requests, never another
		// run's, and each of them for proxyBodyTimeout at most.
		select {
		case run.bodySlot <- struct{}{}:
		case <-run.ctx.Done():
			p.badGateway(w, up, context.Canceled)
			return
		case <-r.Context().Done():
			p.badGateway(w, up, r.Context().Err())
			return
		}
		bodyDeadline(w)
		var ref *egressRefusal
		body, ref = proxyBody(r, up, kind)
		<-run.bodySlot
		if ref != nil {
			run.refuse(w, up, ref)
			return
		}
	}
	switch run.stats.reserve(len(body)) {
	case "run_budget_requests":
		run.refuse(w, up, refuseEgress(http.StatusTooManyRequests, "run_budget_requests", a2ashape.Args{"n": maxReq}))
		return
	case "run_budget_upload":
		run.refuse(w, up, refuseEgress(http.StatusTooManyRequests, "run_budget_upload", a2ashape.Args{"n": modelProxyMaxUpload >> 20}))
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(run.ctx, cancel)
	defer stop()
	var rd io.Reader = http.NoBody
	if hasBody {
		rd = bytes.NewReader(body)
	}
	out, err := http.NewRequestWithContext(ctx, r.Method, up.target(upPath, r.URL.RawQuery).String(), rd)
	if err != nil {
		p.badGateway(w, up, err)
		return
	}
	out.Header = injectHeaders(up, r.Header, hasBody)
	if hasBody {
		out.ContentLength = int64(len(body))
	}
	resp, err := pu.transport.RoundTrip(out)
	if err != nil {
		p.badGateway(w, up, err)
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
			run.stats.down(n)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

// proxyBody reads a POST body of at most egressMaxBody and applies the request policy, with the proxy's
// wording of the refusals that name the sandbox.
func proxyBody(r *http.Request, up *egressUpstream, kind egressRouteKind) ([]byte, *egressRefusal) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, egressMaxBody+1))
	if err != nil {
		return nil, refuseEgress(http.StatusBadRequest, "policy_encoding", nil)
	}
	if len(raw) > egressMaxBody {
		return nil, refuseEgress(http.StatusRequestEntityTooLarge, "egress_too_large", a2ashape.Args{"n": egressMaxBody >> 20})
	}
	out, ref := rewriteInjectBody(up, kind, raw)
	if ref != nil {
		switch ref.Code {
		case "policy_field", "policy_path", "policy_server_tool", "policy_mcp_servers":
			ref.Code += ".proxy"
		}
	}
	return out, ref
}

// pathsText lists the calls the proxy forwards, for policy_path.proxy.
func (p *modelProxy) pathsText() string {
	if len(p.ups) == 0 {
		return "none"
	}
	names := make([]string, 0, len(p.ups))
	for n := range p.ups {
		names = append(names, n)
	}
	sort.Strings(names)
	var parts []string
	for _, n := range names {
		parts = append(parts, p.ups[n].up.pathsText())
	}
	return strings.Join(parts, ", ")
}

// refuse answers a request with a refusal in the dialect of up (the OpenAI error format without one).
func (p *modelProxy) refuse(w http.ResponseWriter, up *egressUpstream, ref *egressRefusal) {
	body := egressRefusalBody(up, ref.Status, a2ashape.ReasonCode(ref.Code), a2ashape.Sentence(ref.Code, p.lang, ref.Args))
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(ref.Status)
	_, _ = w.Write(body)
}

// refuse is the proxy's refusal, counted for the run.
func (r *proxyRun) refuse(w http.ResponseWriter, up *egressUpstream, ref *egressRefusal) {
	r.stats.refused(a2ashape.ReasonCode(ref.Code))
	r.p.refuse(w, up, ref)
}

// badGateway answers a request the model service could not be asked; the detail stays in the local log.
func (p *modelProxy) badGateway(w http.ResponseWriter, up *egressUpstream, err error) {
	if !errors.Is(err, context.Canceled) {
		name := ""
		if up != nil {
			name = up.Name
		}
		log.Printf("anet: auto-reply: model proxy: %s: %v", name, err)
	}
	p.refuse(w, up, refuseEgress(http.StatusBadGateway, "egress_upstream_unreachable", nil))
}
