//go:build !no_x402

package x402

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/relayauth"

	"github.com/ANetResearch/ANet/module"
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

// paymentWindow bounds an authorization. Short: it exists to be spent on
// the delegation being made, and one that outlives that is only useful to
// somebody who kept it.
const paymentWindow = 5 * time.Minute

// hubCallTimeout bounds one call to the hub.
const hubCallTimeout = 30 * time.Second

// paymentRequired builds the x402 402 body for a priced capability.
//
// Our own hub's ledger first, then every ledger our hub says it will
// clear against. The second group is what makes a cross-hub purchase
// possible: a buyer registered elsewhere holds no credit here, and if the
// only offered rail is this hub's own it can do nothing but be told it
// has insufficient funds.
//
// Ours is offered first because it is the one settlement is cheapest and
// most certain on — no second hub has to be reachable for it to complete.
// The buyer picks; this only says what would be accepted.
func (m *Module) paymentRequired(capID string, price uint64) *payment.PaymentRequired {
	opt := func(network string) payment.PaymentOption {
		return payment.PaymentOption{
			Scheme:            payment.SchemeCredit,
			Network:           network,
			Amount:            payment.Amount(price),
			Asset:             payment.AssetCredit,
			PayTo:             m.AID(),
			MaxTimeoutSeconds: int(paymentWindow / time.Second),
		}
	}
	accepts := []payment.PaymentOption{opt(payment.CreditNetwork(m.hubAID()))}
	for _, n := range m.clearableNetworks() {
		if n != accepts[0].Network {
			accepts = append(accepts, opt(n))
		}
	}
	return &payment.PaymentRequired{
		X402Version: payment.Version,
		Resource:    &payment.Resource{URL: "anet:capability/" + capID, Description: capID},
		Accepts:     accepts,
	}
}

// HomeNetwork is the ledger this node's credits live on.
func (m *Module) HomeNetwork() string {
	if m.seam == nil {
		return ""
	}
	hub := m.hubAID()
	if hub == "" {
		return ""
	}
	return payment.CreditNetwork(hub)
}

// clearableNetworks is what our hub's facilitator says it will settle on,
// minus our own, cached.
//
// Cached because a 402 is on the hot path of every priced call and the
// answer changes only when an operator edits federation.json. Failure is
// silent and cached as empty for the same interval: a hub that cannot be
// asked right now should not stop us quoting our own ledger, and the next
// refresh will pick the peers up.
func (m *Module) clearableNetworks() []string {
	const ttl = 5 * time.Minute
	m.clearMu.Lock()
	defer m.clearMu.Unlock()
	hub := hubKey(m.hubURL())
	if hub == m.clearHub && time.Since(m.clearAt) < ttl {
		return m.clearNets
	}
	m.clearAt, m.clearHub = time.Now(), hub
	m.clearNets = nil
	if hub == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), hubCallTimeout)
	defer cancel()
	var sup payment.Supported
	if err := m.getJSON(ctx, "/x402/supported", &sup); err != nil {
		return nil
	}
	mine := payment.CreditNetwork(m.hubAID())
	for _, k := range sup.Kinds {
		if k.Scheme == payment.SchemeCredit && k.Network != mine && k.Network != "" {
			m.clearNets = append(m.clearNets, k.Network)
		}
	}
	return m.clearNets
}

// EvPaymentAuthorized records that this node signed an authorization.
const EvPaymentAuthorized = "anet.payment.authorized"

// EvPaymentSettled records that this node had one settled in its favour.
const EvPaymentSettled = "anet.payment.settled"

// EvCreditRedeemed records credit this node took back out.
const EvCreditRedeemed = "anet.credit.redeemed"

