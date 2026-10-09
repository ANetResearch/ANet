package daemon

// extcard.go is the provider side of the extended card (HPP §4.3.1,
// §4.3.2; core CORE-K23): the AgentCard this node signs and gives a caller
// that asks for it end to end (anet.card.get/1), with the prices of its
// skills and, for a caller on its allow or trust list, the capabilities it
// serves to those lists only. The public card (a2a_card.go) never carries
// either; it says only that an extended card may be asked for.
//
// Two variants exist at a time (internal/netcard): general, for every
// caller, and list, for the callers in allow ∪ trust. A peer on the deny list
// gets the general one, as a stranger does. Each is signed under the key at
// the head of this node's KEL (a2acard.SignWithSnapshot, KEL C-23), without
// a jku, and kept until something it says changes: the public card's seq,
// the variant's content (its skills and prices among the rest), or the key
// state. So after a rotation the next answer is signed under the new key,
// with no hook of its own (PAY-C4).
//
// An answer neither creates nor changes an interaction, is not held for
// approval and is not a task of the inbound policy (PAY-C9). It is
// cardAnswer's: the variant the caller gets, unless that variant says
// nothing the public card does not (not_configured), or the caller has
// asked more than six times in a minute (busy).

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/netcard"
	"github.com/ANetResearch/ANet/module"
)

// Per-sender limit of card requests (PAY-C8 rule 2, HPP §0.4): at most
// cardRequestsPerWindow in cardRequestWindow; the next is answered busy.
const (
	cardRequestsPerWindow = 6
	cardRequestWindow     = time.Minute
	// cardRequestSenders bounds the senders the window remembers. Past it the
	// one heard from least recently is forgotten; the hub's per-recipient
	// buckets bound how many distinct senders can ask in a minute anyway.
	cardRequestSenders = 4096
)

// extCardState is this node's signed extended cards and who asked for one
// lately. Lock order: netCard.mu, then mu.
type extCardState struct {
	mu     sync.Mutex
	signed map[module.CardVariant]*signedExtCard
	// allowOnly stands in for the capabilities served to allow ∪ trust only
	// until the configuration says which they are (allowOnlyCapabilityIDs).
	allowOnly []string

	reqMu    sync.Mutex
	requests map[string][]uint64 // sender -> times (unix ms) of its requests in the window
}

// extCardKey is what a signed extended card was made from: when any part
// differs, the card is made again.
type extCardKey struct {
	pubSeq  uint64             // the public card's seq, which the card repeats
	variant module.CardVariant //
	digest  [32]byte           // the unsigned card without seq and times: its skills, prices and every other member
	keySeq  uint64             // the key state it is signed under
}

type signedExtCard struct {
	key  extCardKey
	card []byte
}

// extVariant is one variant as cardAnswer sees it.
type extVariant struct {
	// Card is the signed extended card. Nil when Same, or when it could not
	// be made (Err).
	Card []byte
	// Same: the variant says nothing the public card does not — no price and
	// no skill beyond it (CORE-K23 rule 1) — so there is no extended card.
	Same bool
	// Err is why the card could not be made; doctor's payments.card reports it.
	Err error
}

// extCardError is a failure to make an extended card, with the step.
type extCardError struct {
	variant module.CardVariant
	step    string // build, sign, size or verify
	err     error
}

func (e *extCardError) Error() string {
	return fmt.Sprintf("anet: the %s extended card: %s: %v", e.variant, e.step, e.err)
}
func (e *extCardError) Unwrap() error { return e.err }

// code is the {code} of payments.card_failed: variant and step, in a
// code's form (a2ashape shows nothing else there).
func (e *extCardError) code() string { return string(e.variant) + "_" + e.step }

// cardAnswerInput is everything cardAnswer decides on.
type cardAnswerInput struct {
	// Listed: the sender is in allow ∪ trust and not denied.
	Listed bool
	// Recent is the sender's card requests in the last minute, this one
	// included.
	Recent int
	// General and List are the two variants at this moment.
	General, List extVariant
}

