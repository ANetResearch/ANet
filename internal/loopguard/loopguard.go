// Package loopguard holds the loopback checks shared by the two local HTTP
// surfaces of a node: the control plane (internal/daemon ctlsec.go) and the
// local A2A interface (module/a2a). Both answer only to a Host header that
// names the listener by a loopback name and its own port (421 otherwise),
// and both refuse to listen anywhere but loopback (A2A-DESIGN SI-7, §7.1,
// §11.4).
//
// Why a package of its own: the rules used to live twice, once as methods
// and helpers of the daemon and once copied into module/a2a, which may not
// import internal/daemon. Two copies of a security check drift; one of them
// accepting a name the other refuses is a DNS-rebinding hole in whichever is
// looser. Here they are pure functions with no dependency beyond the
// standard library, so neither side pulls the other in — in particular this
// package must never import the A2A SDK or internal/daemon (SI-8; the test
// in deps_test.go enforces it).
//
// What differs between the two surfaces stays with them: the control plane
// accepts an Origin naming itself (the console page) where the A2A
// interface refuses every Origin, and each writes its own error body.
package loopguard

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// LoopbackName reports whether host (without port; brackets are removed) is
// one of the three names a local surface answers to: 127.0.0.1, localhost
// or ::1. The comparison is case-insensitive.
//
// It is a list of names, not a test of where a name resolves: a name that
// DNS points at 127.0.0.1 is exactly what a rebinding attack sends, and
// other 127/8 addresses are left out so that a client never has a working
// address whose Host the guard refuses.
func LoopbackName(host string) bool {
	switch strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// AllowedHost reports whether a Host header value names the listener on
// port: a loopback name and exactly that port. A Host without a port is
// accepted only for a listener on port 80, which is what a browser sends
// for that port. An empty port matches nothing.
func AllowedHost(host, port string) bool {
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		h, p = host, "80"
	}
	return port != "" && p == port && LoopbackName(h)
}

// AllowedOrigin reports whether an Origin header value is a page served by
// the listener on port: plain http, no user info, no path beyond "/", no
// query, and a host AllowedHost accepts.
func AllowedOrigin(origin, port string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return false
	}
	return AllowedHost(u.Host, port)
}

// ListenerPort is the port of the listener that accepted r. A request
// constructed in-process (no connection) falls back to the port of
// fallbackAddr, and to "" when that has none — which AllowedHost matches
// with nothing.
func ListenerPort(r *http.Request, fallbackAddr string) string {
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && a != nil {
		if _, p, err := net.SplitHostPort(a.String()); err == nil && p != "" {
			return p
		}
	}
	_, p, _ := net.SplitHostPort(fallbackAddr)
	return p
}

// ErrNotLoopback is wrapped by the error CheckLoopbackAddr returns for an
// address whose host is not one of the loopback names, so that a caller can
// word its own refusal.
var ErrNotLoopback = errors.New("not a loopback address")

// CheckLoopbackAddr accepts a listen address host:port whose host is one of
// the names LoopbackName accepts. An empty host (":39811") listens on every
// interface and is refused, as is any other address.
//
// It checks the host only. The port is the caller's rule: the control plane
// is bound on port 0 in tests, the local A2A interface records a real port.
func CheckLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port: %w", addr, err)
	}
	if !LoopbackName(host) {
		return fmt.Errorf("%q: %w (only 127.0.0.1, localhost or [::1])", addr, ErrNotLoopback)
	}
	return nil
}
