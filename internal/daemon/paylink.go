package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
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

// HubIdentity returns the hub's AID and key history as this node pinned
// them from GET /hub/identity (hub_client.go): the KEL must replay to the
// AID and may only ever extend the pinned one, so a hub that later serves a
// forked or truncated history is not believed.
func (s paymentSeam) HubIdentity() (string, []identity.SignedEvent, bool) {
	d := s.d
	hub := d.config().HubURL
	if hub == "" {
		return "", nil, false
	}
	aid, kel, err := d.hubIdentity(d.ctx, hub)
	if err != nil {
		log.Printf("anet: the hub's key history: %v", err)
		return "", nil, false
	}
	return aid, kel, true
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

// PaymentSeam implements module.Host: the ability to act as this node in
// a payment. Absent only when there is no node.
//
// It used to be withheld while no hub was configured, on the reading that
// "no hub means no ledger and nothing can be charged". That reading is
// true of the moment and wrong about the grant. A module takes this seam
// ONCE, at Start, and holds it for the process lifetime; a fresh data
// directory necessarily has an empty hub_url at Start, so the x402 module
// received nil and kept it. `hub-register` then wrote the hub into the
// config and every other subsystem picked it up live, while balance,
// audit-hub, reconcile, redeem, x402-authorize and `delegate --pay` went
// on answering "no hub configured" until the daemon was restarted.
//
// There is no ordering that avoided it: `hub-register` needs a running
// daemon, so daemon-then-register is the only possible first run, and
// that is exactly the sequence that broke. Every new install of a build
// carrying x402 landed on it. Found by the release matrix on the paid
// variant, on both Debian and Ubuntu.
//
// So the seam is granted whenever there is a daemon behind it, and
// "where does this node bank" is answered live by HubURL — which already
// read the config on every call, and which every consumer already checks
// for empty before doing anything. The grant says what a module may do;
// it does not snapshot where.
func (h moduleHost) PaymentSeam() (module.PaymentSeam, bool) {
	if h.d == nil {
		return nil, false
	}
	return paymentSeam{d: h.d}, true
}

// HubSeam is the narrow grant: sign as this node, and where its hub is.
//
// Separate from the payment seam and strictly smaller. A module that
// authenticates requests to its own hub needs a signature and an address;
// giving it the payment seam would hand over the hub's key history and
// this node's evidence log for no reason.
// Granted whenever there is a daemon, for the same reason PaymentSeam is:
// a module holds the seam for its lifetime, and a node that joins a hub
// after start must not need a restart to notice.
func (h moduleHost) HubSeam() (module.HubSeam, bool) {
	if h.d == nil {
		return nil, false
	}
	return hubSeam{d: h.d}, true
}

// hubSeam implements module.HubSeam over the daemon.
type hubSeam struct{ d *Daemon }

func (s hubSeam) Sign(preimage []byte) ([]byte, uint64) { return s.d.self.Sign(preimage) }

func (s hubSeam) HubURL() string { return s.d.config().HubURL }

// PayAndRetry pays a quote up front and delegates the same work again as
// a new task carrying the payment (DelegateReq.Payment, A2A-DESIGN §8.3:
// kept, and checked and settled on the same path as a payment on the
// task). A gateway-tier payment (§8.6).
//
// The interaction id and the task nonce are minted before the
// authorization, since the authorization is bound to both: that binding
// is what stops the payment being reusable on any other work this
// provider is owed for.
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
	opt := pickRail(p, quoted.Accepts)
	if opt == nil {
		return "", fmt.Errorf("anet: this node can pay %q and the provider accepts none of it",
			payment.SchemeCredit)
	}
	// pickRail falls back to the first credit option when none is on this
	// node's ledger, or when that ledger is not known; PayTask refuses both
	// before signing (0017 Q28), and so does this [redteam:Q28].
	if home := p.HomeNetwork(); home == "" || opt.Network != home {
		return "", fmt.Errorf("anet: %s: %s", x402a2a.ReasonRailNotPayable,
			railNotPayableText(opt.Network, home, quoted.Accepts))
	}
	if opt.PayTo != providerAID {
		return "", ErrPayeeNotPeer
	}
	id, err := newInteractionID()
	if err != nil {
		return "", err
	}
	nonce, err := newTaskNonce()
	if err != nil {
		return "", err
	}
	raw, err := p.Authorize(*opt, id, x402a2a.PayBind(id, nonce), module.PurposeGateway)
	if err != nil {
		return "", err
	}
	return d.delegateCapabilityCtx(ctx, id, nonce, providerAID, capID, args, raw, "")
}