// cardAnswer is the provider's answer to anet.card.get/1 (PAY-C8): the
// variant for the sender — list for allow ∪ trust, general for everyone
// else, deny included — and then, in order:
//
//  1. that variant is the public card → not_configured;
//  2. the sender asked more than six times in the last minute → busy;
//  3. otherwise the variant's signed card.
//
// A variant that could not be made is answered busy: the caller shows the
// card as unavailable for now and asks again later, and doctor's
// payments.card tells the operator why. Answering not_configured would tell
// the caller this node has no prices, which is not known to be so.
func cardAnswer(in cardAnswerInput) delegation.CardResp {
	v := in.General
	if in.Listed {
		v = in.List
	}
	switch {
	case v.Same:
		return delegation.CardResp{Error: delegation.CardNotConfigured}
	case in.Recent > cardRequestsPerWindow:
		return delegation.CardResp{Error: delegation.CardBusy}
	case len(v.Card) == 0:
		return delegation.CardResp{Error: delegation.CardBusy}
	}
	return delegation.CardResp{Card: v.Card}
}

// answerCardGet is this node's answer to an anet.card.get/1 from sender,
// for the receive pipeline to send back under the one-shot grant the
// request carried (engine §4.5; ENG-K2: no grant, no answer). It touches no
// interaction.
func (d *Daemon) answerCardGet(sender string) delegation.CardResp {
	recent := d.noteCardRequest(sender, d.nowMS())
	listed := d.readPeers().allowed(sender)
	general, list := d.extendedCards()
	return cardAnswer(cardAnswerInput{Listed: listed, Recent: recent, General: general, List: list})
}

// noteCardRequest records a card request from sender at now (unix ms) and
// returns how many it made in the window, this one included.
func (d *Daemon) noteCardRequest(sender string, now uint64) int {
	s := &d.extCard
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	if s.requests == nil {
		s.requests = map[string][]uint64{}
	}
	since := now - min(now, uint64(cardRequestWindow.Milliseconds()))
	keep := func(ts []uint64) []uint64 {
		i := 0
		for i < len(ts) && ts[i] <= since {
			i++
		}
		return ts[i:]
	}
	ts := keep(s.requests[sender])
	if len(ts) == 0 && len(s.requests) >= cardRequestSenders {
		// Forget the senders whose window has passed, and if that is not
		// enough the one heard from least recently.
		var oldest string
		var oldestAt uint64
		for k, v := range s.requests {
			if v = keep(v); len(v) == 0 {
				delete(s.requests, k)
				continue
			}
			if last := v[len(v)-1]; oldest == "" || last < oldestAt {
				oldest, oldestAt = k, last
			}
		}
		if len(s.requests) >= cardRequestSenders {
			delete(s.requests, oldest)
		}
	}
	// A sender over the limit is not remembered for more of its requests
	// than the limit needs: the window still covers its last minute.
	if len(ts) > cardRequestsPerWindow {
		ts = ts[len(ts)-cardRequestsPerWindow:]
	}
	ts = append(ts, now)
	s.requests[sender] = ts
	return len(ts)
}

// extendedCards returns both variants as they are now, made or taken from
// what was signed before. A node without a hub or without a public card
// has no extended card to give: both are the same as (no) public card.
func (d *Daemon) extendedCards() (general, list extVariant) {
	same := extVariant{Same: true}
	cfg := d.config()
	hub := strings.TrimRight(strings.TrimSpace(cfg.HubURL), "/")
	if hub == "" {
		return same, same
	}
	d.netCard.mu.Lock()
	defer d.netCard.mu.Unlock()
	_, pubSeq, in, err := d.issueNetworkCardLocked(hub, d.issuedCardNameLocked(cfg.Name), false)
	switch {
	case errors.Is(err, errNoPublicSkill):
		return same, same
	case err != nil:
		return extVariant{Err: &extCardError{variant: module.VariantGeneral, step: "build", err: err}},
			extVariant{Err: &extCardError{variant: module.VariantList, step: "build", err: err}}
	}
	// One key state for both: the kid that names it and the key that signs.
	signer := d.self.Snapshot()
	return d.extendedCardLocked(in, pubSeq, signer, module.VariantGeneral),
		d.extendedCardLocked(in, pubSeq, signer, module.VariantList)
}

