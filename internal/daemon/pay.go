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
	"github.com/ANetResearch/ANetCore/identity"
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

// DelegateAndPay delegates, and pays if the provider asks to be paid.
//
// One call because that is the shape a caller wants: "do this, and if it
// costs something, buy it". The quote is still a real interaction with a
// real receipt — this does not hide it, it acts on it — and the paid
// interaction is a second one, because the first ended with an answer
// that is true and worth keeping.
//
// Returns the paid interaction's id, and what was paid, so a caller that
// wanted to know the price before committing can read it afterwards and
// a caller that did not can ignore it.
func (d *Daemon) DelegateAndPay(ctx context.Context, providerAID, capID string,
	args map[string]any) (string, *payment.PaymentOption, error) {
	id, err := d.DelegateCapability(ctx, providerAID, capID, args)
	if err != nil {
		return "", nil, err
	}
	quote, err := d.awaitQuote(ctx, id)
	if err != nil {
		return "", nil, err
	}
	if quote == nil {
		return id, nil, nil // free, and already delegated
	}
	paidID, err := d.PayAndRetry(ctx, providerAID, capID, args, quote)
	if err != nil {
		return "", nil, err
	}
	var opt *payment.PaymentOption
	if len(quote.Accepts) > 0 {
		opt = &quote.Accepts[0]
	}
	return paidID, opt, nil
}

// awaitQuote waits for the first answer and reports a price if one came.
//
// nil means the work was free and is already done or under way — the
// absence of a 402 is how x402 says free, and it is how this says it too.
func (d *Daemon) awaitQuote(ctx context.Context, interactionID string) (*payment.PaymentRequired, error) {
	deadline := time.Now().Add(quoteWait)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		results, err := d.Results(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range results {
			if r.InteractionID != interactionID {
				continue
			}
			var res capabilityResult
			if err := json.Unmarshal([]byte(r.Result), &res); err != nil {
				return nil, err
			}
			if res.Status == string(effect.PaymentRequired) {
				if res.Payment == nil {
					return nil, fmt.Errorf("anet: provider asked to be paid but quoted no price")
				}
				return res.Payment, nil
			}
			return nil, nil // answered, and not with a price
		}
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("anet: no answer within %s, so nothing to pay for yet", quoteWait)
}

// quoteWait bounds how long a caller waits to learn a price. Short,
// because a quote is computed rather than performed: a provider that
// cannot say what something costs inside a minute is not going to.
const quoteWait = 60 * time.Second

// Balance reads this node's credit standing off its hub, with the recent
// entries that produced it.
//
// The hub is asked because the hub is the custodian — that is the deal
// stated at the top of this file, and pretending a local number were
// authoritative would misrepresent it. What makes the arrangement
// survivable is the second half: every line here has a counterpart on
// somebody's signed chain, so a balance that disagrees with the evidence
// is a balance that can be shown to be wrong.
func (d *Daemon) Balance(ctx context.Context) (map[string]any, error) {
	hub := d.config().HubURL
	if hub == "" {
		return nil, fmt.Errorf("anet: no hub configured, so there is no ledger to read")
	}
	get := func(path string, into any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, hub+path, nil)
		if err != nil {
			return err
		}
		resp, err := (&http.Client{Timeout: hubCallTimeout}).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("anet: hub answered %s for %s", resp.Status, path)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into)
	}
	// The hub's field is "credits" (aghub.Balance). Reading "balance"
	// here silently reported zero for every funded account — the request
	// succeeded, the JSON parsed, and the number was wrong, which is the
	// quietest way for a wire mismatch to fail.
	var bal struct {
		Credits int64 `json:"credits"`
	}
	if err := get("/agents/"+d.AID()+"/balance", &bal); err != nil {
		return nil, err
	}
	out := map[string]any{
		"aid": d.AID(), "hub": hub, "network": payment.CreditNetwork(d.hubAID()),
		"asset": payment.AssetCredit, "balance": bal.Credits,
	}
	var led struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := get("/agents/"+d.AID()+"/ledger", &led); err == nil {
		out["entries"] = led.Entries
	}
	return out, nil
}

