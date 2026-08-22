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
	"net/http"
	"time"

	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"

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
func (m *Module) paymentRequired(capID string, price uint64) *payment.PaymentRequired {
	return &payment.PaymentRequired{
		X402Version: payment.Version,
		Resource:    &payment.Resource{URL: "anet:capability/" + capID, Description: capID},
		Accepts: []payment.PaymentOption{{
			Scheme:            payment.SchemeCredit,
			Network:           payment.CreditNetwork(m.hubAID()),
			Amount:            payment.Amount(price),
			Asset:             payment.AssetCredit,
			PayTo:             m.AID(),
			MaxTimeoutSeconds: int(paymentWindow / time.Second),
		}},
	}
}

// EvPaymentAuthorized records that this node signed an authorization.
const EvPaymentAuthorized = "anet.payment.authorized"

// EvPaymentSettled records that this node had one settled in its favour.
const EvPaymentSettled = "anet.payment.settled"

// EvCreditRedeemed records credit this node took back out.
const EvCreditRedeemed = "anet.credit.redeemed"

// Authorize signs a payment for one delegation and records it.
//
// The authorization names the amount, the payee, the hub whose ledger it
// settles on and the interaction it pays for. Every one of those is
// inside the signature, so it cannot be re-aimed at other work, a larger
// sum, a different provider or a different hub.
//
// Returns the marshalled PaymentPayload, ready to ride with a delegation.
// The kernel drives the delegation; this signs the money.
func (m *Module) Authorize(opt payment.PaymentOption, interactionID string) ([]byte, error) {
	if m.seam == nil {
		return nil, fmt.Errorf("x402: no hub, so nothing can be paid for")
	}
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
		Payer: m.AID(), PayTo: opt.PayTo, Amount: amount, Network: opt.Network,
		Nonce:         hex.EncodeToString(nonce[:]),
		IssuedAt:      now.UnixMilli(),
		NotAfter:      now.Add(paymentWindow).UnixMilli(),
		InteractionID: interactionID,
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
	// signed is one we cannot dispute later.
	if lerr := m.record(EvPaymentAuthorized, map[string]any{
		"interaction_id": interactionID, "authorization_id": id,
		"pay_to": opt.PayTo, "amount": opt.Amount, "network": opt.Network,
	}); lerr != nil {
		return nil, lerr
	}
	return json.Marshal(&payment.PaymentPayload{
		X402Version: payment.Version,
		Accepted:    opt,
		Payload:     map[string]any{"authorization": base64.StdEncoding.EncodeToString(raw)},
	})
}

// settle presents an authorization to the hub's facilitator.
//
// The provider settles because the provider is the payee, which is the
// shape x402 gives it: the resource server asks the facilitator, not the
// client. A caller that settled its own payment would be reporting that
// it had paid.
func (m *Module) settle(ctx context.Context, raw []byte) (*payment.SettlementResponse, error) {
	hub := m.hubURL()
	if hub == "" {
		return nil, fmt.Errorf("x402: no hub configured, so no facilitator to settle with")
	}
	var pp payment.PaymentPayload
	if err := json.Unmarshal(raw, &pp); err != nil {
		return nil, fmt.Errorf("x402: payment payload malformed: %w", err)
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
			return fmt.Errorf("x402: hub answered %s for %s", resp.Status, path)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into)
	}
	// "credits", the field the hub actually sends. Reading "balance" here
	// once reported zero for every funded account — the request
	// succeeded, the JSON parsed, and the number was wrong, which is the
	// quietest way a wire mismatch can fail.
	var bal struct {
		Credits int64 `json:"credits"`
	}
	if err := get("/agents/"+m.AID()+"/balance", &bal); err != nil {
		return nil, err
	}
	out := map[string]any{
		"aid": m.AID(), "hub": hub, "network": payment.CreditNetwork(m.hubAID()),
		"asset": payment.AssetCredit, "balance": bal.Credits,
	}
	var led struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := get("/agents/"+m.AID()+"/ledger", &led); err == nil {
		out["entries"] = led.Entries
	}
	return out, nil
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
func (m *Module) Redeem(ctx context.Context, amount uint64, reference string) (map[string]any, error) {
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
	pp, err := m.Authorize(payment.PaymentOption{
		Scheme:  payment.SchemeCredit,
		Network: payment.CreditNetwork(hubAID),
		Amount:  payment.Amount(amount),
		Asset:   payment.AssetCredit,
		PayTo:   hubAID,
	}, "redeem:"+reference)
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