// issuedCardNameLocked is the name on the public card last issued, or
// fallback when there is none: the extended cards repeat the public card as
// it was published, and a registration may have published it under another
// name than the configured one (cardForRegistration). netCard.mu is held.
func (d *Daemon) issuedCardNameLocked(fallback string) string {
	d.loadIssuedCardLocked()
	if d.netCard.issued == nil {
		return fallback
	}
	var c struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(d.netCard.issued.Card, &c) != nil || c.Name == "" {
		return fallback
	}
	return c.Name
}

// extendedCardLocked makes variant v of the extended card for the public
// card in (issued as pubSeq), or returns the one signed before when nothing
// it says has changed. netCard.mu is held.
func (d *Daemon) extendedCardLocked(in netcard.Input, pubSeq uint64, signer identity.Signer, v module.CardVariant) extVariant {
	fail := func(step string, err error) extVariant {
		return extVariant{Err: &extCardError{variant: v, step: step, err: err}}
	}
	skills, err := netcard.VariantSkills(in, v)
	if err != nil {
		return fail("build", err)
	}
	cc := module.CardContext{AID: d.AID(), HubURL: strings.TrimRight(strings.TrimSpace(in.HubURL), "/"),
		Skills: netcard.SkillIDs(skills), Extended: true, Variant: v}
	exts, _ := d.cardContributions(cc)

	// The template, without seq and times, is the variant's content: equal to
	// the public card's template when it says nothing more, and the cache
	// key otherwise.
	tmpl := in
	tmpl.Seq, tmpl.IssuedAtMs, tmpl.NotBeforeMs = 0, 0, 0
	template, same, err := netcard.BuildExtended(tmpl, v, exts)
	if err != nil {
		return fail("build", err)
	}
	if same {
		return extVariant{Same: true}
	}
	payload, err := a2acard.SigningPayload(template)
	if err != nil {
		return fail("build", err)
	}
	keySeq, err := identity.SeqOf(signer)
	if err != nil {
		return fail("sign", err)
	}
	key := extCardKey{pubSeq: pubSeq, variant: v, digest: sha256.Sum256(payload), keySeq: keySeq}

	d.extCard.mu.Lock()
	defer d.extCard.mu.Unlock()
	if c := d.extCard.signed[v]; c != nil && c.key == key {
		return extVariant{Card: c.card}
	}
	now := d.nowMS()
	full := in
	full.Seq = pubSeq
	full.IssuedAtMs = now
	full.NotBeforeMs = now - min(now, uint64(cardNotBeforeSkew.Milliseconds()))
	unsigned, _, err := netcard.BuildExtended(full, v, exts)
	if err != nil {
		return fail("build", err)
	}
	signed, err := a2acard.SignWithSnapshot(unsigned, signer, "")
	if err != nil {
		return fail("sign", err)
	}
	if len(signed) > delegation.MaxCardBytes {
		return fail("size", fmt.Errorf("%d bytes, a card answer holds at most %d", len(signed), delegation.MaxCardBytes))
	}
	// What a caller checks (PAY-C13), checked here first: a card this node
	// made wrongly is an error at the source, with the reason.
	if _, err := a2acard.Verify(signed, d.selfKELResolver, now); err != nil {
		return fail("verify", err)
	}
	if d.extCard.signed == nil {
		d.extCard.signed = map[module.CardVariant]*signedExtCard{}
	}
	d.extCard.signed[v] = &signedExtCard{key: key, card: signed}
	return extVariant{Card: signed}
}

// allowOnlyCapabilityIDs are the capabilities this node serves only to the
// callers in allow ∪ trust (CUX-F12), which the list variant lists.
//
// v030-todo(UX-4): read inbound.public_capabilities[].allow_only; until
// then only a test sets them.
func (d *Daemon) allowOnlyCapabilityIDs() []string {
	d.extCard.mu.Lock()
	defer d.extCard.mu.Unlock()
	return append([]string(nil), d.extCard.allowOnly...)
}

