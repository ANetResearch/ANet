package daemon

// The extended card end to end (HPP J-PAY-4, J-PAY-15 card parts, PAY-N2;
// M-PAY-31, 32, 39, 53): requesters and providers registered at one fake
// hub, the request and the answer through the relay as one-shot envelopes.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// cardLabProvider registers a provider at srv serving public and listOnly
// capabilities at prices.
func cardLabProvider(t *testing.T, srv *httptest.Server, name string, public, listOnly []string, prices map[string]uint64) *Daemon {
	t.Helper()
	if len(prices) > 0 && !paymentsCompiled() {
		t.Skip("no payment module in this build: a priced capability is published nowhere")
	}
	prov := newTestDaemon(t, srv.URL, false)
	w := &extWork{id: "extwork", caps: append(append([]string(nil), public...), listOnly...), prices: map[string]uint64{}}
	for k, v := range prices {
		w.prices[k] = v
	}
	if err := prov.Providers().Register(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	setPublic(prov, public...)
	setAllowOnly(prov, listOnly...)
	if err := prov.RegisterWithHub(context.Background(), srv.URL, name, nil); err != nil {
		t.Fatal(err)
	}
	return prov
}

// cardLabRequester registers a requester at srv.
func cardLabRequester(t *testing.T, srv *httptest.Server, name string) *Daemon {
	t.Helper()
	d := newTestDaemon(t, srv.URL, false)
	if err := d.RegisterWithHub(context.Background(), srv.URL, name, nil); err != nil {
		t.Fatal(err)
	}
	return d
}

// whilePolling runs f while ds read their mailboxes, as their relay loops
// would, and returns what f returns.
func whilePolling[T any](t *testing.T, f func() T, ds ...*Daemon) T {
	t.Helper()
	done := make(chan T, 1)
	go func() { done <- f() }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case v := <-done:
			return v
		case <-time.After(20 * time.Millisecond):
		}
		for _, d := range ds {
			_ = d.pollOnce(context.Background())
		}
		if time.Now().After(deadline) {
			t.Fatal("the card exchange did not end")
		}
	}
}

// cardRoute is POST /agents/card as the CLI and MCP call it, decoded.
func cardRoute(t *testing.T, req *Daemon, aid string, prov ...*Daemon) module.RemoteAgent {
	t.Helper()
	w := whilePolling(t, func() *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"aid": aid})
		w := httptest.NewRecorder()
		req.hAgentsCard(w, httptest.NewRequest(http.MethodPost, "/agents/card", bytes.NewReader(b)))
		return w
	}, append([]*Daemon{req}, prov...)...)
	if w.Code != http.StatusOK {
		t.Fatalf("/agents/card: %d %s", w.Code, w.Body)
	}
	var ra module.RemoteAgent
	if err := json.Unmarshal(w.Body.Bytes(), &ra); err != nil {
		t.Fatal(err)
	}
	return ra
}

