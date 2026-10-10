// Package p2paddr decides where a direct (p2p) delivery may be dialled
// (sealed-sender SSR-P4, SI-18).
//
// The address comes from the recipient: it is the p2p field of the
// contact-class peer capability the recipient gave this node (SSR-D8), and
// the recipient chose it. Dialled as given, it would let any contact point
// this node's peer process at the machine it runs on, or at the network
// behind it — a port scanner driven from outside. So an address is dialled
// only when every IP it is dialled at is a public one: never loopback, the
// unspecified address, multicast, link-local, a private range (RFC 1918,
// IPv6 unique local), the shared address space 100.64.0.0/10 or 0.0.0.0/8.
// LAN mode (modules.p2p.lan, anetpeer --lan) lets the private ranges and
// 100.64.0.0/10 through, for a lab on one network; everything else stays
// refused.
//
// The check is made twice. The daemon checks the literal before it hands
// the address to its peer process (LiteralRefused), so that an address no
// dial could go to never leaves it. The peer process resolves a name once,
// checks every address it resolved to, and dials only those it checked
// (Refused): a name that resolves to a public address for the check and a
// private one for the dial (DNS rebinding) gets no second lookup.
package p2paddr

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// MaxLen is the longest address a grant carries (core §2.3).
const MaxLen = 262

var (
	sharedSpace = netip.MustParsePrefix("100.64.0.0/10") // RFC 6598, carrier-grade NAT
	thisNetwork = netip.MustParsePrefix("0.0.0.0/8")     // RFC 1122 "this network"
)

// Split reads an address as a grant carries it: host:port, the port a
// decimal number from 1 to 65535, at most MaxLen bytes. The host is a name
// or an IP literal (an IPv6 one in brackets).
func Split(addr string) (host string, port uint16, err error) {
	if len(addr) > MaxLen {
		return "", 0, fmt.Errorf("the address is %d bytes, over %d", len(addr), MaxLen)
	}
	h, p, err := net.SplitHostPort(addr)
	if err != nil || h == "" {
		return "", 0, fmt.Errorf("%q is not host:port", addr)
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil || n < 1 || strings.TrimLeft(p, "0123456789") != "" {
		return "", 0, fmt.Errorf("the port of %q is not a number from 1 to 65535", addr)
	}
	return h, uint16(n), nil
}

// Refused says why a dial to ip is refused, "" when it may go. An
// IPv4-mapped IPv6 address is judged as the IPv4 address it maps.
func Refused(ip netip.Addr, lan bool) string {
	ip = ip.Unmap()
	switch {
	case !ip.IsValid():
		return "not an IP address"
	case ip.IsLoopback():
		return "a loopback address"
	case ip.IsUnspecified():
		return "the unspecified address"
	case ip.IsMulticast(), ip.IsLinkLocalMulticast():
		return "a multicast address"
	case ip.IsLinkLocalUnicast():
		return "a link-local address"
	case ip.Is4() && thisNetwork.Contains(ip):
		return "an address of 0.0.0.0/8"
	case lan:
		return ""
	case ip.IsPrivate():
		return "a private address (LAN mode is off)"
	case ip.Is4() && sharedSpace.Contains(ip):
		return "an address of the shared space 100.64.0.0/10 (LAN mode is off)"
	}
	return ""
}

// ErrRefused is what a refused address matches with errors.Is.
var ErrRefused = errors.New("p2p: the address is not one a direct delivery is dialled at")

// RefusedError is a refused address: the address and why.
type RefusedError struct {
	Addr string
	Why  string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("p2p: %s is not dialled: %s", e.Addr, e.Why)
}

// Is makes errors.Is(err, ErrRefused) hold.
func (e *RefusedError) Is(target error) bool { return target == ErrRefused }

// LiteralRefused is the daemon's check of an address before its peer
// process gets it: a *RefusedError for an address that is not host:port,
// for an IP literal Refused refuses and for the name localhost (or a name
// under it); nil for any other name, which the peer process checks when it
// has resolved it.
func LiteralRefused(addr string, lan bool) error {
	host, _, err := Split(addr)
	if err != nil {
		return &RefusedError{Addr: addr, Why: err.Error()}
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if why := Refused(ip, lan); why != "" {
			return &RefusedError{Addr: addr, Why: why}
		}
		return nil
	}
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return &RefusedError{Addr: addr, Why: "the name localhost"}
	}
	return nil
}

// CheckAdvertise is why an advertise value (modules.p2p.advertise) is not an
// address a contact could dial, nil when it is: a local socket, or anything
// but host:port as Split reads it. "tcp://host:port" is taken as host:port,
// and surrounding space is ignored. Empty is no address, and fine.
func CheckAdvertise(a string) error {
	if a == "" {
		return nil
	}
	addr, hadScheme := strings.CutPrefix(strings.TrimSpace(a), "tcp://")
	if strings.HasPrefix(addr, "unix://") || (!hadScheme && strings.Contains(addr, "/")) {
		return fmt.Errorf("%q is a local socket; give the host:port other machines dial", a)
	}
	if _, _, err := Split(addr); err != nil {
		return err
	}
	return nil
}
