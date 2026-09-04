//go:build !no_x402

package x402

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

func init() { module.Register("x402", New) }

// Config is the module's block in the daemon config.
//
//	"modules": {"x402": {
//	    "voucher_addr": "0.0.0.0:8402",
//	    "voucher_url":  "https://your-node.example/x402/redeem"
//	}}
//
// Both empty is the ordinary case: a node that pays for work but does not
// sell any needs no public face, and opening one it does not need would
// be a listener nobody asked for.
type Config struct {
	// VoucherAddr opens a PUBLIC listener where buyers who paid at a hub
	// redeem vouchers directly. Unlike the control API this is meant to
	// be reachable from outside, so it is off unless set.
	VoucherAddr string `json:"voucher_addr,omitempty"`
	// VoucherURL is the address the world reaches VoucherAddr at, which
	// is what goes in this node's signed card.
	//
	// Separate from VoucherAddr because a listen address is routinely
	// 0.0.0.0 or a container port, and signing one of those into a card
	// advertises somewhere nobody can get to. Only the operator knows
	// what the world sees.
	VoucherURL string `json:"voucher_url,omitempty"`
	// WitnessHub makes this node pin its hub's issuance head
	// periodically, recording it on its own evidence chain and offering
	// a signed attestation back to the hub.
	//
	// Off by default. A node may be a trimmed build that serves nothing
	// and only talks to its hub; making such a node do work for the
	// network is not something it asked for. A node that turns this on
	// gains the most direct benefit — independent evidence about the
	// ledger its own balance lives on.
	WitnessHub bool `json:"witness_hub,omitempty"`
	// WitnessEverySeconds is the interval. Default one hour: what an
	// auditor needs is a head from an hour ago, not from ninety seconds
	// ago, and pinning faster multiplies stored attestations for nothing.
	WitnessEverySeconds int `json:"witness_every_seconds,omitempty"`
}

// Module is the payment subsystem.
type Module struct {
	cfg   Config
	host  module.Host
	seam  module.PaymentSeam
	spent *spentVouchers

	mu        sync.Mutex
	cachedHub string
	cachedKEL []identity.SignedEvent

	// Which peer ledgers our hub will clear against, cached: asked once
	// per interval rather than on every 402.
	clearMu   sync.Mutex
	clearAt   time.Time
	clearNets []string
}

// New builds the module. Returning (nil, nil) means compiled in and not
// configured — but payment is not like the others: a node with a hub can
// pay for things without configuring anything, so an absent block still
// yields a module. Only an unparseable one is an error.
func New(raw []byte) (module.Module, error) {
	m := &Module{spent: newSpentVouchers()}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m.cfg); err != nil {
			return nil, fmt.Errorf("x402: config: %w", err)
		}
	}
	if (m.cfg.VoucherAddr == "") != (m.cfg.VoucherURL == "") {
		// Half-configured is the shape that fails silently later: a
		// listener nobody is told about, or a card pointing at a port
		// that was never opened. Both sell work that cannot be collected.
		return nil, fmt.Errorf(
			"x402: voucher_addr and voucher_url must be set together " +
				"(one is where to listen, the other is what the world sees)")
	}
	if err := checkVoucherURL(m.cfg.VoucherURL); err != nil {
		return nil, err
	}
	return m, nil
}

// checkVoucherURL refuses a voucher_url a buyer could not open.
//
// The value is signed into this node's card as the x402-redeem endpoint
// and republished by the hub as the address to redeem at. Nothing between
// here and the buyer looks at its shape: the hub checks that the endpoint
// exists and that its URI is non-empty, and forwards whatever it was
// given. A value with no path, or with no scheme at all, therefore
// travelled all the way to a buyer who had already paid before it turned
// out not to open. Found while reading the card a prodtest node
// published.
//
// Missing pieces are refused rather than filled in. Completing a
// half-written address would leave the operator believing the value in
// their config is the one being advertised, and the next person to read
// that config would have no way to tell the two apart. The cost is that
// an operator who was getting away with a sloppy value now has to fix it
// before the daemon starts, which is the same trade the half-configured
// check above makes.
func checkVoucherURL(raw string) error {
	if raw == "" {
		return nil // not selling through a gateway; see New
	}
	const want = "an absolute URL ending in " + redeemPath +
		", such as https://node.example:8402" + redeemPath
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("x402: voucher_url %q is not a URL (%v); it must be %s", raw, err, want)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("x402: voucher_url %q has no http:// or https:// scheme; "+
			"it must be %s", raw, want)
	case u.Host == "":
		return fmt.Errorf("x402: voucher_url %q names no host; it must be %s", raw, want)
	case !strings.HasSuffix(u.Path, redeemPath):
		return fmt.Errorf("x402: voucher_url %q does not end in %s, which is the only path "+
			"this module answers redemptions on, so a buyer sent there would get nothing; "+
			"it must be %s", raw, redeemPath, want)
	}
	// A listen address is not a destination. voucher_addr is routinely
	// 0.0.0.0 and copying it into voucher_url is the easiest mistake to
	// make here, since the two sit next to each other in the config.
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsUnspecified() {
		return fmt.Errorf("x402: voucher_url %q advertises %s, which is an address to listen "+
			"on rather than one a buyer can reach; it must name the host the world sees this "+
			"node at, as %s", raw, u.Hostname(), want)
	}
	return nil
}

