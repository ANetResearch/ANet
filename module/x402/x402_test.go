//go:build !no_x402

package x402

// The module's own half: signing, settling, and the public door.
//
// Against a stand-in host rather than a daemon, because that is what the
// seam is for — a payment subsystem that could only be tested inside a
// running daemon would be a subsystem in name. The loop where both halves
// meet is tested in internal/daemon/pay_test.go with the real module, and
// both are needed: this one says the module is right, that one says the
// two are connected, and a month of green suites proved those are
// different claims.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// testHost is the node, as the module is allowed to see it.
type testHost struct {
	self *identity.Controller
	hub  *identity.Controller
	url  string
	reg  *provider.Registry

	mu     sync.Mutex
	events []hostEvent
}

type hostEvent struct {
	kind    string
	payload map[string]any
}

func newHost(t *testing.T) *testHost {
	t.Helper()
	self, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	hub, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	return &testHost{self: self, hub: hub, url: "http://hub.invalid", reg: provider.NewRegistry()}
}

func (h *testHost) AID() string                   { return h.self.AID() }
func (h *testHost) Providers() *provider.Registry { return h.reg }
func (h *testHost) RecordEvidence(kind string, payload any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, _ := payload.(map[string]any)
	h.events = append(h.events, hostEvent{kind: kind, payload: m})
	return nil
}
func (h *testHost) ResolveKEL(string) ([]identity.SignedEvent, bool) { return nil, false }
func (h *testHost) PaymentSeam() (module.PaymentSeam, bool)          { return h, true }

func (h *testHost) Sign(pre []byte) ([]byte, uint64) { return h.self.Sign(pre) }
func (h *testHost) HubURL() string                   { return h.url }
func (h *testHost) HubIdentity() (string, []identity.SignedEvent, bool) {
	return h.hub.AID(), h.hub.KEL(), true
}
func (h *testHost) ReadEvidence(kind string, limit int) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, e := range h.events {
		if e.kind == kind {
			out = append(out, e.payload)
		}
	}
	return out
}

func (h *testHost) eventsOf(kind string) []hostEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []hostEvent
	for _, e := range h.events {
		if e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func newModule(t *testing.T, h *testHost) *Module {
	t.Helper()
	m, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	return m.(*Module)
}

// An authorization names the work it pays for, so it cannot be spent on
// anything else the provider is owed for.
func TestAnAuthorizationIsBoundToItsInteraction(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	opt := payment.PaymentOption{
		Scheme: payment.SchemeCredit, Network: payment.CreditNetwork(h.hub.AID()),
		Amount: "50", Asset: payment.AssetCredit, PayTo: "did:anet:provider",
	}
	raw, err := m.Authorize(opt, "ix-42")
	if err != nil {
		t.Fatal(err)
	}
	var pp payment.PaymentPayload
	if err := json.Unmarshal(raw, &pp); err != nil {
		t.Fatal(err)
	}
	encoded, _ := pp.Payload["authorization"].(string)
	authRaw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := payment.UnmarshalAuthorization(authRaw)
	if err != nil {
		t.Fatal(err)
	}
	if auth.InteractionID != "ix-42" {
		t.Errorf("authorization is not bound to the work: %q", auth.InteractionID)
	}
	if auth.Payer != h.AID() {
		t.Errorf("payer = %s, want this node", auth.Payer)
	}
	// Signed through the seam, so it must verify against the node's key
	// history exactly as if the kernel had signed it. A module holding a
	// key of its own would produce a signature nobody could attribute.
	if err := auth.Verify(h.self.KEL(), time.Now().UnixMilli()); err != nil {
		t.Errorf("an authorization signed through the seam does not verify: %v", err)
	}
	// Signing it put it on our chain: an authorization we cannot show we
	// signed is one we cannot dispute later.
	evs := h.eventsOf(EvPaymentAuthorized)
	if len(evs) != 1 || evs[0].payload["interaction_id"] != "ix-42" {
		t.Errorf("the authorization is not on our chain: %+v", evs)
	}
}

// A quote names this node as payee and this node's hub as the ledger.
func TestAQuoteNamesUsAndOurLedger(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	q := m.Quote("work.do", 120)
	if len(q.Accepts) != 1 {
		t.Fatalf("quote = %+v", q)
	}
	opt := q.Accepts[0]
	if opt.PayTo != h.AID() {
		t.Errorf("payTo = %s, want this node", opt.PayTo)
	}
	if opt.Network != payment.CreditNetwork(h.hub.AID()) {
		t.Errorf("network = %s, want our hub's ledger", opt.Network)
	}
	if opt.Amount != "120" {
		t.Errorf("amount = %s", opt.Amount)
	}
}

// A provider that costs money, for the voucher door.
type pricedWork struct {
	price   uint64
	invoked int
}

func (p *pricedWork) ID() string { return "priced" }
func (p *pricedWork) Capabilities(context.Context) ([]string, error) {
	return []string{"work.do"}, nil
}
func (p *pricedWork) Describe(context.Context) (string, error) { return "", nil }
func (p *pricedWork) Health(context.Context) error             { return nil }
func (p *pricedWork) Price(capID string) (uint64, bool) {
	if capID == "work.do" {
		return p.price, true
	}
	return 0, false
}
func (p *pricedWork) Invoke(context.Context, provider.Call) (effect.Effect, error) {
	p.invoked++
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{"done": 1}},
		Evidence: &effect.Evidence{Protocol: "test", Requested: "work.do",
			NativeAck: true, VerifyTrust: 1},
	}, nil
}