// listOnlySkillIDs are the skills the list variant adds to the public
// ones: the allow_only capabilities this node serves, under the rules
// publicSkillIDs applies (a provider serves it, a priced one only where
// this node can charge, not refused to remote callers), none that is
// already public, sorted, and no more than a card holds beside the public
// skills.
func (d *Daemon) listOnlySkillIDs(public []string) []string {
	if d.providers == nil {
		return nil
	}
	canCharge := d.payer() != nil
	remote := d.remoteCheck()
	seen := map[string]bool{}
	for _, id := range public {
		seen[id] = true
	}
	var out []string
	for _, id := range d.allowOnlyCapabilityIDs() {
		if strings.TrimSpace(id) == "" || seen[id] {
			continue
		}
		seen[id] = true
		prov, ok := d.providers.Resolve(id)
		if !ok {
			continue
		}
		if _, priced := priceOfCapability(prov, id); priced && !canCharge {
			continue
		}
		if remote.refused(id) {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	if room := a2acard.MaxSkills - len(public); len(out) > room {
		log.Printf("anet: %d capabilities for the allow and trust lists; the extended card lists the first %d", len(out), max(room, 0))
		out = out[:max(room, 0)]
	}
	return out
}

// cardCheck is doctor's payments.card as the daemon finds it (GET
// /status?doctor=1): ok, or fail with the reason-table key and its
// arguments.
type cardCheck struct {
	Status string            `json:"status"`
	Reason string            `json:"reason,omitempty"`
	Args   map[string]string `json:"args,omitempty"`
}

// paymentsCardCheck is doctor's payments.card (HPP §5.6, ux §5.2), nil when
// this node lists no priced skill on any card. Otherwise, in order: every
// price is between 1 and 2^63-1 (payments.price_invalid, the first one
// that is not); the public card declares an extended card; and each
// variant is made, verifies under this node's own KEL and states exactly
// the prices of its skills (payments.card_failed, with variant and step).
func (d *Daemon) paymentsCardCheck() *cardCheck {
	p := d.payer()
	if p == nil || strings.TrimSpace(d.config().HubURL) == "" {
		return nil
	}
	public := d.publicSkillIDs()
	listOnly := d.listOnlySkillIDs(public)
	prices := map[string]uint64{}
	var priced []string
	for _, id := range append(append([]string(nil), public...), listOnly...) {
		if n, ok := p.Price(id); ok {
			prices[id] = n
			priced = append(priced, id)
		}
	}
	if len(priced) == 0 {
		return nil
	}
	sort.Strings(priced)
	for _, id := range priced {
		if !netcard.ValidPrice(prices[id]) {
			return &cardCheck{Status: "fail", Reason: "payments.price_invalid", Args: map[string]string{"name": id}}
		}
	}
	failed := func(code string) *cardCheck {
		return &cardCheck{Status: "fail", Reason: "payments.card_failed", Args: map[string]string{"code": code}}
	}
	card, err := d.NetworkCard()
	if err != nil {
		return failed("public_build")
	}
	if ext, err := netcard.DeclaresExtended(card); err != nil || !ext {
		return failed("public_extended")
	}
	general, list := d.extendedCards()
	for _, c := range []struct {
		v      module.CardVariant
		got    extVariant
		skills []string
	}{
		{module.VariantGeneral, general, public},
		{module.VariantList, list, append(append([]string(nil), public...), listOnly...)},
	} {
		if c.got.Err != nil {
			var e *extCardError
			if errors.As(c.got.Err, &e) {
				return failed(e.code())
			}
			return failed(string(c.v) + "_build")
		}
		want := map[string]uint64{}
		for _, id := range c.skills {
			if n, ok := prices[id]; ok {
				want[id] = n
			}
		}
		got := map[string]uint64{}
		if !c.got.Same {
			if got, err = netcard.PricesOf(c.got.Card); err != nil {
				return failed(string(c.v) + "_prices")
			}
		}
		if !samePrices(got, want) {
			return failed(string(c.v) + "_prices")
		}
	}
	return &cardCheck{Status: "ok"}
}

func samePrices(a, b map[string]uint64) bool {
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
