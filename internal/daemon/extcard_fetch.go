package daemon

// extcard_fetch.go is the requester side of the extended card (HPP §4.3.2,
// §4.3.3; core CORE-K23; engine §2.12, §4.5): asking an agent end to end
// for the AgentCard it signs for this node (anet.card.get/1), checking the
// answer (anet.card/1), keeping it for a day (peer_ext_card), and what the
// three surfaces — the CLI, MCP and the local A2A interface — show of it.
//
// When this node asks, and only then (PAY-C10):
//
//  1. a card is shown (anet card, MCP get_agent_card, the local A2A
//     GetExtendedAgentCard, /agents/card), the cached card is not fresh,
//     and CORE-K23 rule 2 holds: the agent's public card declares an
//     extended card and either declares a2a-x402 or this node holds a
//     contact capability the agent issued it (it is on the agent's allow
//     or trust list). Otherwise the agent is not contacted at all;
//  2. a payment decision needs the price the agent signed for a skill and
//     the cache has no fresh one (PAY-D2 step 3);
//  3. a quote is above the cached signed price and the cache is older than
//     a minute: asked again, once (PAY-D2 step 4).
//
// A list (anet find, MCP list_agents, the local A2A agent list) never
// asks: its charge column is read from the public card.
//
// A request goes once through the one-shot sender (ENG-O8, ENG-O9) with a
// new card-<32 hex> exchange id, which is remembered here (pending) until
// the request's outer exp. The caller waits at most ten seconds, the
// one-shot sender's single resend included (ENG-O8a). An answer that comes
// later is checked and kept all the same (PAY-C12, ENG-O11), and wakes the
// payment decisions that waited for it (PAY-D5).
//
// An answer is taken only for an exchange this node has pending with that
// agent. Its card must verify under the head of the agent's key history as
// the receive pipeline resolved it (SI-36), be signed by the agent, carry
// a seq not below the agent's public card this node knows, be in publish
// form, and state a price list that holds, with an a2a-x402 declaration
// that agrees with it (PAY-C13). It never enters the public card's
// high-water mark: the provider signs its extended card under its public
// card's seq, so the same seq with another payload would be taken for a
// fork (PAY-C14).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/netcard"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// The waits of the extended card (HPP §0.4, §4.7). Code constants.
const (
	// priceRefetchAge is how old a cached card must be before a quote above
	// its price asks again (PAY-D2 step 4).
	priceRefetchAge = time.Minute
	// cardBusyRetry and cardUnavailableRetry are the Retry-After of an
	// extended card the agent was too busy to give (it limits each sender
	// per minute) and of one that did not come in time or could not be
	// asked for (a late answer is still kept).
	cardBusyRetry        = 60 * time.Second
	cardUnavailableRetry = 15 * time.Second
	// cardPendingLimit bounds the exchanges remembered at once; past it the
	// one that expires first is forgotten.
	cardPendingLimit = 256
)

// Counters of the requester side.
const (
	cntCardFetchSent    = "card-fetch-sent"
	cntCardFetchLate    = "card-fetch-late"
	cntCardBadSignature = "card-bad-signature"
	cntCardCacheWrite   = "card-cache-write-failed"
)

// extOutcome is how asking for an extended card ended.
type extOutcome uint8

const (
	// extNotOffered: CORE-K23 rule 2 does not hold; nobody was asked.
	extNotOffered extOutcome = iota + 1
	// extGot: a verified card, from the cache or just fetched.
	extGot
	// extNotConfigured: the agent's card for this node is its public card.
	extNotConfigured
	// extBusy: the agent answered busy.
	extBusy
	// extNoAnswer: the request was handed over and no answer came in time.
	extNoAnswer
	// extNotSent: the request did not leave this node, or its hub refused
	// it; Why says why.
	extNotSent
	// extBadSignature: the answer's card did not pass verifyExtCard.
	extBadSignature
)

// extFetchResult is the outcome of asking for aid's extended card.
type extFetchResult struct {
	Outcome extOutcome
	// Card is the verified card when Outcome is extGot.
	Card *extCard
	// Why is the short reason of card_price_unavailable (ux D1; PAY-D2
	// step 3): timeout, not_offered, bad_signature, hub_unreachable or the
	// wait code that held the request back.
	Why string
}