func withWork(t *testing.T, h *testHost, price uint64) *pricedWork {
	t.Helper()
	w := &pricedWork{price: price}
	if err := h.reg.Register(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	return w
}

// The voucher door: paid at the hub, worked here, and the hub saw
// neither the request nor the result.
func TestAVoucherBuysWorkTheHubNeverSees(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	w := withWork(t, h, 120)

	v := signVoucher(t, h, h.AID(), "work.do", "buyer-1", "nonce-1")
	res, code := m.RedeemVoucher(context.Background(), redeemRequest{
		Voucher: v, Capability: "work.do", Args: map[string]any{"n": 1}})
	if code != 200 {
		t.Fatalf("redeem = %d %v", code, res)
	}
	if res["status"] != string(effect.OK) {
		t.Fatalf("status = %v", res["status"])
	}
	if w.invoked != 1 {
		t.Fatalf("work ran %d times, want 1", w.invoked)
	}
	// The effect is on the chain like any other. A second door to the
	// same work must not be a door around the evidence.
	effs := h.eventsOf(EvCapabilityEffect)
	if len(effs) != 1 || effs[0].payload["via"] != "voucher" {
		t.Errorf("the chain does not record how this was paid for: %+v", effs)
	}
	if red := h.eventsOf(EvVoucherRedeemed); len(red) != 1 || red[0].payload["payer"] != "buyer-1" {
		t.Errorf("the redemption is not on the chain: %+v", red)
	}
}

// Every way of getting free work out of the voucher door.
func TestTheVoucherDoorRefusesWhatItShould(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	w := withWork(t, h, 120)

	cases := []struct {
		name string
		req  redeemRequest
		code int
	}{
		{"no voucher at all", redeemRequest{Capability: "work.do"}, 402},
		{"not base64", redeemRequest{Voucher: "!!!", Capability: "work.do"}, 402},
		{"random bytes", redeemRequest{
			Voucher:    base64.StdEncoding.EncodeToString([]byte("nonsense")),
			Capability: "work.do"}, 402},
		{"bought from somebody else, presented here", redeemRequest{
			Voucher:    signVoucher(t, h, "did:anet:elsewhere", "work.do", "buyer-1", "n-a"),
			Capability: "work.do"}, 402},
		{"bought a different capability", redeemRequest{
			Voucher:    signVoucher(t, h, h.AID(), "work.cheap", "buyer-1", "n-b"),
			Capability: "work.do"}, 402},
		{"signed by somebody who is not our hub", redeemRequest{
			Voucher:    forgeVoucher(t, h.AID(), "work.do", "buyer-1", "n-c"),
			Capability: "work.do"}, 402},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := w.invoked
			res, code := m.RedeemVoucher(context.Background(), tc.req)
			if code != tc.code {
				t.Errorf("code = %d, want %d (%v)", code, tc.code, res)
			}
			if w.invoked != before {
				t.Error("work was done for a voucher that should have been refused")
			}
		})
	}
	// And the refusals are on the chain, so an operator can find out its
	// node is turning away every buyer.
	if refs := h.eventsOf(EvVoucherRefused); len(refs) != len(cases) {
		t.Errorf("%d refusals recorded, want %d", len(refs), len(cases))
	}
}

