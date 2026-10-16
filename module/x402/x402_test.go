//go:build !no_x402

package x402

// The module's own half: signing and settling.
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
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/module/moduletest"
	"github.com/ANetResearch/ANet/provider"
)

// testHost is the node, as the module is allowed to see it.
type testHost struct {
	moduletest.NopHost
	self *identity.Controller
	hub  *identity.Controller
	url  string
	reg  *provider.Registry

	mu     sync.Mutex
	events []hostEvent
	// clock is the node's time for each hub's requests; stales counts the
	// ts_stale answers the module reported; registered is RegisteredAt;
	// stateDir is the module's state directory.
	clock      relayauth.MonoClock
	stales     int
	registered int64
	stateDir   string
	ticks      []int64
	// pinnedKEL, when set, is the hub history the node pinned before the hub rotated.
	pinnedKEL []identity.SignedEvent
	// spendRefusal, when set, is what AdmitSpend answers; spends records
	// the calls.
	spendRefusal error
	spends       []spendCall
}

type spendCall struct {
	payTo   string
	amount  uint64
	purpose string
}

func (h *testHost) AdmitSpend(payTo string, amount uint64, purpose string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.spends = append(h.spends, spendCall{payTo, amount, purpose})
	return h.spendRefusal
}

type hostEvent struct {
	kind    string
	payload map[string]any
}

func newHost(t *testing.T) *testHost {
	t.Helper()
	self, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	hub, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	return &testHost{self: self, hub: hub, url: "http://hub.invalid", reg: provider.NewRegistry()}
}

func (h *testHost) AID() string                   { return h.self.AID() }
func (h *testHost) Providers() *provider.Registry { return h.reg }
func (h *testHost) StateDir() string              { return h.stateDir }

// RecordEvidence keeps the event with the time it was recorded
// (module.EvidenceAt), unless the test gave one.
func (h *testHost) RecordEvidence(kind string, payload any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, _ := payload.(map[string]any)
	cp := make(map[string]any, len(m)+1)
	for k, v := range m {
		cp[k] = v
	}
	if _, ok := cp[module.EvidenceAt]; !ok {
		cp[module.EvidenceAt] = time.Now().UnixMilli()
	}
	h.events = append(h.events, hostEvent{kind: kind, payload: cp})
	return nil
}

// SignHubRequest signs as the node does: this node's clock for the hub, the
// action, both AIDs, the method, the target and the body.
func (h *testHost) SignHubRequest(req *http.Request, action string, body []byte) error {
	ts := uint64(h.clock.Next(h.hub.AID(), time.Now().UnixMilli()))
	sig, seq := h.self.Sign(relayauth.PreimageV2(action, h.self.AID(), h.hub.AID(), ts, req.Method, req.URL.RequestURI(), body))
	req.Header.Set(relayauth.HeaderAID, h.self.AID())
	req.Header.Set(relayauth.HeaderTS, strconv.FormatUint(ts, 10))
	req.Header.Set(relayauth.HeaderSeq, strconv.FormatUint(seq, 10))
	req.Header.Set(relayauth.HeaderSig, relayauth.EncodeSig(sig))
	return nil
}

func (h *testHost) NoteHubStale(floor int64) bool {
	h.mu.Lock()
	h.stales++
	h.mu.Unlock()
	return h.clock.Raise(h.hub.AID(), floor, time.Now().UnixMilli())
}

// RefreshHubIdentity re-learns the hub's key history, which a test that
// rotated the hub has left behind in pinnedKEL.
func (h *testHost) RefreshHubIdentity() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pinnedKEL == nil || len(h.pinnedKEL) == len(h.hub.KEL()) {
		return false
	}
	h.pinnedKEL = nil
	return true
}

func (h *testHost) RegisteredAt() (int64, bool)             { return h.registered, h.registered > 0 }
func (h *testHost) PaymentSeam() (module.PaymentSeam, bool) { return h, true }

func (h *testHost) Sign(pre []byte) ([]byte, uint64) { return h.self.Sign(pre) }
func (h *testHost) HubURL() string                   { return h.url }

// HubDo stands in for the node's hub client: the request as the module
// built it, with the wire version the node adds.
func (h *testHost) HubDo(r *http.Request) (*http.Response, error) {
	r.Header.Set("X-ANet-Wire", "3")
	return http.DefaultClient.Do(r)
}
func (h *testHost) HubIdentity() (string, []identity.SignedEvent, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pinnedKEL != nil {
		return h.hub.AID(), h.pinnedKEL, true
	}
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
	raw, err := m.Authorize(opt, "ix-42", "bind-42", module.PurposeTaskAgent)
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
	if auth.InteractionID != "bind-42" {
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
	if len(evs) != 1 || evs[0].payload["interaction_id"] != "ix-42" ||
		evs[0].payload["pay_bind"] != "bind-42" || evs[0].payload["purpose"] != module.PurposeTaskAgent {
		t.Errorf("the authorization is not on our chain as signed: %+v", evs)
	}
	// The spending policy was asked, with the terms that were then signed.
	if len(h.spends) != 1 || h.spends[0] != (spendCall{"did:anet:provider", 50, module.PurposeTaskAgent}) {
		t.Errorf("AdmitSpend calls = %+v", h.spends)
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

// A provider that costs money.
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

// clockTick takes a time from the node's clock for the hub, as another part
// of the node signing a request would, and fails when it is not above the
// last one handed out.
func (h *testHost) clockTick() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ticks = append(h.ticks, h.clock.Next(h.hub.AID(), time.Now().UnixMilli()))
	return nil
}
