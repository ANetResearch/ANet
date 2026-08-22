package daemon

import (
	"encoding/json"
	"fmt"
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
//
// Seq is the registration time in seconds. ADP admits a card only if its
// seq exceeds the highest already seen for that subject, which makes a
// replayed older card a no-op rather than a rollback — and using the
// clock means two registrations a second apart cannot tie.
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

func (d *Daemon) signedCardWithPrices(name string, caps []string, prices map[string]uint64) (json.RawMessage, error) {
	now := time.Now()
	card := &adp.AgentCard{
		SubjectDID:         d.AID(),
		CardSchema:         adp.CardSchema{Major: cardSchemaMajor},
		Seq:                uint64(now.Unix()),
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
