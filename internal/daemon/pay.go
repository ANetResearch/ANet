package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"

	"github.com/ANetResearch/ANet/provider"
)

// Paying for work, x402 style.
//
// A priced capability answers PAYMENT_REQUIRED with a price rather than
// failing, the caller signs an authorization for exactly that work, and
// the provider — the party being paid, as x402 has it — presents it to
// the hub's facilitator. The hub moves the credit.
//
// The hub keeps the balances and is therefore their custodian; that is
// stated in ANetCore's payment package and in the hub's own code. What it
// cannot do is rewrite what happened, and this file is where that becomes
// concrete: the payer signs the authorization, the settlement comes back
// naming a transaction, and both parties append the event to their own
// evidence chain. The balance is the hub's. The record is theirs.

// EvPaymentAuthorized records that this node signed an authorization.
const EvPaymentAuthorized = "anet.payment.authorized"

// EvPaymentSettled records that this node had one settled in its favour.
const EvPaymentSettled = "anet.payment.settled"

// paymentWindow bounds an authorization. Short: it exists to be spent on
// the delegation being made, and one that outlives that is only useful to
// somebody who kept it.
const paymentWindow = 5 * time.Minute

// priceOf asks a provider what a capability costs.
func priceOf(p provider.CapabilityProvider, capID string) (uint64, bool) {
	priced, ok := p.(provider.Priced)
	if !ok {
		return 0, false
	}
	return priced.Price(capID)
}

// paymentRequired builds the x402 402 body for a priced capability.
func (d *Daemon) paymentRequired(capID string, price uint64) *payment.PaymentRequired {
	return &payment.PaymentRequired{
		X402Version: payment.Version,
		Resource:    &payment.Resource{URL: "anet:capability/" + capID, Description: capID},
		Accepts: []payment.PaymentOption{{
			Scheme:            payment.SchemeCredit,
			Network:           payment.CreditNetwork(d.hubAID()),
			Amount:            payment.Amount(price),
			Asset:             payment.AssetCredit,
			PayTo:             d.AID(),
			MaxTimeoutSeconds: int(paymentWindow / time.Second),
		}},
	}
}

