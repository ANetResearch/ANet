package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity/identitytest"

	"github.com/ANetResearch/ANet/internal/netcard"
	"github.com/ANetResearch/ANet/module"
)

// extWork serves capabilities at prices that a test may change.
type extWork struct {
	lampProvider
	id     string
	caps   []string
	mu     sync.Mutex
	prices map[string]uint64
}

func (w *extWork) ID() string { return w.id }
func (w *extWork) Capabilities(context.Context) ([]string, error) {
	return append([]string(nil), w.caps...), nil
}
func (w *extWork) Price(c string) (uint64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.prices[c]
	return p, ok
}
func (w *extWork) setPrice(c string, p uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prices[c] = p
}

func paymentsCompiled() bool {
	for _, n := range module.Compiled() {
		if n == "x402" {
			return true
		}
	}
	return false
}

// extNode is a daemon registered nowhere but configured with a hub (the
// payment module reads its ledger there), serving public and allow_only
// capabilities at the given prices.
func extNode(t *testing.T, public, listOnly []string, prices map[string]uint64) (*Daemon, *extWork) {
	t.Helper()
	h := newFakeHub(t)
	d := newCardDaemon(t, "Extended", "Signs extended cards.")
	w := &extWork{id: "extwork", caps: append(append([]string(nil), public...), listOnly...), prices: map[string]uint64{}}
	for k, v := range prices {
		w.prices[k] = v
	}
	if err := d.Providers().Register(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	setPublic(d, public...)
	d.mu.Lock()
	d.cfg.HubURL = h.URL
	d.mu.Unlock()
	setAllowOnly(d, listOnly...)
	return d, w
}

// setAllowOnly stands in for inbound.public_capabilities[].allow_only
// (allowOnlyCapabilityIDs) until the configuration has it.
func setAllowOnly(d *Daemon, ids ...string) {
	d.extCard.mu.Lock()
	defer d.extCard.mu.Unlock()
	d.extCard.allowOnly = append([]string(nil), ids...)
}

// PAY-C8, rule by rule: the variant for the sender, then not_configured,
// busy over six requests a minute, the card. A variant that could not be
// made is answered busy, never not_configured.
func TestCardAnswerTable(t *testing.T) {
	general, list := []byte(`{"general":1}`), []byte(`{"list":1}`)
	cards := func(g, l extVariant) cardAnswerInput { return cardAnswerInput{General: g, List: l} }
	same := extVariant{Same: true}
	notConfigured := delegation.CardResp{Error: delegation.CardNotConfigured}
	busy := delegation.CardResp{Error: delegation.CardBusy}
	for _, c := range []struct {
		name   string
		in     cardAnswerInput
		listed bool
		recent int
		want   delegation.CardResp
	}{
		{"stranger, public card only", cards(same, same), false, 1, notConfigured},
		{"listed, public card only", cards(same, same), true, 1, notConfigured},
		{"stranger, prices", cards(extVariant{Card: general}, extVariant{Card: list}), false, 1, delegation.CardResp{Card: general}},
		{"listed, prices", cards(extVariant{Card: general}, extVariant{Card: list}), true, 1, delegation.CardResp{Card: list}},
		// The face node: the list variant adds an allow_only skill and no price.
		{"stranger, only list skills", cards(same, extVariant{Card: list}), false, 1, notConfigured},
		{"listed, only list skills", cards(same, extVariant{Card: list}), true, 1, delegation.CardResp{Card: list}},
		{"sixth request in a minute", cards(extVariant{Card: general}, same), false, 6, delegation.CardResp{Card: general}},
		{"seventh request in a minute", cards(extVariant{Card: general}, same), false, 7, busy},
		{"seventh request, public card only", cards(same, same), false, 7, notConfigured},
		{"could not be made", cards(extVariant{Err: fmt.Errorf("x")}, same), false, 1, busy},
	} {
		in := c.in
		in.Listed, in.Recent = c.listed, c.recent
		got := cardAnswer(in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
		if _, err := got.Marshal(); err != nil {
			t.Errorf("%s: the answer does not encode: %v", c.name, err)
		}
	}
}

// answerOf decodes what answerCardGet sends sender.
func answerOf(t *testing.T, d *Daemon, sender string) delegation.CardResp {
	t.Helper()
	got := d.answerCardGet(sender)
	b, err := got.Marshal()
	if err != nil {
		t.Fatalf("the answer to %s does not encode: %v", sender, err)
	}
	back, err := delegation.UnmarshalCardResp(b)
	if err != nil {
		t.Fatal(err)
	}
	return *back
}

func extCardSkillIDs(t *testing.T, raw []byte) []string {
	t.Helper()
	var c struct {
		Skills []struct {
			ID string `json:"id"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range c.Skills {
		out = append(out, s.ID)
	}
	return out
}

// The answers of whole nodes (PAY-C8 three conclusions, CORE-K23 rule 1,
// J-PAY-4's cases): what a stranger, a peer on the allow list, one on the
// trust list and a denied one (also on the allow list) are told, the card
// verified as a caller verifies it.
func TestCardAnswerOfANode(t *testing.T) {
	const stranger, friend, trusted, denied = "did:anet:bstranger", "did:anet:bfriend", "did:anet:btrusted", "did:anet:bdenied"
	type want struct {
		notConfigured bool
		skills        []string
		prices        map[string]uint64
	}
	for _, c := range []struct {
		name             string
		public, listOnly []string
		prices           map[string]uint64
		needsPayments    bool
		general, list    want
	}{
		{name: "free, nothing for the lists", public: []string{lampCap},
			general: want{notConfigured: true}, list: want{notConfigured: true}},
		// A face-detection node: face.compare only for the lists, nothing priced.
		// The list variant differs from the public card by a skill alone and
		// is still an extended card.
		{name: "free, an allow_only skill", public: []string{"face.detect"}, listOnly: []string{"face.compare"},
			general: want{notConfigured: true},
			list:    want{skills: []string{"face.compare", "face.detect"}, prices: map[string]uint64{}}},
		{name: "priced, with a priced allow_only skill", public: []string{"translate", "chat.free"}, listOnly: []string{"x"},
			prices: map[string]uint64{"translate": 3, "x": 2}, needsPayments: true,
			general: want{skills: []string{"chat.free", "translate"}, prices: map[string]uint64{"translate": 3}},
			list:    want{skills: []string{"chat.free", "translate", "x"}, prices: map[string]uint64{"translate": 3, "x": 2}}},
		{name: "only the allow_only skill priced", public: []string{"echo"}, listOnly: []string{"y"},
			prices: map[string]uint64{"y": 2}, needsPayments: true,
			general: want{notConfigured: true},
			list:    want{skills: []string{"echo", "y"}, prices: map[string]uint64{"y": 2}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.needsPayments && !paymentsCompiled() {
				t.Skip("no payment module in this build: a priced capability is published nowhere")
			}
			d, _ := extNode(t, c.public, c.listOnly, c.prices)
			for _, p := range []struct{ aid, list string }{{friend, ListAllow}, {trusted, ListTrust}, {denied, ListAllow}, {denied, ListDeny}} {
				if _, err := d.editPeerFile(p.list, p.aid, true); err != nil {
					t.Fatal(err)
				}
			}
			for _, s := range []struct {
				aid  string
				want want
			}{{stranger, c.general}, {denied, c.general}, {friend, c.list}, {trusted, c.list}} {
				got := answerOf(t, d, s.aid)
				if s.want.notConfigured {
					if got.Error != delegation.CardNotConfigured {
						t.Errorf("%s: %+v, want not_configured", s.aid, got)
					}
					continue
				}
				if len(got.Card) == 0 {
					t.Errorf("%s: no card: %+v", s.aid, got)
					continue
				}
				v := verifyOwn(t, d, got.Card)
				if !slices.Equal(extCardSkillIDs(t, got.Card), s.want.skills) {
					t.Errorf("%s: skills %v, want %v", s.aid, extCardSkillIDs(t, got.Card), s.want.skills)
				}
				if p, err := netcard.PricesOf(got.Card); err != nil || !reflect.DeepEqual(p, s.want.prices) {
					t.Errorf("%s: prices %v (%v), want %v", s.aid, p, err, s.want.prices)
				}
				// The public card's seq, repeated; no jku; extendedAgentCard.
				_, pubSeq, err := d.networkCard(d.config().HubURL)
				if err != nil || v.Seq != pubSeq {
					t.Errorf("%s: card seq %d, public card seq %d (%v)", s.aid, v.Seq, pubSeq, err)
				}
				if hdr := extProtected(t, got.Card); hdr["jku"] != nil {
					t.Errorf("%s: an extended card names a jku: %v", s.aid, hdr)
				}
				if ok, _ := netcard.DeclaresExtended(got.Card); !ok {
					t.Errorf("%s: no extendedAgentCard", s.aid)
				}
				if len(got.Card) > delegation.MaxCardBytes {
					t.Errorf("%s: %d bytes", s.aid, len(got.Card))
				}
			}
			// The public card names no price and no allow_only skill.
			pub, _, err := d.networkCard(d.config().HubURL)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range c.listOnly {
				if slices.Contains(extCardSkillIDs(t, pub), id) {
					t.Errorf("the public card lists the allow_only skill %s", id)
				}
			}
			if bytes.Contains(pub, []byte(module.ExtPriceURI)) || bytes.Contains(pub, []byte(`"prices"`)) {
				t.Errorf("the public card carries prices:\n%s", pub)
			}
		})
	}
}

func extProtected(t *testing.T, card []byte) map[string]any {
	t.Helper()
	var c map[string]any
	if err := json.Unmarshal(card, &c); err != nil {
		t.Fatal(err)
	}
	sigs, _ := c["signatures"].([]any)
	if len(sigs) != 1 {
		t.Fatalf("signatures %v", c["signatures"])
	}
	return decodeProtected(t, sigs[0].(map[string]any)["protected"].(string))
}

// PAY-C4: a signed card is kept while nothing it says changes and made
// again when something does — a price, the skills for the lists — and only
// the variant that changed.
func TestTheExtendedCardIsKeptUntilWhatItSaysChanges(t *testing.T) {
	if !paymentsCompiled() {
		t.Skip("no payment module in this build: a priced capability is published nowhere")
	}
	d, w := extNode(t, []string{"translate"}, []string{"x", "z"}, map[string]uint64{"translate": 3, "x": 2})
	setAllowOnly(d, "x")
	g1, l1 := d.extendedCards()
	g2, l2 := d.extendedCards()
	if g1.Err != nil || l1.Err != nil || !bytes.Equal(g1.Card, g2.Card) || !bytes.Equal(l1.Card, l2.Card) {
		t.Fatalf("the same cards were made again (%v %v)", g1.Err, l1.Err)
	}
	// Another allow_only skill: the list card changes, the general does not.
	setAllowOnly(d, "x", "z")
	g3, l3 := d.extendedCards()
	if !bytes.Equal(g3.Card, g1.Card) || bytes.Equal(l3.Card, l1.Card) {
		t.Fatal("a new allow_only skill: want only the list card made again")
	}
	if !slices.Contains(extCardSkillIDs(t, l3.Card), "z") {
		t.Fatalf("the list card does not list z: %v", extCardSkillIDs(t, l3.Card))
	}
	// A new price: both cards carry it.
	w.setPrice("translate", 4)
	g4, l4 := d.extendedCards()
	for _, c := range []extVariant{g4, l4} {
		if p, err := netcard.PricesOf(c.Card); err != nil || p["translate"] != 4 {
			t.Fatalf("prices %v (%v), want translate at 4", p, err)
		}
	}
	if q, ok := d.payer().Price("translate"); !ok || q != 4 {
		t.Fatalf("the quote's price %d %v: the card and the quote come from one answer", q, ok)
	}
}

// After a rotation the extended cards are signed under the new key, with
// its kid, and verify against the rotated KEL; the old ones do not (KEL E4,
// PAY-C4: no hook, the key state is part of what a card is made from).
func TestTheExtendedCardIsSignedAgainAfterARotation(t *testing.T) {
	d, _ := extNode(t, []string{"face.detect"}, []string{"face.compare"}, nil)
	if _, err := d.editPeerFile(ListAllow, "did:anet:bfriend", true); err != nil {
		t.Fatal(err)
	}
	before := answerOf(t, d, "did:anet:bfriend").Card
	if len(before) == 0 {
		t.Fatal("no list card")
	}
	if kid := extProtected(t, before)["kid"]; kid != a2acard.KID(d.AID(), d.self.CurrentSeq()) {
		t.Fatalf("kid %v", kid)
	}
	if err := identitytest.Rotate(d.self, d.nowMS()); err != nil {
		t.Fatal(err)
	}
	after := answerOf(t, d, "did:anet:bfriend").Card
	if len(after) == 0 || bytes.Equal(after, before) {
		t.Fatal("the card was not made again after the rotation")
	}
	if kid := extProtected(t, after)["kid"]; kid != a2acard.KID(d.AID(), d.self.CurrentSeq()) || d.self.CurrentSeq() == 0 {
		t.Fatalf("kid %v after the rotation to seq %d", kid, d.self.CurrentSeq())
	}
	verifyOwn(t, d, after)
	if _, err := a2acard.Verify(before, d.selfKELResolver, d.nowMS()); err == nil {
		t.Fatal("the card signed before the rotation still verifies")
	}
}

// PAY-C4: the public card's seq is part of what an extended card is made
// from. A public card issued again with the same content under a new seq (a
// hub's conflict answer: networkCardLocked with fresh) is followed by new
// extended cards that repeat it. A caller that has seen the new public card
// refuses an extended card with a smaller seq (PAY-C13), so keeping the one
// signed before would leave it with no card at all.
func TestTheExtendedCardFollowsANewPublicSeq(t *testing.T) {
	d, _ := extNode(t, []string{"face.detect"}, []string{"face.compare"}, nil)
	if _, err := d.editPeerFile(ListAllow, "did:anet:bfriend", true); err != nil {
		t.Fatal(err)
	}
	before := answerOf(t, d, "did:anet:bfriend").Card
	if len(before) == 0 {
		t.Fatal("no list card")
	}
	hub := d.config().HubURL
	_, seq1, err := d.networkCard(hub)
	if err != nil || verifyOwn(t, d, before).Seq != seq1 {
		t.Fatalf("the extended card does not repeat the public seq %d (%v)", seq1, err)
	}
	d.netCard.mu.Lock()
	_, seq2, err := d.networkCardLocked(hub, d.config().Name, true)
	d.netCard.mu.Unlock()
	if err != nil || seq2 <= seq1 {
		t.Fatalf("re-issued public card: seq %d after %d (%v)", seq2, seq1, err)
	}
	after := answerOf(t, d, "did:anet:bfriend").Card
	if len(after) == 0 {
		t.Fatal("no list card after the public card was issued again")
	}
	if got := verifyOwn(t, d, after).Seq; got != seq2 {
		t.Fatalf("extended card seq %d, the public card's is now %d", got, seq2)
	}
}

// The extended cards repeat the public card as it was issued, under the name
// a registration published it with, which can differ from the configured
// one until the hub has accepted it (cardForRegistration). Asking for an
// extended card does not issue another public card under the configured name.
func TestTheExtendedCardRepeatsThePublishedName(t *testing.T) {
	d, _ := extNode(t, []string{"face.detect"}, []string{"face.compare"}, nil)
	if _, err := d.editPeerFile(ListAllow, "did:anet:bfriend", true); err != nil {
		t.Fatal(err)
	}
	const published = "Published Name"
	if d.config().Name == published {
		t.Fatal("the fixture's configured name is the published one")
	}
	card, seq := d.cardForRegistration(d.config().HubURL, published, false)
	if len(card) == 0 {
		t.Fatal("no public card issued")
	}
	got := answerOf(t, d, "did:anet:bfriend").Card
	if len(got) == 0 {
		t.Fatal("no list card")
	}
	var c struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(got, &c); err != nil || c.Name != published {
		t.Fatalf("extended card name %q, the published card's is %q (%v)", c.Name, published, err)
	}
	if v := verifyOwn(t, d, got); v.Seq != seq {
		t.Fatalf("extended card seq %d, the published card's %d", v.Seq, seq)
	}
	d.netCard.mu.Lock()
	issued := d.netCard.issued
	d.netCard.mu.Unlock()
	if issued == nil || issued.Seq != seq {
		t.Fatalf("answering issued another public card: %+v, published seq %d", issued, seq)
	}
}

// PAY-C8 rule 2: six requests a minute per sender; the seventh is busy for
// a priced node, and the window moves on. Other senders are not affected,
// and the senders remembered stay bounded. The times are passed in, not
// read from the daemon's clock, which its background loops also read.
func TestCardRequestsAreLimitedPerSender(t *testing.T) {
	d := newCardDaemon(t, "Limit", "")
	now := uint64(1_800_000_000_000)
	for i := 1; i <= 9; i++ {
		if n := d.noteCardRequest("did:anet:ba", now); n != min(i, cardRequestsPerWindow+1) {
			t.Fatalf("request %d counted %d", i, n)
		}
		now += 1000
	}
	if n := d.noteCardRequest("did:anet:bb", now); n != 1 {
		t.Fatalf("another sender counted %d", n)
	}
	now += uint64(cardRequestWindow.Milliseconds())
	if n := d.noteCardRequest("did:anet:ba", now); n != 1 {
		t.Fatalf("a minute later the window still counts %d", n)
	}
	for i := 0; i < cardRequestSenders+50; i++ {
		d.noteCardRequest(fmt.Sprintf("did:anet:bs%d", i), now)
		now++
	}
	d.extCard.reqMu.Lock()
	n := len(d.extCard.requests)
	d.extCard.reqMu.Unlock()
	if n > cardRequestSenders {
		t.Fatalf("%d senders remembered, bound %d", n, cardRequestSenders)
	}
}

// The window is the minute up to now on the daemon's wall clock. When that
// clock steps back (a time correction), the requests noted "later" are not
// in the window, so the sender is not kept busy until the clock catches
// up; after the step the limit counts from the new time as before.
func TestCardRequestLimitSurvivesAClockStepBack(t *testing.T) {
	d := newCardDaemon(t, "Limit", "")
	now := uint64(1_800_000_000_000)
	for i := 0; i <= cardRequestsPerWindow; i++ {
		d.noteCardRequest("did:anet:ba", now+uint64(i))
	}
	if n := d.noteCardRequest("did:anet:ba", now+10); n <= cardRequestsPerWindow {
		t.Fatalf("over the limit, counted %d", n)
	}
	back := now - uint64(time.Hour.Milliseconds())
	if n := d.noteCardRequest("did:anet:ba", back); n != 1 {
		t.Fatalf("after the clock stepped back an hour the window counts %d", n)
	}
	for i := 2; i <= cardRequestsPerWindow+1; i++ {
		if n := d.noteCardRequest("did:anet:ba", back+uint64(i)); n != i {
			t.Fatalf("request %d after the step counted %d", i, n)
		}
	}
}

// The limit through the whole answer: the seventh card request of a minute
// from the same peer is busy; a free node still says not_configured.
func TestTheSeventhCardRequestIsBusy(t *testing.T) {
	d, _ := extNode(t, []string{"face.detect"}, []string{"face.compare"}, nil)
	if _, err := d.editPeerFile(ListAllow, "did:anet:bfriend", true); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= cardRequestsPerWindow; i++ {
		if got := answerOf(t, d, "did:anet:bfriend"); len(got.Card) == 0 {
			t.Fatalf("request %d: %+v", i, got)
		}
		if got := answerOf(t, d, "did:anet:bstranger"); got.Error != delegation.CardNotConfigured {
			t.Fatalf("stranger request %d: %+v", i, got)
		}
	}
	if got := answerOf(t, d, "did:anet:bfriend"); got.Error != delegation.CardBusy {
		t.Fatalf("seventh request: %+v", got)
	}
	if got := answerOf(t, d, "did:anet:bstranger"); got.Error != delegation.CardNotConfigured {
		t.Fatalf("seventh stranger request: %+v (rule 1 comes first)", got)
	}
}

// A node without a hub, or without a public card, has no extended card.
func TestNoPublicCardNoExtendedCard(t *testing.T) {
	d := newCardDaemon(t, "Nothing", "")
	if g, l := d.extendedCards(); !g.Same || !l.Same {
		t.Fatalf("no hub: %+v %+v", g, l)
	}
	d2, _ := extNode(t, nil, []string{"face.compare"}, nil)
	if g, l := d2.extendedCards(); !g.Same || !l.Same {
		t.Fatalf("no public skill: %+v %+v", g, l)
	}
}

// doctor's payments.card, as GET /status?doctor=1 carries it (ux §5.2, HPP
// §5.6): absent on a node with nothing priced; ok on one whose cards are
// made and verify; price_invalid naming the capability whose price is out
// of 1..2^63-1; card_failed with variant and step when a card cannot be
// made. A plain /status does not make the cards.
func TestPaymentsCardCheck(t *testing.T) {
	status := func(d *Daemon, query string) map[string]any {
		rec := httptest.NewRecorder()
		d.hStatus(rec, httptest.NewRequest(http.MethodGet, "/status"+query, nil))
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	free, _ := extNode(t, []string{"face.detect"}, []string{"face.compare"}, nil)
	if c := free.paymentsCardCheck(); c != nil {
		t.Fatalf("a free node: %+v", c)
	}
	if _, ok := status(free, "?doctor=1")["payments_card"]; ok {
		t.Fatal("a free node reports payments_card")
	}
	if !paymentsCompiled() {
		t.Skip("no payment module in this build: nothing is priced, so payments.card never applies")
	}
	d, w := extNode(t, []string{"translate"}, []string{"x"}, map[string]uint64{"translate": 3, "x": 2})
	if c := d.paymentsCardCheck(); c == nil || c.Status != "ok" {
		t.Fatalf("priced node: %+v", c)
	}
	if got := status(d, "?doctor=1")["payments_card"]; !reflect.DeepEqual(got, map[string]any{"status": "ok"}) {
		t.Fatalf("/status?doctor=1 payments_card = %v", got)
	}
	if _, ok := status(d, "")["payments_card"]; ok {
		t.Fatal("a plain /status made the cards")
	}
	for _, bad := range []uint64{0, math.MaxInt64 + 1, math.MaxUint64} {
		w.setPrice("x", bad)
		c := d.paymentsCardCheck()
		if c == nil || c.Status != "fail" || c.Reason != "payments.price_invalid" || c.Args["name"] != "x" {
			t.Fatalf("price %d: %+v", bad, c)
		}
	}
	w.setPrice("x", 2)
	// A module that offers a second price list: the cards cannot be made.
	d.mu.Lock()
	d.modules = append(d.modules, &cardModule{ext: []map[string]any{{"uri": module.ExtPriceURI,
		"params": map[string]any{"network": "hub:" + d.AID(), "asset": "credit",
			"prices": []any{map[string]any{"skillId": "translate", "amount": "1"}}}}}})
	d.mu.Unlock()
	c := d.paymentsCardCheck()
	if c == nil || c.Status != "fail" || c.Reason != "payments.card_failed" || c.Args["code"] != "general_build" {
		t.Fatalf("a broken card: %+v", c)
	}
	// The public card still names no price.
	if pub, _, err := d.networkCard(d.config().HubURL); err != nil || bytes.Contains(pub, []byte(`"prices"`)) {
		t.Fatalf("public card (%v):\n%s", err, pub)
	}
}

// The grant's pay flag (core X-SS-8) is the payment module's answer.
func TestPricesSomethingAsksThePaymentModule(t *testing.T) {
	d, w := extNode(t, []string{"echo"}, nil, nil)
	if d.pricesSomething() {
		t.Fatal("a free node prices something")
	}
	w.setPrice("echo", 5)
	if got := d.pricesSomething(); got != paymentsCompiled() {
		t.Fatalf("pricesSomething = %v with payments compiled %v", got, paymentsCompiled())
	}
}

// W4 PAY-10 7: the per-sender limit is judged before anything is made. A
// sender over it is answered busy with no card built, signed or checked;
// another sender is answered as before.
func TestABusyCardRequestBuildsNothing(t *testing.T) {
	if !paymentsCompiled() {
		t.Skip("no payment module in this build: a priced capability is published nowhere")
	}
	d, _ := extNode(t, []string{"translate"}, nil, map[string]uint64{"translate": 3})
	const sender, other = "did:anet:bsender", "did:anet:bother"
	for i := range cardRequestsPerWindow {
		if got := answerOf(t, d, sender); len(got.Card) == 0 {
			t.Fatalf("request %d: %+v", i+1, got)
		}
	}
	built := d.extCard.builds.Load()
	if got := answerOf(t, d, sender); got.Error != delegation.CardBusy {
		t.Fatalf("the seventh request: %+v", got)
	}
	if n := d.extCard.builds.Load(); n != built {
		t.Fatalf("a busy request built the cards (%d times)", n-built)
	}
	if got := answerOf(t, d, other); len(got.Card) == 0 || d.extCard.builds.Load() != built+1 {
		t.Fatalf("another sender: %+v", got)
	}
}

// W4 PAY-10 7: "this variant is the public card" is kept with what it was
// made from. The same content asked again is not built again; a change to
// what the variant says is.
func TestTheSameConclusionIsKept(t *testing.T) {
	d, w := extNode(t, []string{"face.detect"}, []string{"face.compare"}, nil)
	if got := answerOf(t, d, "did:anet:bstranger"); got.Error != delegation.CardNotConfigured {
		t.Fatalf("a stranger: %+v", got)
	}
	built := d.extCard.templates.Load()
	for range 3 {
		if got := answerOf(t, d, "did:anet:bstranger2"); got.Error != delegation.CardNotConfigured {
			t.Fatalf("again: %+v", got)
		}
	}
	// The list variant is a card: kept the way a signed card is.
	if n := d.extCard.templates.Load(); n != built {
		t.Fatalf("the same content was built %d more times", n-built)
	}
	if !paymentsCompiled() {
		return
	}
	w.setPrice("face.detect", 4)
	got := answerOf(t, d, "did:anet:bstranger")
	if len(got.Card) == 0 || d.extCard.templates.Load() == built {
		t.Fatalf("a new price was not built: %+v", got)
	}
	if p, err := netcard.PricesOf(got.Card); err != nil || p["face.detect"] != 4 {
		t.Fatalf("prices %v (%v)", p, err)
	}
}