// DelegateAndPay delegates, and pays if the provider asks to be paid.
//
// One call because that is the shape a caller wants: "do this, and if it
// costs something, buy it". The quote arrives on the task as
// input-required and the payment goes on the same task (A2A-DESIGN §8.3),
// as a gateway-tier payment (§8.6: /delegate pay:true).
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
	if _, err := d.PayTask(ctx, PayRequest{TaskID: id, Decision: PayDecisionSubmit,
		Purpose: module.PurposeGateway}); err != nil && !errors.Is(err, ErrPaymentPending) {
		return "", nil, err
	}
	ix, err := d.ix.Get(id)
	if err != nil {
		return "", nil, err
	}
	var pp payment.PaymentPayload
	if json.Unmarshal(ix.PayPayload, &pp) != nil {
		return id, nil, nil
	}
	return id, &pp.Accepted, nil
}

// awaitQuote waits for the provider's first answer to a task and reports
// the price if it asked for one.
//
// nil means the work was free and is already done — the absence of a
// quote is how x402 says free, and it is how this says it too. A quote is
// read from the task's stored pay_required, which only a status from the
// task's provider writes: the envelope it arrived in was signed by the
// provider (A2A-DESIGN §3.6, SI-4), so a price cannot be injected by
// anything that merely knows the interaction id.
func (d *Daemon) awaitQuote(ctx context.Context, interactionID string) (*payment.PaymentRequired, error) {
	deadline := time.Now().Add(quoteWait)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		d.pollFresh(ctx)
		ix, err := d.ix.Get(interactionID)
		if err != nil {
			return nil, err
		}
		if ix.PayState != interactions.PayNone {
			if q := storedQuote(ix); q != nil {
				return q, nil
			}
			return nil, fmt.Errorf("anet: provider asked to be paid but quoted no price")
		}
		if ix.IsTerminal() {
			return nil, nil // answered, and not with a price
		}
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("anet: no answer within %s, so nothing to pay for yet", quoteWait)
}

// verificationWord renders the three states for a person reading an
// error, keeping "unknown" distinct from "unverified" rather than
// flattening both into "not verified".
func verificationWord(v string) string {
	switch v {
	case string(interactions.VerificationVerified):
		return "verified"
	case string(interactions.VerificationUnverified):
		return "could not be checked"
	default:
		return "predates this check"
	}
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

// RedeemCredit gives amount back to the hub. payTo is the payee the
// operator confirmed (the hub AID `anet redeem` showed, sent as pay_to): it
// is required, and the payment module signs only when it is the hub this
// node settles on now (module.ErrRedeemPayee otherwise), so a redemption
// confirmed for one hub is never signed to another.
func (d *Daemon) RedeemCredit(ctx context.Context, amount uint64, reference, payTo string) (map[string]any, error) {
	p := d.payer()
	if p == nil {
		return nil, errNoPayments()
	}
	if strings.TrimSpace(payTo) == "" {
		return nil, errRedeemNoPayee
	}
	return p.Redeem(ctx, amount, reference, payTo)
}

// errRedeemNoPayee is a /redeem that does not say which hub it was
// confirmed for.
var errRedeemNoPayee = errors.New("anet: pay_to is required: the hub AID the redemption was confirmed for " +
	"(`anet redeem` shows it and asks on the terminal); nothing was signed")

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

// hubAID is the AID of the hub this node settles on, from the pinned hub
// identity. Empty when there is no hub, which is also when nothing can be
// charged for.
func (d *Daemon) hubAID() string {
	hub := d.config().HubURL
	if hub == "" {
		return ""
	}
	aid, _, err := d.hubIdentity(d.ctx, hub)
	if err != nil {
		return ""
	}
	return aid
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

// pickRail chooses which offered rail to pay on.
//
// Our own hub's ledger when it is offered, because that is the one this
// node actually holds credit on. Taking the first credit option instead
// was correct while a provider only ever offered its own hub — a provider
// that also offers the ledgers its hub will clear against lists its own
// first, so the first option is the one a cross-hub buyer has no balance
// on, and paying it can only fail for insufficient funds.
//
// Falls back to the first credit option, which is the single-hub case and
// also the honest answer when our own ledger is not among those offered:
// try, and let the facilitator say no.
func pickRail(p module.Payer, accepts []payment.PaymentOption) *payment.PaymentOption {
	var first *payment.PaymentOption
	var home string
	if p != nil {
		home = p.HomeNetwork()
	}
	for i := range accepts {
		if accepts[i].Scheme != payment.SchemeCredit {
			continue
		}
		if home != "" && accepts[i].Network == home {
			return &accepts[i]
		}
		if first == nil {
			first = &accepts[i]
		}
	}
	return first
}