// Authorize signs a payment for one delegation and records it.
//
// The authorization names the amount, the payee, the hub whose ledger it
// settles on and the interaction it pays for. Every one of those is
// inside the signature, so it cannot be re-aimed at other work, a larger
// sum, a different provider or a different hub.
func (d *Daemon) Authorize(opt payment.PaymentOption, interactionID string) (*payment.PaymentPayload, error) {
	amount, err := payment.ParseAmount(opt.Amount)
	if err != nil {
		return nil, err
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	now := time.Now()
	auth := &payment.Authorization{
		PayTo: opt.PayTo, Amount: amount, Network: opt.Network,
		Nonce:         hex.EncodeToString(nonce[:]),
		IssuedAt:      now.UnixMilli(),
		NotAfter:      now.Add(paymentWindow).UnixMilli(),
		InteractionID: interactionID,
	}
	if err := auth.Sign(d.self); err != nil {
		return nil, err
	}
	raw, err := auth.Marshal()
	if err != nil {
		return nil, err
	}
	id, err := auth.ID()
	if err != nil {
		return nil, err
	}
	// On our chain before it leaves: an authorization we cannot show we
	// signed is one we cannot dispute later.
	if _, lerr := d.ledger.Append(EvPaymentAuthorized, map[string]any{
		"interaction_id": interactionID, "authorization_id": id,
		"pay_to": opt.PayTo, "amount": opt.Amount, "network": opt.Network,
	}); lerr != nil {
		log.Printf("anet: payment evidence: %v", lerr)
	}
	return &payment.PaymentPayload{
		X402Version: payment.Version,
		Accepted:    opt,
		Payload:     map[string]any{"authorization": base64.StdEncoding.EncodeToString(raw)},
	}, nil
}

// settle presents an authorization to the hub's facilitator.
//
// The provider settles because the provider is the payee, which is the
// shape x402 gives it: the resource server asks the facilitator, not the
// client. A caller that settled its own payment would be reporting that
// it had paid.
func (d *Daemon) settle(ctx context.Context, raw []byte) (*payment.SettlementResponse, error) {
	hub := d.config().HubURL
	if hub == "" {
		return nil, fmt.Errorf("anet: no hub configured, so no facilitator to settle with")
	}
	var pp payment.PaymentPayload
	if err := json.Unmarshal(raw, &pp); err != nil {
		return nil, fmt.Errorf("anet: payment payload malformed: %w", err)
	}
	body, err := json.Marshal(map[string]any{
		"x402Version": payment.Version, "paymentPayload": &pp,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hub+"/x402/settle", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: hubCallTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out payment.SettlementResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// hubAID is the AID of the hub this node settles on, cached from
// /hub/identity. Empty when there is no hub, which is also when nothing
// can be charged for.
func (d *Daemon) hubAID() string {
	d.mu.Lock()
	cached := d.cachedHubAID
	hub := d.cfg.HubURL
	d.mu.Unlock()
	if cached != "" || hub == "" {
		return cached
	}
	resp, err := (&http.Client{Timeout: hubCallTimeout}).Get(hub + "/hub/identity")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		AID string `json:"aid"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out) != nil {
		return ""
	}
	d.mu.Lock()
	d.cachedHubAID = out.AID
	d.mu.Unlock()
	return out.AID
}

// answerPaymentRequired quotes a price instead of doing the work.
//
// A full answer, not a refusal: signed, receipted, on the chain and
// delivered like any other. A provider that quoted a price has told the
// caller something true about this interaction, and a quote nobody can
// point back at is not a quote.
func (d *Daemon) answerPaymentRequired(ctx context.Context, interactionID, capID string,
	price uint64, ix *interactions.Interaction) bool {
	res := capabilityResult{
		Capability: capID,
		Status:     string(effect.PaymentRequired),
		Message:    fmt.Sprintf("%s costs %d credits on %s", capID, price, payment.CreditNetwork(d.hubAID())),
		Payment:    d.paymentRequired(capID, price),
	}
	return d.deliverCapabilityResult(ctx, interactionID, capID, ix, res, nil)
}

// PayAndRetry takes a PAYMENT_REQUIRED answer, pays it, and delegates the
// same work again.
//
// A second delegation rather than a resumption of the first, and that is
// the honest shape: the first interaction ended with a real answer — a
// quote — which is signed, receipted and on both chains. Re-opening it to
// pretend the quote never happened would erase the only record that the
// price was ever stated.
//
// It picks the first option it can pay. A caller wanting a different rail
// signs the authorization itself and delegates directly; this is the
// convenience path, not the only one.
func (d *Daemon) PayAndRetry(ctx context.Context, providerAID, capID string,
	args map[string]any, quoted *payment.PaymentRequired) (string, error) {
	if quoted == nil || len(quoted.Accepts) == 0 {
		return "", fmt.Errorf("anet: nothing to pay — the answer carried no accepted rails")
	}
	var opt *payment.PaymentOption
	for i := range quoted.Accepts {
		if quoted.Accepts[i].Scheme == payment.SchemeCredit {
			opt = &quoted.Accepts[i]
			break
		}
	}
	if opt == nil {
		return "", fmt.Errorf("anet: this node can pay %q and the provider accepts none of it",
			payment.SchemeCredit)
	}
	// The interaction id has to be minted before the authorization, since
	// the authorization names it: that binding is what stops the payment
	// being reusable on any other work this provider is owed for.
	id, err := newInteractionID()
	if err != nil {
		return "", err
	}
	pp, err := d.Authorize(*opt, id)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(pp)
	if err != nil {
		return "", err
	}
	return d.delegateCapabilityWithID(ctx, id, providerAID, capID, args, raw)
}
