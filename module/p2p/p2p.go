//go:build !no_p2p

// Package p2p is the peer-to-peer transport module: it lets this node
// deliver a delegation straight to another node instead of relaying it
// through a hub.
//
// The peer stack runs in a separate process.
//
// That is not a preference, it is arithmetic. anet4's core resolves 82
// go.sum entries and two direct dependencies. anet3's libp2p stack brings
// 32 libp2p modules inside a tree of over a thousand — and a build tag
// removes the *code* from the binary, never the dependency from go.mod:
// every build still resolves it, CI still downloads it, and a supply-chain
// audit still has to cover it. Growing the core's dependency surface
// twelvefold for a module that is off by default is the wrong trade, and
// the anet4 rule already names it: out-of-process for the heavy, the
// multi-language, and the high-risk.
//
// So this module is a client. It speaks a small line protocol to a peer
// process over a Unix socket, and presents that process to the daemon as an
// ordinary module.Transport. The daemon learns nothing about peer ids,
// multiaddrs or pubsub topics — the same red line C1 draws around devices,
// drawn again around transports.
package p2p

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/ANetResearch/ANet/internal/p2paddr"
	"github.com/ANetResearch/ANet/module"
)

const name = "p2p"

func init() {
	module.Register(name, func(raw []byte) (module.Module, error) {
		if len(raw) == 0 {
			return nil, nil // compiled in, not configured
		}
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		if cfg.Socket == "" {
			return nil, fmt.Errorf("socket is required")
		}
		if err := platformRefusal(); err != nil {
			return nil, err
		}
		// The daemon refuses a relative socket before it starts
		// (config_invalid); refused here too, for whoever builds the
		// module another way, rather than as a dial that fails every
		// time it is retried.
		if !filepath.IsAbs(cfg.Socket) {
			return nil, fmt.Errorf("socket %q is not an absolute path", cfg.Socket)
		}
		// An advertise value no contact could dial is refused at start,
		// rather than left out of every grant without a word.
		if err := p2paddr.CheckAdvertise(cfg.Advertise); err != nil {
			return nil, fmt.Errorf("advertise: %v", err)
		}
		return &Module{cfg: cfg}, nil
	})
}

// Config points the module at a running peer process.
type Config struct {
	// Socket is where the peer process listens.
	Socket string `json:"socket"`
	// DialTimeoutMS bounds one delivery attempt: how long a send waits for
	// the peer process's answer, delivered or not (default
	// DefaultSendTimeout). It is longer than the peer process's own bound
	// for a delivery (dial, hand-off to the receiving daemon, reply), so the
	// daemon hears that answer instead of giving up while the peer process
	// is still at work: a send given up on goes through the hub, and a peer
	// process that then gets it through has delivered it twice (an internal note item,
	// an internal note N3). What keeps a peer that cannot be reached from
	// making every delegation slow is the peer process, which dials with a
	// short timeout and then does not dial an address it could not dial
	// again for a while. A value below the peer process's bound
	// brings the double delivery back.
	DialTimeoutMS int `json:"dial_timeout_ms"`
	// Advertise is the address other peers dial to reach this node's peer
	// process directly, host:port. It goes only into the contact-class
	// peer capabilities this node gives its contacts (sealed-sender SSR-D8,
	// the daemon reads it): never into the network card, which anyone may
	// read. Empty, contacts get no direct address and reach this node
	// through the hub.
	Advertise string `json:"advertise,omitempty"`
	// LAN lets a direct delivery be dialled at a private address (RFC 1918,
	// IPv6 unique local) and at 100.64.0.0/10 — for a lab on one network,
	// never for a node on the internet. Off, those are refused like
	// loopback and link-local, which stay refused either way
	// (internal/p2paddr). The daemon checks the addresses it hands its
	// peer process with it; the peer process, which resolves names, takes
	// the same choice as its own --lan flag.
	LAN bool `json:"lan,omitempty"`
}

// DefaultSendTimeout is how long a send waits for the peer process's
// answer when dial_timeout_ms is not set. tools/anetpeer bounds one
// delivery at 18 s (a 3 s dial, 10 s for the receiving daemon's ack or
// nack, 5 s for the reply to travel); this is past that.
const DefaultSendTimeout = 20 * time.Second

// Module registers the peer transport.
type Module struct {
	cfg  Config
	tr   *Transport
	stop context.CancelFunc
}

func (m *Module) Name() string { return name }

func (m *Module) Start(ctx context.Context, h module.Host) error {
	th, ok := h.(module.TransportHost)
	if !ok {
		return fmt.Errorf("host does not accept transports")
	}
	send := time.Duration(m.cfg.DialTimeoutMS) * time.Millisecond
	if send == 0 {
		send = DefaultSendTimeout
	}

	m.tr = &Transport{
		socket:      m.cfg.Socket,
		selfAID:     th.AID(),
		sendTimeout: send,
		inbound:     th.Inbound(),
		badDAck:     th.NoteBadDAck,
		addrOf:      th.P2PAddr,
	}
	// Connecting is not required to start. A peer process that is not up
	// yet, or has crashed, must not stop the daemon from running: the hub
	// carries everything this transport cannot, which is the property that
	// makes it safe to treat as an optimisation.
	runCtx, cancel := context.WithCancel(ctx)
	m.stop = cancel
	go m.tr.run(runCtx)

	th.RegisterTransport(m.tr)
	return nil
}

func (m *Module) Stop(context.Context) error {
	if m.stop != nil {
		m.stop()
	}
	if m.tr != nil {
		return m.tr.close()
	}
	return nil
}

// LocalPaths implements module.LocalPaths: the socket of the peer process,
// which a sandbox must never be able to open (K-5).
func (m *Module) LocalPaths() []string {
	if m.cfg.Socket == "" {
		return nil
	}
	return []string{m.cfg.Socket}
}

var _ module.LocalPaths = (*Module)(nil)