// extCard is an extended card that passed verifyExtCard.
type extCard struct {
	AID string
	// Bytes are the card exactly as signed.
	Bytes []byte
	// PublicSeq is the card's anet-card seq; KeyStateSeq the key state it
	// is signed under.
	PublicSeq, KeyStateSeq uint64
	// Skills are the card's skill ids in its order; Prices the price list.
	Skills []string
	Prices map[string]uint64
	// Charge is what its a2a-x402 declaration says (PAY-C15).
	Charge string
	// FetchedAt is when it was verified (unix ms).
	FetchedAt uint64
}

// verifyExtCard is PAY-C13: card, the anet.card/1 answer of aid, checked
// against kel, aid's key history as this node has it (the receive
// pipeline's steps 6 and 7), and knownPublicSeq, the seq of aid's public
// card this node last admitted. Any failure is the bad_signature of ux D1.
// A pure function: the time is a parameter, and nothing is written — in
// particular not the public card's high-water mark (PAY-C14).
func verifyExtCard(card []byte, aid string, kel []identity.SignedEvent, knownPublicSeq, now uint64) (*extCard, error) {
	if len(card) == 0 || len(card) > delegation.MaxCardBytes {
		return nil, fmt.Errorf("an extended card of %d bytes (at most %d)", len(card), delegation.MaxCardBytes)
	}
	if len(kel) == 0 {
		return nil, errors.New("no key history to check the extended card against")
	}
	if err := a2acard.CheckPublishForm(card); err != nil {
		return nil, err
	}
	v, err := a2acard.Verify(card, singleKEL(aid, kel), now)
	if err != nil {
		return nil, err
	}
	if v.AID != aid {
		return nil, fmt.Errorf("the extended card is signed by %s, not %s", v.AID, aid)
	}
	tip, err := kelTipSeq(kel)
	if err != nil {
		return nil, err
	}
	if v.KeyStateSeq != tip {
		// a2acard.Verify takes only a current key state, which in a KEL
		// of rotations is its head; said again here because a card is
		// only ever the head's (SI-36).
		return nil, fmt.Errorf("the extended card is signed under key state %d, not the head %d", v.KeyStateSeq, tip)
	}
	if v.Seq < knownPublicSeq {
		return nil, fmt.Errorf("the extended card's seq %d is below the public card's %d", v.Seq, knownPublicSeq)
	}
	prices, err := netcard.PricesOf(card)
	if err != nil {
		return nil, err
	}
	declared, required, err := netcard.X402Declaration(card)
	if err != nil {
		return nil, err
	}
	c := &extCard{AID: aid, Bytes: append([]byte(nil), card...), PublicSeq: v.Seq, KeyStateSeq: v.KeyStateSeq,
		Prices: prices, Charge: chargeOfDeclaration(declared, required), FetchedAt: now}
	all := len(v.Skills) > 0
	for _, sk := range v.Skills {
		c.Skills = append(c.Skills, sk.ID)
		if _, ok := prices[sk.ID]; !ok {
			all = false
		}
	}
	// The declaration says what the prices say (HPP §2.6): declared when a
	// skill has a price, required when every skill has one. A card that
	// says otherwise would show a charge its own price list contradicts.
	if declared != (len(prices) > 0) || (declared && required != all) {
		return nil, fmt.Errorf("the extended card's a2a-x402 declaration (declared %t, required %t) does not match its %d prices for %d skills",
			declared, required, len(prices), len(v.Skills))
	}
	return c, nil
}

// kelTipSeq is the seq of the head of a key history.
func kelTipSeq(kel []identity.SignedEvent) (uint64, error) {
	states, err := identity.Replay(kel)
	if err != nil {
		return 0, err
	}
	if len(states) == 0 {
		return 0, errors.New("an empty key history")
	}
	return uint64(len(states) - 1), nil
}

// chargeOfDeclaration is PAY-C15's table: no a2a-x402 declaration is free,
// a declaration without required is partial, a required one is paid.
func chargeOfDeclaration(declared, required bool) string {
	switch {
	case !declared:
		return module.ChargeFree
	case required:
		return module.ChargePaid
	}
	return module.ChargePartial
}

// chargeOf is the charge of a card's bytes; "" for a card whose
// declaration cannot be read (declared twice, not a card).
func chargeOf(card []byte) string {
	declared, required, err := netcard.X402Declaration(card)
	if err != nil {
		return ""
	}
	return chargeOfDeclaration(declared, required)
}