func (m *Module) Name() string { return "x402" }

// Start wires the module to the node.
func (m *Module) Start(_ context.Context, h module.Host) error {
	m.host = h
	seam, ok := h.PaymentSeam()
	if !ok {
		// No hub means no ledger. The module stays loaded and answers
		// honestly rather than refusing to start: a node can be joined to
		// a hub later, and a daemon that would not boot because it had
		// not been told where to bank yet is a daemon nobody can set up.
		return nil
	}
	m.seam = seam
	return nil
}

func (m *Module) Stop(context.Context) error { return nil }

// Serve brings up the public voucher face and the witness loop, if
// either is configured.
func (m *Module) Serve(ctx context.Context) error {
	if m.cfg.WitnessHub {
		go m.witnessLoop(ctx)
	}
	return m.startRedeemFace(ctx)
}

// witnessLoop pins the hub's issuance head on a slow cadence.
func (m *Module) witnessLoop(ctx context.Context) {
	every := time.Duration(m.cfg.WitnessEverySeconds) * time.Second
	if every <= 0 {
		every = time.Hour
	}
	// One pass shortly after start, so a node restarted after a long
	// absence does not leave a gap before its first pin.
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if seq, id, err := m.WitnessHub(ctx); err != nil {
			log.Printf("anet: witnessing the hub: %v", err)
		} else if id != "" {
			log.Printf("anet: pinned hub issuance head seq=%d", seq)
		}
		t.Reset(every)
	}
}

// Price reports what a capability costs, asking the provider that serves
// it. The provider knows what its work is worth; whether a caller has
// paid is about the interaction, and that is this module's business.
func (m *Module) Price(capID string) (uint64, bool) {
	if m.host == nil || m.seam == nil {
		// Without a ledger nothing has a price here, and saying so is
		// better than quoting one that cannot be settled.
		return 0, false
	}
	p, ok := m.host.Providers().Resolve(capID)
	if !ok {
		return 0, false
	}
	priced, ok := p.(provider.Priced)
	if !ok {
		return 0, false
	}
	return priced.Price(capID)
}

// Quote builds the x402 402 body for a priced capability.
func (m *Module) Quote(capID string, price uint64) *payment.PaymentRequired {
	return m.paymentRequired(capID, price)
}

// Settle presents a payment to the hub's facilitator.
func (m *Module) Settle(ctx context.Context, raw []byte) (module.Settlement, error) {
	st, err := m.settle(ctx, raw)
	if err != nil {
		return module.Settlement{}, err
	}
	if st == nil {
		return module.Settlement{Failed: "no answer from the facilitator"}, nil
	}
	out := module.Settlement{
		Transaction: st.Transaction, Amount: st.Amount, Network: st.Network,
	}
	if !st.Success {
		out.Failed = st.ErrorReason
		if out.Failed == "" {
			out.Failed = "settlement refused"
		}
		return out, nil
	}
	// The hub signed a statement that it moved the credit. Carry it: it
	// is the only part of this the payer can check for itself.
	if enc, ok := st.Extensions[payment.ExtReceipt].(string); ok {
		out.Receipt = enc
	}
	return out, nil
}

// RedeemURL is where buyers collect, or empty when this node does not
// sell through a gateway.
func (m *Module) RedeemURL() string { return m.cfg.VoucherURL }

// AID is this node's identity, for the objects that name a payer.
func (m *Module) AID() string {
	if m.host == nil {
		return ""
	}
	return m.host.AID()
}

// hubAID is the AID of the hub this node settles on.
func (m *Module) hubAID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cachedHub != "" {
		return m.cachedHub
	}
	if m.seam == nil {
		return ""
	}
	aid, kel, ok := m.seam.HubIdentity()
	if !ok {
		return ""
	}
	m.cachedHub, m.cachedKEL = aid, kel
	return aid
}

// hubKEL is that hub's verified key history — what makes its settlement
// receipts checkable rather than merely received.
func (m *Module) hubKEL() ([]identity.SignedEvent, error) {
	if m.hubAID() == "" {
		return nil, fmt.Errorf("x402: no hub, so no key history to check settlements against")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.cachedKEL) == 0 {
		return nil, fmt.Errorf("x402: the hub published no key history this node could verify")
	}
	return m.cachedKEL, nil
}

// hubURL is where the facilitator lives.
func (m *Module) hubURL() string {
	if m.seam == nil {
		return ""
	}
	return m.seam.HubURL()
}

// record appends to this node's own chain. Modules produce evidence like
// everything else; they do not get a private ledger.
func (m *Module) record(eventType string, payload any) error {
	if m.host == nil {
		return nil
	}
	return m.host.RecordEvidence(eventType, payload)
}

var (
	_ module.Module = (*Module)(nil)
	_ module.Payer  = (*Module)(nil)
)