// listRouteEntry is the entry of aid in POST /agents/list (anet find, MCP
// list_agents).
func listRouteEntry(t *testing.T, req *Daemon, aid string) module.RemoteAgent {
	t.Helper()
	w := httptest.NewRecorder()
	req.hAgentsList(w, httptest.NewRequest(http.MethodPost, "/agents/list", bytes.NewReader([]byte(`{}`))))
	if w.Code != http.StatusOK {
		t.Fatalf("/agents/list: %d %s", w.Code, w.Body)
	}
	var page struct {
		Agents []module.RemoteAgent `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	for _, a := range page.Agents {
		if a.AID == aid {
			return a
		}
	}
	t.Fatalf("%s is not listed: %s", aid, w.Body)
	return module.RemoteAgent{}
}

// extendedCardOf is the local A2A GetExtendedAgentCard of aid.
func extendedCardOf(t *testing.T, req *Daemon, aid string, prov ...*Daemon) (module.ExtendedAgent, error) {
	t.Helper()
	type out struct {
		ea  module.ExtendedAgent
		err error
	}
	o := whilePolling(t, func() out {
		ea, err := req.TaskSeam().ExtendedCard(context.Background(), aid)
		return out{ea, err}
	}, append([]*Daemon{req}, prov...)...)
	return o.ea, o.err
}

// cardRequestsAt is how many card requests d answered.
func cardRequestsAt(d *Daemon) uint64 { return d.ReceiveStats()[cardGetAnswered] }

func skillsOf(prices []module.SkillPrice) map[string]string {
	out := map[string]string{}
	for _, p := range prices {
		out[p.Skill] = p.Amount
	}
	return out
}

func extRefusalCode(err error) string {
	var ref *a2ashape.Refusal
	if errors.As(err, &ref) {
		return a2ashape.ReasonCode(ref.Key)
	}
	return ""
}

func interactionCount(t *testing.T, d *Daemon) int {
	t.Helper()
	n, err := d.ix.Count(interactions.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// J-PAY-4 with provider d3: translate (3 credits) and chat (free) public,
// x (2 credits) for its lists only.
func TestJPAY4PartlyPricedProvider(t *testing.T) {
	srv := newFakeHub(t)
	withRegistry(t, srv.URL)
	d3 := cardLabProvider(t, srv, "Translator", []string{"translate", "chat"}, []string{"x"}, map[string]uint64{"translate": 3, "x": 2})
	d1 := cardLabRequester(t, srv, "Stranger")
	d4 := cardLabRequester(t, srv, "Friend")
	allowPeers(t, d3, d4.AID())
	ixBefore := [3]int{interactionCount(t, d1), interactionCount(t, d3), interactionCount(t, d4)}

	// A list says "some skills paid" from the public card and asks nobody
	// (M-PAY-39).
	if e := listRouteEntry(t, d1, d3.AID()); e.Verification != cardVerified || e.Charge != module.ChargePartial || e.Prices != nil {
		t.Fatalf("the list entry: %+v", e)
	}
	if n := len(queuedFor(t, srv, d3.AID())); n != 0 || cardRequestsAt(d3) != 0 {
		t.Fatalf("listing reached the provider: %d queued, %d answered", n, cardRequestsAt(d3))
	}

	// d1, not on any list: the general card's two prices, no x.
	ra := cardRoute(t, d1, d3.AID(), d3)
	if ra.Charge != module.ChargePartial || ra.PriceNote != "" || ra.PricesFetchedAt == "" ||
		!mapsEqual(skillsOf(ra.Prices), map[string]string{"translate": "3", "chat": "0"}) {
		t.Fatalf("d1's card: %+v", ra)
	}
	if cardRequestsAt(d3) != 1 {
		t.Fatalf("d3 answered %d card requests, want 1", cardRequestsAt(d3))
	}
	// Kept: shown again without asking (PAY-C14).
	if again := cardRoute(t, d1, d3.AID(), d3); !mapsEqual(skillsOf(again.Prices), skillsOf(ra.Prices)) || cardRequestsAt(d3) != 1 {
		t.Fatalf("the second look asked again (%d) or differs: %+v", cardRequestsAt(d3), again)
	}
	ea, err := extendedCardOf(t, d1, d3.AID(), d3)
	if err != nil || slices.Contains(extCardSkillIDs(t, ea.Card), "x") || !slices.Contains(extCardSkillIDs(t, ea.Card), "translate") {
		t.Fatalf("d1's GetExtendedAgentCard: %v %s", err, ea.Card)
	}
	// The extended card did not become the public card's mark: the public
	// card under the same seq still verifies (M-PAY-32).
	if pub, err := d1.agentCard(context.Background(), d3.AID()); err != nil || pub.Verification != cardVerified {
		t.Fatalf("the public card after the extended one: %+v %v", pub, err)
	}
	if e := listRouteEntry(t, d1, d3.AID()); e.Verification != cardVerified {
		t.Fatalf("the list after the extended card: %+v", e)
	}

	// d4, on d3's allow list: x too, in all three places' answer.
	heldCapFrom(t, d3, d4)
	ra = cardRoute(t, d4, d3.AID(), d3)
	if !mapsEqual(skillsOf(ra.Prices), map[string]string{"translate": "3", "chat": "0", "x": "2"}) || ra.Charge != module.ChargePartial {
		t.Fatalf("d4's card: %+v", ra)
	}
	ea, err = extendedCardOf(t, d4, d3.AID(), d3)
	if err != nil || !slices.Contains(extCardSkillIDs(t, ea.Card), "x") {
		t.Fatalf("d4's GetExtendedAgentCard: %v %s", err, ea.Card)
	}
	if row, ok, err := d4.ix.PeerExtCard(d3.AID()); err != nil || !ok || row.Variant != "list" || row.Prices["x"] != 2 {
		t.Fatalf("d4's kept card: %+v %v %v", row, ok, err)
	}

	// No interaction anywhere (M-PAY-31, PAY-C9).
	if got := [3]int{interactionCount(t, d1), interactionCount(t, d3), interactionCount(t, d4)}; got != ixBefore {
		t.Fatalf("interactions %v, before %v", got, ixBefore)
	}
	if p, err := d3.ix.ListPending(); err != nil || len(p) != 0 {
		t.Fatalf("held for approval: %v %v", p, err)
	}
}

// J-PAY-4, the face node (CORE-K23 rule 6): face.detect public and free,
// face.compare for the lists only and free. A stranger is never told of
// face.compare and never asks; a listed caller sees it after its first
// exchange, and not before.
func TestJPAY4FreeFaceNode(t *testing.T) {
	srv := newFakeHub(t)
	d5 := cardLabProvider(t, srv, "Faces", []string{"face.detect"}, []string{"face.compare"}, nil)
	d1 := cardLabRequester(t, srv, "Stranger")
	d4 := cardLabRequester(t, srv, "Friend")
	allowPeers(t, d5, d4.AID())

	pub, err := d1.agentCard(context.Background(), d5.AID())
	if err != nil || pub.Verification != cardVerified || pub.Charge != module.ChargeFree || pub.ExtendedOffered {
		t.Fatalf("the face node's public card: %+v %v", pub, err)
	}
	// d1, and d4 before any exchange: the public card, no request (M-PAY-53).
	for _, d := range []*Daemon{d1, d4} {
		ra := cardRoute(t, d, d5.AID(), d5)
		if ra.Charge != module.ChargeFree || ra.Prices != nil || ra.PriceNote != "" {
			t.Fatalf("before an exchange: %+v", ra)
		}
		if _, err := extendedCardOf(t, d, d5.AID(), d5); extRefusalCode(err) != "extended_card_not_offered" {
			t.Fatalf("GetExtendedAgentCard before an exchange: %v", err)
		}
	}
	if n := len(queuedFor(t, srv, d5.AID())); n != 0 || cardRequestsAt(d5) != 0 {
		t.Fatalf("the face node was asked: %d queued, %d answered", n, cardRequestsAt(d5))
	}

	// d4 after its first exchange: face.compare, free.
	heldCapFrom(t, d5, d4)
	if pub, _ := d4.agentCard(context.Background(), d5.AID()); !pub.ExtendedOffered {
		t.Fatal("a contact capability of the face node's does not make its extended card offered")
	}
	ra := cardRoute(t, d4, d5.AID(), d5)
	if ra.Charge != module.ChargeFree || !mapsEqual(skillsOf(ra.Prices), map[string]string{"face.detect": "0", "face.compare": "0"}) {
		t.Fatalf("d4 after an exchange: %+v", ra)
	}
	ea, err := extendedCardOf(t, d4, d5.AID(), d5)
	if err != nil || !slices.Contains(extCardSkillIDs(t, ea.Card), "face.compare") {
		t.Fatalf("d4's GetExtendedAgentCard: %v", err)
	}
	// d1 still is not told, and asks nothing.
	before := cardRequestsAt(d5)
	if ra := cardRoute(t, d1, d5.AID(), d5); ra.Prices != nil || cardRequestsAt(d5) != before {
		t.Fatalf("d1 after d4's exchange: %+v (%d requests)", ra, cardRequestsAt(d5)-before)
	}
}

// J-PAY-15, the card part: public skills free, the list-only y at 2
// credits. The list says free; a listed caller's card says some skills
// paid, with y's price.
func TestJPAY15OnlyAListSkillIsPriced(t *testing.T) {
	srv := newFakeHub(t)
	withRegistry(t, srv.URL)
	d6 := cardLabProvider(t, srv, "Echo", []string{"echo"}, []string{"y"}, map[string]uint64{"y": 2})
	d4 := cardLabRequester(t, srv, "Friend")
	allowPeers(t, d6, d4.AID())
	heldCapFrom(t, d6, d4)
	if e := listRouteEntry(t, d4, d6.AID()); e.Charge != module.ChargeFree {
		t.Fatalf("the list entry: %+v", e)
	}
	ra := cardRoute(t, d4, d6.AID(), d6)
	if ra.Charge != module.ChargePartial || !mapsEqual(skillsOf(ra.Prices), map[string]string{"echo": "0", "y": "2"}) {
		t.Fatalf("d4's card: %+v", ra)
	}
	if p := d4.signedPriceFor(context.Background(), d6.AID(), "y", 2); !p.Have || p.Price != 2 {
		t.Fatalf("the signed price of y: %+v", p)
	}
}

// PAY-N2 (HPP §7.4 F1, the half the payment module's test cannot reach):
// the prices are not secret from someone who asks the provider. A probe
// identity — an operator's own, say — fetches the extended card end to end
// and reads them; the provider answers it as any other caller.
func TestAProbeIdentityCanReadThePricesFromTheExtendedCard(t *testing.T) {
	srv := newFakeHub(t)
	prov := cardLabProvider(t, srv, "Legal", []string{"legal.contract.review", "text.summarize"}, nil,
		map[string]uint64{"legal.contract.review": 11, "text.summarize": 3})
	probe := cardLabRequester(t, srv, "Probe")
	ra := cardRoute(t, probe, prov.AID(), prov)
	if ra.Charge != module.ChargePaid || !mapsEqual(skillsOf(ra.Prices), map[string]string{"legal.contract.review": "11", "text.summarize": "3"}) {
		t.Fatalf("the probe's card: %+v", ra)
	}
}

// A card request asks for nothing it may not: without the prices member
// set false a display asks; with it (a confirmation prompt wanting a name)
// the agent is not contacted.
func TestTheCardRouteAsksOnlyForADisplay(t *testing.T) {
	srv := newFakeHub(t)
	prov := cardLabProvider(t, srv, "Translator", []string{"translate"}, nil, map[string]uint64{"translate": 3})
	req := cardLabRequester(t, srv, "Req")
	w := httptest.NewRecorder()
	req.hAgentsCard(w, httptest.NewRequest(http.MethodPost, "/agents/card",
		bytes.NewReader([]byte(`{"aid":"`+prov.AID()+`","prices":false}`))))
	var ra module.RemoteAgent
	if err := json.Unmarshal(w.Body.Bytes(), &ra); err != nil || w.Code != 200 || ra.Verification != cardVerified ||
		ra.Charge != module.ChargePaid || ra.Prices != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatalf("%d messages for the provider", n)
	}
}

// PAY-C12, ENG-O11: an answer that comes after the caller stopped waiting
// is checked and kept, and the next display takes it from the cache.
func TestALateCardIsKept(t *testing.T) {
	srv := newFakeHub(t)
	prov := cardLabProvider(t, srv, "Translator", []string{"translate"}, nil, map[string]uint64{"translate": 3})
	req := cardLabRequester(t, srv, "Req")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res := req.fetchExtendedCard(ctx, prov.AID(), nil)
	if res.Outcome != extNoAnswer || res.Why != priceWhyTimeout {
		t.Fatalf("no one read the mailboxes: %+v", res)
	}
	waitUntil(t, "the late answer", func() bool {
		poll(t, prov)
		poll(t, req)
		_, ok, _ := req.ix.PeerExtCard(prov.AID())
		return ok
	})
	if req.ReceiveStats()[cntCardFetchLate] != 1 {
		t.Errorf("the late answer was not counted: %v", req.ReceiveStats())
	}
	before := cardRequestsAt(prov)
	if ra := cardRoute(t, req, prov.AID(), prov); ra.Prices == nil || cardRequestsAt(prov) != before {
		t.Fatalf("the kept card was not used: %+v", ra)
	}
}

// signedPriceFor (PAY-C10 cases 2 and 3): a payment asks without rule 2,
// takes a fresh card from the cache, and asks again once for a quote above
// the price of a card more than a minute old.
func TestSignedPriceForAPayment(t *testing.T) {
	srv := newFakeHub(t)
	prov := cardLabProvider(t, srv, "Translator", []string{"translate"}, nil, map[string]uint64{"translate": 3})
	req := cardLabRequester(t, srv, "Req")
	price := func(capID string, quote uint64) cardPrice {
		return whilePolling(t, func() cardPrice { return req.signedPriceFor(context.Background(), prov.AID(), capID, quote) }, req, prov)
	}
	if p := price("translate", 3); !p.Have || p.Price != 3 || cardRequestsAt(prov) != 1 {
		t.Fatalf("first: %+v, %d requests", p, cardRequestsAt(prov))
	}
	// A skill the card does not list is 0 (PAY-D2 step 4); from the cache.
	if p := price("other", 1); !p.Have || p.Price != 0 || cardRequestsAt(prov) != 1 {
		t.Fatalf("unlisted: %+v, %d requests", p, cardRequestsAt(prov))
	}
	// Above the price, the card just fetched: not asked again.
	if p := price("translate", 5); p.Price != 3 || cardRequestsAt(prov) != 1 {
		t.Fatalf("a higher quote, a fresh card: %+v, %d requests", p, cardRequestsAt(prov))
	}
	// The card a minute old and the price raised: asked again, once.
	row, _, _ := req.ix.PeerExtCard(prov.AID())
	row.FetchedAt -= uint64(2 * time.Minute / time.Millisecond)
	if err := req.ix.PutPeerExtCard(row); err != nil {
		t.Fatal(err)
	}
	if p := price("translate", 5); p.Price != 3 || cardRequestsAt(prov) != 2 {
		t.Fatalf("a higher quote, an older card: %+v, %d requests", p, cardRequestsAt(prov))
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
