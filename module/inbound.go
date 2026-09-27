package module

import (
	"context"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// InboundTaskHost is implemented by a Host that lets a module answer text
// tasks delegated to this node: the provider-side A2A backends of the
// local A2A interface (A2A-DESIGN §11.6), where an agent that already
// speaks A2A is the one answering work that arrives over the network.
// Optional, and type-asserted on the Host like ProxyCardSigner; a host
// without it leaves every task in the inbox, where the operator answers.
//
// It is the inbound counterpart of TaskSeam and deliberately a separate
// grant. TaskSeam reaches only tasks this node started (§11.2), so a local
// A2A client can never read what other nodes sent here; this reaches only
// tasks sent here, and only once the kernel has decided they may be
// answered at all.
type InboundTaskHost interface {
	// InboundTasks delivers the text tasks delegated to this node that
	// wait for its answer: admitted by the inbound policy (a task pending
	// approval is not delivered until it is approved), not capability
	// calls, not terminal, and with the requester's message the latest in
	// the history. A task comes again each time the requester adds a
	// message. Each carries anet.peer_aid and anet.trusted (a bool: the
	// peer is on the trust list) in its metadata, the requester's
	// a2a.serviceParameters in its messages' metadata, and its files inline.
	//
	// A task from a peer that is not on the trust list is delivered only to
	// a module that declared an untrusted backend
	// (Host.DeclareUntrustedBackend). A task delivered here is not also
	// given to the auto-reply agent. The channel closes when ctx ends.
	InboundTasks(ctx context.Context) (<-chan Task, error)

	// ReplyTask answers an inbound task as this node: msg is the answer,
	// and state what it leaves the task in — input-required (the agent
	// asks back), completed, failed or rejected.
	ReplyTask(ctx context.Context, taskID string, msg a2ashape.Message, state a2ashape.TaskState) (Task, error)
}