// extCardFresh is PAY-C14's freshness: fetched at most a day ago (and not
// after now: a clock that stepped back asks again), with a seq not below
// the agent's public card this node knows, and signed under the head of
// the agent's key history as this node knows it now.
func extCardFresh(c interactions.PeerExtCard, now, knownPublicSeq, tipSeq uint64) bool {
	return c.FetchedAt <= now && now-c.FetchedAt <= interactions.ExtCardLife &&
		c.PublicSeq >= knownPublicSeq && c.KeyStateSeq == tipSeq
}

// extendedOffered is CORE-K23 rule 2 for aid, whose verified public card
// is card: the card declares an extended card, and it declares a2a-x402 or
// this node holds a valid contact capability aid issued (a caps_held row
// of aid's, contact class, for any interaction — grant key 4 absent) —
// which is to say this node is on aid's allow or trust list (SS SSR-D5).
// Both are facts this node has without asking anyone.
func (d *Daemon) extendedOffered(aid string, card []byte, now uint64) bool {
	if ext, err := netcard.DeclaresExtended(card); err != nil || !ext {
		return false
	}
	if declared, _, err := netcard.X402Declaration(card); err == nil && declared {
		return true
	}
	return d.holdsContactCap(aid, now)
}

// holdsContactCap reports whether caps_held has a contact-class peer
// capability aid issued that this node may still use.
func (d *Daemon) holdsContactCap(aid string, now uint64) bool {
	if d.ix == nil {
		return false
	}
	held, err := d.ix.HeldCaps(aid)
	if err != nil {
		return false
	}
	for _, h := range held {
		if h.Issuer == aid && h.Scope == interactions.CapScopePeer && h.IX == "" &&
			h.Class == seal.CapClassContact && h.InvalidAt == 0 && h.CapExp > now {
			return true
		}
	}
	return false
}

// knownPublicSeq is the seq of aid's public card this node last admitted:
// in memory, or on aid's peer_identity row.
func (d *Daemon) knownPublicSeq(aid string) uint64 {
	var seq uint64
	if m, ok := cardMarks.get(d.AID(), aid); ok {
		seq = m.Seq
	}
	if d.ix != nil {
		if row, err := d.ix.PeerIdentity(aid); err == nil && row.CardSeq > seq {
			seq = row.CardSeq
		}
	}
	return seq
}

// cachedExtCard is aid's kept extended card when it is fresh against kel,
// aid's key history as this node knows it now.
func (d *Daemon) cachedExtCard(aid string, kel []identity.SignedEvent, now uint64) (*extCard, bool) {
	if d.ix == nil || len(kel) == 0 {
		return nil, false
	}
	row, found, err := d.ix.PeerExtCard(aid)
	if err != nil || !found {
		return nil, false
	}
	tip, err := kelTipSeq(kel)
	if err != nil || !extCardFresh(row, now, d.knownPublicSeq(aid), tip) {
		return nil, false
	}
	// Read again as it was checked: the skills, in order, and the charge.
	c, err := verifyExtCard(row.Card, aid, kel, 0, now)
	if err != nil {
		return nil, false
	}
	c.FetchedAt = row.FetchedAt
	return c, true
}

// extFetchState is the exchanges this node has asked for and not had an
// answer to (pending, by exchange id, until the request's exp) and the
// fetch a caller is waiting on for each agent (inflight), which a second
// caller joins rather than ask again.
type extFetchState struct {
	mu       sync.Mutex
	pending  map[string]*cardFetch
	inflight map[string]*cardFetch
}

// cardFetch is one anet.card.get/1 this node sent.
type cardFetch struct {
	aid string
	ix  string
	exp uint64 // the request's outer exp (unix ms)
	// public are the agent's public skills when the fetch was asked for a
	// display (to tell a list variant from the general one); nil otherwise.
	public []string
	done   chan struct{}
	res    extFetchResult // set before done is closed
	closed bool
	// waiters is how many callers wait for the answer now; an answer that
	// comes when there are none is late (PAY-C12).
	waiters int
}

// finishLocked gives f its result, once. mu is held.
func (f *cardFetch) finishLocked(res extFetchResult) {
	if f.closed {
		return
	}
	f.res, f.closed = res, true
	close(f.done)
}

