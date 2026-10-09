package daemon

// egress_connect.go is CONNECT on a sandboxed run's egress endpoint (LEC §2.6.3, §4.3.5, CN-2 to CN-7;
// EG-4; SI-55): the one way a sandboxed agent reaches the network beyond its model, and only the hosts
// the operator allowed, each by its exact name, on port 443, over TLS that names the same host.
//
// A CONNECT goes through, in this order:
//
//  1. its host is an entry of auto_reply.sandbox.egress_allow, exactly (CN-2; hostname.go), and its port
//     is 443: else 403 egress_host_not_allowed or egress_port. With the list empty, the default, every
//     CONNECT ends here;
//  2. the run's tunnel allowance (EG-4): 16 tunnels at once and 256 in all, else 429 egress_tunnels; once
//     the run's tunnels have sent 32 MiB or received 512 MiB, all of them are closed and every later
//     CONNECT is 429 run_budget_upload or 413 egress_too_large;
//  3. the daemon resolves the host itself, within 5 seconds and through no proxy (CN-3); one address of a
//     special range or of this machine refuses the CONNECT as a whole (CN-4): 403 egress_private_address;
//  4. 200, then the agent's first TLS message is read, within 10 seconds and 16 KiB: a ClientHello naming
//     exactly the CONNECT's host, without Encrypted Client Hello (CN-5; clienthello.go). Otherwise the
//     connection is dropped and the reason, which has no sentence, goes to the run's evidence and the log;
//  5. only then is a checked address dialed, directly (CN-3, CN-4): nothing is resolved again. The
//     ClientHello is passed on as it came, and the tunnel carries bytes both ways, counted (CN-7, EG-4),
//     until a side ends it, a limit is reached or the run ends (EG-1).
//
// anet does not decrypt a tunnel, nor look at the HTTP Host inside it (CN-6): another site behind the
// same CDN as an allowed host can be reached by domain fronting, which the release notes state (LEC §6.2,
// item 4), along with every allowed host being a way to send data out.

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// The CONNECT port and times (CN-2, CN-4, CN-5). An endpoint keeps the resolve and hello times as its
// own (egressEndpoint.connectWait), which tests shorten.
const (
	egressConnectPort           = "443"
	egressConnectResolveTimeout = 5 * time.Second
	egressConnectHelloTimeout   = 10 * time.Second
	egressConnectDialTimeout    = 10 * time.Second
)

// egressConnectWait is how long a CONNECT waits for the host to resolve and for the agent's hello.
type egressConnectWait struct {
	resolve, hello time.Duration
}

// egressConnectNet is how CONNECT reaches the network: a resolver, a dialer that connects to the one
// address it is given and nowhere else, and this machine's own addresses. None of them reads a proxy
// setting (CN-3). Tests replace them.
type egressConnectNet struct {
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	dial   func(ctx context.Context, addr netip.AddrPort) (net.Conn, error)
	local  func() []netip.Addr
}

// newEgressConnectNet is the network of a new endpoint's CONNECTs; a test of a whole sandboxed run
// replaces it with a fake one.
var newEgressConnectNet = defaultEgressConnectNet

func defaultEgressConnectNet() egressConnectNet {
	return egressConnectNet{lookup: egressLookup, dial: egressDialChecked, local: egressLocalAddrs}
}

// egressLookup resolves a host with this machine's resolver. It is given the absolute name (with the
// final dot), so no search domain of the resolver's configuration is tried: an allowed public name never
// becomes an internal one.
func egressLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// egressDialChecked opens a TCP connection to addr, directly. The dialer is given an address, never a
// name, and its Control asserts that the connection it is about to open is to that address (CN-4).
func egressDialChecked(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	d := &net.Dialer{Timeout: egressConnectDialTimeout, Control: egressDialControl(addr)}
	return d.DialContext(ctx, "tcp", addr.String())
}

// egressDialControl refuses a connection to anything but want.
func egressDialControl(want netip.AddrPort) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		got, err := netip.ParseAddrPort(address)
		if err != nil || got.Addr().Unmap() != want.Addr().Unmap() || got.Port() != want.Port() {
			return fmt.Errorf("refusing to connect to %s: the address checked is %s", address, want)
		}
		return nil
	}
}