// Authorize signs a payment for one task and records it.
//
// The authorization names the amount, the payee, the hub whose ledger it
// settles on and the binding of the work it pays for. Every one of those
// is inside the signature, so it cannot be re-aimed at other work, a
// larger sum, a different provider or a different hub.
//
// The node's spending policy is asked first (PaymentSeam.AdmitSpend,
// A2A-DESIGN §8.6), and nothing is signed or recorded when it says no.
// That is the only gate: every signature over money this module makes —
// a task payment, a gateway authorization, a redemption — passes through
// here, so a limit cannot be stepped around by picking another door.
//
// bind is what the authorization's InteractionID carries. For a task it
// is pay_bind(ix, task_nonce), which the hub sees and cannot reverse to
// the interaction id; ix itself goes only on this node's own chain.
//
// Returns the marshalled PaymentPayload.
func (m *Module) Authorize(opt payment.PaymentOption, ix, bind, purpose string) ([]byte, error) {
	if m.seam == nil {
		return nil, fmt.Errorf("x402: no hub, so nothing can be paid for")
	}
	amount, err := payment.ParseAmount(opt.Amount)
	if err != nil {
		return nil, err
	}
	// Not a payment on any credit ledger: hubs book amounts as int64, and
	// one without the range check booked such an amount backwards (red
	// team si9). A quote for it is the provider's to have got wrong;
	// nothing is signed, and the spending policy is not asked.
	if amount > math.MaxInt64 {
		return nil, fmt.Errorf("x402: %s: %d is more than a credit ledger can hold; nothing was signed",
			payment.ReasonInvalidAmount, amount)
	}
	if err := m.seam.AdmitSpend(opt.PayTo, amount, purpose); err != nil {
		return nil, fmt.Errorf("x402: not authorized by this node's spending policy: %w", err)
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	now := time.Now()
	auth := &payment.Authorization{
		Payer: m.AID(), PayTo: opt.PayTo, Amount: amount, Network: opt.Network,
		Nonce:         hex.EncodeToString(nonce[:]),
		IssuedAt:      now.UnixMilli(),
		NotAfter:      now.Add(paymentWindow).UnixMilli(),
		InteractionID: bind,
	}
	// Signed through the seam rather than with a key this module holds.
	// The node's controller never leaves the kernel; what crosses is the
	// ability to make one signature, which is what a payment is.
	pre, err := auth.CanonicalPreimage()
	if err != nil {
		return nil, err
	}
	sig, seq := m.seam.Sign(pre)
	auth.Envelope = &aobj.Envelope{
		SignerAID: m.AID(), KeyStateSeq: seq, Alg: aobj.AlgEdDSA, Sig: sig}
	raw, err := auth.Marshal()
	if err != nil {
		return nil, err
	}
	id, err := auth.ID()
	if err != nil {
		return nil, err
	}
	// On our chain before it leaves: an authorization we cannot show we
	// signed is one we cannot dispute later. The purpose is what the
	// kernel rebuilds its daily totals from after a restart.
	if lerr := m.record(EvPaymentAuthorized, map[string]any{
		"interaction_id": ix, "pay_bind": bind, "purpose": purpose,
		"authorization_id": id,
		"pay_to":           opt.PayTo, "amount": opt.Amount, "network": opt.Network,
	}); lerr != nil {
		return nil, lerr
	}
	// The accepted option travels as quoted, minus anything that describes
	// the work: the facilitator sees this object (SI-1).
	accepted := opt
	accepted.Extra = nil
	return json.Marshal(&payment.PaymentPayload{
		X402Version: payment.Version,
		Accepted:    accepted,
		Payload:     map[string]any{"authorization": base64.StdEncoding.EncodeToString(raw)},
	})
}

// settle presents an authorization to the hub's facilitator, with the
// terms it is checked against.
//
// The provider settles because the provider is the payee, which is the
// shape x402 gives it: the resource server asks the facilitator, not the
// client. A caller that settled its own payment would be reporting that
// it had paid.
//
// The body is x402 v2's {x402Version, paymentPayload, paymentRequirements}.
// Neither object carries a description, an extra or a resource: the hub
// learns who pays whom how much on which ledger for which binding, and
// not what the work was (SI-1). It can still name the skill: the payee
// and the exact amount are in the body, and where the payee publishes a
// different price per skill (anet-pricing/v1 on its card, the ADP card's
// price list) the amount picks one. That is stated in A2A-DESIGN §21;
// payments.publish_prices=false keeps the prices off the cards
// [redteam:F1].
func (m *Module) settle(ctx context.Context, raw []byte, req payment.PaymentRequirements) (*payment.SettlementResponse, error) {
	hub := m.hubURL()
	if hub == "" {
		return nil, fmt.Errorf("x402: no hub configured, so no facilitator to settle with")
	}
	var pp payment.PaymentPayload
	if err := json.Unmarshal(raw, &pp); err != nil {
		return nil, fmt.Errorf("x402: payment payload malformed: %w", err)
	}
	// The payer wrote this object; only what the facilitator settles on
	// goes on: the authorization, not whatever else rode with it.
	pp.Accepted.Extra = nil
	pp.Extensions = nil
	pp.Payload = map[string]any{"authorization": pp.Payload["authorization"]}
	req = sanitizeRequirements(req)
	body, err := json.Marshal(payment.FacilitatorRequest{
		X402Version: payment.Version, PaymentPayload: &pp, PaymentRequirements: &req,
	})
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, hub+"/x402/settle", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: hubCallTimeout}).Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out payment.SettlementResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("x402: the facilitator answered %s unreadably: %w", resp.Status, err)
	}
	if resp.StatusCode/100 != 2 && out.ErrorReason == "" {
		// An error status with no errorReason is not the facilitator's
		// answer but something in front of it (a proxy, a rate limit, a
		// restart): not an outcome, so the payment is presented again.
		// The hub states a reason on every refusal it makes.
		return nil, fmt.Errorf("x402: the facilitator answered %s", resp.Status)
	}
	return &out, nil
}

