//go:build !no_p2p

package p2p

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/ANetResearch/ANet/module"
)

// CardInterfaces adds this node's direct interface to its network card
// (A2A-DESIGN §10.1, §10.4) when an advertise address is configured. It
// routes by tenant, the node's AID, and carries the same sealed envelopes
// as the relay; putting it in the card puts the address under the node's
// signature, which the hub's /agents/{aid}/p2p entry is not.
func (m *Module) CardInterfaces(c module.CardContext) []map[string]any {
	if m.cfg.Advertise == "" || c.AID == "" {
		return nil
	}
	u, err := directURL(m.cfg.Advertise)
	if err != nil {
		return nil // refused at start; not reachable here
	}
	return []map[string]any{{
		"url":             u,
		"protocolBinding": module.BindingP2PURI,
		"protocolVersion": "1.0",
		"tenant":          c.AID,
	}}
}

// CardExtensions adds no extension.
func (m *Module) CardExtensions(module.CardContext) []map[string]any { return nil }

// directURL turns an advertise value into the URL a card carries,
// tcp://host:port. A local socket, or a value without a host and port, is
// refused: neither can be dialled from another machine. A loopback address
// is accepted here, for peers on one machine, and the kernel leaves it off
// the network card, where 127.0.0.1 never goes.
func directURL(a string) (string, error) {
	a = strings.TrimSpace(a)
	addr, hadScheme := strings.CutPrefix(a, "tcp://")
	if strings.HasPrefix(a, "unix://") || (!hadScheme && strings.Contains(a, "/")) {
		return "", fmt.Errorf("p2p: advertise %q is a local socket; give the host:port other machines dial", a)
	}
	host, port, err := net.SplitHostPort(addr)
	if n, perr := strconv.Atoi(port); err != nil || host == "" || perr != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("p2p: advertise %q is not host:port", a)
	}
	return "tcp://" + net.JoinHostPort(host, port), nil
}

var _ module.CardContributor = (*Module)(nil)