// egressTunnels are a run's CONNECT tunnels (EG-4): how many are reserved or open, how many were
// opened, and their connections, which end with the run.
type egressTunnels struct {
	mu     sync.Mutex
	active int // reserved or open
	total  int // opened, and reserved ones not yet refused
	conns  map[net.Conn]struct{}
	closed bool // the run ended: no tunnel opens, every one is closed
}

// reserve takes a place for one tunnel, or returns false: 16 are already open, or 256 were opened.
func (t *egressTunnels) reserve() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active >= egressMaxTunnels || t.total >= egressMaxTunnelsTotal {
		return false
	}
	t.active++
	t.total++
	return true
}

// release gives back a reserved place; one refused before its 200 does not count as opened.
func (t *egressTunnels) release(opened bool) {
	t.mu.Lock()
	t.active--
	if !opened {
		t.total--
	}
	t.mu.Unlock()
}

// track adds connections of an open tunnel, to be closed with the run. It returns false, and closes
// them, when the run has ended.
func (t *egressTunnels) track(cs ...net.Conn) bool {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		for _, c := range cs {
			_ = c.Close()
		}
		return false
	}
	if t.conns == nil {
		t.conns = map[net.Conn]struct{}{}
	}
	for _, c := range cs {
		t.conns[c] = struct{}{}
	}
	t.mu.Unlock()
	return true
}

func (t *egressTunnels) untrack(cs ...net.Conn) {
	t.mu.Lock()
	for _, c := range cs {
		delete(t.conns, c)
	}
	t.mu.Unlock()
}

// closeAll closes every open tunnel; with end, no tunnel opens after it either (the run ended).
func (t *egressTunnels) closeAll(end bool) {
	t.mu.Lock()
	if end {
		t.closed = true
	}
	cs := make([]net.Conn, 0, len(t.conns))
	for c := range t.conns {
		cs = append(cs, c)
	}
	t.mu.Unlock()
	for _, c := range cs {
		_ = c.Close()
	}
}

// connect answers a CONNECT (CN-2 to CN-7).
func (e *egressEndpoint) connect(w http.ResponseWriter, r *http.Request) {
	u := r.URL
	if u == nil || u.Host == "" || u.Scheme != "" || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		// Not CONNECT host:port: there is no host to look at.
		e.refuseConnect(w, http.StatusForbidden, "egress_plain_http", nil)
		return
	}
	raw := u.Hostname()
	host, why := normalizeEgressHost(raw)
	if why != "" || !e.up.ConnectAllow[host] {
		e.refuseConnect(w, http.StatusForbidden, "egress_host_not_allowed", a2ashape.Args{"host": egressArg(raw)})
		return
	}
	if u.Port() != egressConnectPort {
		e.refuseConnect(w, http.StatusForbidden, "egress_port", nil)
		return
	}
	switch e.stats.tunnelSpent() {
	case "up":
		e.refuseConnect(w, http.StatusTooManyRequests, "run_budget_upload", a2ashape.Args{"n": egressMaxTunnelUpload >> 20})
		return
	case "down":
		e.refuseConnect(w, http.StatusRequestEntityTooLarge, "egress_too_large", a2ashape.Args{"n": egressMaxTunnelDownload >> 20})
		return
	}
	if !e.tunnels.reserve() {
		e.refuseConnect(w, http.StatusTooManyRequests, "egress_tunnels", nil)
		return
	}
	opened := false
	defer func() { e.tunnels.release(opened) }()
	// The run's context, not the request's: once hijacked, the connection is no longer the server's to
	// watch, and the tunnel lasts as long as the run at most.
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	addrs, status, code := e.resolveConnect(ctx, host)
	switch {
	case code != "":
		e.refuseConnect(w, status, code, a2ashape.Args{"host": host})
		return
	case status != 0:
		// The host does not resolve: there is nothing to refuse, and no sentence for it.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Connection", "close")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, http.StatusText(status)+"\n")
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	agent, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer agent.Close()
	if !e.tunnels.track(agent) {
		return // the run ended
	}
	defer e.tunnels.untrack(agent)
	opened = true
	_ = agent.SetWriteDeadline(time.Now().Add(e.connectWait.hello))
	if _, err := io.WriteString(agent, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	_ = agent.SetWriteDeadline(time.Time{})
	// CN-5: the agent's first TLS message, before anything is dialed.
	_ = agent.SetReadDeadline(time.Now().Add(e.connectWait.hello))
	hello, body, drop := readClientHello(brw.Reader)
	if drop == "" {
		drop = checkClientHello(body, host)
	}
	if drop != "" {
		e.stats.connectRefused(drop)
		log.Printf("anet: sandbox egress: CONNECT %s dropped: %s", host, drop)
		return
	}
	_ = agent.SetReadDeadline(time.Time{})
	e.stats.connectAllowed(host)
	up, err := e.dialTunnel(ctx, addrs)
	if err != nil {
		log.Printf("anet: sandbox egress: CONNECT %s: %v", host, err)
		return
	}
	defer up.Close()
	if !e.tunnels.track(up) {
		return
	}
	defer e.tunnels.untrack(up)
	if !e.carry(true, len(hello)) {
		return
	}
	if _, err := up.Write(hello); err != nil {
		return
	}
	e.splice(agent, brw.Reader, up)
}

// refuseConnect answers a CONNECT with a refusal: the status and the code's sentence as plain text (LEC
// §2.6.4), counted by code (CN-7).
func (e *egressEndpoint) refuseConnect(w http.ResponseWriter, status int, code string, args a2ashape.Args) {
	e.stats.connectRefused(code)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, egressSentence(e.lang, code, args)+"\n")
}

