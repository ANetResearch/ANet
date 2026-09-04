package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/adp"
)

// A registration says what this node offers. Nothing made that claim
// attributable to the node making it.
//
// The register challenge signs an action, an AID and a timestamp — it
// proves the caller holds the key, and covers none of what was
// registered. A hub could therefore change an agent's name or capability
// list and no party could tell, including the agent. That is tolerable
// for one hub a node chose to trust; it is not tolerable for a directory
// other people search, and it is fatal once directories federate, where
// the whole point is that a peer hub can hide a card but never invent
// one.
//
// So the node signs a card. ADP already defines it — subject, capability
// list, monotonic seq, detached JWS — and ANetCore already implements
// signing, admission and the high-water rule, so this is a use of the
// protocol rather than a new object.
const cardSchemaMajor = 1

// ExtPricing is the card extension a provider publishes its price list
// under: capability id → credits, as a JSON number.
//
// It lives inside the signature deliberately. A hub that hosts an x402
// resource server on a provider's behalf has to quote a price before the
// provider is involved at all, and if the hub were the source of that
// number it could quote whatever it liked and settle it. Reading the
// price out of the provider's own signed card means the worst a hub can
// do is refuse to sell — it cannot sell at a price the provider never
// agreed to, because it cannot produce the signature.
//
// Absent means "not for sale through a gateway", not "free": a provider
// that charges but publishes nothing simply cannot be bought that way,
// and the ordinary delegate path quotes it directly.
const ExtPricing = "anet.pricing"

// EndpointRedeem is the endpoint protocol name for this node's public
// voucher face — where a buyer who paid at the hub brings the voucher.
//
// Published in the card, and so inside the signature, for the same reason
// the prices are: a hub that could nominate this address could point
// buyers at a machine of its own, which is exactly the proxying the
// voucher design exists to avoid. The node says where it lives, or it
// cannot be sold this way at all.
const EndpointRedeem = "x402-redeem"

// signedCard mints and signs this node's current card.
func (d *Daemon) signedCard(name string, caps []string) (json.RawMessage, error) {
	return d.signedCardWithPrices(name, caps, d.priceList(caps))
}

// publicRedeemURL is the address buyers use, or empty.
//
// Deliberately NOT derived from voucher_addr. A listen address is often
// 0.0.0.0 or a container-internal port, and publishing one of those in a
// signed card would advertise an address nobody outside can reach — the
// operator has to say what the world sees, because only the operator
// knows. Configured separately and absent by default.
func (d *Daemon) publicRedeemURL() string {
	if p := d.payer(); p != nil {
		return strings.TrimSpace(p.RedeemURL())
	}
	return ""
}

// priceList asks the registry what each advertised capability costs.
//
// Only what this node actually serves and actually charges for. A price
// for a capability the node does not offer would be an invitation to buy
// something that cannot be delivered.
func (d *Daemon) priceList(caps []string) map[string]uint64 {
	if d.providers == nil {
		return nil
	}
	out := map[string]uint64{}
	for _, capID := range caps {
		p, ok := d.providers.Resolve(capID)
		if !ok {
			continue
		}
		if price, priced := priceOfCapability(p, capID); priced {
			out[capID] = price
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cardSeq is this node's next card sequence: seeded from the clock,
// guaranteed to exceed every sequence this process has already minted.
//
// ADP admits a card only if its seq EXCEEDS the highest the hub has seen
// for that subject, which is what makes a replayed older card a no-op
// rather than a rollback. The number was the registration time in
// seconds, and the comment here claimed that "two registrations a second
// apart cannot tie" — true, and beside the point. Two registrations in
// the SAME second tie, and are then refused with STALE_SEQ.
//
// That is not an edge case. `anet hub-register` twice in a row does it;
// so does a node that registers and immediately re-registers after its
// capability list changes. The second call failed for a reason that had
// nothing to do with what it was doing. Found by the admission joint test
// (scripts/joint-invite.sh), whose "an existing node re-registers without
// an invite" step failed against a hub that had nothing to do with it.
//
// The fix belongs here rather than in the hub: the high-water rule is
// correct, and the party that must mint strictly increasing numbers is
// the party that signs them.
//
// Seconds are kept as the unit on purpose. Switching to milliseconds
// would also break the tie, and would multiply every sequence by a
// thousand — after which a node that ever downgraded to an older build
// could never update its card again, because its second-resolution
// sequence would sit permanently below the hub's high water. The counter
// only ever advances past real time when there are ties, by one per tie,
// and drifts back to the clock as soon as they stop.
//
// The counter is also PERSISTED, which the first version of this fix left
// out on the reading that a restart takes longer than the one-second
// resolution. It does not: a restart inside the same second starts a fresh
// in-memory counter from the same clock value and collides with the hub's
// high water. That was survivable while registration only happened when an
// operator asked for it; once the daemon began re-publishing its
// capabilities at startup, every quick restart hit it.
//
// Persisting also closes the other half — a clock that steps BACKWARDS
// across a restart no longer lands below the high water, because the file
// remembers where the node had got to.
func (d *Daemon) cardSeq() uint64 {
	for {
		last := d.lastCardSeq.Load()
		seq := uint64(time.Now().Unix())
		if seq <= last {
			seq = last + 1
		}
		if d.lastCardSeq.CompareAndSwap(last, seq) {
			d.persistCardSeq(seq)
			return seq
		}
	}
}

// cardSeqPath is where the counter survives a restart. Its own file rather
// than a config field: it is written on every registration and read once,
// and rewriting the whole config for a counter would put an operator's
// hand-edited settings in the path of a background refresh.
func (d *Daemon) cardSeqPath() string { return filepath.Join(d.layout.Root, "card_seq") }

func (d *Daemon) persistCardSeq(seq uint64) {
	if err := os.WriteFile(d.cardSeqPath(), []byte(strconv.FormatUint(seq, 10)), 0o600); err != nil {
		// Not fatal: the clock still supplies a value, and the failure mode
		// is the one this file exists to narrow, not a new one.
		log.Printf("anet: could not persist the card sequence: %v", err)
	}
}

// loadCardSeq seeds the counter from the last sequence this node minted.
func (d *Daemon) loadCardSeq() {
	b, err := os.ReadFile(d.cardSeqPath())
	if err != nil {
		return
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return
	}
	d.lastCardSeq.Store(n)
}

func (d *Daemon) signedCardWithPrices(name string, caps []string, prices map[string]uint64) (json.RawMessage, error) {
	now := time.Now()
	card := &adp.AgentCard{
		SubjectDID:         d.AID(),
		CardSchema:         adp.CardSchema{Major: cardSchemaMajor},
		Seq:                d.cardSeq(),
		IssuedAt:           now.Unix(),
		NotBefore:          now.Add(-1 * time.Minute).Unix(),
		Capabilities:       caps,
		CriticalExtensions: []string{},
		Name:               name,
	}
	if card.Capabilities == nil {
		card.Capabilities = []string{}
	}
	if url := d.publicRedeemURL(); url != "" {
		card.Endpoints = append(card.Endpoints, adp.EndpointDesc{
			Protocol: EndpointRedeem, URI: url, Methods: []string{"POST"},
		})
	}
	if len(prices) > 0 {
		asAny := make(map[string]any, len(prices))
		for k, v := range prices {
			asAny[k] = v
		}
		card.Extensions = map[string]any{ExtPricing: asAny}
	}
	if err := card.Sign(d.self); err != nil {
		return nil, fmt.Errorf("anet: sign agent card: %w", err)
	}
	b, err := json.Marshal(card)
	if err != nil {
		return nil, err
	}
	return b, nil
}
