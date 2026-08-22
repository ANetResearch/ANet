//go:build !no_x402

package x402

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

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
	return m, nil
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

// Serve brings up the public voucher face, if configured.
func (m *Module) Serve(ctx context.Context) error { return m.startRedeemFace(ctx) }

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