// newCardIX is a new card-<32 lowercase hex> exchange id (core §2.4.1).
func newCardIX() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "card-" + hex.EncodeToString(b[:])
}

// addPendingLocked remembers f until its exp. mu is held.
func (s *extFetchState) addPendingLocked(f *cardFetch, now uint64) {
	if s.pending == nil {
		s.pending = map[string]*cardFetch{}
	}
	for k, p := range s.pending {
		if p.exp <= now {
			delete(s.pending, k)
		}
	}
	for len(s.pending) >= cardPendingLimit {
		var first string
		var exp uint64
		for k, p := range s.pending {
			if first == "" || p.exp < exp {
				first, exp = k, p.exp
			}
		}
		delete(s.pending, first)
	}
	s.pending[f.ix] = f
}

// takePending returns and forgets the pending exchange ix of aid, when
// there is one that has not expired.
func (s *extFetchState) takePending(aid, ix string, now uint64) *cardFetch {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.pending[ix]
	if f == nil || f.aid != aid {
		return nil
	}
	delete(s.pending, ix)
	if f.exp <= now {
		return nil
	}
	return f
}

// fetchExtendedCard asks aid for its extended card and waits for the
// answer: at most cardWaitMS from now, or less when ctx says so. A caller
// already waiting for aid's card is joined. public, when not nil, are the
// skills of aid's public card (a display's fetch).
func (d *Daemon) fetchExtendedCard(ctx context.Context, aid string, public []string) extFetchResult {
	start := d.nowMS()
	// The wait is the caller's, on the wall clock; the exchange's exp is
	// on the envelope's clock, as the request's outer exp is.
	deadline := time.Now().Add(time.Duration(cardWaitMS) * time.Millisecond)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	s := &d.extFetch
	s.mu.Lock()
	if f := s.inflight[aid]; f != nil && !f.closed {
		s.mu.Unlock()
		return d.awaitCardFetch(ctx, f, deadline)
	}
	f := &cardFetch{aid: aid, ix: newCardIX(), exp: start + cardLifeMS, public: public, done: make(chan struct{})}
	s.addPendingLocked(f, start)
	if s.inflight == nil {
		s.inflight = map[string]*cardFetch{}
	}
	s.inflight[aid] = f
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.inflight[aid] == f {
			delete(s.inflight, aid)
		}
		s.mu.Unlock()
	}()

	body, err := (&delegation.CardGet{}).Marshal()
	if err != nil {
		return d.cardFetchFailed(f, extFetchResult{Outcome: extNotSent, Why: priceWhyHubUnreachable})
	}
	sctx, cancel := context.WithDeadline(ctx, deadline)
	res := d.oneShotSend(sctx, oneShotCardGet, aid, f.ix, body)
	cancel()
	if why, sent := cardRequestWhy(res); !sent {
		return d.cardFetchFailed(f, extFetchResult{Outcome: extNotSent, Why: why})
	}
	d.count(cntCardFetchSent)
	return d.awaitCardFetch(ctx, f, deadline)
}

// cardFetchFailed ends a fetch whose request did not go: nothing will
// answer it, so it is not pending any more.
func (d *Daemon) cardFetchFailed(f *cardFetch, res extFetchResult) extFetchResult {
	s := &d.extFetch
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[f.ix] == f {
		delete(s.pending, f.ix)
	}
	f.finishLocked(res)
	return f.res
}

// awaitCardFetch waits for f's answer until deadline. A fetch not answered
// by then is a timeout for this caller; its exchange stays pending, and a
// late answer is still checked and kept.
func (d *Daemon) awaitCardFetch(ctx context.Context, f *cardFetch, deadline time.Time) extFetchResult {
	s := &d.extFetch
	s.mu.Lock()
	f.waiters++
	s.mu.Unlock()
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case <-f.done:
	case <-t.C:
	case <-ctx.Done():
	case <-d.ctx.Done():
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f.waiters--
	if f.closed {
		return f.res
	}
	return extFetchResult{Outcome: extNoAnswer, Why: priceWhyTimeout}
}

// The short reasons of card_price_unavailable this file chooses (ux D1).
const (
	priceWhyTimeout        = "timeout"
	priceWhyNotOffered     = "not_offered"
	priceWhyBadSignature   = "bad_signature"
	priceWhyHubUnreachable = "hub_unreachable"
)

