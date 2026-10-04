package module

import (
	"context"
)

// DAckLen is the length of a delivery confirmation (dack): a 16-byte token
// and a 64-byte sealed note, made by the recipient's daemon and by nobody
// else. A transport carries one as opaque bytes. It equals seal.DAckLen.
const DAckLen = 80

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
//
// Only the recipient says a message arrived. A transport reports what it
// did and what came back; it never decides that a message was delivered.
type Transport interface {
	// Name identifies the transport in logs and configuration.
	Name() string

	// Reachable reports whether this transport can deliver to an AID right
	// now. A transport that cannot answer cheaply should answer false: the
	// caller falls through to the next one, and a slow probe would make
	// every delegation pay for an optimisation that may not apply.
	Reachable(ctx context.Context, toAID string) bool

	// Send hands one envelope over and reports three things.
	//
	// dack is the delivery confirmation the recipient's daemon gave back on
	// the same round trip, as the bytes that came back (DAckLen of them
	// when nothing on the way spoiled it), or nil when none did. Only the
	// recipient can make one, and the caller checks it against the keys of
	// the envelope before it believes anything: a transport without one
	// returns nil and never makes one up, and an answer without one —
	// "delivered", an empty error, a 200 — confirms nothing.
	//
	// handed is false only when the transport knows that no byte of the
	// envelope left this process: it was not connected, or the attempt was
	// cancelled before the first write. Once a byte may have gone it is
	// true, whatever comes back afterwards, an error included. What the far
	// side answers cannot unsend an envelope, so the caller cannot report a
	// handed message as one that never left.
	//
	// err says why the attempt did not end in an answer, for the log and
	// for the transport's health. It does not say whether the envelope
	// arrived; dack and handed do.
	Send(ctx context.Context, toAID string, envelope []byte) (dack []byte, handed bool, err error)
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
	// Receive processes one envelope.
	//
	// err == nil means the transport acknowledges the delivery to its
	// sender and carries dack back with it unchanged: the confirmation
	// this node made for the envelope, or nil when it makes none (a
	// one-shot envelope, such as a preview frame). A transport neither
	// reads, changes, checks nor keeps it.
	//
	// A non-nil error is a temporary refusal (a storage error, a rate
	// limit, an envelope this node cannot open, which over a direct path
	// may be another node's): the transport must not acknowledge, so the
	// sender retries or falls back to the hub (A2A-DESIGN §3.6 failure
	// classes, §3.10).
	Receive(ctx context.Context, envelope []byte) (dack []byte, err error)
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
	// NoteBadDAck counts one answer to a Send whose delivery confirmation
	// was malformed: not base64url, or not DAckLen bytes once decoded. The
	// transport then returns no dack for that answer. The count is the
	// node's dack-bad-frame counter, which says that something between
	// here and the recipient spoils confirmations.
	NoteBadDAck()
}
