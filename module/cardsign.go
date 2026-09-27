package module

// ProxyCardSigner is implemented by a Host that signs the proxy cards of
// the local A2A interface (A2A-DESIGN §11.3). Optional, and type-asserted
// on the Host like TransportHost: a host without it gets unsigned cards,
// which A2A permits and a client can see.
//
// A proxy card is this node's statement — "this agent can be reached here,
// with this token" — so it is signed with this node's key, and that is a
// signing grant, which is why it is named rather than folded into
// something broader. It is deliberately not a signer of arbitrary bytes
// (PaymentSeam.Sign, HubSeam.Sign): it signs an A2A AgentCard only, as
// ANetCore a2acard does (kid did:anet:<AID>#<seq>, no jku), and refuses a
// card that could be taken for this node's own network card — one carrying
// the anet-card extension or the relay binding, or an interface that is
// not on this machine's loopback. What a module holding it can produce is
// therefore a card about a local interface and nothing a hub would admit
// as this node's identity.
type ProxyCardSigner interface {
	// SignProxyCard signs card (a JSON AgentCard) and returns the signed
	// card as it is to be served.
	SignProxyCard(card []byte) ([]byte, error)
}
