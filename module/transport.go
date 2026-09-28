package module

import "context"

// Transport is how a message reaches another node.
//
// The daemon has always had exactly one: the hub relay. That is the right
// default and it is not going away — a hub is reachable when nothing else
// is, it works behind NAT, and it holds a mailbox for a peer that is
// offline. What it is not is the only way two nodes on the same network
// should have to talk to each other.
//
// So delivery is a list of transports rather than a hardcoded call. The
// contract is written in ANet's own terms — a recipient AID and the bytes of
// one sealed envelope (A2A-DESIGN §3.3, §3.5) — and deliberately not in any
// transport's terms. A peer-to-peer module speaks its own protocol
// internally and none of it appears here; that is what keeps the daemon from
// learning about peer ids, multiaddrs and pubsub topics the way it once
// learned about organisations.
//
// A transport sees no sender, no message type and no interaction id. They
// are inside the envelope, encrypted to the recipient, and a transport has
// no reason to know them: it routes by recipient AID alone.
type Transport interface {
	// Name identifies the transport in logs and configuration.
	Name() string

	// Reachable reports whether this transport can deliver to an AID right
	// now. A transport that cannot answer cheaply should answer false: the
	// caller falls through to the next one, and a slow probe would make
	// every delegation pay for an optimisation that may not apply.
	Reachable(ctx context.Context, toAID string) bool

	// Send delivers one envelope. Returning an error means the caller moves
	// on to the next transport with the same envelope bytes, so a Send that
	// partially succeeded must report failure. A duplicate delivery is
	// harmless — the receiver drops a second copy of the same envelope by
	// its (sender, message id) replay record — whereas a lost one is not.
	Send(ctx context.Context, toAID string, envelope []byte) error
}

// Inbound receives what a transport delivers to this node. The daemon
// implements it; transports call it.
//
// Deliberately the same shape as Send: an envelope that arrived over
// peer-to-peer and one that arrived from the hub mailbox are the same
// envelope, opened and verified the same way. Anything that needs to know
// which path a message took is asking for a distinction the evidence model
// already carries.
type Inbound interface {
	// Receive processes one envelope. nil means the transport should
	// acknowledge the delivery to its sender: the envelope was accepted, or
	// it was refused for a reason that will not change on retry (a bad
	// signature, an expired message). A non-nil error is a temporary
	// refusal (a storage error, a rate limit, a message that arrived before
	// the task it belongs to, or an envelope this node cannot open, which
	// over a direct path may be another node's): the transport must not
	// acknowledge, so the sender retries or falls back to the hub
	// (A2A-DESIGN §3.6 failure classes, §3.10).
	Receive(ctx context.Context, envelope []byte) error
}

// TransportHost is what a transport module may use of the daemon: the
// narrow Host, plus a way to register a delivery path and hand inbound
// traffic back.
type TransportHost interface {
	Host
	// Inbound is where a transport delivers what it receives.
	Inbound() Inbound
	// RegisterTransport adds this module's delivery path to the daemon's
	// list.
	//
	// Declared here rather than reached for at runtime. The p2p module
	// used to get at it with an unchecked type assertion on the host —
	// which panics against a host that does not have it, and, worse, walks
	// straight past the interface whose entire job is to be the list of
	// things a module is allowed to want. A seam a module can step around
	// is not a seam; the v1 organisation feature did not need a type
	// assertion to spread, only the absence of something saying no.
	RegisterTransport(Transport)
}
