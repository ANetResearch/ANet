package module

import "github.com/ANetResearch/ANet/internal/x402a2a"

// URIs a module may contribute to this node's A2A network card
// (A2A-DESIGN §10.1, §10.4). They are part of signed bytes and of hub
// indexes, so they are fixed strings rather than configuration.
//
// The anet-card extension and the relay binding are the kernel's own and
// live in ANetCore a2acard (ExtCardURI, BindingRelayURI); a contribution
// that names either is dropped by the kernel.
const (
	// ExtX402URI is the a2a-x402 v0.2 extension (A2A-DESIGN §8.1), as
	// internal/x402a2a defines it for the payment flow.
	ExtX402URI = x402a2a.ExtensionURI
	// ExtEvidenceURI says that tasks this node completes carry
	// provider-signed receipts and anet.* evidence metadata.
	ExtEvidenceURI = "https://agentnetwork.org.cn/a2a/ext/anet-evidence/v1"
	// BindingP2PURI is the protocolBinding of a direct peer-to-peer
	// interface. Like the relay binding it routes by tenant, which is the
	// node's AID, and carries the same sealed envelopes.
	BindingP2PURI = "https://agentnetwork.org.cn/a2a/bindings/anet-p2p/v1"
	// ChatSkillID is the skill a node whose inbound policy is open lists
	// for natural-language tasks (an internal note item): not a capability, and never
	// priced. A capability of that id, where one exists, is not it.
	ChatSkillID = "chat"
	// ExtPriceURI is the per-skill price list of an extended card: params
	// {network, asset, prices: [{skillId, amount}]}, amounts as decimal
	// strings of 1..2^63-1, every skillId one of the card's skills, and a
	// skill the list does not name free. It appears only on the extended
	// cards this node signs and answers anet.card.get/1 with, never on the
	// public card: the kernel drops it there, and a hub refuses a public card
	// that carries it.
	ExtPriceURI = "https://agentnetwork.org.cn/a2a/ext/anet-price/v1"
	// ExtPreviewURI is the preview extension (CUX-P30): an agent that
	// declares it sends previews of its reply while it is being written; a
	// client that activates it receives them as updates of the anet.reply
	// artifact. It has no params and is never required.
	ExtPreviewURI = "https://agentnetwork.org.cn/a2a/ext/anet-preview/v1"
)

// CardVariant names one of the two extended cards a node signs at a time.
// The public card is neither; CardContext.Variant is empty for it.
type CardVariant string

const (
	// VariantGeneral is the extended card every caller gets: the public card
	// and the prices of its skills.
	VariantGeneral CardVariant = "general"
	// VariantList is the extended card of the callers in allow ∪ trust: the
	// public card, the capabilities served to those lists only (allow_only),
	// and the prices of all of them.
	VariantList CardVariant = "list"
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
	// Skills are the ids of the skills the card lists, sorted: the public
	// capabilities this node serves and, on a node whose inbound policy is
	// open, ChatSkillID (natural-language tasks, never priced); on the list
	// variant of the extended card also the capabilities served only to the
	// allow and trust lists. Never empty: a node with no skill publishes no
	// card.
	Skills []string
	// Extended is set when the card is an extended card, signed by this node
	// and given only to a caller that asks for it end to end (anet.card.get/1),
	// and Variant says which. A payment module states prices only here
	// (ExtPriceURI); on the public card a price contribution is dropped.
	Extended bool
	Variant  CardVariant
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
// kept once. A price list (ExtPriceURI, or the public price list of 0.2) is
// dropped from the public card whatever the module meant: the kernel
// checks it only on an extended card, against the skills that card lists.
//
// CardExtensions is asked once per card: for the public card, then with
// Extended set for each variant of the extended card, each time with that
// card's skills.
type CardContributor interface {
	CardExtensions(c CardContext) []map[string]any
	CardInterfaces(c CardContext) []map[string]any
}
