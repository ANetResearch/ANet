//go:build !no_x402

package x402

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

func init() { module.Register("x402", New) }

// Config is the module's block in the daemon config.
//
//	"modules": {"x402": {}}
//
// Empty is the ordinary case: a node pays and charges through its hub with
// nothing to configure. The module opens no listener of its own; work is
// paid for inside the task (a2a-x402), on the same end-to-end encrypted
// interaction as the work itself.
type Config struct {
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
	cfg  Config
	host module.Host
	seam module.PaymentSeam

	// The identity of the hub this node settles on, cached for the hub URL
	// it was learned at (cachedURL). A node that moves to another hub signs
	// to that hub from its next payment on: a cache kept for the process
	// would go on signing redemptions, and checking receipts, against the
	// hub it left.
	mu        sync.Mutex
	cachedURL string
	cachedHub string
	cachedKEL []identity.SignedEvent

	// Which peer ledgers our hub will clear against, cached: asked once
	// per interval rather than on every 402, and again when the hub
	// changes (clearHub is the hub URL they were asked of).
	clearMu   sync.Mutex
	clearAt   time.Time
	clearHub  string
	clearNets []string

	// lastSignTS is the time of the last signed account read (signedGet),
	// kept strictly increasing.
	lastSignTS atomic.Uint64
}

// New builds the module. Returning (nil, nil) means compiled in and not
// configured — but payment is not like the others: a node with a hub can
// pay for things without configuring anything, so an absent block still
// yields a module. Only an unparseable one is an error.
func New(raw []byte) (module.Module, error) {
	m := &Module{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m.cfg); err != nil {
			return nil, fmt.Errorf("x402: config: %w", err)
		}
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

// Serve starts the witness loop, if it is configured. The module serves
// nothing to the network: there is no public door of its own.
func (m *Module) Serve(ctx context.Context) error {
	if m.cfg.WitnessHub {
		go m.witnessLoop(ctx)
	}
	return nil
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

// Settle presents a payment to the hub's facilitator with the terms it is
// checked against.
//
// Three outcomes, kept apart: settled; refused, with the facilitator's
// reason and its a2a-x402 code; and not known yet — a transport error is
// returned as an error, and settlement_pending as Pending — which the
// caller resolves by presenting the same payload again.
func (m *Module) Settle(ctx context.Context, raw []byte, req payment.PaymentRequirements) (module.Settlement, error) {
	st, err := m.settle(ctx, raw, req)
	if err != nil {
		return module.Settlement{}, err
	}
	if st == nil {
		return module.Settlement{}, fmt.Errorf("x402: no answer from the facilitator")
	}
	out := module.Settlement{
		Transaction: st.Transaction, Amount: st.Amount, Network: st.Network, Response: st,
	}
	if !st.Success {
		out.Failed = st.ErrorReason
		if out.Failed == "" {
			out.Failed = payment.ReasonSettlementFailed
		}
		code, final := ErrorCode(out.Failed)
		out.Code, out.Pending = code, !final
		return out, nil
	}
	if v, ok := st.Extensions[payment.ExtReplayed].(bool); ok && v {
		out.Replayed = true
	}
	// The hub signed a statement that it moved the credit. Carry it: it
	// is the only part of this the payer can check for itself.
	if enc, ok := st.Extensions[payment.ExtReceipt].(string); ok {
		out.Receipt = enc
	}
	return out, nil
}

// AID is this node's identity, for the objects that name a payer.
func (m *Module) AID() string {
	if m.host == nil {
		return ""
	}
	return m.host.AID()
}

// hubAID is the AID of the hub this node settles on.
func (m *Module) hubAID() string {
	aid, _ := m.hubIdentity()
	return aid
}

// hubIdentity is the AID and verified key history of the hub this node
// settles on now: the hub at the seam's current HubURL. It is learned once
// per hub URL; a change of hub (hub-register to another one) is noticed at
// the next call, which learns the new hub's identity instead of answering
// with the old one's. Nothing is cached when the identity cannot be had,
// and nothing is answered when the hub changed while it was being fetched:
// the identity learned might be either hub's.
func (m *Module) hubIdentity() (string, []identity.SignedEvent) {
	if m.seam == nil {
		return "", nil
	}
	url := hubKey(m.seam.HubURL())
	if url == "" {
		return "", nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cachedHub != "" && m.cachedURL == url {
		return m.cachedHub, m.cachedKEL
	}
	aid, kel, ok := m.seam.HubIdentity()
	if !ok || aid == "" || hubKey(m.seam.HubURL()) != url {
		return "", nil
	}
	m.cachedURL, m.cachedHub, m.cachedKEL = url, aid, kel
	return aid, kel
}

// hubKey is a hub URL as the caches compare it.
func hubKey(u string) string { return strings.TrimRight(strings.TrimSpace(u), "/") }

// hubKEL is that hub's verified key history — what makes its settlement
// receipts checkable rather than merely received.
func (m *Module) hubKEL() ([]identity.SignedEvent, error) {
	aid, kel := m.hubIdentity()
	if aid == "" {
		return nil, fmt.Errorf("x402: no hub, so no key history to check settlements against")
	}
	if len(kel) == 0 {
		return nil, fmt.Errorf("x402: the hub published no key history this node could verify")
	}
	return kel, nil
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
