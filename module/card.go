package module

// URIs a module may contribute to this node's A2A network card
// (A2A-DESIGN §10.1, §10.4). They are part of signed bytes and of hub
// indexes, so they are fixed strings rather than configuration.
//
// The anet-card extension and the relay binding are the kernel's own and
// live in ANetCore a2acard (ExtCardURI, BindingRelayURI); a contribution
// that names either is dropped by the kernel.
const (
	// ExtX402URI is the a2a-x402 v0.2 extension (A2A-DESIGN §8.1).
	ExtX402URI = "https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2"
	// ExtPricingURI carries this node's signed price list:
	// params {network, prices: [{skillId, amount}]}, amounts as decimal
	// strings. Inside the card signature so a hub cannot quote a price the
	// provider never agreed to.
	ExtPricingURI = "https://agentnetwork.org.cn/a2a/ext/anet-pricing/v1"
	// ExtEvidenceURI says that tasks this node completes carry
	// provider-signed receipts and anet.* evidence metadata.
	ExtEvidenceURI = "https://agentnetwork.org.cn/a2a/ext/anet-evidence/v1"
	// BindingP2PURI is the protocolBinding of a direct peer-to-peer
	// interface. Like the relay binding it routes by tenant, which is the
	// node's AID, and carries the same sealed envelopes.
	BindingP2PURI = "https://agentnetwork.org.cn/a2a/bindings/anet-p2p/v1"
)

// CardContext is what the kernel tells a contributor about the network
// card it is building.
//
// The design writes the contributor methods without arguments. A payment
// module cannot price a card without knowing which skills the card
// publishes, and which capabilities are public is the kernel's
// configuration (inbound.public_capabilities), not the module's; passing
// it here keeps the module from reading kernel config and keeps the
// kernel from knowing what a price extension looks like.
type CardContext struct {
	// AID is this node's identity; interfaces route by it (tenant).
	AID string
	// HubURL is the hub the card is published to, without a trailing
	// slash.
	HubURL string
	// Skills are the capability ids the card publishes as skills, sorted.
	// Never empty: a node with no public skill publishes no card.
	Skills []string
}

// CardContributor is implemented by a module that adds to this node's A2A
// network card (A2A-DESIGN §10.4). Optional, and type-asserted like
// Confidential and Payer.
//
// Contributions are plain JSON objects so the kernel never imports an A2A
// library: an extension is {"uri", "description"?, "required"?,
// "params"?}; an interface is {"url", "protocolBinding",
// "protocolVersion", "tenant"?}. The kernel puts them into the card in its
// publish form (A2A specification §8.4.1; internal/netcard): a member
// holding its default value ("", false, empty params) is removed, so a
// contributor that writes "required": false gets the same signed card as
// one that leaves it out. Inside params every value must be a non-empty
// string, a bool, or a non-empty object or array of those: numbers are
// written as decimal strings, and verifiers disagree about empty values.
//
// The kernel drops, with a log line, an extension or interface it cannot
// publish: an unknown member, a kernel-owned URI or the relay binding, a
// direct (anet-p2p) interface whose tenant is not the node's AID, and an
// interface whose URL host is loopback, unspecified or a local socket
// (127.0.0.1 never enters a network card). A repeated extension URI is
// kept once.
type CardContributor interface {
	CardExtensions(c CardContext) []map[string]any
	CardInterfaces(c CardContext) []map[string]any
}