// cardRequestWhy reads a card request's OneShotResult (ENG-O8b): sent when
// the request was taken — by the agent's node directly, or by the hub (a
// hand-over with no hub refusal); otherwise the {why} of ux D1: the wait
// code the hub answered or that held the request back on this node, and
// "the hub cannot be reached" for anything else (PAY-D2 step 3).
func cardRequestWhy(r OneShotResult) (why string, sent bool) {
	if r.Handed && r.HubReason == "" {
		return "", true
	}
	if r.HubReason != "" && a2ashape.IsWaitCode(r.HubReason) {
		return r.HubReason, false
	}
	return priceWhyHubUnreachable, false
}

// cardAnswerArrived is receive step 9 for an anet.card/1 (engine §4.5;
// HPP PAY-C13, C14): taken only for an exchange this node has pending with
// from, a card checked (verifyExtCard) against kel — from's key history as
// steps 6 and 7 resolved it — and kept in peer_ext_card outside any receive
// transaction; then the payment decisions waiting for from's price are
// woken (PAY-D5). It reports whether the answer was taken: a card that
// does not pass is dropped, and the caller waiting for it is told
// bad_signature.
func (d *Daemon) cardAnswerArrived(from, ix string, kel []identity.SignedEvent, resp *delegation.CardResp) bool {
	now := d.nowMS()
	f := d.extFetch.takePending(from, ix, now)
	if f == nil || resp == nil {
		return false
	}
	finish := func(res extFetchResult) {
		d.extFetch.mu.Lock()
		late := f.waiters == 0
		f.finishLocked(res)
		d.extFetch.mu.Unlock()
		if late {
			d.count(cntCardFetchLate)
		}
	}
	switch resp.Error {
	case delegation.CardNotConfigured:
		finish(extFetchResult{Outcome: extNotConfigured, Why: priceWhyNotOffered})
		return true
	case delegation.CardBusy:
		// Busy counts as no answer for a payment (PAY-D2 step 3).
		finish(extFetchResult{Outcome: extBusy, Why: priceWhyTimeout})
		return true
	}
	c, err := verifyExtCard(resp.Card, from, kel, d.knownPublicSeq(from), now)
	if err != nil {
		d.count(cntCardBadSignature)
		log.Printf("anet: the extended card %s sent was dropped: %v", from, err)
		finish(extFetchResult{Outcome: extBadSignature, Why: priceWhyBadSignature})
		return false
	}
	variant := string(module.VariantGeneral)
	if f.public != nil && hasSkillBeyond(c.Skills, f.public) {
		variant = string(module.VariantList)
	}
	if d.ix != nil {
		if err := d.ix.PutPeerExtCard(interactions.PeerExtCard{AID: from, Card: c.Bytes, PublicSeq: c.PublicSeq,
			KeyStateSeq: c.KeyStateSeq, Variant: variant, Prices: c.Prices, FetchedAt: c.FetchedAt}); err != nil {
			d.count(cntCardCacheWrite)
			log.Printf("anet: keep the extended card of %s: %v", from, err)
		}
	}
	finish(extFetchResult{Outcome: extGot, Card: c})
	d.signedPriceArrived(from)
	return true
}

// hasSkillBeyond reports whether skills has an id public does not.
func hasSkillBeyond(skills, public []string) bool {
	in := map[string]bool{}
	for _, id := range public {
		in[id] = true
	}
	for _, id := range skills {
		if !in[id] {
			return true
		}
	}
	return false
}

// extendedForDisplay is the extended card a display shows for aid (PAY-C10
// case 1): nothing when rule 2 does not hold (ra.ExtendedOffered), the
// cached card when it is fresh, and otherwise a fetch.
func (d *Daemon) extendedForDisplay(ctx context.Context, ra module.RemoteAgent, facts *publicCardFacts) extFetchResult {
	if ra.Verification != cardVerified || facts == nil || !ra.ExtendedOffered {
		return extFetchResult{Outcome: extNotOffered, Why: priceWhyNotOffered}
	}
	if c, ok := d.cachedExtCard(ra.AID, facts.KEL, d.nowMS()); ok {
		return extFetchResult{Outcome: extGot, Card: c}
	}
	public := facts.Skills
	if public == nil {
		public = []string{}
	}
	return d.fetchExtendedCard(ctx, ra.AID, public)
}