// VerifyReceipt checks a hub settlement receipt and reports what it says.
//
// The signer is pinned to OUR hub rather than read off the object: a
// receipt naming its own signer proves only that somebody signed it. The
// payer is pinned too, because a receipt for somebody else's payment
// verifies perfectly and means nothing here.
func (m *Module) VerifyReceipt(receiptB64, expectPayer string) (module.ReceiptFacts, bool) {
	var facts module.ReceiptFacts
	raw, err := base64.StdEncoding.DecodeString(receiptB64)
	if err != nil {
		return facts, false
	}
	rec, err := payment.UnmarshalReceipt(raw)
	if err != nil {
		return facts, false
	}
	facts.Payee, facts.AuthID, facts.Amount = rec.PayTo, rec.AuthID, rec.Amount
	hubAID := m.hubAID()
	if hubAID == "" {
		return facts, false
	}
	kel, err := m.hubKEL()
	if err != nil {
		return facts, false
	}
	if rec.Verify(kel, hubAID, time.Now().UnixMilli()) != nil {
		return facts, false
	}
	return facts, rec.Payer == expectPayer
}

// Balance reads this node's credit standing off its hub, with the recent
// entries that produced it.
//
// The hub is asked because the hub is the custodian — that is the deal
// stated at the top of this file, and pretending a local number were
// authoritative would misrepresent it. What makes the arrangement
// survivable is the second half: every line here has a counterpart on
// somebody's signed chain, so a balance that disagrees with the evidence
// is a balance that can be shown to be wrong.
func (m *Module) Balance(ctx context.Context) (map[string]any, error) {
	hub := m.hubURL()
	if hub == "" {
		return nil, fmt.Errorf("x402: no hub configured, so there is no ledger to read")
	}
	// "credits", the field the hub actually sends. Reading "balance" here
	// once reported zero for every funded account — the request
	// succeeded, the JSON parsed, and the number was wrong, which is the
	// quietest way a wire mismatch can fail.
	var bal struct {
		Credits int64 `json:"credits"`
	}
	if err := m.signedGet(ctx, "/agents/"+m.AID()+"/balance", relayauth.ActionBalance, &bal); err != nil {
		return nil, err
	}
	out := map[string]any{
		"aid": m.AID(), "hub": hub, "network": payment.CreditNetwork(m.hubAID()),
		"asset": payment.AssetCredit, "balance": bal.Credits,
	}
	var led struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := m.signedGet(ctx, "/agents/"+m.AID()+"/ledger", relayauth.ActionLedger, &led); err == nil {
		out["entries"] = led.Entries
	}
	// The withdrawals, from the hub's own list: the one place an agent
	// finds a redemption by its reference, and served to the account
	// holder only, like the balance and the ledger (§3.7).
	if red, err := m.redemptions(ctx); err == nil {
		out["redemptions"] = red.Redemptions
		out["redeemed_total"], out["redeemed_sum"] = red.Total, red.Sum
		if red.Truncated {
			out["redemptions_truncated"] = true
		}
	}
	return out, nil
}

// HubRedemption is one withdrawal as the hub lists it
// (GET /agents/{aid}/redemptions).
type HubRedemption struct {
	AuthID    string `json:"auth_id"`
	Amount    uint64 `json:"amount"`
	Reference string `json:"reference"`
	At        string `json:"at,omitempty"`
}

