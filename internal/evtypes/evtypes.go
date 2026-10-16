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
	IssuanceHeadSeen         = "anet.issuance.head_seen"
	ShellCommand             = "anet.shell.command"
	ShellRefused             = "anet.shell.refused"
	PolicyChanged            = "anet.policy.changed"
	AutoReplyInvoked         = "anet.autoreply.invoked"
	BackendForwarded         = "anet.backend.forwarded"
	BackendFailed            = "anet.backend.failed"
	DeliveryExpired          = "anet.delivery.expired"
	TaskNoResponse           = "anet.task.no_response"
	EvidenceGap              = "anet.evidence.gap"
	MessageSent              = "anet.message.sent"
	MessageReceived          = "anet.message.received"
	// HubAdmission is a change of the hub join state (LEC §2.5, AD-7).
	HubAdmission = "anet.hub.admission"
	// RetentionChanged is a change of content_retention made with `anet
	// retention set` (forward-secrecy §2.12).
	RetentionChanged = "anet.retention.changed"
	// LocalStateLost is a rollback of this node's data that it found: the
	// hub held a key set this identity signed and this node did not, and
	// the tasks in flight were ended (forward-secrecy §2.12, FS-B3).
	LocalStateLost = "anet.local_state.lost"
	// IdentityRotated is a key event of this node's identity the witness
	// accepted: the witness designation, a planned or emergency rotation, a
	// hub switch or a recover (KEL §2.10), signed by the new key.
	IdentityRotated = "anet.identity.rotated"
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
	{Type: IssuanceHeadSeen, Label: "hub issuance head seen", Source: SourceHub},
	{Type: ShellCommand, Label: "shell command run", Source: SourceNode},
	{Type: ShellRefused, Label: "shell command refused", Source: SourceNode},
	{Type: PolicyChanged, Label: "policy changed", Source: SourceNode},
	{Type: AutoReplyInvoked, Label: "auto-reply invoked", Source: SourceNode},
	{Type: BackendForwarded, Label: "forwarded to A2A backend", Source: SourceNode},
	{Type: BackendFailed, Label: "A2A backend did not answer (left in the inbox)", Source: SourceNode},
	{Type: DeliveryExpired, Label: "delivery expired", Source: SourceNode},
	{Type: TaskNoResponse, Label: "no answer from the peer (task failed)", Source: SourceNode},
	{Type: EvidenceGap, Label: "evidence gap (a torn record was lost)", Source: SourceNode},
	{Type: MessageSent, Label: "message sent", Source: SourceNode,
		AggregatedLabel: "messages sent (aggregated)"},
	{Type: MessageReceived, Label: "message received", Source: SourcePeer,
		AggregatedLabel: "messages received (aggregated)"},
	{Type: HubAdmission, Label: "hub join state changed", Source: SourceHub},
	{Type: RetentionChanged, Label: "content retention changed", Source: SourceNode},
	{Type: LocalStateLost, Label: "local data went back (tasks in flight ended)", Source: SourceNode},
	{Type: IdentityRotated, Label: "identity key rotated", Source: SourceNode},
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

// Payload is the shape of an event whose payload is fixed (LEC §2.5): the
// members every record of the type has, the members a record has only when
// they apply, and for a member that is an object, its own shape. A record
// has no other member: the tests read the records the daemon writes against
// it, so a payload that grew a path, a request body, a prompt or a key would
// not pass.
type Payload struct {
	Required []string
	Optional []string
	Nested   map[string]Payload
	// Values are the values a member whose values are a closed set may take.
	Values map[string][]string
}

// payloads are the fixed payloads, by event type.
var payloads = map[string]Payload{
	AutoReplyInvoked: {
		Required: []string{"agent", "interaction_id", "peer_aid", "trusted", "sandboxed", "duration_ms",
			"mode", "sandbox", "exit"},
		Optional: []string{"unavailable", "egress"},
		Values: map[string][]string{
			"mode":    {"direct", "sandbox", "openai"},
			"sandbox": {"bwrap", "seatbelt", ""},
			"exit":    {"ok", "error", "sandbox_unavailable", "budget", "resource"},
		},
		Nested: map[string]Payload{"egress": {
			Required: []string{"model_requests", "bytes_up", "bytes_down", "policy_refused", "connect"},
			Nested: map[string]Payload{"connect": {
				Required: []string{"allowed", "refused", "bytes_up", "bytes_down"},
			}},
		}},
	},
	HubAdmission: {Required: []string{"hub", "state"}, Optional: []string{"reason"}},
	// A retention sweep, one record per class it deleted tasks of (FS §2.12):
	// counts, the cutoff and the retention in days; never an id or content.
	InteractionPruned: {
		Required: []string{"trust", "before", "retention_days", "interactions", "messages", "attachments"},
		Values:   map[string][]string{"trust": {"outbound", "peer", "approved", "public", "public_cap"}},
	},
	RetentionChanged: {Required: []string{"from", "to", "deleted_interactions"}},
	// The hub's seq of the set this node's data was behind, this node's
	// last_seq before it (0 when not known), the tasks ended (FS §2.12).
	LocalStateLost: {Required: []string{"hub_seq", "local_seq", "failed_tasks"}},
	// The seq of the event, how it was made and the witness that accepted
	// it (KEL §2.10; core §2.14).
	IdentityRotated: {Required: []string{"seq", "mode", "witness"},
		Values: map[string][]string{"mode": {"witness", "scheduled", "emergency", "recover"}}},
}

// PayloadOf returns the fixed payload of event type t, if it has one.
func PayloadOf(t string) (Payload, bool) {
	p, ok := payloads[t]
	return p, ok
}

// Check reports the first way the members of a record's payload differ
// from the shape: a member it lacks, one the shape does not have, or a value
// outside a member's closed set. Nested members are checked when they are
// objects; other values are not.
func (p Payload) Check(m map[string]any) string {
	allowed := map[string]bool{}
	for _, k := range p.Required {
		allowed[k] = true
		if _, ok := m[k]; !ok {
			return "lacks " + k
		}
	}
	for _, k := range p.Optional {
		allowed[k] = true
	}
	for k, v := range m {
		if !allowed[k] {
			return "has " + k + ", which the payload does not"
		}
		if vals, ok := p.Values[k]; ok && !oneOf(v, vals) {
			return k + " has a value outside its set"
		}
		if sub, ok := p.Nested[k]; ok {
			obj, ok := v.(map[string]any)
			if !ok {
				return k + " is not an object"
			}
			if why := sub.Check(obj); why != "" {
				return k + ": " + why
			}
		}
	}
	return ""
}

// oneOf reports whether v is one of the strings vals.
func oneOf(v any, vals []string) bool {
	str, ok := v.(string)
	for _, x := range vals {
		if ok && str == x {
			return true
		}
	}
	return false
}
