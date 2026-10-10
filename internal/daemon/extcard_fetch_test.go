package daemon

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/netcard"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// extFixture is a provider's extended card as a requester receives it: the
// provider's AID and key history, the card, and its public card's seq.
type extFixture struct {
	aid    string
	kel    []identity.SignedEvent
	card   []byte
	pubSeq uint64
	prov   *Daemon
}

// newExtFixture makes the general extended card of a provider serving
// translate (3 credits) and chat.free publicly.
func newExtFixture(t *testing.T) extFixture {
	t.Helper()
	if !paymentsCompiled() {
		t.Skip("no payment module in this build: a priced capability is published nowhere")
	}
	d, _ := extNode(t, []string{"translate", "chat.free"}, nil, map[string]uint64{"translate": 3})
	general, _ := d.extendedCards()
	if len(general.Card) == 0 {
		t.Fatalf("no general extended card: %+v", general)
	}
	_, seq, err := d.networkCard(d.config().HubURL)
	if err != nil {
		t.Fatal(err)
	}
	return extFixture{aid: d.AID(), kel: d.self.KEL(), card: general.Card, pubSeq: seq, prov: d}
}

// resignCard edits a card's unsigned form and signs it again under
// signer: a card its signer really made, saying something else.
func resignCard(t *testing.T, card []byte, signer identity.Signer, edit func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(card, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "signatures")
	edit(m)
	unsigned, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := a2acard.SignWithSnapshot(unsigned, signer, "")
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// extensionsOf is a card map's capabilities.extensions.
func extensionsOf(m map[string]any) []any {
	caps, _ := m["capabilities"].(map[string]any)
	l, _ := caps["extensions"].([]any)
	return l
}

func setExtensions(m map[string]any, l []any) {
	caps, _ := m["capabilities"].(map[string]any)
	caps["extensions"] = l
}

// PAY-C13, check by check: a card that passes, and one failing each check,
// every failure the same bad_signature for the caller.
func TestVerifyExtCard(t *testing.T) {
	f := newExtFixture(t)
	now := f.prov.nowMS()
	c, err := verifyExtCard(f.card, f.aid, f.kel, f.pubSeq, now)
	if err != nil {
		t.Fatalf("the provider's own card: %v", err)
	}
	if c.PublicSeq != f.pubSeq || c.Prices["translate"] != 3 || len(c.Prices) != 1 || c.Charge != module.ChargePartial ||
		strings.Join(c.Skills, ",") != "chat.free,translate" || !bytes.Equal(c.Bytes, f.card) || c.FetchedAt != now {
		t.Fatalf("the verified card: %+v", c)
	}

	other, err := identitytest.Agent()
	if err != nil {
		t.Fatal(err)
	}
	signer := f.prov.self.Snapshot()
	for _, tc := range []struct {
		name  string
		card  func() []byte
		aid   string
		kel   []identity.SignedEvent
		known uint64
	}{
		{name: "another agent's key history", card: func() []byte { return f.card }, kel: other.KEL()},
		{name: "asked of another agent", card: func() []byte { return f.card }, aid: other.AID()},
		{name: "a seq below the public card's", card: func() []byte { return f.card }, known: f.pubSeq + 1},
		{name: "a byte changed", card: func() []byte { return bytes.Replace(f.card, []byte(`"translate"`), []byte(`"translata"`), 1) }},
		{name: "too large", card: func() []byte {
			return append(append([]byte(nil), f.card...), bytes.Repeat([]byte(" "), delegation.MaxCardBytes)...)
		}},
		// A default value added after signing: the signature still holds
		// (A2A §8.4.1 strips it), the publish form does not.
		{name: "not in publish form", card: func() []byte {
			return bytes.Replace(f.card, []byte(`"tags":[`), []byte(`"examples":[],"tags":[`), 1)
		}},
		{name: "a price list that does not hold", card: func() []byte {
			return resignCard(t, f.card, signer, func(m map[string]any) {
				for _, e := range extensionsOf(m) {
					if e := e.(map[string]any); e["uri"] == module.ExtPriceURI {
						e["params"].(map[string]any)["asset"] = "usd"
					}
				}
			})
		}},
		{name: "a2a-x402 declared without a price", card: func() []byte {
			return resignCard(t, f.card, signer, func(m map[string]any) {
				var keep []any
				for _, e := range extensionsOf(m) {
					if e.(map[string]any)["uri"] != module.ExtPriceURI {
						keep = append(keep, e)
					}
				}
				setExtensions(m, keep)
			})
		}},
		{name: "prices without an a2a-x402 declaration", card: func() []byte {
			return resignCard(t, f.card, signer, func(m map[string]any) {
				var keep []any
				for _, e := range extensionsOf(m) {
					if e.(map[string]any)["uri"] != module.ExtX402URI {
						keep = append(keep, e)
					}
				}
				setExtensions(m, keep)
			})
		}},
		{name: "required with a free skill", card: func() []byte {
			return resignCard(t, f.card, signer, func(m map[string]any) {
				for _, e := range extensionsOf(m) {
					if e := e.(map[string]any); e["uri"] == module.ExtX402URI {
						e["required"] = true
					}
				}
			})
		}},
	} {
		aid, kel := tc.aid, tc.kel
		if aid == "" {
			aid = f.aid
		}
		if kel == nil {
			kel = f.kel
		}
		known := tc.known
		if known == 0 {
			known = f.pubSeq
		}
		if c, err := verifyExtCard(tc.card(), aid, kel, known, now); err == nil {
			t.Errorf("%s: taken: %+v", tc.name, c)
		}
	}
}

// A card is only ever the head key's (SI-36): after the provider rotates,
// a card signed under the key before it is refused, and the next card the
// provider gives is signed under the new key and taken.
func TestAnExtendedCardUnderARetiredKeyIsRefused(t *testing.T) {
	f := newExtFixture(t)
	if err := identitytest.Rotate(f.prov.self, f.prov.nowMS()); err != nil {
		t.Fatal(err)
	}
	rotated := f.prov.self.KEL()
	if _, err := verifyExtCard(f.card, f.aid, rotated, f.pubSeq, f.prov.nowMS()); err == nil {
		t.Fatal("a card under a retired key was taken")
	}
	general, _ := f.prov.extendedCards()
	if _, err := verifyExtCard(general.Card, f.aid, rotated, f.pubSeq, f.prov.nowMS()); err != nil {
		t.Fatalf("the card signed again after the rotation: %v", err)
	}
	if _, err := verifyExtCard(general.Card, f.aid, f.kel, f.pubSeq, f.prov.nowMS()); err == nil {
		t.Fatal("a card under a key the requester's history does not have yet was taken")
	}
}

// PAY-C14: fresh is at most a day old, not below the public card's seq, and
// under the key the agent has now.
func TestExtCardFreshness(t *testing.T) {
	const day = interactions.ExtCardLife
	now := uint64(10 * day)
	row := interactions.PeerExtCard{FetchedAt: now - 1000, PublicSeq: 5, KeyStateSeq: 2}
	for _, tc := range []struct {
		name       string
		row        interactions.PeerExtCard
		known, tip uint64
		fresh      bool
	}{
		{"as fetched", row, 5, 2, true},
		{"a day old", interactions.PeerExtCard{FetchedAt: now - day, PublicSeq: 5, KeyStateSeq: 2}, 5, 2, true},
		{"older than a day", interactions.PeerExtCard{FetchedAt: now - day - 1, PublicSeq: 5, KeyStateSeq: 2}, 5, 2, false},
		{"a newer public card", row, 6, 2, false},
		{"an older public card known", row, 4, 2, true},
		{"the agent rotated", row, 5, 3, false},
		{"fetched after now (the clock went back)", interactions.PeerExtCard{FetchedAt: now + 1, PublicSeq: 5, KeyStateSeq: 2}, 5, 2, false},
	} {
		if got := extCardFresh(tc.row, now, tc.known, tc.tip); got != tc.fresh {
			t.Errorf("%s: fresh %t, want %t", tc.name, got, tc.fresh)
		}
	}
}

// ENG-O8b to PAY-D2 step 3: a request taken is sent; a wait code names
// itself; anything else that kept it from the agent is the hub that
// cannot be reached.
func TestCardRequestWhy(t *testing.T) {
	for _, tc := range []struct {
		res  OneShotResult
		why  string
		sent bool
	}{
		{OneShotResult{Handed: true}, "", true},
		{OneShotResult{}, priceWhyHubUnreachable, false},
		{OneShotResult{HubReason: "notice_stale"}, "notice_stale", false},
		{OneShotResult{HubReason: "session_unavailable"}, "session_unavailable", false},
		{OneShotResult{Handed: true, HubReason: "strangers_busy"}, "strangers_busy", false},
		{OneShotResult{Handed: true, HubReason: "rate_limited"}, "rate_limited", false},
		{OneShotResult{Handed: true, HubReason: "recipient_unknown"}, priceWhyHubUnreachable, false},
	} {
		why, sent := cardRequestWhy(tc.res)
		if why != tc.why || sent != tc.sent {
			t.Errorf("%+v: (%q, %t), want (%q, %t)", tc.res, why, sent, tc.why, tc.sent)
		}
		if !sent {
			s := a2ashape.Sentence("card_price_unavailable", a2ashape.English, a2ashape.Args{"amount": 3, "why": why, "task": "t_1"})
			if strings.Contains(s, "—") || strings.Contains(s, "{") {
				t.Errorf("%q: the sentence has no phrase for it: %s", why, s)
			}
		}
	}
}

// HPP §4.7 and CORE-K23 rule 4: each outcome's code, A2A error and wait.
func TestExtendedRefusals(t *testing.T) {
	for _, tc := range []struct {
		res        extFetchResult
		code, name string
		rpc, rest  int
		retry      int
	}{
		{extFetchResult{Outcome: extNotOffered}, "extended_card_not_offered", "UnsupportedOperationError", -32004, 400, 0},
		{extFetchResult{Outcome: extNotConfigured}, "extended_card_not_configured", "ExtendedAgentCardNotConfiguredError", -32007, 400, 0},
		{extFetchResult{Outcome: extBusy}, "extended_card_unavailable", "InternalError", -32603, 503, 60},
		{extFetchResult{Outcome: extNoAnswer}, "extended_card_unavailable", "InternalError", -32603, 503, 15},
		{extFetchResult{Outcome: extNotSent, Why: priceWhyHubUnreachable}, "extended_card_unavailable", "InternalError", -32603, 503, 15},
		{extFetchResult{Outcome: extBadSignature}, "extended_card_unavailable", "InvalidAgentResponseError", -32006, 500, 0},
	} {
		ref := extendedRefusal(tc.res)
		if ref == nil {
			t.Fatalf("%+v: no refusal", tc.res)
		}
		e := ref.A2A(a2ashape.English)
		if ref.Code() != tc.code || e.Name != tc.name || e.RPCCode != tc.rpc || e.HTTPStatus != tc.rest || e.RetryAfter != tc.retry {
			t.Errorf("%+v: %s %s %d/%d retry %d", tc.res, ref.Code(), e.Name, e.RPCCode, e.HTTPStatus, e.RetryAfter)
		}
	}
	if extendedRefusal(extFetchResult{Outcome: extGot}) != nil {
		t.Error("a card is not refused")
	}
}

// CORE-K23 rule 4, the control plane's (and MCP's) half: the sentence of
// priceNote is the local A2A error's message, word for word, in either
// language; what was had shows every skill's price and nothing else.
func TestApplyExtended(t *testing.T) {
	for _, lang := range []a2ashape.Language{a2ashape.English, a2ashape.Chinese} {
		for _, o := range []extOutcome{extBusy, extNoAnswer, extNotSent, extBadSignature} {
			var ra module.RemoteAgent
			res := extFetchResult{Outcome: o}
			applyExtended(&ra, res, lang)
			if want := extendedRefusal(res).A2A(lang).Message; ra.PriceNote != want || want == "" {
				t.Errorf("outcome %d, %v: priceNote %q, A2A message %q", o, lang, ra.PriceNote, want)
			}
			if ra.Prices != nil || ra.Charge != "" || ra.PricesFetchedAt != "" {
				t.Errorf("outcome %d: %+v", o, ra)
			}
		}
		for _, o := range []extOutcome{extNotOffered, extNotConfigured} {
			ra := module.RemoteAgent{Charge: module.ChargeFree}
			applyExtended(&ra, extFetchResult{Outcome: o}, lang)
			if ra.PriceNote != "" || ra.Prices != nil || ra.Charge != module.ChargeFree {
				t.Errorf("outcome %d: %+v", o, ra)
			}
		}
	}
	at := time.Date(2026, 10, 1, 14, 5, 0, 123e6, time.UTC)
	ra := module.RemoteAgent{Charge: module.ChargeFree}
	applyExtended(&ra, extFetchResult{Outcome: extGot, Card: &extCard{Skills: []string{"translate", "summarize", "chat"},
		Prices: map[string]uint64{"translate": 3, "summarize": 5}, Charge: module.ChargePartial, FetchedAt: uint64(at.UnixMilli())}}, a2ashape.English)
	got, _ := json.Marshal(ra.Prices)
	if ra.Charge != module.ChargePartial || ra.PriceNote != "" || ra.PricesFetchedAt != "2026-10-01T14:05:00.123Z" ||
		string(got) != `[{"skill":"translate","amount":"3","unit":"credits"},{"skill":"summarize","amount":"5","unit":"credits"},{"skill":"chat","amount":"0","unit":"credits"}]` {
		t.Errorf("the card had: %+v %s", ra, got)
	}
	if _, err := time.Parse(time.RFC3339Nano, ra.PricesFetchedAt); err != nil {
		t.Errorf("pricesFetchedAt %q: %v", ra.PricesFetchedAt, err)
	}
}

// netcardExtendedFor is a card's bytes with capabilities.extendedAgentCard
// and the a2a-x402 declaration as given, for the pure checks.
func cardWithDeclaration(t *testing.T, extended, declared, required bool) []byte {
	t.Helper()
	ctl, err := identitytest.Agent()
	if err != nil {
		t.Fatal(err)
	}
	in := netcard.Input{AID: ctl.AID(), Name: "x", HubURL: "https://hub.example", Seq: 1, IssuedAtMs: 1, NotBeforeMs: 1,
		Skills: []netcard.Skill{{ID: "a", Name: "a", Description: "a"}}}
	if declared {
		in.Extensions = []map[string]any{a2acard.ExtensionDecl(module.ExtX402URI, "x402", required, nil)}
	}
	card, err := netcard.Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if !extended {
		card = bytes.Replace(card, []byte(`"extendedAgentCard":true,`), nil, 1)
	}
	return card
}

// pendingFetch remembers an exchange with aid, as fetchExtendedCard does
// before it sends the request.
func pendingFetch(d *Daemon, aid string) *cardFetch {
	now := d.nowMS()
	f := &cardFetch{aid: aid, ix: newCardIX(), exp: now + cardLifeMS, done: make(chan struct{})}
	d.extFetch.mu.Lock()
	d.extFetch.addPendingLocked(f, now)
	d.extFetch.mu.Unlock()
	return f
}

// engine §4.5 and PAY-C13: an answer is taken only for an exchange this
// node has pending with its sender, once, before the request's exp; its
// card is checked, kept and handed to the caller waiting for it, and a card
// that does not pass is dropped and reported as bad_signature.
func TestACardAnswerIsTakenOnlyForItsExchange(t *testing.T) {
	f := newExtFixture(t)
	req := newCardDaemon(t, "Req", "Asks for cards.")
	good := &delegation.CardResp{Card: f.card}
	kept := func() bool {
		_, ok, err := req.ix.PeerExtCard(f.aid)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if req.cardAnswerArrived(f.aid, newCardIX(), f.kel, good) || kept() {
		t.Fatal("an answer to no exchange was taken")
	}
	p := pendingFetch(req, f.aid)
	if req.cardAnswerArrived("did:anet:bsomeoneelse", p.ix, f.kel, good) || kept() {
		t.Fatal("another agent's answer to the exchange was taken")
	}
	if !req.cardAnswerArrived(f.aid, p.ix, f.kel, good) || !kept() {
		t.Fatal("the answer was not taken")
	}
	select {
	case <-p.done:
	default:
		t.Fatal("the waiting caller was not told")
	}
	if p.res.Outcome != extGot || p.res.Card.Prices["translate"] != 3 {
		t.Fatalf("the caller got %+v", p.res)
	}
	if req.cardAnswerArrived(f.aid, p.ix, f.kel, good) {
		t.Fatal("a second answer to one exchange was taken")
	}
	row, _, _ := req.ix.PeerExtCard(f.aid)
	if row.PublicSeq != f.pubSeq || row.Variant != "general" || !bytes.Equal(row.Card, f.card) {
		t.Fatalf("kept %+v", row)
	}

	// An exchange past its exp.
	late := pendingFetch(req, f.aid)
	late.exp = req.nowMS() - 1
	if req.cardAnswerArrived(f.aid, late.ix, f.kel, good) {
		t.Fatal("an answer after the request's exp was taken")
	}

	// A card that does not verify: dropped, bad_signature, nothing kept.
	req2 := newCardDaemon(t, "Req2", "Asks for cards.")
	bad := pendingFetch(req2, f.aid)
	tampered := bytes.Replace(f.card, []byte(`"amount":"3"`), []byte(`"amount":"1"`), 1)
	if bytes.Equal(tampered, f.card) {
		t.Fatal("the fixture has no price to change")
	}
	if req2.cardAnswerArrived(f.aid, bad.ix, f.kel, &delegation.CardResp{Card: tampered}) {
		t.Fatal("a card whose price was changed was taken")
	}
	if bad.res.Outcome != extBadSignature || bad.res.Why != priceWhyBadSignature {
		t.Fatalf("the caller got %+v", bad.res)
	}
	if _, ok, _ := req2.ix.PeerExtCard(f.aid); ok {
		t.Fatal("a card that did not verify was kept")
	}

	// not_configured and busy are taken, and told.
	for _, c := range []struct {
		err  string
		want extOutcome
	}{{delegation.CardNotConfigured, extNotConfigured}, {delegation.CardBusy, extBusy}} {
		p := pendingFetch(req2, f.aid)
		if !req2.cardAnswerArrived(f.aid, p.ix, f.kel, &delegation.CardResp{Error: c.err}) || p.res.Outcome != c.want {
			t.Fatalf("%s: %+v", c.err, p.res)
		}
	}
}

// FuzzVerifyExtCard: whatever an answer's card is, verifyExtCard returns a
// card or an error and never panics; a card it takes is one the provider's
// key signed, for that provider, with a price list that holds.
func FuzzVerifyExtCard(f *testing.F) {
	ctl, err := identitytest.Agent()
	if err != nil {
		f.Fatal(err)
	}
	in := netcard.Input{AID: ctl.AID(), Name: "Fuzz", HubURL: "https://hub.example", Seq: 2, IssuedAtMs: 1, NotBeforeMs: 1,
		Skills: []netcard.Skill{{ID: "a", Name: "a", Description: "a"}, {ID: "b", Name: "b", Description: "b"}}}
	exts := []map[string]any{
		a2acard.ExtensionDecl(module.ExtX402URI, "x402", false, nil),
		a2acard.ExtensionDecl(module.ExtPriceURI, "prices", false, map[string]any{"network": "hub:" + ctl.AID(), "asset": "credit",
			"prices": []any{map[string]any{"skillId": "a", "amount": "3"}}}),
	}
	unsigned, _, err := netcard.BuildExtended(in, module.VariantGeneral, exts)
	if err != nil {
		f.Fatal(err)
	}
	card, err := a2acard.SignWithSnapshot(unsigned, ctl, "")
	if err != nil {
		f.Fatal(err)
	}
	kel := ctl.KEL()
	f.Add(card)
	f.Add([]byte(`{}`))
	f.Add(bytes.Replace(card, []byte(`"3"`), []byte(`"0"`), 1))
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := verifyExtCard(b, ctl.AID(), kel, 2, 2_000_000_000_000)
		if err != nil {
			return
		}
		if c.AID != ctl.AID() || c.PublicSeq < 2 || len(c.Skills) == 0 {
			t.Fatalf("taken: %+v", c)
		}
		for id, p := range c.Prices {
			if !slicesContains(c.Skills, id) || !netcard.ValidPrice(p) {
				t.Fatalf("a price of %q (%d) the card does not hold", id, p)
			}
		}
		if _, err := a2acard.Verify(b, singleKEL(ctl.AID(), kel), 2_000_000_000_000); err != nil {
			t.Fatalf("taken without a valid signature: %v", err)
		}
	})
}

func slicesContains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
