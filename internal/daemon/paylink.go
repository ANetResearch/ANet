package daemon

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/relayauth"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// What the kernel keeps of payment, which is the delegating and none of
// the money.
//
// The split is where the work actually divides. Driving a delegation —
// minting an interaction id, waiting for an answer, carrying a quote and
// a payment on the task — is what this daemon does all day and is kernel
// whether or not anything is being paid for. Minting an authorization and
// talking to a facilitator: that is a subsystem, it needs the node's
// signature and a hub's key history, and a node that neither charges nor
// pays should not carry it. So it lives in module/x402 and this file is
// the seam.
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

// noPayments is what every payment surface says on a node that cannot pay
// or charge: module_absent for the payment module, in the words of the way
// it is absent (not built in, or turned off by without). Named so the
// surfaces cannot each invent their own wording for the same fact (MOD-2).
// A node whose module is available but holds no payer (a start that did not
// finish) is told as not built in.
func (d *Daemon) noPayments() *a2ashape.Refusal {
	if ref := d.avail.Absent("x402", ""); ref != nil {
		return ref
	}
	return a2ashape.Refuse("module_absent", module.AbsentArgs("x402", module.AbsentNotCompiled))
}

// paymentSeam is the narrow grant module/x402 receives: sign as this
// node, know which hub we settle on, and read our own chain back.
//
// Sign is the node's identity.Signer itself, embedded rather than wrapped:
// the module's calls of it are the signing call sites the wire registry
// lists, each with the object it signs (core CORE-L9), and the embedded
// interface hands on Sign, AID and KEL and nothing that changes the
// identity (KEL O-4).
type paymentSeam struct {
	identity.Signer
	d *Daemon
}

func (s paymentSeam) HubURL() string { return s.d.config().HubURL }

// HubDo sends a module's request to this node's hub on the side its route
// belongs to (moduleHubDo, sealed-sender §4.3.11).
func (s paymentSeam) HubDo(req *http.Request) (*http.Response, error) { return s.d.moduleHubDo(req) }

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

// PeerHubKEL is the key history of another hub of the federation, as this
// node pinned it (the hub KEL set, KEL D-24). A hub it holds none of is
// asked for once through the on-demand refresh, which the daemon limits to
// one read a minute; nothing is believed that was not pinned by the
// extension rule.
func (s paymentSeam) PeerHubKEL(aid string) ([]identity.SignedEvent, bool) {
	d := s.d
	if aid == "" {
		return nil, false
	}
	if kel, ok := d.hubKELs(aid); ok {
		return kel, true
	}
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	d.refreshHubKELs(ctx)
	return d.hubKELs(aid)
}

func (s paymentSeam) ReadEvidence(eventType string, limit int) []map[string]any {
	if s.d.ledger == nil {
		return nil
	}
	_, recs := s.d.ledger.Evidence(EvidenceQuery{EventType: eventType, Limit: limit})
	out := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		// The record's own time rides along as module.EvidenceAt, on a copy:
		// a check against the hub's ledger window needs to know which of
		// this node's events fall inside it.
		p := make(map[string]any, len(r.Payload)+1)
		for k, v := range r.Payload {
			p[k] = v
		}
		p[module.EvidenceAt] = r.Timestamp
		out = append(out, p)
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
// audit-hub, reconcile and redeem went on answering "no hub configured"
// until the daemon was restarted.
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
	if h.d == nil || !h.grants(module.GrantPaymentSeam) {
		return nil, false
	}
	return paymentSeam{Signer: h.d.self, d: h.d}, true
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
	if h.d == nil || !h.grants(module.GrantHubSeam) {
		return nil, false
	}
	return hubSeam{Signer: h.d.self, d: h.d}, true
}

// hubSeam implements module.HubSeam over the daemon. Sign is the node's
// identity.Signer, embedded as in paymentSeam.
type hubSeam struct {
	identity.Signer
	d *Daemon
}

func (s hubSeam) HubURL() string { return s.d.config().HubURL }

// HubDo is paymentSeam.HubDo.
func (s hubSeam) HubDo(req *http.Request) (*http.Response, error) { return s.d.moduleHubDo(req) }

// hubAccount is this node's account state at its hub, read with a signed
// balance request by the kernel's own hub client (HPP §4.3.8): the part of
// /balance that belongs to admission rather than to payment. Probation and
// the trial credit show whether or not a payment module is built in.
type hubAccount struct {
	Credits        int64   `json:"credits"`
	ProbationUntil *uint64 `json:"probation_until,omitempty"`
	Trial          string  `json:"trial,omitempty"`
}

// HubAccount reads the account state off the hub.
func (d *Daemon) HubAccount(ctx context.Context) (hubAccount, error) {
	var acct hubAccount
	hub := d.config().HubURL
	if hub == "" {
		return acct, errors.New("anet: no hub configured, so there is no account to read")
	}
	err := d.hubSigned(ctx, hub, http.MethodGet, "/agents/"+d.AID()+"/balance", relayauth.ActionBalance, nil, &acct)
	return acct, err
}

// moduleAbsent is the value of a field only a missing module could fill.
const moduleAbsent = "module_absent"

// Balance is /balance: the account state from the kernel's hub client, and
// the balance with its entries from the payment module. Without a payment
// module the account state is still answered and the balance is
// module_absent.
func (d *Daemon) Balance(ctx context.Context) (map[string]any, error) {
	acct, aerr := d.HubAccount(ctx)
	var out map[string]any
	if p := d.payer(); p != nil {
		var err error
		if out, err = p.Balance(ctx); err != nil {
			return nil, err
		}
	} else {
		if aerr != nil {
			// Neither half to give: the older answer, which names the cause.
			return nil, d.noPayments()
		}
		out = map[string]any{"aid": d.AID(), "hub": d.config().HubURL, "balance": moduleAbsent}
	}
	if aerr == nil {
		if acct.ProbationUntil != nil {
			out["probation_until"] = *acct.ProbationUntil
		}
		if acct.Trial != "" {
			out["trial"] = acct.Trial
		}
	}
	return out, nil
}

// RedeemCredit gives amount back to the hub. payTo is the payee the
// operator confirmed (the hub AID `anet redeem` showed, sent as pay_to): it
// is required, and the payment module signs only when it is the hub this
// node settles on now (module.ErrRedeemPayee otherwise), so a redemption
// confirmed for one hub is never signed to another.
func (d *Daemon) RedeemCredit(ctx context.Context, amount uint64, reference, payTo string) (map[string]any, error) {
	p := d.payer()
	if p == nil {
		return nil, d.noPayments()
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

// serveModuleFaces starts the payment module's background work (Payer.Serve).
// It opens no listener: the module has no public door.
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
