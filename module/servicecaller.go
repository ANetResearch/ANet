package module

import (
	"context"
	"encoding/json"
	"errors"
)

// ServiceCaller is implemented by a Host that lets a module call a
// capability of this node's service module on behalf of a peer: the call
// goes to the same backend with the same token as a call from the network,
// with X-ANet-Caller set to callerAID and X-ANet-Via set to via (for
// example "agent"), so the service applies the caller's own rights and rate
// limits. Only capabilities that the service configuration marks
// "agent_tool": true can be called this way (design v0.3.9 §B); any other
// answers ErrNotAgentTool before anything is sent. Optional, and
// type-asserted on the Host like InboundTaskHost.
type ServiceCaller interface {
	// AgentTools lists the capabilities that CallServiceAs accepts.
	AgentTools() []AgentTool
	// CallServiceAs calls capability with args as callerAID. The result is
	// the service's JSON reply; a refusal of the service is returned as a
	// *ServiceError with its code.
	CallServiceAs(ctx context.Context, callerAID, capability string, args json.RawMessage, via string) (json.RawMessage, error)
}

// AgentTool is one capability that an agent may call through ServiceCaller.
type AgentTool struct {
	// Capability is the capability id, for example "board.post".
	Capability string
	// Description is the capability's description from the service config.
	Description string
	// InputSchema is the JSON Schema of the arguments, or nil when the
	// configuration declares none.
	InputSchema json.RawMessage
}

// ServiceError is a refusal answered by the service: its error code and,
// when the service gave one, its message.
type ServiceError struct {
	Code    string
	Message string
}

func (e *ServiceError) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

// ErrNotAgentTool is CallServiceAs's answer for a capability that is not
// marked agent_tool, or that this node's service module does not declare.
var ErrNotAgentTool = errors.New("not_agent_tool")
