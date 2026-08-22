// Package module is the daemon's optional-subsystem seam.
//
// The org lesson, made structural. In v1 the organisation feature grew into
// thirty-five daemon files and could not be pulled back out; the open-source
// release of 1.3.0-v3 needed an axe and 683,852 deleted lines. What was
// missing was never discipline — it was a seam. Code with nowhere to attach
// attaches everywhere.
//
// So a subsystem that is not the kernel attaches here, and only here:
//
//	provider registry ─┐
//	identity / ledger ─┼─ kernel, never optional
//	hub client ────────┘
//	                    ├─ module ─ p2p
//	                    ├─ module ─ blackboard (shared-brain)
//	                    ├─ module ─ store (distributed storage)
//	                    ├─ module ─ org
//	                    └─ module ─ anetlink (device capabilities)
//
// Two rules give the seam teeth:
//
//   - A module receives a Host, not the *Daemon. It can register providers,
//     read identity, append evidence, and nothing else. A module that needs
//     more is telling you it belongs in the kernel or does not belong at all
//     — and the compiler is what says so, at the moment the reach is written
//     rather than two years later.
//   - Registration happens under a build tag, so "pluggable" is a property
//     the build proves rather than a claim in a design document. A
//     distribution that ships alongside a hub and needs no peer-to-peer
//     transport is `-tags no_p2p`, not a fork.
package module

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/provider"
)

// Host is everything a module may use of the daemon.
//
// Deliberately small, and deliberately an interface rather than a struct
// pointer: it is the list of things a subsystem is allowed to want. Growing
// it is a decision someone has to make on purpose, in this file, which is
// exactly the moment that went unmarked in v1.
type Host interface {
	// AID is this node's identity anchor.
	AID() string
	// Providers is the C1 registry — the only way a module offers
	// capabilities to the network.
	Providers() *provider.Registry
	// RecordEvidence appends to this node's own chain. Modules produce
	// evidence like everything else; they do not get a private ledger.
	RecordEvidence(eventType string, payload any) error

	// ResolveKEL returns a peer's key history, if this node has verified
	// one. Added for the shared blackboard, which must be able to answer
	// "whose key signed this contribution" before merging it.
	//
	// Widening this interface is meant to be a deliberate act — it is the
	// list of things a subsystem is allowed to want, and the moment it
	// grows is the moment that went unmarked in v1. This one earns its
	// place: verifying authorship is not something a module can do with an
	// AID alone, and the alternative was a board that accepts anything.
	//
	// It answers only for peers this node has actually talked to. The hub
	// holds KELs but does not publish them, so a node vouches for what it
	// verified itself and for nothing else.
	ResolveKEL(aid string) ([]identity.SignedEvent, bool)

	// PaymentSeam hands over the ability to act as this node in a
	// payment. Absent when there is no hub, which is also when there is
	// no ledger and nothing can be charged.
	//
	// One method, and it grants more than the others do: what comes back
	// can sign as this node. That is not a detail to leave implicit, so
	// it is named — PaymentSeam, not Identity, not Keys — and it is worth
	// saying why the smaller grant does not work. A payment module could
	// be given the hub's identity read-only and would then be able to
	// verify what it is told and unable to authorise anything, because an
	// authorization the payer did not sign is not an authorization. The
	// signing is the payment.
	//
	// So the trade is deliberate: signing leaves the kernel, and in
	// exchange 800 lines of facilitator client, voucher door and
	// redemption path leave with it — including a PUBLIC listener, which
	// is a security posture nothing else in the kernel has. A build with
	// -tags no_x402 has none of it, which it could not claim while the
	// code sat in the kernel being linked in regardless.
	PaymentSeam() (PaymentSeam, bool)
}

// PaymentSeam is exactly what a payment subsystem needs of the node.
//
// Narrow on purpose, and named after what it is for rather than what it
// contains: a reader asking "what can the payment module do to my key"
// should find the answer in one place.
type PaymentSeam interface {
	// Sign signs one object's preimage as this node, returning the
	// signature and the key-state sequence it was made under.
	Sign(preimage []byte) (sig []byte, keyStateSeq uint64)
	// HubIdentity is the hub this node settles on: its AID and its
	// verified key history. The AID goes into every authorization's
	// network field, so a payment signed for one hub cannot be replayed
	// at another; the key history is what makes the hub's settlement
	// receipts checkable rather than merely received.
	HubIdentity() (aid string, kel []identity.SignedEvent, ok bool)
	// HubURL is where to reach that hub.
	HubURL() string
	// ReadEvidence reads this node's own chain back, newest last, for one
	// event type.
	//
	// Here rather than on Host because it is the read half of a write
	// Host already grants, and because the payment module has the one use
	// that needs it: a voucher is spent once, the guard must survive a
	// restart, and the chain is where the redemption was recorded. A
	// control nobody can consult is not a control — the alternative was a
	// second persistence layer holding the same facts, which is how two
	// records of one event start disagreeing.
	ReadEvidence(eventType string, limit int) []map[string]any
}

// Module is an optional daemon subsystem.
type Module interface {
	// Name is the module's stable name; it matches its build tag, so
	// `no_<name>` compiles it out.
	Name() string
	// Start brings the module up. An error is fatal to the daemon: a module
	// that was compiled in and configured but cannot run is an operator
	// error, and starting without it would be silent capability loss.
	Start(ctx context.Context, h Host) error
	// Stop shuts it down.
	Stop(ctx context.Context) error
}

