// Package localpeer confirms that a loopback listener is this user's own anet daemon before a credential
// is sent to it (A2A-DESIGN §7.1, SI-7 [redteam:F18]).
//
// The local surfaces of a node listen on loopback TCP ports, and a loopback port is not private: any
// local account can bind one that is free. The daemon does not always hold its port — after a reboot or
// a crash, during an upgrade, before `anet up` has started it — and the ports are predictable (fixed
// scan bases, and listed for every user in /proc/net/tcp). A client that sends its bearer token to
// whatever answers on the configured port hands the credential to whoever bound that port first. The
// token files are 0600 because other local users are adversaries (§7.8, X5); a client that gives the
// token away over a socket undoes that.
//
// So a client verifies the far end of each connection before the first request that carries a token:
//
//   - On Linux, from the kernel's socket table: the server end of this very connection (/proc/net/tcp
//     and tcp6, matched by the four-tuple) must belong to this process's uid. The check is made on the
//     connection that will carry the token, so nothing can change between checking and sending, and it
//     needs nothing from the daemon, so an older daemon passes it.
//   - Elsewhere, and on Linux when the socket table cannot be read, by challenge: the client sends a
//     random nonce to GET /ping?proof=<nonce> on the same connection, and the daemon answers with an
//     HMAC, keyed by a key derived from the token, over the nonce and the address of the listener that
//     accepted the connection (Answer). Only a holder of the token can answer. Binding the listener
//     address defeats a squatter that relays the challenge to the real daemon listening elsewhere
//     (another port, or the other loopback family on the same port).
//
// A process of the same uid is not stopped by either check, and need not be: it can read the token file
// itself (A2A-DESIGN §21 item 13).
//
// Standard library only; never internal/daemon or an A2A SDK (the deps test in internal/loopguard).
package localpeer

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrNotOurs is wrapped by the error a check returns when the listener is not this user's daemon.
var ErrNotOurs = errors.New("the listener is not this user's anet daemon")

// NotOursError names the listener that failed a check. UID is the uid that owns it when the socket
// table said so, and -1 when the listener failed the challenge.
type NotOursError struct {
	Addr string
	UID  int
}

func (e *NotOursError) Error() string {
	if e.UID >= 0 {
		return fmt.Sprintf("%s is held by uid %d, not by this user's anet daemon (uid %d); the token was not sent",
			e.Addr, e.UID, os.Getuid())
	}
	return fmt.Sprintf("the process listening on %s did not prove it is this node's anet daemon; the token was not sent "+
		"(an anet daemon older than this client cannot prove it: restart it)", e.Addr)
}

func (e *NotOursError) Unwrap() error { return ErrNotOurs }

// errNoSocketTable means the socket table could not be read on this system; the challenge is used.
var errNoSocketTable = errors.New("localpeer: no socket table")

// ProofParam is the query parameter of GET /ping carrying the client's challenge, in hex.
const ProofParam = "proof"

// Nonce bounds, in bytes. The client sends nonceBytes; the daemon answers any nonce in range.
const (
	nonceBytes    = 32
	minNonceBytes = 16
	maxNonceBytes = 64
)

// challengeTimeout bounds the challenge exchange when the caller's context sets no earlier deadline.
const challengeTimeout = 5 * time.Second

// Proof is the answer to a challenge: HMAC-SHA256 under a key derived from token, over the nonce and the
// address of the listener that accepted the connection. The key derivation keeps the token itself from
// being a MAC key for data an unauthenticated caller chooses.
func Proof(token string, nonce []byte, listener netip.AddrPort) string {
	k := hmac.New(sha256.New, []byte(token))
	k.Write([]byte("anet/localpeer-key/v1"))
	m := hmac.New(sha256.New, k.Sum(nil))
	m.Write([]byte("anet/localpeer-proof/v1\x00"))
	m.Write(nonce)
	m.Write([]byte{0})
	m.Write([]byte(canonical(listener).String()))
	return hex.EncodeToString(m.Sum(nil))
}

