package daemon

import (
	"context"
	"encoding/base64"
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
	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// What the kernel keeps of payment, which is the delegating and none of
// the money.
//
// The split is where the work actually divides. Driving a delegation —
// minting an interaction id, waiting for an answer, delegating again —
// is what this daemon does all day and is kernel whether or not anything
// is being paid for. Minting an authorization, talking to a facilitator,
// opening a public door for vouchers: that is a subsystem, it needs the
// node's signature and a hub's key history, and a node that neither
// charges nor pays should not carry it. So it lives in module/x402 and
// this file is the seam.
//
// A build without the module does not quietly do paid work for free. A
// priced capability is refused and says why, because "I cannot charge
// you, so I will not do it" is true and "here, have it" is a decision
// nobody made.

// payer returns this node's payment subsystem, or nil.
func (d *Daemon) payer() module.Payer {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pay
}

// errNoPayments is what every payment surface says in a build that has
// none. Named so the surfaces cannot each invent their own wording for
// the same fact.
func errNoPayments() error {
	return fmt.Errorf("anet: this build has no payment support " +
		"(built with -tags no_x402, or the x402 module is not configured)")
}

// paymentSeam is the narrow grant module/x402 receives: sign as this
// node, know which hub we settle on, and read our own chain back.
type paymentSeam struct{ d *Daemon }

func (s paymentSeam) Sign(preimage []byte) ([]byte, uint64) { return s.d.self.Sign(preimage) }

func (s paymentSeam) HubURL() string { return s.d.config().HubURL }

// HubIdentity learns the hub's AID and verified key history.
//
// Fetched here rather than in the module because the daemon already has
// the machinery that verifies a key history before trusting it — peers
// remembers only what replayed — and two implementations of "is this key
// history real" is one more than the number that can be right.
func (s paymentSeam) HubIdentity() (string, []identity.SignedEvent, bool) {
	d := s.d
	hub := d.config().HubURL
	if hub == "" {
		return "", nil, false
	}
	aid := d.hubAID()
	if aid == "" {
		return "", nil, false
	}
	if kel, ok := d.peers.resolve(aid); ok {
		return aid, kel, true
	}
	kel, err := fetchAgentKEL(hub, aid)
	if err != nil {
		log.Printf("anet: the hub's key history: %v", err)
		return aid, nil, false
	}
	// remember verifies before storing, so a hub that served a forked or
	// unverifiable history is refused here rather than trusted later.
	d.peers.remember(aid, kel)
	if kel, ok := d.peers.resolve(aid); ok {
		return aid, kel, true
	}
	return aid, nil, false
}

func (s paymentSeam) ReadEvidence(eventType string, limit int) []map[string]any {
	if s.d.ledger == nil {
		return nil
	}
	_, recs := s.d.ledger.Evidence(EvidenceQuery{EventType: eventType, Limit: limit})
	out := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Payload)
	}
	return out
}

// PaymentSeam implements module.Host. Absent when there is no hub, which
// is also when there is no ledger and nothing can be charged.
func (h moduleHost) PaymentSeam() (module.PaymentSeam, bool) {
	if h.d == nil || h.d.config().HubURL == "" {
		return nil, false
	}
	return paymentSeam{d: h.d}, true
}

// answerPaymentRequired quotes a price instead of doing the work.
//
// A full answer, not a refusal: signed, receipted, on the chain and
// delivered like any other. A provider that quoted a price has told the
// caller something true about this interaction, and a quote nobody can
// point back at is not a quote.
func (d *Daemon) answerPaymentRequired(ctx context.Context, interactionID, capID string,
	price uint64, ix *interactions.Interaction) bool {
	p := d.payer()
	res := capabilityResult{
		Capability: capID,
		Status:     string(effect.PaymentRequired),
		Message:    fmt.Sprintf("%s costs %d credits", capID, price),
	}
	if p != nil {
		res.Payment = p.Quote(capID, price)
		if len(res.Payment.Accepts) > 0 {
			res.Message = fmt.Sprintf("%s costs %d credits on %s",
				capID, price, res.Payment.Accepts[0].Network)
		}
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
	p := d.payer()
	if p == nil {
		return "", errNoPayments()
	}
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
	raw, err := p.Authorize(*opt, id)
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
func (d *Daemon) DelegateAndPay(ctx context.Context, providerAID, capID string,
	args map[string]any) (string, *payment.PaymentOption, error) {
	if d.payer() == nil {
		return "", nil, errNoPayments()
	}
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

// Balance and RedeemCredit forward to the payment module, or say plainly
// that this build has none.
func (d *Daemon) Balance(ctx context.Context) (map[string]any, error) {
	p := d.payer()
	if p == nil {
		return nil, errNoPayments()
	}
	return p.Balance(ctx)
}

func (d *Daemon) RedeemCredit(ctx context.Context, amount uint64, reference string) (map[string]any, error) {
	p := d.payer()
	if p == nil {
		return nil, errNoPayments()
	}
	return p.Redeem(ctx, amount, reference)
}

// EvPaymentSettled records that this node had a payment settled in its
// favour. The kernel writes it because the kernel is what knows the
// capability call it belongs to.
const EvPaymentSettled = "anet.payment.settled"

// priceOfCapability asks a provider what a capability costs.
//
// In the kernel because it is a question about a provider, which the
// kernel owns, not a question about money. A build with no payment module
// still needs the answer, so it can refuse the work instead of doing it
// for free.
func priceOfCapability(p provider.CapabilityProvider, capID string) (uint64, bool) {
	priced, ok := p.(provider.Priced)
	if !ok {
		return 0, false
	}
	return priced.Price(capID)
}

// fetchAgentKEL pulls a key history the hub publishes.
func fetchAgentKEL(hubURL, aid string) ([]identity.SignedEvent, error) {
	resp, err := (&http.Client{Timeout: hubCallTimeout}).Get(hubURL + "/agents/" + aid + "/kel")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anet: hub answered %s for %s's key history", resp.Status, aid)
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
	return identity.UnmarshalKEL(raw)
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

// serveModuleFaces brings up the public listeners modules asked for.
func (d *Daemon) serveModuleFaces(ctx context.Context) error {
	if p := d.payer(); p != nil {
		if err := p.Serve(ctx); err != nil {
			return err
		}
	}
	return nil
}