// agentCardWithPrices is /agents/card (and MCP get_agent_card, anet card):
// agentCard, and for a VERIFIED card the extended card when CORE-K23 rule
// 2 holds — charge, prices and when they were fetched from it, or the
// sentence of extended_card_unavailable when it should have been had and
// was not (HPP §2.7). The fetch's outcome is returned for the local A2A
// interface, which answers each one with its own error (§4.7).
func (d *Daemon) agentCardWithPrices(ctx context.Context, aid string) (module.RemoteAgent, extFetchResult, error) {
	ra, facts, err := d.resolveAgentCard(ctx, aid)
	if err != nil {
		return ra, extFetchResult{}, err
	}
	res := d.extendedForDisplay(ctx, ra, facts)
	applyExtended(&ra, res, a2ashape.Lang())
	return ra, res, nil
}

// applyExtended records on ra what a display shows of res (HPP §2.7,
// CORE-K23 rule 4): the extended card's charge and every skill's price,
// "0" for a free one, when it was had; the unavailable sentence when it
// should have been had and was not; nothing else.
func applyExtended(ra *module.RemoteAgent, res extFetchResult, lang a2ashape.Language) {
	switch res.Outcome {
	case extGot:
		c := res.Card
		ra.Charge = c.Charge
		ra.Prices = make([]module.SkillPrice, 0, len(c.Skills))
		for _, id := range c.Skills {
			ra.Prices = append(ra.Prices, module.SkillPrice{Skill: id, Amount: strconv.FormatUint(c.Prices[id], 10), Unit: "credits"})
		}
		ra.PricesFetchedAt = time.UnixMilli(int64(c.FetchedAt)).UTC().Format("2006-01-02T15:04:05.000Z07:00")
	case extBusy, extNoAnswer, extNotSent:
		ra.PriceNote = a2ashape.Sentence("extended_card_unavailable", lang, nil)
	case extBadSignature:
		ra.PriceNote = a2ashape.Sentence("extended_card_unavailable.bad_signature", lang, nil)
	}
}

// extendedRefusal is the local A2A interface's answer to a fetch that did
// not give a card (HPP §4.7, CORE-K23 rule 4): the refusal of its code,
// with the Retry-After of a passing one; nil for extGot.
func extendedRefusal(res extFetchResult) *a2ashape.Refusal {
	switch res.Outcome {
	case extGot:
		return nil
	case extNotOffered:
		return a2ashape.Refuse("extended_card_not_offered", nil)
	case extNotConfigured:
		return a2ashape.Refuse("extended_card_not_configured", nil)
	case extBusy:
		r := a2ashape.Refuse("extended_card_unavailable", nil)
		r.RetryAfter = cardBusyRetry
		return r
	case extBadSignature:
		return a2ashape.Refuse("extended_card_unavailable.bad_signature", nil)
	}
	r := a2ashape.Refuse("extended_card_unavailable", nil)
	r.RetryAfter = cardUnavailableRetry
	return r
}

// cardPrice is the price an agent signed for one capability, as a payment
// decision reads it (PAY-D2 steps 3 and 4).
type cardPrice struct {
	// Have: a verified extended card was had. Price is then the capability's
	// signed price, 0 when the card does not list it (PAY-D2 step 4).
	Have  bool
	Price uint64
	// Why is the short reason of card_price_unavailable when !Have.
	Why string
}

// signedPriceFor is the price aid signed for capID, for a quote of quote
// credits (PAY-C10 cases 2 and 3): the cached card when it is fresh, else
// a fetch — without CORE-K23 rule 2, which binds only a display. A quote
// above the price of a card more than a minute old asks again, once.
func (d *Daemon) signedPriceFor(ctx context.Context, aid, capID string, quote uint64) cardPrice {
	kel, _ := d.peerKEL(aid)
	c, ok := d.cachedExtCard(aid, kel, d.nowMS())
	if !ok {
		res := d.fetchExtendedCard(ctx, aid, nil)
		if res.Outcome != extGot {
			return cardPrice{Why: res.Why}
		}
		c = res.Card
	} else if price := c.Prices[capID]; quote > price && d.nowMS()-min(d.nowMS(), c.FetchedAt) > uint64(priceRefetchAge.Milliseconds()) {
		if res := d.fetchExtendedCard(ctx, aid, nil); res.Outcome == extGot {
			c = res.Card
		}
	}
	return cardPrice{Have: true, Price: c.Prices[capID]}
}
