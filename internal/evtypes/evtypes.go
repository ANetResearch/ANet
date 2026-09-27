// Package evtypes is the one list of evidence event types a node writes to
// its own chain (A2A-DESIGN §14).
//
// Every event the daemon or a module appends — through the kernel ledger's
// Append or a module's Host.RecordEvidence — is named here, with how a
// person is shown it and where the fact it records came from. `anet audit`
// builds its table of known events from this list, so an event written
// without being registered would be listed as "type not known here" by the
// node's own tool; a test in this package reads the source tree and fails
// on any evidence write whose event type is not in the list.
//
// The constants beside the write sites (EvXxx in internal/daemon and the
// modules) keep their own declarations; the test checks that their values
// are registered. New event types should use the constants below.
//
// Events of a hub's issuance chain (anet.credit.issued, anet.credit.retired)
// are not written to a node's chain and are not listed here.
package evtypes

// Event types of a node's own evidence chain.
const (
	DelegationSent           = "anet.delegation.sent"
	DelegationReceived       = "anet.delegation.received"
	DelegationRefusedSummary = "anet.delegation.refused_summary"
	ResultAccepted           = "anet.result.accepted"
	InteractionReceipt       = "anet.interaction.receipt"
	InteractionPruned        = "anet.interaction.pruned"
	CapabilityEffect         = "anet.capability.effect"
	PaymentSettled           = "anet.payment.settled"
	PaymentAuthorized        = "anet.payment.authorized"
	PaymentQuoted            = "anet.payment.quoted"
	CreditRedeemed           = "anet.credit.redeemed"
	VoucherRedeemed          = "anet.voucher.redeemed"
	VoucherRefused           = "anet.voucher.refused"
	IssuanceHeadSeen         = "anet.issuance.head_seen"
	ShellCommand             = "anet.shell.command"
	ShellRefused             = "anet.shell.refused"
	PolicyChanged            = "anet.policy.changed"
	AutoReplyInvoked         = "anet.autoreply.invoked"
	BackendForwarded         = "anet.backend.forwarded"
	DeliveryExpired          = "anet.delivery.expired"
	EvidenceGap              = "anet.evidence.gap"
	MessageSent              = "anet.message.sent"
	MessageReceived          = "anet.message.received"
)

// Source says where the fact an event records came from.
type Source string

const (
	// SourceNode is this node's own action, signed by it.
	SourceNode Source = "node"
	// SourcePeer is content from a peer; the verdict on it is this node's.
	SourcePeer Source = "peer"
	// SourceHub is a hub's statement.
	SourceHub Source = "hub"
)

// Event is one registered event type.
type Event struct {
	Type   string
	Label  string
	Source Source
	// AggregatedLabel, when set, is the label of the aggregated form of the
	// event: a record whose payload has "aggregated": true stands for a
	// window of events from parties this node did not name (public and
	// public_cap), counted rather than written one by one.
	AggregatedLabel string
}

var registry = []Event{
	{Type: DelegationSent, Label: "delegation sent", Source: SourceNode},
	{Type: DelegationReceived, Label: "delegation received", Source: SourcePeer,
		AggregatedLabel: "delegations received (aggregated)"},
	{Type: DelegationRefusedSummary, Label: "delegations refused (aggregated)", Source: SourceNode},
	{Type: ResultAccepted, Label: "result accepted", Source: SourcePeer},
	{Type: InteractionReceipt, Label: "receipt issued", Source: SourceNode},
	{Type: InteractionPruned, Label: "interactions pruned (retention)", Source: SourceNode},
	{Type: CapabilityEffect, Label: "capability executed", Source: SourceNode},
	{Type: PaymentSettled, Label: "payment settled", Source: SourceHub},
	{Type: PaymentAuthorized, Label: "payment authorized", Source: SourceNode},
	{Type: PaymentQuoted, Label: "payment quoted", Source: SourceNode},
	{Type: CreditRedeemed, Label: "credit redeemed", Source: SourceHub},
	{Type: VoucherRedeemed, Label: "voucher redeemed", Source: SourceHub},
	{Type: VoucherRefused, Label: "voucher refused", Source: SourceNode},
	{Type: IssuanceHeadSeen, Label: "hub issuance head seen", Source: SourceHub},
	{Type: ShellCommand, Label: "shell command run", Source: SourceNode},
	{Type: ShellRefused, Label: "shell command refused", Source: SourceNode},
	{Type: PolicyChanged, Label: "policy changed", Source: SourceNode},
	{Type: AutoReplyInvoked, Label: "auto-reply invoked", Source: SourceNode},
	{Type: BackendForwarded, Label: "forwarded to A2A backend", Source: SourceNode},
	{Type: DeliveryExpired, Label: "delivery expired", Source: SourceNode},
	{Type: EvidenceGap, Label: "evidence gap (a torn record was lost)", Source: SourceNode},
	{Type: MessageSent, Label: "message sent", Source: SourceNode,
		AggregatedLabel: "messages sent (aggregated)"},
	{Type: MessageReceived, Label: "message received", Source: SourcePeer,
		AggregatedLabel: "messages received (aggregated)"},
}

var byType = func() map[string]Event {
	m := make(map[string]Event, len(registry))
	for _, e := range registry {
		m[e.Type] = e
	}
	return m
}()

// All returns the registered event types, in registry order.
func All() []Event {
	return append([]Event(nil), registry...)
}

// Lookup returns the registered event type t.
func Lookup(t string) (Event, bool) {
	e, ok := byType[t]
	return e, ok
}

// Registered reports whether t is a registered event type.
func Registered(t string) bool {
	_, ok := byType[t]
	return ok
}