// hubRedemptions is the hub's redemption list: the newest page, and the
// count and sum over the whole account.
type hubRedemptions struct {
	Redemptions []HubRedemption `json:"redemptions"`
	Total       int             `json:"total"`
	Sum         uint64          `json:"sum"`
	Truncated   bool            `json:"truncated"`
}

// redemptions reads this node's withdrawals off its hub, signed as this
// node (relayauth v2, action "redemptions"): the list names every
// reference the account redeemed against, which is nobody else's
// business.
func (m *Module) redemptions(ctx context.Context) (hubRedemptions, error) {
	var out hubRedemptions
	err := m.signedGet(ctx, "/agents/"+m.AID()+"/redemptions?limit=500", relayauth.ActionRedemptions, &out)
	if out.Redemptions == nil {
		out.Redemptions = []HubRedemption{}
	}
	return out, err
}

// Redeem gives credit back to the hub against an external reference.
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
//
// payTo is the payee the operator confirmed: the hub AID `anet redeem`
// showed before asking. The authorization is signed to the hub this node
// settles on now, and only when that is payTo; otherwise the node changed
// hubs since the question was asked, or the caller named another payee,
// and the redemption is refused (module.ErrRedeemPayee) before anything is
// signed, so what was confirmed and what is signed cannot differ.
func (m *Module) Redeem(ctx context.Context, amount uint64, reference, payTo string) (map[string]any, error) {
	if amount == 0 {
		return nil, fmt.Errorf("x402: redeem what?")
	}
	hub := m.hubURL()
	if hub == "" {
		return nil, fmt.Errorf("x402: no hub, so no ledger to redeem from")
	}
	hubAID := m.hubAID()
	if hubAID == "" {
		return nil, fmt.Errorf("x402: cannot learn this hub's identity, so cannot sign a redemption to it")
	}
	if payTo = strings.TrimSpace(payTo); payTo != hubAID || hubKey(m.hubURL()) != hubKey(hub) {
		return nil, fmt.Errorf("x402: %w: confirmed %q, the hub at %s is %s; nothing was signed",
			module.ErrRedeemPayee, payTo, hub, hubAID)
	}
	pp, err := m.Authorize(payment.PaymentOption{
		Scheme:  payment.SchemeCredit,
		Network: payment.CreditNetwork(hubAID),
		Amount:  payment.Amount(amount),
		Asset:   payment.AssetCredit,
		PayTo:   hubAID,
	}, "", "redeem:"+reference, module.PurposeRedeem)
	if err != nil {
		return nil, err
	}
	var payload payment.PaymentPayload
	if err := json.Unmarshal(pp, &payload); err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{
		"x402Version": payment.Version, "paymentPayload": &payload, "reference": reference})
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
		return out, fmt.Errorf("x402: redemption refused: %s", msg)
	}
	entry := map[string]any{"amount": amount, "reference": reference,
		"auth_id": out["auth_id"], "verified": false}
	// Verified means the hub signed for what it took. A chain that
	// recorded the redemption either way could not later tell a
	// documented withdrawal from an undocumented one.
	if enc, ok := out["receipt"].(string); ok && enc != "" {
		entry["receipt"] = enc
		if facts, ok := m.VerifyReceipt(enc, m.AID()); ok && facts.Amount == amount {
			entry["verified"] = true
		}
	}
	if lerr := m.record(EvCreditRedeemed, entry); lerr != nil {
		return out, lerr
	}
	out["verified"] = entry["verified"]
	return out, nil
}

// fetchHubKEL pulls the hub's key history and verifies it belongs to the
// AID claimed.
//
// The hub publishes every agent's history at this path including its own,
// so nothing special is required — the custodian is an agent on its own
// registry, and being checkable the same way everyone else is, is the
// point.
func fetchHubKEL(hubURL, aid string) ([]identity.SignedEvent, error) {
	resp, err := (&http.Client{Timeout: hubCallTimeout}).Get(hubURL + "/agents/" + aid + "/kel")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("x402: hub answered %s for its own key history", resp.Status)
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
	states, err := identity.Replay(kel)
	if err != nil {
		return nil, fmt.Errorf("x402: the hub's key history does not replay: %w", err)
	}
	if got := states[len(states)-1].AID; got != aid {
		return nil, fmt.Errorf("x402: the hub published %s's key history, not its own", got)
	}
	return kel, nil
}
