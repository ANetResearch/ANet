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
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	// same holds, per variant, the digest of what it was last made from
	// when that made it the same as the public card: the same content again
	// is answered from here, without building it (W4 PAY-10 7).
	same map[module.CardVariant][32]byte
	// builds counts the times both variants were made or looked up
	// (extendedCards), and templates the variant templates built, so a
	// test can tell a request that built nothing.
	builds, templates atomic.Uint64
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
	key extCardKey
	// input is the digest of what the card was made from (extInputDigest):
	// the same input under the same public seq and key state is the same
	// card, given without building it again.
	input [32]byte
	card  []byte
}

// extInputDigest is the digest of what variant v is made from: the public
// card's input without its seq and times, and the modules' contributions
// to the variant. Equal digests make equal templates.
func extInputDigest(tmpl netcard.Input, v module.CardVariant, exts []map[string]any) ([32]byte, error) {
	in, err := json.Marshal(tmpl)
	if err != nil {
		return [32]byte{}, err
	}
	ex, err := json.Marshal(exts)
	if err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	for _, part := range [][]byte{[]byte(v), in, ex} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
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
//
// The per-sender limit is judged before anything is made: a sender over it
// is answered from what is already known — not_configured when its variant
// was last found to be the public card (rule 1 still comes first), busy
// otherwise — with nothing built, signed or checked for it (W4 PAY-10 7),
// so asking faster costs this node no more than the count.
func (d *Daemon) answerCardGet(sender string) delegation.CardResp {
	recent := d.noteCardRequest(sender, d.nowMS())
	listed := d.readPeers().allowed(sender)
	if recent > cardRequestsPerWindow {
		if d.knownSame(listed) {
			return delegation.CardResp{Error: delegation.CardNotConfigured}
		}
		return delegation.CardResp{Error: delegation.CardBusy}
	}
	general, list := d.extendedCards()
	return cardAnswer(cardAnswerInput{Listed: listed, Recent: recent, General: general, List: list})
}

// knownSame reports whether the variant a sender gets (the list variant
// for a listed one) was last found to say nothing the public card does
// not.
func (d *Daemon) knownSame(listed bool) bool {
	v := module.VariantGeneral
	if listed {
		v = module.VariantList
	}
	d.extCard.mu.Lock()
	defer d.extCard.mu.Unlock()
	_, ok := d.extCard.same[v]
	return ok
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
	// The requests in the window: after since and not after now. A time
	// after now was noted before the wall clock stepped back; counting it
	// would keep the sender busy until the clock caught up again.
	keep := func(ts []uint64) []uint64 {
		var out []uint64
		for _, t := range ts {
			if t > since && t <= now {
				out = append(out, t)
			}
		}
		return out
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
	d.extCard.builds.Add(1)
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
	// key otherwise. What it is made from is looked up first: the same
	// input as last time is the same conclusion, or the same card under the
	// same public seq and key state, and is not built again.
	tmpl := in
	tmpl.Seq, tmpl.IssuedAtMs, tmpl.NotBeforeMs = 0, 0, 0
	keySeq, err := identity.SeqOf(signer)
	if err != nil {
		return fail("sign", err)
	}
	input, err := extInputDigest(tmpl, v, exts)
	if err != nil {
		return fail("build", err)
	}
	d.extCard.mu.Lock()
	if h, ok := d.extCard.same[v]; ok && h == input {
		d.extCard.mu.Unlock()
		return extVariant{Same: true}
	}
	if c := d.extCard.signed[v]; c != nil && c.input == input && c.key.pubSeq == pubSeq && c.key.keySeq == keySeq {
		d.extCard.mu.Unlock()
		return extVariant{Card: c.card}
	}
	d.extCard.mu.Unlock()
	d.extCard.templates.Add(1)
	template, same, err := netcard.BuildExtended(tmpl, v, exts)
	if err != nil {
		return fail("build", err)
	}
	if same {
		d.extCard.mu.Lock()
		if d.extCard.same == nil {
			d.extCard.same = map[module.CardVariant][32]byte{}
		}
		d.extCard.same[v] = input
		d.extCard.mu.Unlock()
		return extVariant{Same: true}
	}
	payload, err := a2acard.SigningPayload(template)
	if err != nil {
		return fail("build", err)
	}
	key := extCardKey{pubSeq: pubSeq, variant: v, digest: sha256.Sum256(payload), keySeq: keySeq}

	d.extCard.mu.Lock()
	defer d.extCard.mu.Unlock()
	delete(d.extCard.same, v)
	if c := d.extCard.signed[v]; c != nil && c.key == key {
		c.input = input
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
	d.extCard.signed[v] = &signedExtCard{key: key, input: input, card: signed}
	return extVariant{Card: signed}
}

// allowOnlyCapabilityIDs are the capabilities this node serves only to the
// callers in allow ∪ trust (inbound.public_capabilities[].allow_only,
// CUX-F12), which the list variant lists.
func (d *Daemon) allowOnlyCapabilityIDs() []string {
	return d.config().inbound().allowOnlyIDs()
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

// unchargeableCount is how many capabilities carry a price, public or for
// the allow and trust lists only, on a node that has no payment module: the
// number doctor's payments.unchargeable tells (MOD-2). Calls to them are
// refused (capability.go); the count exists so the operator learns it before
// a caller does. 0 when the node can charge.
func (d *Daemon) unchargeableCount() int {
	if d.payer() != nil || d.providers == nil {
		return 0
	}
	seen := map[string]bool{}
	n := 0
	ids := make([]string, 0, 8)
	for _, p := range d.config().inbound().PublicCapabilities {
		ids = append(ids, p.ID)
	}
	ids = append(ids, d.allowOnlyCapabilityIDs()...)
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || seen[id] {
			continue
		}
		seen[id] = true
		if prov, ok := d.providers.Resolve(id); ok {
			if _, priced := priceOfCapability(prov, id); priced {
				n++
			}
		}
	}
	return n
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
// this node has no priced capability, public or for the allow and trust
// lists only. Otherwise, in order: every price is between 1 and 2^63-1
// (payments.price_invalid, the first one that is not); a node with no
// public card — the priced capabilities are all allow_only and nothing is
// public — is warned that it has no extended card either, so its listed
// callers pay each task by hand (payments.card_no_public); the public card
// declares an extended card; and each variant is made, verifies under this
// node's own KEL and states exactly the prices of its skills
// (payments.card_failed, with variant and step).
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
	// An A2A card lists at least one skill: with nothing public (no
	// public capability, no chat skill) there is no public card, and no
	// extended card is asked for. Not a failure to report (2026-10-10, the
	// owner's ruling on PAY-10's record): a choice to tell the operator of.
	if len(public) == 0 && !d.publishesChat() {
		return &cardCheck{Status: "warn", Reason: "payments.card_no_public"}
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