// resolveConnect resolves an allowed host (CN-4): the addresses to dial on port 443, or the refusal
// (403 egress_private_address) when one of them is special or this machine's, or 502 when the host does
// not resolve.
func (e *egressEndpoint) resolveConnect(ctx context.Context, host string) (addrs []netip.AddrPort, status int, code string) {
	rctx, cancel := context.WithTimeout(ctx, e.connectWait.resolve)
	defer cancel()
	// The absolute name: a relative one would let the resolver try it under its search domains first
	// (with ndots above 1) or after a failure, and answer for another host than the one allowed.
	ips, err := e.net.lookup(rctx, host+".")
	if err != nil || len(ips) == 0 {
		log.Printf("anet: sandbox egress: CONNECT %s: the host does not resolve: %v", host, err)
		return nil, http.StatusBadGateway, ""
	}
	local := e.net.local()
	for _, ip := range ips {
		a := ip.Unmap().WithZone("")
		if egressAddrRefused(ip) || slices.Contains(local, a) {
			return nil, http.StatusForbidden, "egress_private_address"
		}
		if ap := netip.AddrPortFrom(a, 443); !slices.Contains(addrs, ap) {
			addrs = append(addrs, ap)
		}
	}
	return addrs, 0, ""
}

// dialTunnel connects to the first of the checked addresses that answers, within the dial time.
func (e *egressEndpoint) dialTunnel(ctx context.Context, addrs []netip.AddrPort) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, egressConnectDialTimeout)
	defer cancel()
	var last error
	for _, a := range addrs {
		c, err := e.net.dial(ctx, a)
		if err == nil {
			return c, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

// splice carries a tunnel both ways until it ends: when the agent stops sending, the upstream is told
// so and its answer still reaches the agent; when the upstream stops, the tunnel is over. A limit
// reached or the run's end closes both sides.
func (e *egressEndpoint) splice(agent net.Conn, fromAgent io.Reader, up net.Conn) {
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		e.pump(up, fromAgent, true)
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = up.Close()
		}
	}()
	e.pump(agent, up, false)
	_ = agent.Close()
	_ = up.Close()
	<-sent
}

// pump copies src to dst, counting what it carries against the run's tunnel allowance.
func (e *egressEndpoint) pump(dst io.Writer, src io.Reader, up bool) {
	buf := make([]byte, 32<<10)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if !e.carry(up, n) {
				return
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if rerr != nil {
			return
		}
	}
}

// carry counts n tunnel bytes, sent (up) or received (CN-7). When they would go over the run's
// allowance (EG-4: 32 MiB sent, 512 MiB received) they are not carried: every tunnel of the run is
// closed, and no new one opens.
func (e *egressEndpoint) carry(up bool, n int) bool {
	if e.stats.tunnelBytes(up, n) {
		return true
	}
	e.tunnels.closeAll(false)
	return false
}
