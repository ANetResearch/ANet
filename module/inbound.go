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
	// a2a.serviceParameters in its messages' metadata, and the files of its
	// latest message inline (earlier messages' files as references: they
	// came inline with the delivery that brought them).
	// Its contextId is not the requester's: the kernel derives it from the
	// peer and the task's context, so tasks one peer sends in one context
	// share it and two peers never do, whatever context they name (an agent
	// behind a backend keeps a conversation per context).
	//
	// A task from a peer that is not on the trust list is delivered only
	// once a module has declared an untrusted backend
	// (Host.DeclareUntrustedBackend), and only from a peer someone named (on
	// the allow list, or approved) — the declaration is the node's, not one
	// module's. A task delivered here is not also given to the auto-reply
	// agent. Nothing is delivered before the daemon has finished starting.
	// The channel closes when ctx ends.
	InboundTasks(ctx context.Context) (<-chan Task, error)

	// ReplyTask answers an inbound task as this node: msg is the answer,
	// and state what it leaves the task in — input-required (the agent
	// asks back), completed, failed or rejected. State working with no msg
	// is not an answer: it tells the requester the task is being worked on
	// (a status), which a module sends when its agent takes long, so that
	// the requester does not fail the task as no_response (A2A-DESIGN
	// §4.2, §11.6).
	ReplyTask(ctx context.Context, taskID string, msg a2ashape.Message, state a2ashape.TaskState) (Task, error)

	// InboundTask is task taskID as InboundTasks would deliver it now,
	// decided afresh: the trust and deny lists read now, the task as it is
	// now. ok is false when a module may not have it any more — its peer
	// left the trust list, the task ended or was answered, its latest
	// message is not the requester's — and for an id that is not such a
	// task. A module that tries a failed forward again asks this before each
	// attempt rather than reuse what it was given, so a retry never forwards
	// on the strength of an old reading (A2A-DESIGN §11.6).
	InboundTask(ctx context.Context, taskID string) (task Task, ok bool, err error)
}