// Answer is the daemon's side of the challenge: the proof for the nonce in r's ProofParam, bound to the
// local address of the connection r arrived on. It reports false when r carries no valid nonce or was
// not received on a TCP connection.
func Answer(r *http.Request, token string) (string, bool) {
	q := r.URL.Query().Get(ProofParam)
	if q == "" || token == "" {
		return "", false
	}
	nonce, err := hex.DecodeString(q)
	if err != nil || len(nonce) < minNonceBytes || len(nonce) > maxNonceBytes {
		return "", false
	}
	a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || a == nil {
		return "", false
	}
	ap, ok := addrPort(a)
	if !ok {
		return "", false
	}
	return Proof(token, nonce, ap), true
}

// Verify confirms that the far end of conn, a connection to a loopback listener, is this user's anet
// daemon holding token. It returns nil, an error wrapping ErrNotOurs when the listener is somebody
// else's, or another error when the check could not be made. Either way a non-nil error means the
// token must not be sent on conn.
func Verify(ctx context.Context, conn net.Conn, token string) error {
	local, lok := addrPort(conn.LocalAddr())
	far, rok := addrPort(conn.RemoteAddr())
	if !lok || !rok {
		return fmt.Errorf("localpeer: %v is not a TCP connection", conn.RemoteAddr())
	}
	if !far.Addr().IsLoopback() {
		return fmt.Errorf("localpeer: %s is not a loopback address; a token is sent only to loopback", far)
	}
	uid, err := serverUID(local, far)
	switch {
	case err == nil && uid == os.Getuid():
		return nil
	case err == nil:
		return &NotOursError{Addr: far.String(), UID: uid}
	case errors.Is(err, errNoSocketTable):
		return challenge(ctx, conn, token, far)
	default:
		return err
	}
}