// Confidential is implemented by a module holding values that must never
// appear in anything this node publishes publicly — INV-2.
//
// Optional, and one-directional on purpose. The org module knows its org
// id is confidential; the daemon knows only that some string must not
// leave. Asking the daemon to understand what an org is, so that it could
// decide for itself, is how the organisation feature got into thirty-five
// daemon files last time.
type Confidential interface {
	// ForbiddenTokens returns strings that must not appear in a public
	// publication. Called on every publish, so it must be cheap.
	ForbiddenTokens() []string
}

// Payer is implemented by a module that can charge for work and pay for
// it — the x402 subsystem.
//
// Optional, and type-asserted at build time exactly like Confidential.
// The kernel therefore never imports the module (K207): it knows only
// that something may be able to price and settle, and answers honestly
// when nothing can.
//
// A build without it does not silently do paid work for free. A priced
// capability is refused with a message saying this build cannot take
// payment, because "I cannot charge you, so I will not do it" is true and
// "here, have it" is a decision nobody made.
type Payer interface {
	// Price reports what a capability costs here and whether it costs
	// anything. Free is (0, false), never (0, true).
	Price(capID string) (uint64, bool)
	// Quote builds the PAYMENT_REQUIRED body for a priced capability.
	Quote(capID string, price uint64) *payment.PaymentRequired
	// Authorize signs a payment for one interaction and returns the
	// marshalled payload, ready to ride with a delegation. The kernel
	// drives the delegation; this signs the money.
	Authorize(opt payment.PaymentOption, interactionID string) ([]byte, error)
	// Settle presents a payment to the facilitator and reports what
	// happened, including the hub's signed receipt when it sent one.
	Settle(ctx context.Context, raw []byte) (Settlement, error)
	// VerifyReceipt checks a hub settlement receipt, pinning the signer to
	// this node's own hub and the payer to expectPayer. Reports what the
	// receipt says either way: "the provider told us it was paid" and "the
	// hub signed that it moved the credit" are different facts, and a
	// caller that cannot tell them apart will record the stronger.
	VerifyReceipt(receiptB64, expectPayer string) (ReceiptFacts, bool)
	// Balance reads this node's standing off the custodian.
	Balance(ctx context.Context) (map[string]any, error)
	// Redeem gives credit back to the hub against an external reference.
	Redeem(ctx context.Context, amount uint64, reference string) (map[string]any, error)
	// RedeemURL is this node's public voucher face, or empty. It goes in
	// the signed card so a gateway can tell buyers where to collect.
	RedeemURL() string
	// Serve brings up whatever public face the module needs. Called once,
	// after Start, by the kernel that owns the process lifetime.
	Serve(ctx context.Context) error
}

// ReceiptFacts is what a settlement receipt says, once checked.
type ReceiptFacts struct {
	Payee  string
	AuthID string
	Amount uint64
}

// Settlement is a completed payment as the kernel needs to see it.
//
// Deliberately not the x402 response type: the kernel carries these
// fields into a result and onto a chain and has no business with the
// rest. Receipt stays base64 rather than parsed, because the kernel is
// passing it through to whoever can check it and checking is not its job.
type Settlement struct {
	Transaction string
	Amount      string
	Network     string
	Receipt     string // base64 CoreDet-CBOR, empty if the hub signed nothing
	Failed      string // non-empty when the payment did not settle, and why
}

// Factory builds a module from its configuration block. Returning (nil, nil)
// means "compiled in, not configured" — the ordinary case for a module the
// operator has not asked for.
type Factory func(raw []byte) (Module, error)

type registration struct {
	name    string
	factory Factory
}

var (
	regMu    sync.Mutex
	registry []registration
)

// Register adds a module factory. Called from an init() in a file carrying
// the module's build tag, which is what makes the tag the on/off switch.
func Register(name string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	for _, r := range registry {
		if r.name == name {
			panic("module: duplicate registration " + name)
		}
	}
	registry = append(registry, registration{name, f})
}

// Compiled lists the module names in this build, sorted. The daemon logs it
// at startup so an operator can see what their binary actually contains
// rather than what the documentation says it might.
func Compiled() []string {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]string, 0, len(registry))
	for _, r := range registry {
		out = append(out, r.name)
	}
	sort.Strings(out)
	return out
}

// BuildOne instantiates a single registered module from its config
// block, for callers that want one rather than the set.
//
// Exists because a module's factory is the only thing standing between an
// operator's typo and a daemon that starts without the capabilities they
// asked for, and testing that through Build meant standing up every other
// module too. Returns (nil, nil) for "compiled in, not configured", the
// same as Build does.
func BuildOne(name string, cfg []byte) (Module, error) {
	regMu.Lock()
	defer regMu.Unlock()
	for _, r := range registry {
		if r.name == name {
			return r.factory(cfg)
		}
	}
	return nil, fmt.Errorf("module %q is not compiled into this build", name)
}

// Build instantiates every compiled-in module that is configured.
//
// A configured module that is not compiled in is an error, not a no-op. The
// alternative is a daemon that reads `"p2p": {...}` from a config, silently
// ignores it, and leaves an operator wondering why nothing is peering.
func Build(cfg map[string][]byte) ([]Module, error) {
	regMu.Lock()
	regs := append([]registration(nil), registry...)
	regMu.Unlock()

	known := make(map[string]bool, len(regs))
	var out []Module
	for _, r := range regs {
		known[r.name] = true
		m, err := r.factory(cfg[r.name])
		if err != nil {
			return nil, fmt.Errorf("module %q: %w", r.name, err)
		}
		if m != nil {
			out = append(out, m)
		}
	}
	for name := range cfg {
		if !known[name] {
			return nil, fmt.Errorf(
				"module %q is configured but not compiled into this build (built with no_%s?)",
				name, name)
		}
	}
	return out, nil
}