// hubKEL fetches and caches the hub's key history.
//
// Needed because the hub signs settlements, and a settlement receipt is
// only worth having if the signature can be checked. The hub publishes
// every agent's KEL at this path including its own, so nothing special is
// required — the custodian is an agent on its own registry, and being
// checkable the same way everyone else is, is the point.
func (d *Daemon) hubKEL() ([]identity.SignedEvent, error) {
	aid := d.hubAID()
	if aid == "" {
		return nil, fmt.Errorf("anet: no hub, so no key history to check settlements against")
	}
	if kel, ok := d.peers.resolve(aid); ok {
		return kel, nil
	}
	hub := d.config().HubURL
	resp, err := (&http.Client{Timeout: hubCallTimeout}).Get(hub + "/agents/" + aid + "/kel")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anet: hub answered %s for its own key history", resp.Status)
	}
	var out struct {
		KEL string `json:"kel"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(out.KEL)
	if err != nil {
		return nil, err
	}
	kel, err := identity.UnmarshalKEL(raw)
	if err != nil {
		return nil, err
	}
	// remember verifies before storing, so a hub that served a forked or
	// unverifiable history is refused here rather than trusted later.
	d.peers.remember(aid, kel)
	if kel, ok := d.peers.resolve(aid); ok {
		return kel, nil
	}
	return nil, fmt.Errorf("anet: the hub's key history did not verify")
}

// EvCreditRedeemed records credit this node took back out.
const EvCreditRedeemed = "anet.credit.redeemed"

// RedeemCredit gives credit back to the hub against an external reference.
//
// Signed as a payment to the hub, because that is what it is: the agent
// authorises the hub to take N credits, and the hub destroys them and
// says under signature that it did. What the reference is worth outside
// this ledger is between the operator and the agent — this code does not
// model it and does not pretend to.
//
// The receipt comes back and goes on this node's chain. Without it the
// agent has a smaller balance and nothing to point at, which is the worst
// possible shape for the one operation that gives money away.
func (d *Daemon) RedeemCredit(ctx context.Context, amount uint64, reference string) (map[string]any, error) {
	if amount == 0 {
		return nil, fmt.Errorf("anet: redeem what?")
	}
	hub := d.config().HubURL
	if hub == "" {
		return nil, fmt.Errorf("anet: no hub, so no ledger to redeem from")
	}
	hubAID := d.hubAID()
	if hubAID == "" {
		return nil, fmt.Errorf("anet: cannot learn this hub's identity, so cannot sign a redemption to it")
	}
	pp, err := d.Authorize(payment.PaymentOption{
		Scheme:  payment.SchemeCredit,
		Network: payment.CreditNetwork(hubAID),
		Amount:  payment.Amount(amount),
		Asset:   payment.AssetCredit,
		PayTo:   hubAID,
	}, "redeem:"+reference)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{
		"x402Version": payment.Version, "paymentPayload": pp, "reference": reference})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hub+"/x402/redeem", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: hubCallTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := out["error"].(string)
		if msg == "" {
			msg = resp.Status
		}
		return out, fmt.Errorf("anet: redemption refused: %s", msg)
	}
	entry := map[string]any{"amount": amount, "reference": reference,
		"auth_id": out["auth_id"], "verified": false}
	// Verified means the hub signed for what it took. A chain that
	// recorded the redemption either way could not later tell a
	// documented withdrawal from an undocumented one.
	if enc, ok := out["receipt"].(string); ok && enc != "" {
		entry["receipt"] = enc
		if raw, derr := base64.StdEncoding.DecodeString(enc); derr == nil {
			if rec, rerr := payment.UnmarshalReceipt(raw); rerr == nil {
				if kel, kerr := d.hubKEL(); kerr == nil {
					if rec.Verify(kel, hubAID, nowMillis()) == nil &&
						rec.Payer == d.AID() && rec.Amount == amount {
						entry["verified"] = true
					}
				}
			}
		}
	}
	if _, lerr := d.ledger.Append(EvCreditRedeemed, entry); lerr != nil {
		log.Printf("anet: redemption evidence: %v", lerr)
	}
	out["verified"] = entry["verified"]
	return out, nil
}