// A voucher is a bearer object. Presenting it twice must buy one job.
func TestAVoucherIsSpentOnce(t *testing.T) {
	h := newHost(t)
	m := newModule(t, h)
	w := withWork(t, h, 120)

	v := signVoucher(t, h, h.AID(), "work.do", "buyer-1", "nonce-once")
	req := redeemRequest{Voucher: v, Capability: "work.do"}
	if _, code := m.RedeemVoucher(context.Background(), req); code != 200 {
		t.Fatalf("first redemption failed")
	}
	if _, code := m.RedeemVoucher(context.Background(), req); code != 409 {
		t.Errorf("second redemption should be 409")
	}
	if w.invoked != 1 {
		t.Errorf("work ran %d times for one voucher", w.invoked)
	}

	// And the guard survives a restart, because it is read back off the
	// chain rather than held only in memory. A buyer who kept a voucher
	// over a crash must not get a second job out of it.
	m.spent = newSpentVouchers()
	if _, code := m.RedeemVoucher(context.Background(), req); code != 409 {
		t.Error("after a restart the voucher was spendable again")
	}
	if w.invoked != 1 {
		t.Errorf("work ran %d times across the restart", w.invoked)
	}
}

// Half-configured is the shape that fails silently later.
func TestAHalfConfiguredVoucherFaceIsRefusedAtStartup(t *testing.T) {
	for _, raw := range []string{
		`{"voucher_addr":"127.0.0.1:0"}`,
		`{"voucher_url":"https://x.example/x402/redeem"}`,
	} {
		if _, err := New([]byte(raw)); err == nil {
			t.Errorf("%s was accepted — one half alone sells work nobody can collect", raw)
		}
	}
	if _, err := New([]byte(`{}`)); err != nil {
		t.Errorf("an unconfigured module must still load (a node can pay without selling): %v", err)
	}
}

// signVoucher mints one signed by this host's hub.
func signVoucher(t *testing.T, h *testHost, payTo, capID, payer, nonce string) string {
	t.Helper()
	return mintVoucher(t, h.hub, payTo, capID, payer, nonce,
		payment.CreditNetwork(h.hub.AID()))
}

// forgeVoucher mints a perfectly valid voucher signed by a stranger.
func forgeVoucher(t *testing.T, payTo, capID, payer, nonce string) string {
	t.Helper()
	impostor, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	return mintVoucher(t, impostor, payTo, capID, payer, nonce,
		payment.CreditNetwork(impostor.AID()))
}

func mintVoucher(t *testing.T, signer *identity.Controller,
	payTo, capID, payer, nonce, network string) string {
	t.Helper()
	v := &payment.Voucher{
		AuthID: "auth-" + nonce, Payer: payer, PayTo: payTo, Capability: capID,
		Amount: 120, Network: network,
		NotAfter: time.Now().Add(time.Minute).UnixMilli(), Nonce: nonce,
	}
	if err := v.Sign(signer); err != nil {
		t.Fatal(err)
	}
	raw, err := v.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}
