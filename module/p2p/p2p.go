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
	"time"

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
		return &Module{cfg: cfg}, nil
	})
}

// Config points the module at a running peer process.
type Config struct {
	// Socket is where the peer process listens.
	Socket string `json:"socket"`
	// DialTimeoutMS bounds one delivery attempt. Kept short: this transport
	// is the optimisation, and the hub is waiting behind it. A slow peer
	// must not make every delegation slow.
	DialTimeoutMS int `json:"dial_timeout_ms"`
	// ReachTimeoutMS bounds the reachability question, which is asked
	// before every send and must therefore be cheap.
	ReachTimeoutMS int `json:"reach_timeout_ms"`
}

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
	dial := time.Duration(m.cfg.DialTimeoutMS) * time.Millisecond
	if dial == 0 {
		dial = 3 * time.Second
	}
	reach := time.Duration(m.cfg.ReachTimeoutMS) * time.Millisecond
	if reach == 0 {
		reach = 300 * time.Millisecond
	}

	m.tr = &Transport{
		socket:       m.cfg.Socket,
		selfAID:      th.AID(),
		dialTimeout:  dial,
		reachTimeout: reach,
		inbound:      th.Inbound(),
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
