package daemon

import (
	"context"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// The payment half of the task surface. PLACEHOLDERS for the payment work
// package (C3, wp/x402d), which owns the flow (A2A-DESIGN §8.3, §8.6,
// §8.7): comparing the chosen option with the stored requirements, the
// agent-tier spending limits, signing, and forwarding the PaymentPayload.
// Until it is merged, payment is refused rather than attempted, and a
// payment message is not passed on as chat.

// taskPaymentMessage handles a client message that carries x402.* metadata
// on an existing task: payment-submitted with anet.payment.accept, or
// payment-rejected (§8.7). It returns the state_seq its own write left, for
// the blocking wait that follows.
func (d *Daemon) taskPaymentMessage(_ context.Context, ix *interactions.Interaction, _ a2ashape.Message) (int64, error) {
	return 0, a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "task %s: payment messages are not handled by this build", ix.ID)
}

// taskPay is TaskSeam.Pay: submit or reject at the agent tier, purpose
// task-agent.
func (d *Daemon) taskPay(_ context.Context, ix *interactions.Interaction, _ module.PayDecision) error {
	return a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "task %s: payment is not handled by this build", ix.ID)
}
