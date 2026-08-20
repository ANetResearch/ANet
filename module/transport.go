package module

import "context"

// Transport is how a delegation reaches another node.
//
// The daemon has always had exactly one: the hub relay. That is the right
// default and it is not going away — a hub is reachable when nothing else
// is, it works behind NAT, and it holds a mailbox for a peer that is
// offline. What it is not is the only way two nodes on the same network
// should have to talk to each other.
//
// So delivery becomes a list of transports rather than a hardcoded call.
// The contract is written in ANet's own terms — an AID, a kind, an
// interaction id, bytes — and deliberately not in any transport's terms. A
// peer-to-peer module speaks libp2p internally and libp2p appears nowhere
// here; that is what keeps the daemon from learning about peer ids,
// multiaddrs and pubsub topics the way it once learned about organisations.
type Transport interface {
	// Name identifies the transport in logs and configuration.
	Name() string

	// Reachable reports whether this transport can deliver to an AID right
	// now. A transport that cannot answer cheaply should answer false: the
	// caller falls through to the next one, and a slow probe would make
	// every delegation pay for an optimisation that may not apply.
	Reachable(ctx context.Context, toAID string) bool

	// Send delivers one payload. Returning an error means the caller moves
	// on to the next transport, so a Send that partially succeeded must
	// report failure — a delegation delivered twice is worse than one
	// delivered late.
	Send(ctx context.Context, toAID, kind, interactionID string, payload []byte) error
}

// Inbound receives what a transport delivers to this node. The daemon
// implements it; transports call it.
//
// Deliberately the same shape as Send: a message that arrived over
// peer-to-peer and one that arrived from the hub mailbox are the same
// message, and the daemon must not be able to tell them apart. Anything
// that needs to know which path a delegation took is asking for a
// distinction the evidence model already carries.
type Inbound interface {
	Receive(ctx context.Context, fromAID, kind, interactionID string, payload []byte) error
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
