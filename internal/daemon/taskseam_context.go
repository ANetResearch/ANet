package daemon

// taskseam_context.go is the one leniency the A2A interface allows itself
// in SendMessage (0017 Q22): a message that names a contextId and no taskId
// continues the task that is waiting for it, when there is exactly one.
//
// The specification reads such a message as a new task in that context,
// and a client that follows it names the task it answers. Some clients do
// not: Hermes answers an input-required task by sending its next message
// with only the contextId, so under the strict reading the task it answers
// stays input-required for ever and the provider receives a second,
// unrelated delegation. So when the context holds exactly one task of this
// endpoint (outbound, to the agent the client is scoped to) that is
// input-required, the message is appended to it, as if the client had named
// it. Anything else is the specification's reading — a new task in the
// context: no such task, several (the client must say which), a capability
// call (always a new task), and a text message for a waiting capability
// call (a capability call takes no follow-up text). The control plane is
// not lenient: its callers know task ids.
//
// A retry of a message that already made or continued a task is found by
// sendTask's (contextId, messageId) lookup before this rule is consulted,
// so it returns that task and is not appended somewhere else.

import (
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// continuedTask returns the id of the task a contextId-only message
// continues, or "" when it starts a new task.
func (d *Daemon) continuedTask(sc taskScope, peer, contextID string, in *taskInput, meta map[string]any) (string, error) {
	if sc.all || sc.peer == "" || contextID == "" || in == nil || in.capID != "" {
		return "", nil
	}
	page, err := d.ix.ListPage(interactions.ListFilter{Role: interactions.RoleOutbound, ContextID: contextID,
		PeerAID: peer, States: []interactions.State{interactions.StateInputRequired}, Limit: 2})
	if err != nil {
		return "", err
	}
	if len(page.Items) != 1 {
		return "", nil
	}
	ix := page.Items[0]
	if ix.IsCapability && !hasX402Meta(meta) {
		return "", nil
	}
	return ix.ID, nil
}
