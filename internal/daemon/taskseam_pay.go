package daemon

import (
	"context"
	"fmt"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// taskPaymentMessage handles a client message that carries x402.* metadata
// on an existing task: payment-submitted with anet.payment.accept, or
// payment-rejected (A2A-DESIGN §8.7). It returns the state_seq its own
// write left, for the blocking wait that follows.
//
// PLACEHOLDER for the payment work package (C3, wp/x402d), which owns the
// flow: comparing the chosen option with the stored requirements, the
// agent-tier spending limits, and forwarding the PaymentPayload. Until it
// is merged a payment message is refused rather than sent on as chat.
func (d *Daemon) taskPaymentMessage(_ context.Context, ix *interactions.Interaction, _ a2ashape.Message) (int64, error) {
	return 0, fmt.Errorf("%w: task %s: payment messages are not handled by this build", a2ashape.ErrUnsupportedOperation, ix.ID)
}
