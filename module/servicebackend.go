package module

import (
	"context"
	"encoding/json"
)

// ServiceBackend is implemented by the capability provider of the service
// module (module/service): the kernel's way to the service backends for the
// calls that do not come from the network (design v0.3.9 §A, §B). The kernel
// finds it by type assertion on the provider the service module registered;
// no other module's provider implements it.
//
// Two kinds of capability are reached this way, and each only through its
// own method:
//
//   - an internal capability ("internal": true in the service
//     configuration) is the kernel's alone. The provider does not list it
//     among its capabilities, so it is on no card, in no registration and
//     callable by no peer; CallInternal is the one way to it. The kernel
//     uses it to ask a local service whether a peer is admitted
//     (inbound.admit).
//   - an agent tool ("agent_tool": true) is an ordinary capability that an
//     agent running for a peer may also call as that peer
//     (ServiceCaller.CallServiceAs); CallAgentTool is the way to it.
//
// A capability is never both: the service configuration refuses it.
type ServiceBackend interface {
	// InternalCapability reports whether id is an internal capability of
	// the service module.
	InternalCapability(id string) bool
	// AgentTools lists the capabilities marked agent_tool, in the order of
	// the configuration.
	AgentTools() []AgentTool
	// CallAgentTool posts args to the backend of capability, an agent tool,
	// with X-ANet-Caller set to callerAID and X-ANet-Via to via: the same
	// backend, token and deadline as a call from the network. Any other
	// capability answers ErrNotAgentTool, and nothing is sent. Every failure
	// is a *ServiceError: the service's own code for a refusal it answered,
	// otherwise one of the reason codes busy, unavailable, timeout,
	// connection_lost or backend_failed.
	CallAgentTool(ctx context.Context, callerAID, capability string, args json.RawMessage, via string) (json.RawMessage, error)
	// CallInternal posts args to the backend of capability, an internal
	// capability, with X-ANet-Caller set to callerAID. Any other capability
	// is refused, and nothing is sent. Failures are as for CallAgentTool.
	CallInternal(ctx context.Context, callerAID, capability string, args json.RawMessage) (json.RawMessage, error)
}