// challenge sends a nonce on conn and checks the proof that comes back (see Answer). The exchange is an
// ordinary HTTP/1.1 request on the connection, which then carries the caller's own requests.
func challenge(ctx context.Context, conn net.Conn, token string, far netip.AddrPort) error {
	if token == "" {
		return errors.New("localpeer: no token to check the listener against")
	}
	nonce := make([]byte, nonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	dl := time.Now().Add(challengeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(dl) {
		dl = d
	}
	_ = conn.SetDeadline(dl)
	defer conn.SetDeadline(time.Time{})
	if _, err := fmt.Fprintf(conn, "GET /ping?%s=%s HTTP/1.1\r\nHost: %s\r\nUser-Agent: anet-localpeer\r\nAccept: application/json\r\n\r\n",
		ProofParam, hex.EncodeToString(nonce), far.String()); err != nil {
		return fmt.Errorf("localpeer: challenge %s: %w", far, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return fmt.Errorf("localpeer: challenge %s: %w", far, err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("localpeer: challenge %s: %w", far, err)
	}
	// The connection goes on to carry the caller's requests; anything left over, or a listener that
	// means to close it, would desynchronize them.
	if br.Buffered() != 0 || resp.Close {
		return &NotOursError{Addr: far.String(), UID: -1}
	}
	var out struct {
		Proof string `json:"proof"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK || !hmac.Equal([]byte(out.Proof), []byte(Proof(token, nonce, far))) {
		return &NotOursError{Addr: far.String(), UID: -1}
	}
	return nil
}

// Transport returns an HTTP transport whose every connection is verified (Verify) before it is used.
// Transports are cached per token, so connections are reused across the clients Client returns.
func Transport(token string) *http.Transport {
	transports.mu.Lock()
	defer transports.mu.Unlock()
	if t, ok := transports.m[token]; ok {
		return t
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	t := &http.Transport{
		// Never through a proxy: the check is about the socket the token travels on.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if err := Verify(ctx, c, token); err != nil {
				_ = c.Close()
				return nil, err
			}
			return c, nil
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 90 * time.Second,
	}
	if transports.m == nil {
		transports.m = map[string]*http.Transport{}
	}
	transports.m[token] = t
	return t
}

var transports struct {
	mu sync.Mutex
	m  map[string]*http.Transport
}

// Client returns an HTTP client for a local surface that holds token: connections are verified before
// use (Transport), and redirects are not followed, so a request and its token go only where the caller
// sent them. A zero timeout means none.
func Client(token string, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: Transport(token),
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ListenerUIDs returns the uids owning the TCP listeners that hold addr (host:port): a listener on that
// address, or on the wildcard address of its family with the same port. An empty result means nothing
// listens there. The error is non-nil when the socket table cannot be read on this system (only Linux
// has one), and the caller then cannot tell.
func ListenerUIDs(addr string) ([]int, error) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		host, port, serr := net.SplitHostPort(addr)
		if serr != nil || !strings.EqualFold(host, "localhost") {
			return nil, fmt.Errorf("localpeer: %q is not ip:port", addr)
		}
		ap, err = netip.ParseAddrPort("127.0.0.1:" + port)
		if err != nil {
			return nil, fmt.Errorf("localpeer: %q is not ip:port", addr)
		}
	}
	ap = canonical(ap)
	if uid, ok := foreignForTest(ap); ok {
		return []int{uid}, nil
	}
	ents, err := readTables(func(e sockEntry) bool {
		if e.state != stateListen || e.local.Port() != ap.Port() {
			return false
		}
		switch la := e.local.Addr(); {
		case la == ap.Addr():
			return true
		case !la.IsUnspecified():
			return false
		case la.Is4():
			return ap.Addr().Is4()
		default:
			return true // [::], which on a dual-stack socket holds the IPv4 port too
		}
	})
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.uid)
	}
	return out, nil
}

// serverUID returns the uid owning the server end of the connection local -> far.
func serverUID(local, far netip.AddrPort) (int, error) {
	if uid, ok := foreignForTest(far); ok {
		return uid, nil
	}
	var ents []sockEntry
	var err error
	// The server end is in the table from the moment the listener saw the SYN (as SYN_RECV, carrying the
	// listener's uid). A short retry covers a table read that races the handshake's last step.
	for attempt := 0; attempt < 3; attempt++ {
		ents, err = readTables(func(e sockEntry) bool {
			return (e.state == stateEstablished || e.state == stateSynRecv) && e.local == far && e.remote == local
		})
		if err != nil || len(ents) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		return 0, err
	}
	if len(ents) == 0 {
		return 0, fmt.Errorf("localpeer: the server end of %s -> %s is not in the socket table; the token was not sent", local, far)
	}
	uid := ents[0].uid
	for _, e := range ents[1:] {
		if e.uid != uid {
			return 0, fmt.Errorf("localpeer: %s: ambiguous socket table entries; the token was not sent", far)
		}
	}
	return uid, nil
}

// addrPort converts a TCP address, unmapping an IPv4-mapped IPv6 address.
func addrPort(a net.Addr) (netip.AddrPort, bool) {
	ta, ok := a.(*net.TCPAddr)
	if !ok || ta == nil {
		ap, err := netip.ParseAddrPort(a.String())
		return canonical(ap), err == nil
	}
	return canonical(ta.AddrPort()), true
}

func canonical(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap().WithZone(""), ap.Port())
}

// foreign holds the addresses TreatAsForeignForTest has marked.
var foreign sync.Map // netip.AddrPort -> struct{}

func foreignForTest(ap netip.AddrPort) (int, bool) {
	if _, ok := foreign.Load(canonical(ap)); ok {
		return os.Getuid() + 1, true
	}
	return 0, false
}

// TreatAsForeignForTest makes the socket-table lookups report the listener on addr (ip:port) as owned
// by another uid, until restore is called. It exists because a test cannot bind a port as a second
// local user without privileges it does not have; with it, a listener the test runs stands in for
// another user's. Nothing but tests calls it.
func TreatAsForeignForTest(addr string) (restore func()) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		panic("localpeer: TreatAsForeignForTest: " + err.Error())
	}
	ap = canonical(ap)
	foreign.Store(ap, struct{}{})
	return func() { foreign.Delete(ap) }
}
